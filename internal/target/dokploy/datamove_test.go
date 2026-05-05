package dokploy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/preparer"
	"gopkg.in/yaml.v3"
)

type fakeDockerRunner struct {
	outputs    map[string][]byte
	runOutputs map[string][]byte
	outputArgs [][]string
	runs       []fakeDockerRun
	copies     []fakeDockerCopy
	runErr     error
}

type fakeDockerRun struct {
	Args   []string
	Stdin  []byte
	Output string
}

type fakeDockerCopy struct {
	Source      string
	Destination string
	Contents    string
}

func (f *fakeDockerRunner) Output(_ context.Context, args ...string) ([]byte, error) {
	f.outputArgs = append(f.outputArgs, append([]string{}, args...))
	if len(args) == 3 && args[0] == "cp" {
		contents, err := os.ReadFile(args[1])
		if err != nil {
			return nil, err
		}
		f.copies = append(f.copies, fakeDockerCopy{Source: args[1], Destination: args[2], Contents: string(contents)})
		return []byte{}, nil
	}
	key := strings.Join(args, " ")
	if data, ok := f.outputs[key]; ok {
		return data, nil
	}
	for prefix, data := range f.outputs {
		if strings.HasPrefix(key, prefix) {
			return data, nil
		}
	}
	return nil, errors.New("docker output not stubbed: " + key)
}

func (f *fakeDockerRunner) Run(_ context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	key := strings.Join(args, " ")
	run := fakeDockerRun{Args: append([]string{}, args...)}
	if stdin != nil {
		buf, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		run.Stdin = buf
	}
	if stdout != nil {
		output := []byte("dump-bytes")
		if stub, ok := f.runOutputs[key]; ok {
			output = stub
		}
		_, _ = stdout.Write(output)
		run.Output = string(output)
	}
	f.runs = append(f.runs, run)
	return f.runErr
}

func TestStopContainerKillsAfterStopTimeout(t *testing.T) {
	runner := &timeoutStopRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if err := stopContainer(ctx, runner, "c1"); err != nil {
		t.Fatalf("stopContainer: %v", err)
	}
	want := []string{"stop c1", "stop -t 2 c1", "inspect --type container c1", "kill c1"}
	if strings.Join(runner.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("expected calls %v, got %v", want, runner.calls)
	}
}

func TestStopContainerTreatsStoppedContainerAsSuccessAfterShortStopFailure(t *testing.T) {
	runner := &stopKillInspectRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if err := stopContainer(ctx, runner, "c1"); err != nil {
		t.Fatalf("stopContainer: %v", err)
	}
	want := []string{"stop c1", "stop -t 2 c1", "inspect --type container c1"}
	if strings.Join(runner.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("expected calls %v, got %v", want, runner.calls)
	}
}

type timeoutStopRunner struct {
	calls []string
}

func (r *timeoutStopRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	if len(args) == 2 && args[0] == "stop" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if len(args) > 0 && args[0] == "stop" {
		return nil, errors.New("short stop failed")
	}
	return []byte{}, nil
}

func (r *timeoutStopRunner) Run(context.Context, io.Reader, io.Writer, ...string) error {
	return nil
}

type stopKillInspectRunner struct {
	calls []string
}

func (r *stopKillInspectRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	switch args[0] {
	case "stop":
		if len(args) > 2 {
			return nil, errors.New("short stop failed")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	case "kill":
		return nil, errors.New("signal: killed")
	case "inspect":
		return []byte(`[{"Id":"c1","Name":"/worker","State":{"Running":false,"Status":"exited"}}]`), nil
	default:
		return nil, errors.New("unexpected docker command")
	}
}

func (r *stopKillInspectRunner) Run(context.Context, io.Reader, io.Writer, ...string) error {
	return nil
}

func TestApplyDumpDataStorePostgres(t *testing.T) {
	runDir := t.TempDir()
	app := preparer.AppPlan{Name: "api"}
	app.Resources.DataStores = []preparer.DataStoreResource{{
		Kind:                "postgres",
		Service:             "db",
		Strategy:            "migrate",
		SourceContainerID:   "src-id",
		SourceContainerName: "coolify-pg",
	}}

	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container src-id": []byte(`[{"Id":"src-id","Name":"/coolify-pg","Config":{"Env":["POSTGRES_USER=bob","POSTGRES_PASSWORD=secret","POSTGRES_DB=app"]},"State":{"Running":true,"Status":"running"}}]`),
		},
	}
	client := &Client{Docker: runner}

	plan := Plan{
		Steps:   []Step{{Kind: StepDumpDataStore, App: "api", Ref: "data-store:db"}},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{app}},
		RunDir:  runDir,
	}

	if err := client.Apply(context.Background(), plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(runner.runs) != 1 {
		t.Fatalf("expected 1 docker run, got %d: %#v", len(runner.runs), runner.runs)
	}
	got := runner.runs[0]
	wantContains := []string{"exec", "-e", "PGPASSWORD=secret", "src-id", "pg_dump", "-U", "bob", "-d", "app"}
	for _, fragment := range wantContains {
		found := false
		for _, arg := range got.Args {
			if arg == fragment {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected dump args to contain %q, got %v", fragment, got.Args)
		}
	}

	dumpPath := filepath.Join(runDir, "data", "api", "db.pgdump")
	contents, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("read dump file: %v", err)
	}
	if string(contents) != "dump-bytes" {
		t.Fatalf("expected dump bytes, got %q", string(contents))
	}
}

