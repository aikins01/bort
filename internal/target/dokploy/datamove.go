package dokploy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aikins01/bort/internal/preparer"
	"github.com/aikins01/bort/internal/safepath"
)

const composeServiceLabel = "com.docker.compose.service"

// applySyncVolume mirrors a source-side volume into the matching dokploy
// target volume. named volumes copy via tar/busybox; bind mounts copy
// via rsync. raw copy clobbers the target volume's contents, so the
// dokploy-managed target container is stopped for the duration so it
// doesn't see files disappear and reappear underneath it.
func (c *Client) applySyncVolume(ctx context.Context, actx *applyContext, step Step) error {
	app, ok := findPrepareApp(actx.plan.Prepare, step.App)
	if !ok {
		return fmt.Errorf("app %s not found in prepare result", step.App)
	}
	volume, ok := findPrepareVolume(app, step.Ref)
	if !ok {
		return fmt.Errorf("volume %s for app %s not found", step.Ref, step.App)
	}
	if volumeOwnedByLogicalOrSkippedStore(app, volume) {
		// logical-dump stores own their migration; skip-strategy stores
		// must keep the target empty. raw copy in either case is wrong.
		return nil
	}

	runner := c.dockerRunner()
	target, err := c.targetContainerForService(ctx, runner, actx, step.App, volume.Service)
	if err != nil {
		return err
	}
	copy, err := c.planVolumeCopy(ctx, runner, volume, target)
	if err != nil {
		return err
	}
	return withTargetStopped(ctx, runner, target, copy)
}

// planVolumeCopy resolves source/target locations and returns the copy
// closure to invoke once the target container is stopped.
func (c *Client) planVolumeCopy(ctx context.Context, runner dockerRunner, volume preparer.VolumeResource, target dockerContainer) (func() error, error) {
	switch volume.Type {
	case "volume":
		mount, ok := findMountByTarget(target, volume.Target)
		if !ok || mount.Type != "volume" || mount.Name == "" {
			return nil, fmt.Errorf("named volume for target %s not found on dokploy compose container", volume.Target)
		}
		srcVolName, err := resolveSourceVolume(ctx, runner, volume)
		if err != nil {
			return nil, err
		}
		dstVolName := mount.Name
		return func() error { return copyNamedVolume(ctx, runner, srcVolName, dstVolName) }, nil
	case "bind":
		mount, ok := findMountByTarget(target, volume.Target)
		if !ok || mount.Type != "bind" || mount.Source == "" {
			return nil, fmt.Errorf("bind mount for target %s not found on dokploy compose container", volume.Target)
		}
		if !mount.RW {
			return nil, fmt.Errorf("dokploy bind mount %s -> %s is read-only; refusing to sync", mount.Source, volume.Target)
		}
		srcPath, err := resolveSourceBindPath(ctx, runner, volume)
		if err != nil {
			return nil, err
		}
		dstPath := mount.Source
		return func() error { return rsyncBindMount(ctx, runner, srcPath, dstPath) }, nil
	default:
		return nil, fmt.Errorf("%w: volume type %q is not supported", ErrNotImplemented, volume.Type)
	}
}

// withTargetStopped runs op while the dokploy target container is
// stopped so raw volume rewrites don't race with a live service that
// would otherwise see files disappear and reappear mid-flight (Redis,
// SQLite, etc.). already-stopped containers are not touched. start
// always runs in a fresh context so a cancellation during op still
// returns the container to its prior state.
func withTargetStopped(ctx context.Context, runner dockerRunner, target dockerContainer, op func() error) error {
	if !target.State.Running {
		return op()
	}
	if err := stopContainer(ctx, runner, target.ID); err != nil {
		return fmt.Errorf("stop target container %s: %w", target.ID, err)
	}
	opErr := op()
	if err := startContainer(context.Background(), runner, target.ID); err != nil {
		if opErr != nil {
			return fmt.Errorf("%w (target %s also failed to restart: %v)", opErr, target.ID, err)
		}
		return fmt.Errorf("start target container %s: %w", target.ID, err)
	}
	return opErr
}

func (c *Client) pauseTargetWritersForState(ctx context.Context, runner dockerRunner, actx *applyContext, appName string) error {
	if !appHasStateWork(actx.plan, appName) {
		return nil
	}
	return c.pauseTargetWriters(ctx, runner, actx, appName, targetWriterKeepServices(actx.plan, appName))
}