func TestApplyDumpDataStoreSkipsRecreate(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.DataStores = []preparer.DataStoreResource{{
		Kind:     "postgres",
		Service:  "db",
		Strategy: "recreate",
	}}
	runner := &fakeDockerRunner{}
	client := &Client{Docker: runner}
	plan := Plan{
		Steps:   []Step{{Kind: StepDumpDataStore, App: "api", Ref: "data-store:db"}},
		Prepare: preparer.Result{Apps: []preparer.AppPlan{app}},
		RunDir:  t.TempDir(),
	}
	if err := client.Apply(context.Background(), plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("expected no docker runs for recreate strategy, got %#v", runner.runs)
	}
}

func TestApplyPushImageTagsMissingComposeImageFromSourceContainer(t *testing.T) {
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatalf("mkdir app dir: %v", err)
	}
	compose := `services:
  worker:
    image: example/worker:local
  redis:
    image: redis:7
`
	if err := os.WriteFile(filepath.Join(appDir, "compose.yaml"), []byte(compose), 0o600); err != nil {
		t.Fatalf("write compose: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/compose.deploy" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"image inspect redis:7":                        []byte(`[{}]`),
		"inspect --type container src-worker":          []byte(`[{"Id":"src-worker","Name":"/worker","Image":"sha256:worker-image","Config":{"Image":"example/worker:old"}}]`),
		"tag sha256:worker-image example/worker:local": []byte(""),
	}}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	app := preparer.AppPlan{Name: "api", Directory: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{{ServiceName: "worker", ContainerID: "src-worker"}}
	app.TargetResources = &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{ComposePath: "compose.yaml"}}}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{app}}}}
	actx.entry("api").ComposeID = "compose-1"

	if err := client.applyPushImage(context.Background(), actx, Step{Kind: StepPushImage, App: "api"}); err != nil {
		t.Fatalf("applyPushImage: %v", err)
	}
	if !fakeOutputCalled(runner, "tag", "sha256:worker-image", "example/worker:local") {
		t.Fatalf("expected missing image to be tagged from source container, calls=%#v", runner.outputArgs)
	}
}

func TestComposeFileForApplyReusesSourceImageForBuildOnlyService(t *testing.T) {
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatalf("mkdir app dir: %v", err)
	}
	compose := `services:
  app:
    build:
      context: .
      dockerfile: Dockerfile
    environment:
      PORT: "8080"
`
	if err := os.WriteFile(filepath.Join(appDir, "compose.yaml"), []byte(compose), 0o600); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"inspect --type container src-app": []byte(`[{"Id":"src-app","Name":"/app","Image":"sha256:app-image","Config":{"Image":"example/app:latest"}}]`),
	}}
	client := &Client{Docker: runner}
	app := preparer.AppPlan{Name: "api", Directory: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{{ServiceName: "app", ContainerID: "src-app"}}
	app.TargetResources = &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{ComposePath: "compose.yaml"}}}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{app}}}}

	out, err := client.composeFileForApply(context.Background(), actx, "api")
	if err != nil {
		t.Fatalf("composeFileForApply: %v", err)
	}
	if strings.Contains(out, "build:") || !strings.Contains(out, "image: example/app:latest") {
		t.Fatalf("expected build-only service to reuse source image, got:\n%s", out)
	}
}