func (c *Client) primeTargetWritersForResume(ctx context.Context, runner dockerRunner, actx *applyContext, appName string) error {
	if !appHasStateWork(actx.plan, appName) {
		return nil
	}
	return c.pauseTargetWritersWithMode(ctx, runner, actx, appName, targetWriterKeepServices(actx.plan, appName), true)
}

func appHasStateWork(plan Plan, appName string) bool {
	for _, step := range plan.Steps {
		if step.App == appName && step.Kind == StepPauseSource {
			return true
		}
	}
	return false
}

func targetWriterKeepServices(plan Plan, appName string) map[string]struct{} {
	keep := map[string]struct{}{}
	app, ok := findPrepareApp(plan.Prepare, appName)
	if !ok {
		return keep
	}
	for _, store := range app.Resources.DataStores {
		service := strings.TrimSpace(store.Service)
		if service != "" && dataStoreMigrationKind(store) == dataStoreMigrationLogical {
			keep[service] = struct{}{}
		}
	}
	return keep
}

func (c *Client) pauseTargetWriters(ctx context.Context, runner dockerRunner, actx *applyContext, appName string, keepServices map[string]struct{}) error {
	return c.pauseTargetWritersWithMode(ctx, runner, actx, appName, keepServices, false)
}

func (c *Client) pauseTargetWritersWithMode(ctx context.Context, runner dockerRunner, actx *applyContext, appName string, keepServices map[string]struct{}, recordAlreadyStopped bool) error {
	entry := actx.entry(appName)
	if len(entry.TargetWritersStopped) > 0 {
		return nil
	}
	containers, err := c.targetWriterContainers(ctx, runner, actx, appName, keepServices, recordAlreadyStopped)
	if err != nil {
		return err
	}
	stopped := []dockerContainer{}
	for _, container := range containers {
		service := container.Config.Labels[composeServiceLabel]
		if _, keep := keepServices[service]; keep {
			continue
		}
		if !container.State.Running {
			if recordAlreadyStopped {
				stopped = append(stopped, container)
			}
			continue
		}
		if err := stopContainer(ctx, runner, container.ID); err != nil {
			for _, stoppedContainer := range stopped {
				_ = startContainer(context.Background(), runner, stoppedContainer.ID)
			}
			return fmt.Errorf("stop target writer container %s: %w", container.ID, err)
		}
		stopped = append(stopped, container)
	}
	entry.TargetWritersStopped = stopped
	return nil
}

func (c *Client) withTargetWritersStopped(ctx context.Context, runner dockerRunner, actx *applyContext, appName string, keepServices map[string]struct{}, op func() error) error {
	entry := actx.entry(appName)
	alreadyPaused := len(entry.TargetWritersStopped) > 0
	if !alreadyPaused {
		if err := c.pauseTargetWriters(ctx, runner, actx, appName, keepServices); err != nil {
			return err
		}
	}
	opErr := op()
	if alreadyPaused {
		return opErr
	}
	if err := c.applyResumeTarget(context.Background(), actx, Step{Kind: StepResumeTarget, App: appName, Ref: appName}); err != nil {
		if opErr != nil {
			return fmt.Errorf("%w (target writers also failed to restart: %v)", opErr, err)
		}
		return err
	}
	return opErr
}

func (c *Client) targetWriterContainers(ctx context.Context, runner dockerRunner, actx *applyContext, appName string, keepServices map[string]struct{}, acceptStoppedExpected bool) ([]dockerContainer, error) {
	entry := actx.entry(appName)
	if entry.ComposeAppName == "" {
		if err := c.refreshComposeAppName(ctx, entry); err != nil {
			return nil, err
		}
	}
	expectedServices, haveExpectedServices, err := targetWriterExpectedServices(actx.plan, appName, keepServices)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(targetDiscoveryTimeout)
	lastSet := ""
	for {
		containers, err := listContainersByLabel(ctx, runner, "com.docker.compose.project="+entry.ComposeAppName)
		if err != nil {
			return nil, err
		}
		if haveExpectedServices {
			if len(expectedServices) == 0 {
				return containers, nil
			}
			if len(missingTargetWriterServices(containers, expectedServices, acceptStoppedExpected)) == 0 {
				set := targetContainerSetSignature(containers)
				if set == lastSet {
					return containers, nil
				}
				lastSet = set
			} else {
				lastSet = ""
			}
		} else if len(containers) > 0 {
			set := targetContainerSetSignature(containers)
			if set == lastSet {
				return containers, nil
			}
			lastSet = set
		}
		if err := c.composeDeploymentError(ctx, entry); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			if haveExpectedServices {
				missing := missingTargetWriterServices(containers, expectedServices, acceptStoppedExpected)
				if len(missing) > 0 {
					return nil, fmt.Errorf("target containers for dokploy compose project %q missing services %s after %s", entry.ComposeAppName, strings.Join(missing, ", "), targetDiscoveryTimeout)
				}
			}
			return nil, fmt.Errorf("target containers for dokploy compose project %q were not found after %s", entry.ComposeAppName, targetDiscoveryTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(targetWriterDiscoveryDelay):
		}
	}
}