func fakeOutputCalled(runner *fakeDockerRunner, args ...string) bool {
	for _, call := range runner.outputArgs {
		if len(call) != len(args) {
			continue
		}
		match := true
		for i := range args {
			if call[i] != args[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestApplyRestoreDataStoreFiltersEventTriggers(t *testing.T) {
	runDir := t.TempDir()
	app := preparer.AppPlan{Name: "api"}
	app.Resources.DataStores = []preparer.DataStoreResource{{
		Kind:     "postgres",
		Service:  "db",
		Strategy: "migrate",
	}}
	plan := Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}, RunDir: runDir}
	dumpPath, err := dataStoreDumpPath(plan, "api", "data-store:db")
	if err != nil {
		t.Fatalf("dataStoreDumpPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dumpPath), 0o700); err != nil {
		t.Fatalf("mkdir dump dir: %v", err)
	}
	if err := os.WriteFile(dumpPath, []byte("pg-dump"), 0o600); err != nil {
		t.Fatalf("write dump: %v", err)
	}

	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": []byte("dst-id\n"),
			"inspect --type container dst-id":                                          []byte(`[{"Id":"dst-id","Name":"/dokploy-db","Config":{"Env":["POSTGRES_USER=bob","POSTGRES_PASSWORD=secret","POSTGRES_DB=app"],"Labels":{"com.docker.compose.service":"db","com.docker.compose.project":"stack-1"}},"State":{"Running":true,"Status":"running"}}]`),
		},
		runOutputs: map[string][]byte{
			"exec -i dst-id pg_restore -l": []byte("; archive header\n20; 2615 16457 SCHEMA - auth supabase_admin\n7; 3079 16950 EXTENSION - supabase_vault \n239; 1259 16488 TABLE auth users supabase_auth_admin\n5332; 0 16458 TABLE DATA auth users supabase_auth_admin\n5337; 0 16496 TABLE DATA auth schema_migrations supabase_auth_admin\n260; 1259 16970 VIEW vault decrypted_secrets supabase_admin\n271; 1259 100 TABLE public widgets bob\n2; 0 0 EVENT TRIGGER - pgrst_drop_watch bob\n3; 0 0 COMMENT - EVENT TRIGGER pgrst_drop_watch bob\n4; 0 100 TABLE DATA public widgets bob\n"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan}
	actx.entry("api").ComposeAppName = "stack-1"

	step := Step{Kind: StepRestoreDataStore, App: "api", Ref: "data-store:db"}
	if err := client.applyRestoreDataStore(context.Background(), actx, step); err != nil {
		t.Fatalf("applyRestoreDataStore: %v", err)
	}
	if len(runner.copies) != 1 {
		t.Fatalf("expected one staged restore list, got %#v", runner.copies)
	}
	if runner.copies[0].Destination != "dst-id:/tmp/bort-restore-list-api-db.list" {
		t.Fatalf("unexpected restore list destination: %#v", runner.copies[0])
	}
	for _, blocked := range []string{"EVENT TRIGGER", "SCHEMA - auth", "EXTENSION - supabase_vault", "TABLE auth users", "TABLE DATA auth schema_migrations", "VIEW vault decrypted_secrets"} {
		if strings.Contains(runner.copies[0].Contents, blocked) {
			t.Fatalf("expected %q filtered, got:\n%s", blocked, runner.copies[0].Contents)
		}
	}
	if !strings.Contains(runner.copies[0].Contents, "TABLE public widgets") || !strings.Contains(runner.copies[0].Contents, "TABLE DATA public widgets") || !strings.Contains(runner.copies[0].Contents, "TABLE DATA auth users") {
		t.Fatalf("expected non-event-trigger entries retained, got:\n%s", runner.copies[0].Contents)
	}

	var restoreRun fakeDockerRun
	foundRestore := false
	for _, run := range runner.runs {
		joined := strings.Join(run.Args, " ")
		if strings.Contains(joined, " pg_restore ") && !strings.Contains(joined, " -l") {
			restoreRun = run
			foundRestore = true
			break
		}
	}
	if !foundRestore {
		t.Fatalf("expected pg_restore run, got %#v", runner.runs)
	}
	if string(restoreRun.Stdin) != "pg-dump" {
		t.Fatalf("expected restore stdin dump bytes, got %q", string(restoreRun.Stdin))
	}
	foundListFlag := false
	for i, arg := range restoreRun.Args {
		if arg == "-L" && i+1 < len(restoreRun.Args) && restoreRun.Args[i+1] == "/tmp/bort-restore-list-api-db.list" {
			foundListFlag = true
			break
		}
	}
	if !foundListFlag {
		t.Fatalf("expected restore args to include filtered list, got %v", restoreRun.Args)
	}
}

func TestFilterPgRestoreListLeavesRegularExtensionDumpsAlone(t *testing.T) {
	list := "1; 3079 123 EXTENSION - uuid-ossp \n2; 0 0 COMMENT - EXTENSION \"uuid-ossp\" \n3; 0 0 EVENT TRIGGER - ddl_watch postgres\n"
	filtered, changed := filterPgRestoreList(list)
	if !changed {
		t.Fatalf("expected event trigger filtering to mark list changed")
	}
	if !strings.Contains(filtered, "EXTENSION - uuid-ossp") || !strings.Contains(filtered, "COMMENT - EXTENSION \"uuid-ossp\"") {
		t.Fatalf("expected regular extension entries retained, got:\n%s", filtered)
	}
	if strings.Contains(filtered, "EVENT TRIGGER") {
		t.Fatalf("expected event trigger entry filtered, got:\n%s", filtered)
	}
}

func TestRefreshComposeAppNameFetchesByID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/compose.one" || r.URL.Query().Get("composeId") != "c1" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(Compose{ComposeID: "c1", Name: "api", AppName: "stack-1"})
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client()}
	entry := &appCache{ComposeID: "c1"}
	if err := client.refreshComposeAppName(context.Background(), entry); err != nil {
		t.Fatalf("refreshComposeAppName: %v", err)
	}
	if entry.ComposeAppName != "stack-1" {
		t.Fatalf("expected appName=stack-1, got %q", entry.ComposeAppName)
	}
}

func TestApplySyncVolumeCopiesNamedVolume(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{
		Service:             "web",
		Type:                "volume",
		Name:                "src-vol",
		Target:              "/data",
		SourceContainerID:   "src-id",
		SourceContainerName: "coolify-web",
	}}

	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": []byte("dst-id\n"),
			"inspect --type container dst-id":                                          []byte(`[{"Id":"dst-id","Name":"/dokploy-web","Config":{"Labels":{"com.docker.compose.service":"web","com.docker.compose.project":"stack-1"}},"Mounts":[{"Type":"volume","Name":"dst-vol","Destination":"/data","RW":true}]}]`),
			"volume inspect src-vol":                                                   []byte(`[{"Name":"src-vol"}]`),
			"volume inspect dst-vol":                                                   []byte(`[{"Name":"dst-vol"}]`),
		},
	}
	client := &Client{Docker: runner}

	actx := &applyContext{cache: map[string]*appCache{}}
	actx.entry("api").ComposeAppName = "stack-1"
	actx.plan = Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}

	step := Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"}
	if err := client.applySyncVolume(context.Background(), actx, step); err != nil {
		t.Fatalf("applySyncVolume: %v", err)
	}
	if len(runner.runs) != 1 {
		t.Fatalf("expected 1 docker run, got %d", len(runner.runs))
	}
	args := runner.runs[0].Args
	if args[0] != "run" {
		t.Fatalf("expected docker run, got %v", args)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "src-vol:/from:ro") || !strings.Contains(joined, "dst-vol:/to") {
		t.Fatalf("expected src/dst volume mounts in args, got %v", args)
	}
}

func TestInlineComposeEnvFilesRemovesRefsAndInjectsEnvironment(t *testing.T) {
	appDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(appDir, ".env.redis"), []byte("REDIS_PASSWORD=secret\nREDIS_DB=0\nEXISTING=from-file\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	compose := `services:
  redis:
    image: redis:7
    env_file:
      - .env.redis
    environment:
      EXISTING: from-compose
    volumes:
      - redis-data:/data
  web:
    image: example/web
    environment:
      FOO: bar
volumes:
  redis-data: {}
`
	out, err := inlineComposeEnvFiles(compose, appDir)
	if err != nil {
		t.Fatalf("inlineComposeEnvFiles: %v", err)
	}
	if strings.Contains(out, "env_file") || strings.Contains(out, ".env.redis") {
		t.Fatalf("expected env_file refs removed, got:\n%s", out)
	}
	for _, want := range []string{"redis:", "image: redis:7", "redis-data:/data", "volumes:", "REDIS_PASSWORD: secret", "REDIS_DB: \"0\"", "EXISTING: from-compose"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected sanitized compose to retain %q, got:\n%s", want, out)
		}
	}
}

func TestInlineComposeEnvFilesRemovesMissingEnvFileRefs(t *testing.T) {
	appDir := t.TempDir()
	compose := `services:
  web:
    image: example/web
    env_file: .env
    command: ./serve
`
	out, err := inlineComposeEnvFiles(compose, appDir)
	if err != nil {
		t.Fatalf("inlineComposeEnvFiles: %v", err)
	}
	if strings.Contains(out, "env_file") || strings.Contains(out, ".env") {
		t.Fatalf("expected missing env_file ref removed, got:\n%s", out)
	}
	for _, want := range []string{"web:", "image: example/web", "command: ./serve"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected sanitized compose to retain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "environment:") {
		t.Fatalf("expected missing env_file not to create empty environment block, got:\n%s", out)
	}
}

func TestInlineComposeEnvFilesUsesServiceEnvFallbacks(t *testing.T) {
	appDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(appDir, ".env.web-stack"), []byte("DATABASE_URL=postgres://web\nEXISTING=from-file\n"), 0o600); err != nil {
		t.Fatalf("write web env file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(appDir, ".env.web-blue-stack"), []byte("BLUE_ONLY=yes\n"), 0o600); err != nil {
		t.Fatalf("write web-blue env file: %v", err)
	}
	compose := `services:
  web:
    image: example/web
    env_file: .env.missing
    environment:
      EXISTING: from-compose
  web-blue:
    image: example/web-blue
`
	out, err := inlineComposeEnvFilesWithFallbacks(compose, appDir, []string{".env.web-stack", ".env.web-blue-stack"})
	if err != nil {
		t.Fatalf("inlineComposeEnvFilesWithFallbacks: %v", err)
	}
	if strings.Contains(out, "env_file") || strings.Contains(out, ".env") {
		t.Fatalf("expected env_file refs removed, got:\n%s", out)
	}
	var parsed struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("parse sanitized compose: %v", err)
	}
	if got := parsed.Services["web"].Environment["DATABASE_URL"]; got != "postgres://web" {
		t.Fatalf("expected web fallback env, got %q in compose:\n%s", got, out)
	}
	if got := parsed.Services["web"].Environment["EXISTING"]; got != "from-compose" {
		t.Fatalf("expected compose environment to win, got %q in compose:\n%s", got, out)
	}
	if _, ok := parsed.Services["web"].Environment["BLUE_ONLY"]; ok {
		t.Fatalf("expected longest service-name fallback match, got web env %#v", parsed.Services["web"].Environment)
	}
	if got := parsed.Services["web-blue"].Environment["BLUE_ONLY"]; got != "yes" {
		t.Fatalf("expected web-blue fallback env, got %q in compose:\n%s", got, out)
	}
}

func TestInlineComposeEnvFilesPreservesSharedEnvFileWhenUploadingEnv(t *testing.T) {
	appDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(appDir, ".env"), []byte("DATABASE_URL=postgres://shared\n"), 0o600); err != nil {
		t.Fatalf("write shared env file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(appDir, ".env.api"), []byte("API_ONLY=yes\n"), 0o600); err != nil {
		t.Fatalf("write api env file: %v", err)
	}
	compose := `services:
  api:
    image: example/api
    env_file:
      - .env.api
      - .env
    environment:
      DATABASE_URL: postgres://compose
  web:
    image: example/web
    env_file: ./.env
`
	out, err := inlineComposeEnvFilesWithFallbacks(compose, appDir, []string{".env", ".env.api"})
	if err != nil {
		t.Fatalf("inlineComposeEnvFilesWithFallbacks: %v", err)
	}
	if strings.Contains(out, "API_ONLY") || strings.Contains(out, "postgres://shared") {
		t.Fatalf("expected uploaded shared .env to stay out of compose body, got:\n%s", out)
	}
	var parsed struct {
		Services map[string]struct {
			EnvFile     any               `yaml:"env_file"`
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("parse sanitized compose: %v", err)
	}
	if parsed.Services["api"].EnvFile != ".env" {
		t.Fatalf("expected api env_file to keep only .env, got %#v in compose:\n%s", parsed.Services["api"].EnvFile, out)
	}
	if parsed.Services["web"].EnvFile != "./.env" {
		t.Fatalf("expected web shared env_file preserved, got %#v in compose:\n%s", parsed.Services["web"].EnvFile, out)
	}
	if got := parsed.Services["api"].Environment["DATABASE_URL"]; got != "postgres://compose" {
		t.Fatalf("expected explicit compose environment preserved, got %q in compose:\n%s", got, out)
	}
}

func TestInlineComposeEnvFilesAddsSharedEnvFileForAppServices(t *testing.T) {
	appDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(appDir, ".env"), []byte("DATABASE_URL=postgres://shared\nAPI_KEY=secret\n"), 0o600); err != nil {
		t.Fatalf("write shared env file: %v", err)
	}
	compose := `services:
  api:
    image: example/api
  worker:
    image: example/worker
    environment:
      QUEUE: default
  redis:
    image: redis:7
`
	out, err := inlineComposeEnvFilesWithFallbacks(compose, appDir, []string{".env"})
	if err != nil {
		t.Fatalf("inlineComposeEnvFilesWithFallbacks: %v", err)
	}
	if strings.Contains(out, "postgres://shared") || strings.Contains(out, "API_KEY") {
		t.Fatalf("expected shared env to stay in uploaded env file, got:\n%s", out)
	}
	var parsed struct {
		Services map[string]struct {
			EnvFile any `yaml:"env_file"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("parse sanitized compose: %v", err)
	}
	if parsed.Services["api"].EnvFile != ".env" || parsed.Services["worker"].EnvFile != ".env" {
		t.Fatalf("expected app services to reference shared .env, got:\n%s", out)
	}
	if parsed.Services["redis"].EnvFile != nil {
		t.Fatalf("expected infrastructure service to avoid app env_file, got:\n%s", out)
	}
}