func targetWriterExpectedServices(plan Plan, appName string, keepServices map[string]struct{}) (map[string]struct{}, bool, error) {
	app, ok := findPrepareApp(plan.Prepare, appName)
	if !ok || plan.Prepare.BundleDir == "" || app.TargetResources == nil || app.TargetResources.Dokploy == nil {
		return nil, false, nil
	}
	composeFile, err := readComposeFile(plan, appName)
	if err != nil {
		return nil, false, err
	}
	services, err := composeServiceSummaries(composeFile)
	if err != nil {
		return nil, false, err
	}
	expected := map[string]struct{}{}
	for service := range services {
		if _, keep := keepServices[service]; keep {
			continue
		}
		expected[service] = struct{}{}
	}
	return expected, true, nil
}

func missingTargetWriterServices(containers []dockerContainer, expected map[string]struct{}, acceptStopped bool) []string {
	present := map[string]struct{}{}
	for _, container := range containers {
		service := strings.TrimSpace(container.Config.Labels[composeServiceLabel])
		if service != "" && (acceptStopped || container.State.Running) {
			present[service] = struct{}{}
		}
	}
	missing := []string{}
	for service := range expected {
		if _, ok := present[service]; !ok {
			missing = append(missing, service)
		}
	}
	sort.Strings(missing)
	return missing
}

func targetContainerSetSignature(containers []dockerContainer) string {
	parts := make([]string, 0, len(containers))
	for _, container := range containers {
		parts = append(parts, container.ID+":"+container.Config.Labels[composeServiceLabel]+":"+container.State.Status)
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

func (c *Client) applyResumeTarget(ctx context.Context, actx *applyContext, step Step) error {
	entry := actx.entry(step.App)
	if len(entry.TargetWritersStopped) == 0 {
		return nil
	}
	if err := startStoppedTargetWriters(c.dockerRunner(), entry.TargetWritersStopped); err != nil {
		return err
	}
	entry.TargetWritersStopped = nil
	return nil
}

func startStoppedTargetWriters(runner dockerRunner, containers []dockerContainer) error {
	var result error
	for _, container := range containers {
		if err := startContainer(context.Background(), runner, container.ID); err != nil {
			if result != nil {
				result = fmt.Errorf("%w; start target writer container %s: %v", result, container.ID, err)
				continue
			}
			result = fmt.Errorf("start target writer container %s: %w", container.ID, err)
		}
	}
	return result
}

// applyDumpDataStore writes a logical dump of the source data store to a
// run-scoped file. only postgres is supported in this pass.
func (c *Client) applyDumpDataStore(ctx context.Context, actx *applyContext, step Step) error {
	app, ok := findPrepareApp(actx.plan.Prepare, step.App)
	if !ok {
		return fmt.Errorf("app %s not found in prepare result", step.App)
	}
	store, ok := findPrepareDataStore(app, step.Ref)
	if !ok {
		return fmt.Errorf("data store %s for app %s not found", step.Ref, step.App)
	}
	if dataStoreMigrationKind(store) != dataStoreMigrationLogical {
		// non-logical stores migrate via stopped-volume copy, so the
		// dump step is a noop. plan generation already filters these
		// out; this branch is defense-in-depth.
		return nil
	}

	runner := c.dockerRunner()
	src, err := sourceContainerForQuiesce(ctx, runner, commitTargetRef{id: store.SourceContainerID, name: store.SourceContainerName, service: store.Service})
	if err != nil {
		return fmt.Errorf("inspect source data store %s: %w", step.Ref, err)
	}
	creds := postgresCredsFromEnv(envMap(src.Config.Env))

	dumpPath, err := dataStoreDumpPath(actx.plan, step.App, step.Ref)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dumpPath), 0o700); err != nil {
		return fmt.Errorf("prepare dump dir: %w", err)
	}
	file, err := os.OpenFile(dumpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open dump file %s: %w", dumpPath, err)
	}
	defer file.Close()

	args := []string{"exec"}
	if creds.Password != "" {
		args = append(args, "-e", "PGPASSWORD="+creds.Password)
	}
	args = append(args, src.ID, "pg_dump", "-w",
		"-U", creds.User,
		"-d", creds.Database,
		"-Fc", "--no-owner", "--no-acl",
	)
	return runner.Run(ctx, nil, file, args...)
}

// applyRestoreDataStore loads the dump file into the target data store
// container. the target dokploy compose stack must already be deployed.
func (c *Client) applyRestoreDataStore(ctx context.Context, actx *applyContext, step Step) error {
	app, ok := findPrepareApp(actx.plan.Prepare, step.App)
	if !ok {
		return fmt.Errorf("app %s not found in prepare result", step.App)
	}
	store, ok := findPrepareDataStore(app, step.Ref)
	if !ok {
		return fmt.Errorf("data store %s for app %s not found", step.Ref, step.App)
	}
	if dataStoreMigrationKind(store) != dataStoreMigrationLogical {
		// non-logical stores migrate via stopped-volume copy, so the
		// restore step is a noop. plan generation already filters these
		// out; this branch is defense-in-depth.
		return nil
	}

	runner := c.dockerRunner()
	dst, err := c.targetContainerForService(ctx, runner, actx, step.App, store.Service)
	if err != nil {
		return err
	}
	creds := postgresCredsFromEnv(envMap(dst.Config.Env))

	return c.withTargetWritersStopped(ctx, runner, actx, step.App, map[string]struct{}{store.Service: {}}, func() error {
		if err := waitPostgresReady(ctx, runner, dst.ID, creds); err != nil {
			return err
		}

		dumpPath, err := dataStoreDumpPath(actx.plan, step.App, step.Ref)
		if err != nil {
			return err
		}
		restoreListPath, cleanupRestoreList, err := preparePgRestoreList(ctx, runner, dst.ID, dumpPath, step.App, step.Ref)
		if err != nil {
			return err
		}
		defer cleanupRestoreList()

		file, err := os.Open(dumpPath)
		if err != nil {
			return fmt.Errorf("open dump file %s: %w", dumpPath, err)
		}
		defer file.Close()

		args := []string{"exec", "-i"}
		if creds.Password != "" {
			args = append(args, "-e", "PGPASSWORD="+creds.Password)
		}
		args = append(args, dst.ID, "pg_restore", "-w",
			"-U", creds.User,
			"-d", creds.Database,
			"--clean", "--if-exists", "--no-owner", "--no-acl",
			"--single-transaction", "--exit-on-error",
		)
		if restoreListPath != "" {
			args = append(args, "-L", restoreListPath)
		}
		return runner.Run(ctx, file, nil, args...)
	})
}

func preparePgRestoreList(ctx context.Context, runner dockerRunner, containerID, dumpPath, appName, ref string) (string, func(), error) {
	noop := func() {}
	dump, err := os.Open(dumpPath)
	if err != nil {
		return "", noop, fmt.Errorf("open dump file %s: %w", dumpPath, err)
	}
	defer dump.Close()

	var list bytes.Buffer
	if err := runner.Run(ctx, dump, &list, "exec", "-i", containerID, "pg_restore", "-l"); err != nil {
		return "", noop, fmt.Errorf("list postgres dump %s: %w", dumpPath, err)
	}
	filtered, changed := filterPgRestoreList(list.String())
	if !changed {
		return "", noop, nil
	}

	listFile, err := os.CreateTemp(filepath.Dir(dumpPath), ".bort-pg-restore-list-")
	if err != nil {
		return "", noop, fmt.Errorf("create postgres restore list: %w", err)
	}
	hostListPath := listFile.Name()
	if _, err := listFile.WriteString(filtered); err != nil {
		listFile.Close()
		os.Remove(hostListPath)
		return "", noop, fmt.Errorf("write postgres restore list: %w", err)
	}
	if err := listFile.Chmod(0o644); err != nil {
		listFile.Close()
		os.Remove(hostListPath)
		return "", noop, fmt.Errorf("chmod postgres restore list: %w", err)
	}
	if err := listFile.Close(); err != nil {
		os.Remove(hostListPath)
		return "", noop, fmt.Errorf("close postgres restore list: %w", err)
	}

	containerListPath := pgRestoreListContainerPath(appName, ref)
	if _, err := runner.Output(ctx, "cp", hostListPath, containerID+":"+containerListPath); err != nil {
		os.Remove(hostListPath)
		return "", noop, fmt.Errorf("stage postgres restore list: %w", err)
	}
	cleanup := func() {
		os.Remove(hostListPath)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = runner.Output(cleanupCtx, "exec", containerID, "rm", "-f", containerListPath)
	}
	return containerListPath, cleanup, nil
}