func TestInlineComposeEnvFilesSanitizesCoolifyRuntimeCompose(t *testing.T) {
	appDir := t.TempDir()
	compose := `services:
  api:
    container_name: api-source
    build:
      context: .
    image: example/api:latest
    environment:
      COOLIFY_FQDN: api.example.com
      SOURCE_COMMIT: abc123
      APP_ENV: production
    ports:
      - 8080:80
    volumes:
      - app-data:/data
    networks:
      source-net: null
    labels:
      traefik.enable: "true"
      traefik.http.routers.api.rule: Host(` + "`" + `api.example.com` + "`" + `)
      caddy_0: https://api.example.com
      coolify.managed: "true"
      com.example.keep: yes
  worker:
    image: example/worker:latest
    environment:
      - COOLIFY_URL=https://worker.example.com
      - SOURCE_COMMIT=abc123
      - QUEUE=default
    labels:
      - traefik.enable=true
      - traefik.http.routers.worker.rule=Host(` + "`" + `worker.example.com` + "`" + `)
      - caddy_0=https://worker.example.com
      - coolify.managed=true
      - com.example.keep=yes
volumes:
  app-data:
    name: source-app-data
    external: true
networks:
  source-net:
    name: source-net
    external: true
  dokploy-network:
    external: true
`
	out, err := inlineComposeEnvFiles(compose, appDir)
	if err != nil {
		t.Fatalf("inlineComposeEnvFiles: %v", err)
	}
	for _, blocked := range []string{"build:", "container_name:", "ports:", "name: source-app-data", "name: source-net"} {
		if strings.Contains(out, blocked) {
			t.Fatalf("expected %q removed from sanitized compose, got:\n%s", blocked, out)
		}
	}
	for _, blocked := range []string{"traefik.enable", "traefik.http.routers", "caddy_0", "coolify.managed", "COOLIFY_FQDN", "COOLIFY_URL", "SOURCE_COMMIT"} {
		if strings.Contains(out, blocked) {
			t.Fatalf("expected source platform label %q removed from sanitized compose, got:\n%s", blocked, out)
		}
	}
	for _, want := range []string{"image: example/api:latest", "APP_ENV: production", "QUEUE=default", "app-data:/data", "source-net:", "dokploy-network:", "external: true"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected sanitized compose to retain %q, got:\n%s", want, out)
		}
	}
	if strings.Count(out, "com.example.keep") != 2 {
		t.Fatalf("expected non-proxy labels retained, got:\n%s", out)
	}
}