func pgRestoreListContainerPath(appName, ref string) string {
	store := strings.TrimPrefix(ref, "data-store:")
	return "/tmp/bort-restore-list-" + safeDataPathSegment(appName) + "-" + safeDataPathSegment(store) + ".list"
}

func filterPgRestoreList(list string) (string, bool) {
	var filtered strings.Builder
	changed := false
	supabaseManaged := pgRestoreListHasSupabaseManagedSchemas(list)
	for _, line := range strings.SplitAfter(list, "\n") {
		if shouldSkipPgRestoreListLine(line, supabaseManaged) {
			changed = true
			continue
		}
		filtered.WriteString(line)
	}
	return filtered.String(), changed
}

func shouldSkipPgRestoreListLine(line string, supabaseManaged bool) bool {
	if isPgRestoreEventTriggerListLine(line) {
		return true
	}
	if !supabaseManaged {
		return false
	}
	fields := pgRestoreListFields(line)
	if len(fields) == 0 {
		return false
	}
	if isSupabaseManagedExtensionListLine(fields) || isSupabaseRealtimePublicationListLine(fields) {
		return true
	}
	namespace := pgRestoreListNamespace(fields)
	if !supabaseManagedSchemas[namespace] {
		return false
	}
	if isPgRestoreManagedDataListLine(fields) && !isSupabaseManagedMigrationDataListLine(fields) {
		return false
	}
	return true
}

func pgRestoreListFields(line string) []string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ";") {
		return nil
	}
	return strings.Fields(trimmed)
}

func isPgRestoreEventTriggerListLine(line string) bool {
	fields := pgRestoreListFields(line)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "EVENT" && fields[i+1] == "TRIGGER" {
			return true
		}
	}
	return false
}

var supabaseManagedSchemas = map[string]bool{
	"auth":           true,
	"extensions":     true,
	"graphql":        true,
	"graphql_public": true,
	"pgbouncer":      true,
	"pgsodium":       true,
	"realtime":       true,
	"storage":        true,
	"vault":          true,
}

var supabaseMarkerSchemas = map[string]bool{
	"auth":           true,
	"graphql":        true,
	"graphql_public": true,
	"pgsodium":       true,
	"realtime":       true,
	"storage":        true,
	"vault":          true,
}

var supabaseManagedExtensions = map[string]bool{
	"pg_graphql":         true,
	"pg_stat_statements": true,
	"pgcrypto":           true,
	"pgjwt":              true,
	"pgsodium":           true,
	"supabase_vault":     true,
	"uuid-ossp":          true,
}

func pgRestoreListHasSupabaseManagedSchemas(list string) bool {
	for _, line := range strings.Split(list, "\n") {
		fields := pgRestoreListFields(line)
		if len(fields) > 5 && fields[3] == "EXTENSION" && fields[4] == "-" && fields[5] == "supabase_vault" {
			return true
		}
		if len(fields) < 6 || fields[3] != "SCHEMA" || fields[4] != "-" {
			continue
		}
		owner := fields[len(fields)-1]
		if supabaseMarkerSchemas[fields[5]] && strings.Contains(owner, "supabase") {
			return true
		}
	}
	return false
}

func pgRestoreListNamespace(fields []string) string {
	if len(fields) < 4 {
		return ""
	}
	switch fields[3] {
	case "SCHEMA":
		if len(fields) > 5 && fields[4] == "-" {
			return fields[5]
		}
	case "TABLE":
		if len(fields) > 5 && fields[4] == "DATA" {
			return fields[5]
		}
		if len(fields) > 4 {
			return fields[4]
		}
	case "FUNCTION", "VIEW", "DEFAULT", "CONSTRAINT", "INDEX", "TRIGGER", "POLICY":
		if len(fields) > 4 {
			return fields[4]
		}
	case "SEQUENCE":
		if len(fields) > 5 && fields[4] == "SET" {
			return fields[5]
		}
		if len(fields) > 6 && fields[4] == "OWNED" && fields[5] == "BY" {
			return fields[6]
		}
		if len(fields) > 4 {
			return fields[4]
		}
	case "COMMENT", "ACL":
		if len(fields) > 4 && fields[4] != "-" {
			return fields[4]
		}
	case "FK":
		if len(fields) > 5 && fields[4] == "CONSTRAINT" {
			return fields[5]
		}
	case "ROW":
		if len(fields) > 5 && fields[4] == "SECURITY" {
			return fields[5]
		}
	}
	return ""
}

func isPgRestoreManagedDataListLine(fields []string) bool {
	return len(fields) > 5 && ((fields[3] == "TABLE" && fields[4] == "DATA") || (fields[3] == "SEQUENCE" && fields[4] == "SET"))
}

func isSupabaseManagedMigrationDataListLine(fields []string) bool {
	if !isPgRestoreManagedDataListLine(fields) || len(fields) <= 6 {
		return false
	}
	namespace := fields[5]
	name := fields[6]
	return (namespace == "auth" && name == "schema_migrations") || (namespace == "storage" && name == "migrations")
}

func isSupabaseManagedExtensionListLine(fields []string) bool {
	name := ""
	if len(fields) > 5 && fields[3] == "EXTENSION" && fields[4] == "-" {
		name = fields[5]
	} else if len(fields) > 6 && fields[3] == "COMMENT" && fields[4] == "-" && fields[5] == "EXTENSION" {
		name = fields[6]
	}
	name = strings.Trim(name, "\"")
	return supabaseManagedExtensions[name]
}

func isSupabaseRealtimePublicationListLine(fields []string) bool {
	return len(fields) > 5 && fields[3] == "PUBLICATION" && fields[4] == "-" && fields[5] == "supabase_realtime"
}

// waitPostgresReady polls pg_isready inside the target container so the
// restore step does not race compose.deploy bringing up the database.
func waitPostgresReady(ctx context.Context, runner dockerRunner, containerID string, creds postgresCreds) error {
	deadline := time.Now().Add(targetDiscoveryTimeout)
	for {
		args := []string{"exec"}
		if creds.Password != "" {
			args = append(args, "-e", "PGPASSWORD="+creds.Password)
		}
		args = append(args, containerID, "pg_isready", "-q", "-U", creds.User, "-d", creds.Database)
		if err := runner.Run(ctx, nil, nil, args...); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("target postgres %s was not ready after %s", containerID, targetDiscoveryTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(targetDiscoveryDelay):
		}
	}
}

type postgresCreds struct {
	User     string
	Password string
	Database string
}

func postgresCredsFromEnv(env map[string]string) postgresCreds {
	user := firstNonEmptyEnv(env, "PGUSER", "POSTGRES_USER")
	if user == "" {
		user = "postgres"
	}
	db := firstNonEmptyEnv(env, "PGDATABASE", "POSTGRES_DB")
	if db == "" {
		db = user
	}
	return postgresCreds{
		User:     user,
		Password: firstPresentEnv(env, "PGPASSWORD", "POSTGRES_PASSWORD"),
		Database: db,
	}
}

func firstNonEmptyEnv(env map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(env[key]); value != "" {
			return value
		}
	}
	return ""
}

// firstPresentEnv returns the raw first non-empty value for the given
// keys without trimming, so passwords are preserved verbatim.
func firstPresentEnv(env map[string]string, keys ...string) string {
	for _, key := range keys {
		if value, ok := env[key]; ok && value != "" {
			return value
		}
	}
	return ""
}

// shouldMigrateDataStore answers whether the apply phase should attempt
// dump/restore. it skips the user-selected "recreate" and "managed"
// decisions while treating engine-specific or absent strategies as
// implicit migrate.
func shouldMigrateDataStore(store preparer.DataStoreResource) bool {
	switch strings.ToLower(strings.TrimSpace(store.Strategy)) {
	case "recreate", "managed":
		return false
	}
	return true
}

// migration kind classifies how a data store will be migrated.
//
//	logical: a tool-specific dump (e.g. pg_dump) reads a live source.
//	volume:  no logical dump; raw volume copy with the source paused.
//	skip:    user opted out (recreate/managed) and bort copies nothing.
const (
	dataStoreMigrationLogical = "logical"
	dataStoreMigrationVolume  = "volume"
	dataStoreMigrationSkip    = "skip"
)

func dataStoreMigrationKind(store preparer.DataStoreResource) string {
	if !shouldMigrateDataStore(store) {
		return dataStoreMigrationSkip
	}
	if isPostgresStore(store) {
		return dataStoreMigrationLogical
	}
	return dataStoreMigrationVolume
}

func isPostgresStore(store preparer.DataStoreResource) bool {
	kind := strings.ToLower(store.Kind)
	engine := strings.ToLower(store.Engine)
	return kind == "postgres" || kind == "postgresql" || engine == "postgres" || engine == "postgresql"
}

func findPrepareVolume(app preparer.AppPlan, ref string) (preparer.VolumeResource, bool) {
	for _, volume := range app.Resources.Volumes {
		label := volumeRefLabel(volume)
		if label == ref || strings.TrimPrefix(ref, "volume:") == label {
			return volume, true
		}
	}
	return preparer.VolumeResource{}, false
}