func TestResolveRouteForComposeUsesCurrentServiceByPort(t *testing.T) {
	compose := `services:
  web:
    image: example/web
    expose:
      - "3000"
  api-current:
    image: example/api
    expose:
      - "8080"
`
	route := gateway.Route{
		Host:        "api.example.com",
		ServiceName: "api-stale",
		Port:        "8080",
		Source:      "traefik.http.routers.https-0-stack-api.rule",
	}
	resolved, err := resolveRouteForCompose(route, compose)
	if err != nil {
		t.Fatalf("resolveRouteForCompose: %v", err)
	}
	if resolved.ServiceName != "api-current" {
		t.Fatalf("expected route to use current compose service, got %#v", resolved)
	}
}

func TestResolveRouteForComposeStripsGeneratedCoolifyServiceSuffix(t *testing.T) {
	compose := `services:
  proxy:
    image: example/proxy
  cache:
    image: example/cache
`
	cases := []struct {
		name string
		want string
	}{
		{name: "proxy-ab12cd34ef56-123456789012", want: "proxy"},
		{name: "cache-ab12cd34ef56", want: "cache"},
	}
	for _, tc := range cases {
		route := gateway.Route{
			Host:        "app.example.com",
			ServiceName: tc.name,
		}
		resolved, err := resolveRouteForCompose(route, compose)
		if err != nil {
			t.Fatalf("resolveRouteForCompose(%s): %v", tc.name, err)
		}
		if resolved.ServiceName != tc.want {
			t.Fatalf("expected generated service suffix stripped to %q, got %#v", tc.want, resolved)
		}
	}
}

func TestTargetContainerForServiceReportsComposeDeploymentError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/compose.one" || r.URL.Query().Get("composeId") != "c1" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(Compose{
			ComposeID:     "c1",
			AppName:       "stack-1",
			ComposeStatus: "error",
			Deployments: []Deployment{{
				Status:       "error",
				ErrorMessage: "env file missing",
				LogPath:      "/etc/dokploy/logs/stack-1/deploy.log",
			}},
		})
	}))
	defer server.Close()
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": []byte(""),
	}}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}}
	entry := actx.entry("api")
	entry.ComposeID = "c1"
	entry.ComposeAppName = "stack-1"

	_, err := client.targetContainerForService(context.Background(), runner, actx, "api", "web")
	if err == nil || !strings.Contains(err.Error(), "deployment failed") || !strings.Contains(err.Error(), "env file missing") || !strings.Contains(err.Error(), "/etc/dokploy/logs/stack-1/deploy.log") {
		t.Fatalf("expected compose deployment error with details, got %v", err)
	}
}

func TestApplySyncVolumeRsyncsBindMount(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{
		Service:             "web",
		Type:                "bind",
		Source:              "/coolify/data/web",
		Target:              "/data",
		SourceContainerID:   "src-id",
		SourceContainerName: "coolify-web",
	}}

	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container src-id":                                          []byte(`[{"Id":"src-id","Name":"/coolify-web","Mounts":[{"Type":"bind","Source":"/coolify/data/web","Destination":"/data","RW":true}]}]`),
			"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": []byte("dst-id\n"),
			"inspect --type container dst-id":                                          []byte(`[{"Id":"dst-id","Name":"/dokploy-web","Config":{"Labels":{"com.docker.compose.service":"web","com.docker.compose.project":"stack-1"}},"Mounts":[{"Type":"bind","Source":"/etc/dokploy/data/web","Destination":"/data","RW":true}]}]`),
		},
	}
	client := &Client{Docker: runner}

	actx := &applyContext{cache: map[string]*appCache{}}
	actx.entry("api").ComposeAppName = "stack-1"
	actx.plan = Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}

	step := Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"}
	if err := client.applySyncVolume(context.Background(), actx, step); err != nil {
		t.Fatalf("applySyncVolume: %v", err)
	}
	if len(runner.runs) != 1 {
		t.Fatalf("expected 1 docker run, got %d", len(runner.runs))
	}
	args := runner.runs[0].Args
	joined := strings.Join(args, " ")
	for _, fragment := range []string{
		"--security-opt label=disable",
		"type=bind,src=/coolify/data/web,dst=/from,readonly",
		"type=bind,src=/etc/dokploy/data/web,dst=/to",
		"rsync -aHAX --numeric-ids --filter=-x security.selinux --delete /from/ /to/",
	} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("expected args to contain %q, got %v", fragment, args)
		}
	}
}

func TestApplySyncVolumeStopsRunningTargetForCopy(t *testing.T) {
	// volume rewrite while the target is running corrupts services
	// like Redis/SQLite. running targets must be stopped for the copy
	// and started again after.
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{
		Service: "redis",
		Type:    "volume",
		Name:    "src-vol",
		Target:  "/data",
	}}
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": []byte("dst-id\n"),
			"inspect --type container dst-id":                                          []byte(`[{"Id":"dst-id","Name":"/dokploy-redis","Config":{"Labels":{"com.docker.compose.service":"redis","com.docker.compose.project":"stack-1"}},"State":{"Running":true,"Status":"running"},"Mounts":[{"Type":"volume","Name":"dst-vol","Destination":"/data","RW":true}]}]`),
			"volume inspect src-vol":                                                   []byte(`[{"Name":"src-vol"}]`),
			"volume inspect dst-vol":                                                   []byte(`[{"Name":"dst-vol"}]`),
			"stop dst-id":                                                              []byte("dst-id\n"),
			"start dst-id":                                                             []byte("dst-id\n"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}}
	actx.entry("api").ComposeAppName = "stack-1"
	actx.plan = Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}
	step := Step{Kind: StepSyncVolume, App: "api", Ref: "volume:redis -> /data"}
	if err := client.applySyncVolume(context.Background(), actx, step); err != nil {
		t.Fatalf("applySyncVolume: %v", err)
	}
}