func findPrepareDataStore(app preparer.AppPlan, ref string) (preparer.DataStoreResource, bool) {
	target := strings.TrimPrefix(ref, "data-store:")
	for _, store := range app.Resources.DataStores {
		if store.Service != "" && store.Service == target {
			return store, true
		}
	}
	for _, store := range app.Resources.DataStores {
		if store.Service == "" && store.Kind == target {
			return store, true
		}
	}
	return preparer.DataStoreResource{}, false
}

func volumeRefLabel(volume preparer.VolumeResource) string {
	label := volume.Service
	if label == "" {
		label = "app"
	}
	if volume.Target != "" {
		label += " -> " + volume.Target
	}
	return label
}

// dataStoreForVolume returns the data store whose service owns the given
// volume, when the volume's service maps to a classified data store.
func dataStoreForVolume(app preparer.AppPlan, volume preparer.VolumeResource) (preparer.DataStoreResource, bool) {
	if volume.Service == "" {
		return preparer.DataStoreResource{}, false
	}
	for _, store := range app.Resources.DataStores {
		if store.Service == volume.Service {
			return store, true
		}
	}
	return preparer.DataStoreResource{}, false
}

// volumeOwnedByLogicalOrSkippedStore answers whether a raw volume copy
// for this volume must be suppressed. logical-dump stores own their
// migration via dump/restore; skip-strategy stores must keep the target
// empty. only "volume" migration kind keeps raw copy enabled.
func volumeOwnedByLogicalOrSkippedStore(app preparer.AppPlan, volume preparer.VolumeResource) bool {
	store, ok := dataStoreForVolume(app, volume)
	if !ok {
		return false
	}
	return dataStoreMigrationKind(store) != dataStoreMigrationVolume
}

func dataStoreDumpPath(plan Plan, appName, ref string) (string, error) {
	if plan.RunDir == "" {
		return "", fmt.Errorf("plan.RunDir is empty; cannot stage data dump for %s/%s", appName, ref)
	}
	store := strings.TrimPrefix(ref, "data-store:")
	dir := filepath.Join(plan.RunDir, "data", safeDataPathSegment(appName))
	if err := safepath.ContainedPath(plan.RunDir, dir); err != nil {
		return "", err
	}
	full := filepath.Join(dir, safeDataPathSegment(store)+".pgdump")
	if err := safepath.ContainedPath(dir, full); err != nil {
		return "", err
	}
	return full, nil
}

func safeDataPathSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// resolveSourceVolume returns the source-side named volume identifier for
// a stateful volume. it prefers the explicit name, then falls back to
// inspecting the source container mount table.
func resolveSourceVolume(ctx context.Context, runner dockerRunner, volume preparer.VolumeResource) (string, error) {
	if volume.Name != "" {
		return volume.Name, nil
	}
	if volume.SourceContainerID == "" && volume.SourceContainerName == "" {
		return "", fmt.Errorf("source volume name unknown and no source container ref recorded")
	}
	src, err := sourceContainer(ctx, runner, volume.SourceContainerID, volume.SourceContainerName)
	if err != nil {
		return "", err
	}
	mount, ok := findMountByTarget(src, volume.Target)
	if !ok || mount.Type != "volume" || mount.Name == "" {
		return "", fmt.Errorf("named volume for target %s not found on source container", volume.Target)
	}
	return mount.Name, nil
}

// resolveSourceBindPath returns the source-side host path for a bind
// mount. when a source container ref exists, it inspects live and
// rejects stale-but-existing recorded paths so we don't silently copy
// outdated data into the new stack.
func resolveSourceBindPath(ctx context.Context, runner dockerRunner, volume preparer.VolumeResource) (string, error) {
	hasContainerRef := volume.SourceContainerID != "" || volume.SourceContainerName != ""
	if !hasContainerRef {
		if volume.Source == "" {
			return "", fmt.Errorf("source bind path unknown and no source container ref recorded")
		}
		return volume.Source, nil
	}
	src, err := sourceContainer(ctx, runner, volume.SourceContainerID, volume.SourceContainerName)
	if err != nil {
		return "", err
	}
	mount, ok := findMountByTarget(src, volume.Target)
	if !ok || mount.Type != "bind" || mount.Source == "" {
		return "", fmt.Errorf("bind mount for target %s not found on source container", volume.Target)
	}
	if volume.Source != "" && volume.Source != mount.Source {
		return "", fmt.Errorf("recorded source bind path %q for %s does not match live container mount %q; rescan before retrying",
			volume.Source, volume.Target, mount.Source)
	}
	return mount.Source, nil
}