func TestApplySyncVolumeBindMountFailsOnStaleSource(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{
		Service:             "web",
		Type:                "bind",
		Source:              "/coolify/old-path",
		Target:              "/data",
		SourceContainerID:   "src-id",
		SourceContainerName: "coolify-web",
	}}
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": []byte("dst-id\n"),
			"inspect --type container dst-id":                                          []byte(`[{"Id":"dst-id","Name":"/dokploy-web","Config":{"Labels":{"com.docker.compose.service":"web","com.docker.compose.project":"stack-1"}},"Mounts":[{"Type":"bind","Source":"/etc/dokploy/data/web","Destination":"/data","RW":true}]}]`),
			"inspect --type container src-id":                                          []byte(`[{"Id":"src-id","Name":"/coolify-web","Mounts":[{"Type":"bind","Source":"/coolify/new-path","Destination":"/data","RW":true}]}]`),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}}
	actx.entry("api").ComposeAppName = "stack-1"
	actx.plan = Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}
	step := Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"}
	err := client.applySyncVolume(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "rescan before retrying") {
		t.Fatalf("expected stale-source error, got %v", err)
	}
}

func TestApplySyncVolumeBindMountRejectsReadOnlyTarget(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{
		Service:           "web",
		Type:              "bind",
		Source:            "/coolify/data/web",
		Target:            "/data",
		SourceContainerID: "src-id",
	}}
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container src-id":                                          []byte(`[{"Id":"src-id","Name":"/coolify-web","Mounts":[{"Type":"bind","Source":"/coolify/data/web","Destination":"/data","RW":true}]}]`),
			"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": []byte("dst-id\n"),
			"inspect --type container dst-id":                                          []byte(`[{"Id":"dst-id","Name":"/dokploy-web","Config":{"Labels":{"com.docker.compose.service":"web","com.docker.compose.project":"stack-1"}},"Mounts":[{"Type":"bind","Source":"/etc/dokploy/data/web","Destination":"/data","RW":false}]}]`),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}}
	actx.entry("api").ComposeAppName = "stack-1"
	actx.plan = Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}
	step := Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"}
	err := client.applySyncVolume(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected read-only target rejection, got %v", err)
	}
}

func TestValidateBindPathRejectsUnsafeInputs(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"empty", "", "is empty"},
		{"relative", "relative/path", "must be absolute"},
		{"root", "/", "host root"},
		{"comma", "/data,with-comma", "unsupported character"},
		{"quote", "/data\"with-quote", "unsupported character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBindPath("source", tc.path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestApplySyncVolumeSkipsDataStoreBackingVolume(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.DataStores = []preparer.DataStoreResource{{
		Kind:    "postgres",
		Service: "db",
	}}
	app.Resources.Volumes = []preparer.VolumeResource{{
		Service: "db",
		Type:    "volume",
		Name:    "pgdata",
		Target:  "/var/lib/postgresql/data",
	}}
	runner := &fakeDockerRunner{}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}}
	step := Step{Kind: StepSyncVolume, App: "api", Ref: "volume:db -> /var/lib/postgresql/data"}
	if err := client.applySyncVolume(context.Background(), actx, step); err != nil {
		t.Fatalf("applySyncVolume: %v", err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("expected no docker runs when volume backs a postgres store, got %#v", runner.runs)
	}
}

func TestPostgresCredsFromEnvDefaults(t *testing.T) {
	creds := postgresCredsFromEnv(map[string]string{})
	if creds.User != "postgres" || creds.Database != "postgres" {
		t.Fatalf("unexpected defaults: %+v", creds)
	}
	creds = postgresCredsFromEnv(map[string]string{"POSTGRES_USER": "alice", "POSTGRES_DB": "app", "POSTGRES_PASSWORD": "pw"})
	if creds.User != "alice" || creds.Database != "app" || creds.Password != "pw" {
		t.Fatalf("unexpected creds: %+v", creds)
	}
}