const (
	targetDiscoveryTimeout     = 60 * time.Second
	targetDiscoveryDelay       = 2 * time.Second
	targetWriterDiscoveryDelay = 250 * time.Millisecond
)

// targetContainerForService discovers the dokploy-managed container for a
// compose service by docker label, polling briefly because compose.deploy
// returns before the containers may finish starting.
func (c *Client) targetContainerForService(ctx context.Context, runner dockerRunner, actx *applyContext, appName, service string) (dockerContainer, error) {
	entry := actx.entry(appName)
	if entry.ComposeAppName == "" {
		if err := c.refreshComposeAppName(ctx, entry); err != nil {
			return dockerContainer{}, err
		}
	}
	deadline := time.Now().Add(targetDiscoveryTimeout)
	for {
		containers, err := listContainersByLabel(ctx, runner, "com.docker.compose.project="+entry.ComposeAppName)
		if err != nil {
			return dockerContainer{}, err
		}
		if container, ok := pickComposeService(containers, service); ok {
			return container, nil
		}
		if err := c.composeDeploymentError(ctx, entry); err != nil {
			return dockerContainer{}, err
		}
		if len(containers) == 0 && entry.ComposeID != "" && !entry.DiscoveryRedeployAttempted {
			entry.DiscoveryRedeployAttempted = true
			if err := c.DeployCompose(ctx, entry.ComposeID, deployComposeTitle(actx.plan)); err != nil {
				return dockerContainer{}, fmt.Errorf("redeploy dokploy compose project %q for target discovery: %w", entry.ComposeAppName, err)
			}
		}
		if time.Now().After(deadline) {
			return dockerContainer{}, fmt.Errorf("target service %q not found in dokploy compose project %q after %s", service, entry.ComposeAppName, targetDiscoveryTimeout)
		}
		select {
		case <-ctx.Done():
			return dockerContainer{}, ctx.Err()
		case <-time.After(targetDiscoveryDelay):
		}
	}
}

func (c *Client) composeDeploymentError(ctx context.Context, entry *appCache) error {
	if entry == nil || entry.ComposeID == "" {
		return nil
	}
	compose, err := c.GetCompose(ctx, entry.ComposeID)
	if err != nil {
		return nil
	}
	if !strings.EqualFold(compose.ComposeStatus, "error") {
		return nil
	}
	return fmt.Errorf("dokploy compose project %q deployment failed%s", entry.ComposeAppName, composeDeploymentDetails(compose))
}

func composeDeploymentDetails(compose *Compose) string {
	if compose == nil || len(compose.Deployments) == 0 {
		return ""
	}
	latest := compose.Deployments[0]
	for _, deployment := range compose.Deployments {
		if strings.EqualFold(deployment.Status, "error") {
			latest = deployment
			break
		}
	}
	parts := []string{}
	if strings.TrimSpace(latest.ErrorMessage) != "" {
		parts = append(parts, latest.ErrorMessage)
	}
	if strings.TrimSpace(latest.LogPath) != "" {
		parts = append(parts, "log: "+latest.LogPath)
	}
	if len(parts) == 0 {
		return ""
	}
	return ": " + strings.Join(parts, "; ")
}

// refreshComposeAppName fetches the compose record by id and caches its
// appName. it lets apply tolerate dokploy responses that omit appName on
// compose.create/search.
func (c *Client) refreshComposeAppName(ctx context.Context, entry *appCache) error {
	if entry.ComposeID == "" {
		return fmt.Errorf("dokploy compose id missing; cannot refresh appName")
	}
	compose, err := c.GetCompose(ctx, entry.ComposeID)
	if err != nil {
		return fmt.Errorf("refresh compose appName: %w", err)
	}
	if compose.AppName == "" {
		return fmt.Errorf("dokploy compose %s returned no appName", entry.ComposeID)
	}
	entry.ComposeAppName = compose.AppName
	return nil
}

func pickComposeService(containers []dockerContainer, service string) (dockerContainer, bool) {
	if service == "" {
		return dockerContainer{}, false
	}
	var fallback dockerContainer
	haveFallback := false
	for _, c := range containers {
		if c.Config.Labels[composeServiceLabel] != service {
			continue
		}
		if c.State.Running {
			return c, true
		}
		if !haveFallback {
			fallback = c
			haveFallback = true
		}
	}
	return fallback, haveFallback
}
