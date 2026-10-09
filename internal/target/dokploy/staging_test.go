package dokploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/preparer"
	"gopkg.in/yaml.v3"
)

type sequencedOutputRunner struct {
	fakeDockerRunner
	sequences map[string][][]byte
}

type deadlineRecordingRunner struct {
	fakeDockerRunner
	deadlines []time.Time
}

type foreignVolumeAfterCreateRunner struct {
	fakeDockerRunner
	volumeName string
	created    bool
}

func (r *deadlineRecordingRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("docker call has no deadline")
	}
	r.deadlines = append(r.deadlines, deadline)
	return r.fakeDockerRunner.Output(ctx, args...)
}

func (r *deadlineRecordingRunner) Run(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("docker call has no deadline")
	}
	r.deadlines = append(r.deadlines, deadline)
	return r.fakeDockerRunner.Run(ctx, stdin, stdout, args...)
}

func (r *foreignVolumeAfterCreateRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	if key == "volume inspect "+r.volumeName {
		r.outputArgs = append(r.outputArgs, append([]string{}, args...))
		if !r.created {
			return nil, errors.New("Error response from daemon: volume " + r.volumeName + " not found")
		}
		return []byte(`[{"Name":"` + r.volumeName + `","Labels":{"bort.run-id":"other-run"}}]`), nil
	}
	out, err := r.fakeDockerRunner.Output(ctx, args...)
	if err == nil && len(args) > 2 && args[0] == "volume" && args[1] == "create" && args[len(args)-1] == r.volumeName {
		r.created = true
	}
	return out, err
}

func (r *sequencedOutputRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	if seq, ok := r.sequences[key]; ok && len(seq) > 0 {
		out := seq[0]
		if len(seq) > 1 {
			r.sequences[key] = seq[1:]
		}
		r.outputArgs = append(r.outputArgs, append([]string{}, args...))
		return out, nil
	}
	return r.fakeDockerRunner.Output(ctx, args...)
}

func stagedPlan(t *testing.T, app preparer.AppPlan, runDir string, steps ...Step) Plan {
	t.Helper()
	return Plan{
		RunName: "run1",
		RunDir:  runDir,
		Prepare: preparer.Result{Apps: []preparer.AppPlan{app}},
		Steps:   append(steps, Step{Kind: StepPushImage, App: app.Name}),
	}
}

func decodeCompose(t *testing.T, compose string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(compose), &doc); err != nil {
		t.Fatalf("decode compose: %v\n%s", err, compose)
	}
	return doc
}

func composeVolumeDef(t *testing.T, doc map[string]any, key string) map[string]any {
	t.Helper()
	volumes, _ := doc["volumes"].(map[string]any)
	def, ok := volumes[key].(map[string]any)
	if !ok {
		t.Fatalf("top-level volume %q missing or not a mapping: %#v", key, doc["volumes"])
	}
	return def
}

func TestRewriteComposeStagedVolumesHandlesShortAndLongSyntax(t *testing.T) {
	compose := "services:\n  db:\n    image: postgres:16\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n      - ./local:/host\n  web:\n    image: example/web\n    volumes:\n      - type: volume\n        source: uploads\n        target: /uploads\n      - type: bind\n        source: ./cfg\n        target: /cfg\nvolumes:\n  pgdata:\n  uploads:\n    driver: local\n"
	staged := []stagedVolume{
		{Service: "db", Target: "/var/lib/postgresql/data", VolumeName: "bort-run1-app-db-aaaaaaaa"},
		{Service: "web", Target: "/uploads", VolumeName: "bort-run1-app-web-bbbbbbbb"},
	}
	out, err := rewriteComposeStagedVolumes(compose, staged)
	if err != nil {
		t.Fatalf("rewriteComposeStagedVolumes: %v", err)
	}
	doc := decodeCompose(t, out)
	for key, want := range map[string]string{"pgdata": "bort-run1-app-db-aaaaaaaa", "uploads": "bort-run1-app-web-bbbbbbbb"} {
		def := composeVolumeDef(t, doc, key)
		if def["name"] != want || def["external"] != true {
			t.Fatalf("volume %s not rewritten to external staging volume: %#v", key, def)
		}
	}
	services := doc["services"].(map[string]any)
	dbVolumes := services["db"].(map[string]any)["volumes"].([]any)
	wantDBVolumes := []any{"pgdata:/var/lib/postgresql/data", "./local:/host"}
	if !reflect.DeepEqual(dbVolumes, wantDBVolumes) {
		t.Fatalf("service mounts must be left untouched, got %#v", dbVolumes)
	}
	webVolumes := services["web"].(map[string]any)["volumes"].([]any)
	wantWebVolumes := []any{
		map[string]any{"type": "volume", "source": "uploads", "target": "/uploads"},
		map[string]any{"type": "bind", "source": "./cfg", "target": "/cfg"},
	}
	if !reflect.DeepEqual(webVolumes, wantWebVolumes) {
		t.Fatalf("long-form service mounts must be left untouched, got %#v", webVolumes)
	}

	_, err = rewriteComposeStagedVolumes(compose, []stagedVolume{{Service: "web", Target: "/cfg", VolumeName: "bort-x"}})
	if err == nil || !strings.Contains(err.Error(), "no named volume mounted at /cfg") {
		t.Fatalf("expected error for bind-mounted target, got %v", err)
	}
}

func TestStagingComposeFileIsolatesDataStoreService(t *testing.T) {
	compose := "services:\n  db:\n    image: postgres:16\n    container_name: prod-db\n    restart: always\n    ports:\n      - \"5432:5432\"\n    depends_on:\n      - redis\n    networks:\n      - backend\n    environment:\n      POSTGRES_PASSWORD: ${DB_PASSWORD}\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n      - pglogs:/var/log/postgresql\n      - /srv/db/init:/docker-entrypoint-initdb.d\n      - /srv/db/conf:/etc/postgresql\n  redis:\n    image: redis:7\nvolumes:\n  pgdata:\n  pglogs:\nnetworks:\n  backend:\n"
	staged := []stagedVolume{{Service: "db", Target: "/var/lib/postgresql/data", VolumeName: "bort-run1-app-db-aaaaaaaa"}}
	out, err := stagingComposeFile(compose, "db", staged)
	if err != nil {
		t.Fatalf("stagingComposeFile: %v", err)
	}
	doc := decodeCompose(t, out)
	if _, ok := doc["networks"]; ok {
		t.Fatalf("top-level networks must be dropped:\n%s", out)
	}
	services := doc["services"].(map[string]any)
	if len(services) != 1 {
		t.Fatalf("staging compose must contain only the data store service, got %v", services)
	}
	db := services["db"].(map[string]any)
	for _, key := range []string{"container_name", "ports", "depends_on", "networks"} {
		if _, ok := db[key]; ok {
			t.Fatalf("staging service must not carry %s:\n%s", key, out)
		}
	}
	if db["restart"] != "no" {
		t.Fatalf("staging service restart policy must be \"no\", got %#v", db["restart"])
	}
	if env := db["environment"].(map[string]any); env["POSTGRES_PASSWORD"] != "${DB_PASSWORD}" {
		t.Fatalf("environment must be preserved for interpolation, got %#v", env)
	}
	mounts := db["volumes"].([]any)
	if len(mounts) != 2 || mounts[0] != "pgdata:/var/lib/postgresql/data" || mounts[1] != "/srv/db/init:/docker-entrypoint-initdb.d:ro" {
		t.Fatalf("staging must mount only staged named volumes plus read-only init scripts, got %#v", mounts)
	}
	volumes := doc["volumes"].(map[string]any)
	if len(volumes) != 1 {
		t.Fatalf("only staged volumes may be declared, got %#v", volumes)
	}
	def := composeVolumeDef(t, doc, "pgdata")
	if def["name"] != "bort-run1-app-db-aaaaaaaa" || def["external"] != true {
		t.Fatalf("pgdata must point at the external staging volume, got %#v", def)
	}
}

func TestStagingComposeFileKeepsNestedAndLongFormInitScriptMounts(t *testing.T) {
	compose := "services:\n  db:\n    image: postgres:16\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n      - /srv/db/roles.sql:/docker-entrypoint-initdb.d/01-roles.sql:rw,z\n      - type: bind\n        source: /srv/db/init\n        target: /docker-entrypoint-initdb.d/extra\n        bind:\n          create_host_path: false\n      - /srv/db/seed:/opt/../docker-entrypoint-initdb.d/seed\n      - type: bind\n        source: /srv/db/conf\n        target: /etc/postgresql\n      - /srv/db/initdb.dump:/docker-entrypoint-initdb.dump\n      - /srv/db/other:/docker-entrypoint-initdb.d/../other\nvolumes:\n  pgdata:\n"
	staged := []stagedVolume{{Service: "db", Target: "/var/lib/postgresql/data", VolumeName: "bort-run1-app-db-aaaaaaaa"}}
	out, err := stagingComposeFile(compose, "db", staged)
	if err != nil {
		t.Fatalf("stagingComposeFile: %v", err)
	}
	doc := decodeCompose(t, out)
	mounts := doc["services"].(map[string]any)["db"].(map[string]any)["volumes"].([]any)
	want := []any{
		"pgdata:/var/lib/postgresql/data",
		"/srv/db/roles.sql:/docker-entrypoint-initdb.d/01-roles.sql:z,ro",
		map[string]any{"type": "bind", "source": "/srv/db/init", "target": "/docker-entrypoint-initdb.d/extra", "bind": map[string]any{"create_host_path": false}, "read_only": true},
		"/srv/db/seed:/opt/../docker-entrypoint-initdb.d/seed:ro",
	}
	if fmt.Sprint(mounts) != fmt.Sprint(want) {
		t.Fatalf("staging mounts = %#v, want %#v", mounts, want)
	}
}

func TestStagingComposeFileRefusesRelativeInitScriptMount(t *testing.T) {
	compose := "services:\n  db:\n    image: postgres:16\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n      - ./init:/docker-entrypoint-initdb.d\nvolumes:\n  pgdata:\n"
	staged := []stagedVolume{{Service: "db", Target: "/var/lib/postgresql/data", VolumeName: "bort-run1-app-db-aaaaaaaa"}}
	_, err := stagingComposeFile(compose, "db", staged)
	if err == nil || !strings.Contains(err.Error(), "init script mount ./init -> /docker-entrypoint-initdb.d must use an absolute host path") {
		t.Fatalf("expected relative init script mount refusal, got %v", err)
	}
}

func TestStagingComposeFileExpandsAnchorsAndMergeKeysFromPrunedSections(t *testing.T) {
	compose := "x-pg-env: &pg-env\n  POSTGRES_USER: bob\n  POSTGRES_DB: shared\nx-common: &common\n  restart: always\n  logging: &log\n    driver: json-file\nservices:\n  db:\n    <<: *common\n    image: postgres:16\n    environment:\n      <<: *pg-env\n      POSTGRES_DB: app\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n  web:\n    image: example/web\n    logging: *log\nvolumes:\n  pgdata:\n"
	staged := []stagedVolume{{Service: "db", Target: "/var/lib/postgresql/data", VolumeName: "bort-run1-app-db-aaaaaaaa"}}
	out, err := stagingComposeFile(compose, "db", staged)
	if err != nil {
		t.Fatalf("stagingComposeFile: %v", err)
	}
	if strings.Contains(out, "*") || strings.Contains(out, "&") || strings.Contains(out, "<<") {
		t.Fatalf("staging compose must not reference anchors or merge keys:\n%s", out)
	}
	db := decodeCompose(t, out)["services"].(map[string]any)["db"].(map[string]any)
	env := db["environment"].(map[string]any)
	if env["POSTGRES_USER"] != "bob" || env["POSTGRES_DB"] != "app" {
		t.Fatalf("merged environment must keep explicit keys over anchored ones, got %#v", env)
	}
	if db["logging"].(map[string]any)["driver"] != "json-file" || db["restart"] != "no" {
		t.Fatalf("service-level merge must apply and restart must still be overridden, got %#v", db)
	}

	_, err = stagingComposeFile("services:\n  db:\n    extends:\n      file: base.yaml\n      service: pg\n    volumes:\n      - pgdata:/var/lib/postgresql/data\nvolumes:\n  pgdata:\n", "db", staged)
	if err == nil || !strings.Contains(err.Error(), "extends") {
		t.Fatalf("extends must be refused rather than dropped, got %v", err)
	}
}

func TestStagingEnvFileContentQuotesAndEscapesLikeDokploy(t *testing.T) {
	env := "# comment\nexport PLAIN=hello world\nPRICE=cost is $5 and ${PLAIN:-x} and $$\nQUOTED=\"say \\\"hi\\\"\"\n\nBROKEN_LINE\nPOSTGRES_USER=app # db user\nP=abc#123\nQ=\"a#b\" # c\nR=${X?must-be-set}\nS=${F+--v}\nT=\"unterminated # x\nU='single' trailing\nV=\"line\\nbreak\"\n"
	out := stagingEnvFileContent("stack-1", env, stagingEnvFormatKeepInterpolation)
	want := "APP_NAME=\"stack-1\"\nCOMPOSE_PROJECT_NAME=\"stack-1\"\nPLAIN=\"hello world\"\nPRICE=\"cost is \\$5 and ${PLAIN:-x} and \\$\\$\"\nQUOTED=\"say \\\\\\\"hi\\\\\\\"\"\nPOSTGRES_USER=\"app\"\nP=\"abc\"\nQ=\"a#b\"\nR=\"${X?must-be-set}\"\nS=\"${F+--v}\"\nT=\"\\\"unterminated\"\nU=\"'single' trailing\"\nV=\"line\nbreak\"\nDOCKER_CONFIG=\"/root/.docker\"\n"
	if out != want {
		t.Fatalf("env content mismatch\n got: %q\nwant: %q", out, want)
	}
	if got := stagingEnvFileContent("s", "DOCKER_CONFIG=/custom\n", stagingEnvFormatKeepInterpolation); strings.Count(got, "DOCKER_CONFIG=") != 1 || !strings.Contains(got, "DOCKER_CONFIG=\"/custom\"") {
		t.Fatalf("existing DOCKER_CONFIG must not be overridden, got %q", got)
	}
	mixed := "PRICE=cost is $5 and ${PLAIN:-x}\nQUOTED=\"say \\\"hi\\\"\"\n"
	if got, want := stagingEnvFileContent("s", mixed, stagingEnvFormatEscapeEveryDollar), "APP_NAME=\"s\"\nCOMPOSE_PROJECT_NAME=\"s\"\nPRICE=\"cost is \\$5 and \\${PLAIN:-x}\"\nQUOTED=\"say \\\\\\\"hi\\\\\\\"\"\nDOCKER_CONFIG=\"/root/.docker\"\n"; got != want {
		t.Fatalf("v0.30.0 format mismatch\n got: %q\nwant: %q", got, want)
	}
	if got, want := stagingEnvFileContent("s", mixed, stagingEnvFormatRaw), "APP_NAME=s\nCOMPOSE_PROJECT_NAME=s\nPRICE=cost is $5 and ${PLAIN:-x}\nQUOTED=say \\\"hi\\\"\nDOCKER_CONFIG=/root/.docker\n"; got != want {
		t.Fatalf("v0.29 format mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestStagingEnvFormatForDokployVersion(t *testing.T) {
	for version, want := range map[string]stagingEnvFormat{
		"v0.29.3":  stagingEnvFormatRaw,
		"v0.30.0":  stagingEnvFormatEscapeEveryDollar,
		"v0.30.2":  stagingEnvFormatEscapeEveryDollar,
		"v0.30.3":  stagingEnvFormatKeepInterpolation,
		"v0.30.7":  stagingEnvFormatKeepInterpolation,
		"v0.31.0":  stagingEnvFormatKeepInterpolation,
		"1.0.0":    stagingEnvFormatKeepInterpolation,
		" v0.30.3": stagingEnvFormatKeepInterpolation,
	} {
		got, err := stagingEnvFormatForDokployVersion(version)
		if err != nil || got != want {
			t.Fatalf("version %q: got format %d err %v, want %d", version, got, err, want)
		}
	}
	for _, version := range []string{"", "latest", "v0.30", "v0.30.3-canary", "canary"} {
		if got, err := stagingEnvFormatForDokployVersion(version); err == nil || got != stagingEnvFormatUnresolved {
			t.Fatalf("version %q must fail closed, got format %d err %v", version, got, err)
		}
	}
}

func TestApplyResolvesDokployVersionBeforeStagedRestore(t *testing.T) {
	plan, _, _ := stagedRestoreFixture(t)
	for name, tc := range map[string]struct {
		version string
		status  int
		want    string
	}{
		"unparseable version": {version: `"latest"`, status: http.StatusOK, want: `Dokploy reported version "latest"`},
		"request failure":     {version: `{"message":"Unauthorized"}`, status: http.StatusUnauthorized, want: "resolve Dokploy version before staged state transfer"},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/settings.getDokployVersion" || r.Header.Get("x-api-key") != "secret" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.version))
			}))
			defer server.Close()
			runner := &fakeDockerRunner{}
			client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
			err := client.Apply(context.Background(), plan)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if len(runner.outputArgs) != 0 || len(runner.runs) != 0 {
				t.Fatalf("source was touched before the version refusal: outputs=%v runs=%v", runner.outputArgs, runner.runs)
			}
		})
	}
}

func TestLocalDockerRunnerIsolatesComposeEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim")
	}
	shim := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\necho \"args: $*\"\nenv | sort\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BORT_SHELL_LEAK", "1")
	runner := localDockerRunner{Path: shim}
	command, err := runner.command(context.Background(), []string{"compose", "-p", "stage", "config"})
	if err != nil {
		t.Fatalf("build compose command: %v", err)
	}
	keys := make([]string, 0, len(command.Env))
	for _, entry := range command.Env {
		key, _, _ := strings.Cut(entry, "=")
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"HOME", "PATH"}) {
		t.Fatalf("compose command environment keys must be exactly HOME and PATH, got %v", keys)
	}
	compose, err := runner.Output(context.Background(), "compose", "-p", "stage", "config")
	if err != nil {
		t.Fatalf("compose shim: %v", err)
	}
	if !strings.HasPrefix(string(compose), "args: --host unix:///var/run/docker.sock compose -p stage config\n") {
		t.Fatalf("compose must pin the local daemon through --host, got %s", compose)
	}
	if strings.Contains(string(compose), "BORT_SHELL_LEAK=") || strings.Contains(string(compose), "DOCKER_HOST=") || !strings.Contains(string(compose), "\nPATH=") || !strings.Contains(string(compose), "HOME=") {
		t.Fatalf("compose must run with only PATH and HOME, got %s", compose)
	}
	other, err := runner.Output(context.Background(), "ps")
	if err != nil {
		t.Fatalf("ps shim: %v", err)
	}
	if !strings.HasPrefix(string(other), "args: ps\n") || !strings.Contains(string(other), "BORT_SHELL_LEAK=1") || !strings.Contains(string(other), "DOCKER_HOST=unix:///var/run/docker.sock") {
		t.Fatalf("non-compose commands must keep the local environment and DOCKER_HOST pin, got %s", other)
	}
}

func TestValidatePlanReadyForLiveApplyRefusesComposeProjectNameInStagedDataStore(t *testing.T) {
	plan, _, _ := stagedRestoreFixture(t)
	composePath := filepath.Join(plan.Prepare.BundleDir, "api", "compose.yaml")
	original := string(plan.BundleFiles[filepath.Clean(composePath)])
	for name, tc := range map[string]struct {
		compose string
		refuse  bool
	}{
		"braced reference in db":                {compose: strings.Replace(original, "${DB_PASSWORD}", "${COMPOSE_PROJECT_NAME}_db", 1), refuse: true},
		"bare reference in db":                  {compose: strings.Replace(original, "${DB_PASSWORD}", "$COMPOSE_PROJECT_NAME", 1), refuse: true},
		"escaped reference in db":               {compose: strings.Replace(original, "${DB_PASSWORD}", "$${COMPOSE_PROJECT_NAME}_db", 1), refuse: false},
		"reference before escaped dollar in db": {compose: strings.Replace(original, "${DB_PASSWORD}", "$COMPOSE_PROJECT_NAME$$archive", 1), refuse: true},
		"longer variable name in db":            {compose: strings.Replace(original, "${DB_PASSWORD}", "${COMPOSE_PROJECT_NAME_SUFFIX}", 1), refuse: false},
		"reference in other service":            {compose: strings.Replace(original, "image: example/web\n", "image: example/web\n    environment:\n      APP: ${COMPOSE_PROJECT_NAME}\n", 1), refuse: false},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.compose == original {
				t.Fatal("fixture did not change")
			}
			plan.BundleFiles[filepath.Clean(composePath)] = []byte(tc.compose)
			err := validatePlanReadyForLiveApply(plan)
			if tc.refuse && (!errors.Is(err, ErrNotImplemented) || !strings.Contains(err.Error(), "COMPOSE_PROJECT_NAME")) {
				t.Fatalf("expected COMPOSE_PROJECT_NAME refusal, got %v", err)
			}
			if !tc.refuse && err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
		})
	}
	plan.BundleFiles[filepath.Clean(composePath)] = []byte(original)
	plan.BundleFiles[filepath.Clean(filepath.Join(plan.Prepare.BundleDir, "api", ".env"))] = []byte("DB_PASSWORD=s3cret\nPGDATA=/var/lib/postgresql/data/${COMPOSE_PROJECT_NAME}\n")
	if err := validatePlanReadyForLiveApply(plan); !errors.Is(err, ErrNotImplemented) || !strings.Contains(err.Error(), "COMPOSE_PROJECT_NAME") {
		t.Fatalf("expected refusal for a COMPOSE_PROJECT_NAME reference in the env file, got %v", err)
	}
}

func TestValidatePlanReadyForLiveApplyRefusesRelativeInitScriptMountBeforePause(t *testing.T) {
	plan, _, _ := stagedRestoreFixture(t)
	composePath := filepath.Join(plan.Prepare.BundleDir, "api", "compose.yaml")
	original := string(plan.BundleFiles[filepath.Clean(composePath)])
	withInit := strings.Replace(original, "      - pgdata:/var/lib/postgresql/data\n", "      - pgdata:/var/lib/postgresql/data\n      - ./init:/docker-entrypoint-initdb.d\n", 1)
	if withInit == original {
		t.Fatal("fixture did not change")
	}
	plan.BundleFiles[filepath.Clean(composePath)] = []byte(withInit)
	err := validatePlanReadyForLiveApply(plan)
	if err == nil || !strings.Contains(err.Error(), "init script mount ./init -> /docker-entrypoint-initdb.d must use an absolute host path") || !strings.Contains(err.Error(), "choose a recreate or managed data store strategy") {
		t.Fatalf("expected relative init script mount refusal before live apply, got %v", err)
	}
	plan.BundleFiles[filepath.Clean(composePath)] = []byte(strings.Replace(withInit, "./init:", "/srv/db/init:", 1))
	if err := validatePlanReadyForLiveApply(plan); err != nil {
		t.Fatalf("absolute init script mount must stage: %v", err)
	}
}

func TestValidatePlanReadyForLiveApplyRefusesStagedComposeSecretsAndConfigsBeforePause(t *testing.T) {
	for _, input := range []string{"secrets", "configs"} {
		t.Run(input, func(t *testing.T) {
			plan, _, _ := stagedRestoreFixture(t)
			composePath := filepath.Join(plan.Prepare.BundleDir, "api", "compose.yaml")
			original := string(plan.BundleFiles[filepath.Clean(composePath)])
			compose := strings.Replace(original, "    volumes:\n", "    "+input+":\n      - db_input\n    volumes:\n", 1) + "\n" + input + ":\n  db_input:\n    file: /run/bort-db-input\n"
			plan.BundleFiles[filepath.Clean(composePath)] = []byte(compose)

			err := ValidatePlannedPostgresDataDirs(plan)
			if !errors.Is(err, ErrNotImplemented) || !strings.Contains(err.Error(), "uses Compose "+input) || !strings.Contains(err.Error(), "cannot preserve") {
				t.Fatalf("expected staged %s refusal before live apply, got %v", input, err)
			}
		})
	}
}

func stagingCompatibleClient(t *testing.T, runner dockerRunner, createEnvFile bool, command string) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/compose.one" || r.Header.Get("x-api-key") != "secret" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(Compose{ComposeID: r.URL.Query().Get("composeId"), AppName: "stack-1", CreateEnvFile: &createEnvFile, Command: command})
	}))
	t.Cleanup(server.Close)
	return &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
}

func TestStagedRestoreRefusesIncompatibleDokployComposeSettings(t *testing.T) {
	plan, step, _ := stagedRestoreFixture(t)
	for name, tc := range map[string]struct {
		createEnvFile bool
		command       string
		want          string
	}{
		"env file disabled": {createEnvFile: false, want: ".env creation disabled"},
		"custom command":    {createEnvFile: true, command: "docker compose -f compose.yaml up -d", want: "custom deploy command"},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeDockerRunner{}
			client := stagingCompatibleClient(t, runner, tc.createEnvFile, tc.command)
			actx := &applyContext{cache: map[string]*appCache{}, plan: plan, stagingEnvFormat: stagingEnvFormatKeepInterpolation}
			actx.entry("api").ComposeID = "c1"
			actx.entry("api").ComposeAppName = "stack-1"
			err := client.applyRestoreDataStore(context.Background(), actx, step)
			if !errors.Is(err, ErrNotImplemented) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q refusal, got %v", tc.want, err)
			}
			if len(runner.outputArgs) != 0 || len(runner.runs) != 0 {
				t.Fatalf("staging volumes were touched before the refusal: outputs=%v runs=%v", runner.outputArgs, runner.runs)
			}
		})
	}
}

func TestApplyCreateServiceRefusesAdoptedComposeWithoutEnvFile(t *testing.T) {
	plan, _, _ := stagedRestoreFixture(t)
	disabled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.search":
			_ = json.NewEncoder(w).Encode(composeSearchResponse{Items: []Compose{{ComposeID: "c1", Name: "api", AppName: "stack-1"}}, Total: 1})
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.one":
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: "c1", Name: "api", AppName: "stack-1", CreateEnvFile: &disabled})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: &fakeDockerRunner{}}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{"api": {EnvironmentID: "env1"}}}
	err := client.applyCreateService(context.Background(), actx, Step{Kind: StepCreateService, App: "api", Ref: "api"})
	if !errors.Is(err, ErrNotImplemented) || !strings.Contains(err.Error(), ".env creation disabled") {
		t.Fatalf("expected adopted compose refusal before the source pause, got %v", err)
	}
}

func TestDeployComposeForApplyRefusesIncompatibleSettingsAfterStagedRestore(t *testing.T) {
	plan, _, _ := stagedRestoreFixture(t)
	disabled := false
	var writes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.one":
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: "c1", Name: "api", AppName: "stack-1", CreateEnvFile: &disabled})
		default:
			writes = append(writes, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: &fakeDockerRunner{}}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	actx.entry("api").ComposeID = "c1"
	actx.entry("api").ComposeAppName = "stack-1"
	err := client.deployComposeForApply(context.Background(), actx, "api", "services: {}\n", "DB_PASSWORD=s3cret\n")
	if !errors.Is(err, ErrNotImplemented) || !strings.Contains(err.Error(), ".env creation disabled") {
		t.Fatalf("expected deploy-time refusal, got %v", err)
	}
	if len(writes) != 0 {
		t.Fatalf("compose was written or deployed despite the refusal: %v", writes)
	}
}

func stagedSyncFixture(t *testing.T) (preparer.AppPlan, Plan, Step, stagedVolume) {
	t.Helper()
	app := preparer.AppPlan{Name: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{{ServiceName: "web", ContainerID: "src-id", ContainerName: "coolify-web"}}
	app.Resources.Volumes = []preparer.VolumeResource{{
		Service:             "web",
		Type:                "volume",
		Name:                "src-vol",
		Target:              "/data",
		SourceContainerID:   "src-id",
		SourceContainerName: "coolify-web",
	}}
	step := Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"}
	plan := stagedPlan(t, app, t.TempDir(), Step{Kind: StepPauseSource, App: "api"}, step)
	staged, ok := stagedVolumeFor(plan, "api", app.Resources.Volumes[0])
	if !ok {
		t.Fatalf("volume not staged in plan %v", stepKinds(plan.Steps))
	}
	return app, plan, step, staged
}

func stoppedSourceInspect(startedAt, finishedAt string) []byte {
	return []byte(`[{"Id":"src-id","Name":"/coolify-web","State":{"Running":false,"StartedAt":"` + startedAt + `","FinishedAt":"` + finishedAt + `"}}]`)
}

func ownedStagingVolumeInspect(plan Plan, volume stagedVolume) []byte {
	owner := plan.RunID
	if owner == "" {
		owner = plan.RunName
	}
	return stagingVolumeInspect(plan, volume, owner)
}

func stagingVolumeInspect(plan Plan, volume stagedVolume, owner string) []byte {
	labels := map[string]string{
		"bort.run":     plan.RunName,
		"bort.run-id":  owner,
		"bort.app":     "api",
		"bort.service": volume.Service,
		"bort.target":  volume.Target,
	}
	state, _ := json.Marshal([]stagingVolumeState{{Name: volume.VolumeName, Labels: labels}})
	return state
}

func sharedStagingVolumeInspect(plan Plan, volume stagedVolume) []byte {
	owner := plan.RunID
	if owner == "" {
		owner = plan.RunName
	}
	labels := map[string]string{
		"bort.run":    plan.RunName,
		"bort.run-id": owner,
		"bort.app":    "api",
		"bort.source": volume.Source.Name,
	}
	state, _ := json.Marshal([]stagingVolumeState{{Name: volume.VolumeName, Labels: labels}})
	return state
}

func targetAuthorityTestClient(t *testing.T, runner dockerRunner, plan *Plan) *Client {
	t.Helper()
	const projectID, environmentID, composeID, composeAppName = "project-1", "environment-1", "compose-1", "stack-1"
	plan.TargetIdentities = map[string]TargetIdentity{"api": {
		ProjectID: projectID, EnvironmentID: environmentID, ComposeID: composeID, ComposeAppName: composeAppName,
	}}
	plan.Prepare.Apps[0].TargetResources = &preparer.TargetResources{Dokploy: &preparer.DokployResources{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/project.one":
			_ = json.NewEncoder(w).Encode(Project{ProjectID: projectID, Environments: []ProjectEnvironment{{EnvironmentID: environmentID}}})
		case "/api/compose.one":
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: composeID, EnvironmentID: environmentID, AppName: composeAppName})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
}

func TestRequireStagingVolumeOwnedRejectsMismatchedIdentityLabel(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	for label, value := range map[string]string{
		"bort.run":     "other-run",
		"bort.app":     "other-app",
		"bort.service": "worker",
		"bort.target":  "/other",
	} {
		t.Run(label, func(t *testing.T) {
			labels := map[string]string{
				"bort.run":     plan.RunName,
				"bort.run-id":  plan.RunName,
				"bort.app":     "api",
				"bort.service": staged.Service,
				"bort.target":  staged.Target,
			}
			labels[label] = value
			state, err := json.Marshal([]stagingVolumeState{{Name: staged.VolumeName, Labels: labels}})
			if err != nil {
				t.Fatal(err)
			}
			runner := &fakeDockerRunner{outputs: map[string][]byte{"volume inspect " + staged.VolumeName: state}}
			err = requireStagingVolumeOwned(context.Background(), runner, plan, "api", staged)
			if err == nil || !strings.Contains(err.Error(), "label "+label) {
				t.Fatalf("expected %s mismatch refusal, got %v", label, err)
			}
		})
	}
}

func TestApplyCreateVolumeCreatesOnlyItsOwnStagedVolume(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", Name: "src-data", Target: "/data"},
		{Service: "web", Type: "volume", Name: "", Target: "/cache"},
	}
	plan := stagedPlan(t, app, t.TempDir(),
		Step{Kind: StepCreateVolume, App: "api", Ref: "src-data"},
		Step{Kind: StepCreateVolume, App: "api", Ref: "/cache"},
		Step{Kind: StepPauseSource, App: "api"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /cache"},
	)
	staged := stagedVolumesForApp(plan, "api")
	if len(staged) != 2 {
		t.Fatalf("expected two staged volumes, got %#v", staged)
	}
	runner := &fakeDockerRunner{outputs: map[string][]byte{"volume create": []byte("created\n")}, outputErrs: map[string]error{}}
	for _, volume := range staged {
		runner.outputErrs["volume inspect --format {{index .Labels \"bort.run-id\"}} "+volume.VolumeName] = errors.New("Error response from daemon: volume " + volume.VolumeName + " not found")
		runner.outputs["volume inspect "+volume.VolumeName] = ownedStagingVolumeInspect(plan, volume)
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan}
	if err := client.applyCreateVolume(context.Background(), actx, plan.Steps[1]); err != nil {
		t.Fatalf("applyCreateVolume: %v", err)
	}
	var created []string
	for _, args := range runner.outputArgs {
		if len(args) > 2 && args[0] == "volume" && args[1] == "create" {
			created = append(created, args[len(args)-1])
		}
	}
	if len(created) != 1 || created[0] != staged[1].VolumeName {
		t.Fatalf("expected only %s to be created for the /cache step, got %v", staged[1].VolumeName, created)
	}
}

func TestEnsureStagingVolumeRejectsConcurrentForeignCreate(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	runner := &foreignVolumeAfterCreateRunner{
		fakeDockerRunner: fakeDockerRunner{outputs: map[string][]byte{"volume create": []byte(staged.VolumeName + "\n")}},
		volumeName:       staged.VolumeName,
	}
	if err := ensureStagingVolume(context.Background(), runner, plan, "api", staged); err == nil || !strings.Contains(err.Error(), "not the staged") {
		t.Fatalf("expected concurrent foreign volume to be rejected, got %v", err)
	}
	if !runner.created {
		t.Fatalf("foreign replacement was exposed before volume creation: calls=%v", runner.outputArgs)
	}
}

func TestAcquireStagingVolumePinVerifiesOwnershipAndKeepsRunning(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
	}}
	pin, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
	if err != nil || pin.containerID != "pin-id" {
		t.Fatalf("expected an owned staging volume pin, pin=%#v err=%v", pin, err)
	}
	if !runner.activePins[pin.containerID].State.Running || fakeOutputCalled(runner, "rm", "-f", pin.containerID) {
		t.Fatalf("staging volume pin was not left running: active=%#v calls=%v", runner.activePins, runner.outputArgs)
	}
	pinRuns := 0
	for _, args := range runner.outputArgs {
		if len(args) > 1 && args[0] == "run" && args[1] == "-d" {
			pinRuns++
			if got := strings.Join(args, " "); !strings.HasSuffix(got, volumeCopyImage+" sh -c while :; do sleep 2147483647; done") {
				t.Fatalf("pin does not use the long-running keepalive command: %s", got)
			}
		}
	}
	if pinRuns != 1 {
		t.Fatalf("pin creation calls = %d, want 1: %v", pinRuns, runner.outputArgs)
	}
}

func TestAcquireStagingVolumePinRejectsReplacementBeforeOperation(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + staged.VolumeName: []byte(`[{"Name":"` + staged.VolumeName + `","Labels":{"bort.run-id":"other-run"}}]`),
	}}
	_, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
	if err == nil || !strings.Contains(err.Error(), "not the staged") {
		t.Fatalf("expected replacement volume rejection, got %v", err)
	}
}

type replaceStagingVolumeAfterPinRunner struct {
	*fakeDockerRunner
	volumeName string
}

func (r *replaceStagingVolumeAfterPinRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	out, err := r.fakeDockerRunner.Output(ctx, args...)
	if err == nil && len(args) > 1 && args[0] == "run" && args[1] == "-d" && slices.Contains(args, stagingVolumePinLabel+"=true") {
		r.outputs["volume inspect "+r.volumeName] = []byte(`[{"Name":"` + r.volumeName + `","Labels":{"bort.run":"run1","bort.run-id":"other-run","bort.app":"api","bort.service":"web","bort.target":"/data"}}]`)
	}
	return out, err
}

func TestSyncVolumeToStagingRechecksOwnershipAfterPinCreation(t *testing.T) {
	_, plan, step, staged := stagedSyncFixture(t)
	runner := &replaceStagingVolumeAfterPinRunner{
		fakeDockerRunner: &fakeDockerRunner{outputs: map[string][]byte{
			"volume inspect src-vol":              []byte(`[{"Name":"src-vol"}]`),
			"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
		}},
		volumeName: staged.VolumeName,
	}
	client := &Client{Docker: runner}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}

	err := client.applySyncVolume(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "verify staging volume ownership after pinning") || !strings.Contains(err.Error(), "bort.run-id") {
		t.Fatalf("expected post-pin ownership refusal, got %v", err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("ownership drift after pin creation must abort before copy or clear, runs=%#v", runner.runs)
	}
	if pin := actx.stagingVolumePins["api"]; pin.containerID != "pin-id" {
		t.Fatalf("known pin was not cached for cleanup after post-pin refusal: %#v", actx.stagingVolumePins)
	}
}

func TestApplyPushImageRefusesMissingPinAfterDurableTransfer(t *testing.T) {
	app, plan, _, staged := stagedSyncFixture(t)
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  web:\n    image: example/web\n    volumes:\n      - data:/data\nvolumes:\n  data:\n"
	composePath := filepath.Join(appDir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	app.Directory = "api"
	app.TargetResources = &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{ComposePath: "compose.yaml"}}}
	plan.Prepare = preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{app}}
	plan.BundleFiles = map[string][]byte{filepath.Clean(composePath): []byte(compose)}
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"image inspect example/web":           []byte(`[{}]`),
		"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
	}}
	client := &Client{Docker: runner}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	entry := actx.entry("api")
	entry.ComposeID = "compose-1"
	entry.ComposeAppName = "stack-1"
	entry.MigratedVolumeMounts = map[string]migratedVolumeMount{
		migratedMountKey("web", "/data"): {Service: "web", Target: "/data", VolumeName: staged.VolumeName},
	}

	err := client.applyPushImage(context.Background(), actx, Step{Kind: StepPushImage, App: "api", Ref: "api"})
	if err == nil || !strings.Contains(err.Error(), "missing after state transfer") || !strings.Contains(err.Error(), "identity was unprotected") {
		t.Fatalf("expected missing transferred-state pin refusal, got %v", err)
	}
	for _, args := range runner.outputArgs {
		if len(args) > 1 && args[0] == "run" && args[1] == "-d" {
			t.Fatalf("push_image recreated a missing transferred-state pin: calls=%v", runner.outputArgs)
		}
	}
}

func TestApplyKeepsSourceStoppedWhenTransferredPinIsMissing(t *testing.T) {
	for _, failBeforePin := range []bool{false, true} {
		name := "missing pin at acquisition"
		if failBeforePin {
			name = "failure before pin acquisition"
		}
		t.Run(name, func(t *testing.T) {
			testApplyKeepsSourceStoppedWhenTransferredPinIsMissing(t, failBeforePin)
		})
	}
}

func testApplyKeepsSourceStoppedWhenTransferredPinIsMissing(t *testing.T, failBeforePin bool) {
	t.Helper()
	app, plan, step, staged := stagedSyncFixture(t)
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  web:\n    image: example/web\n    volumes:\n      - data:/data\nvolumes:\n  data:\n"
	composePath := filepath.Join(appDir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	app.Directory = "api"
	app.TargetResources = &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{ComposePath: "compose.yaml"}}}
	plan.Prepare = preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{app}}
	plan.BundleFiles = map[string][]byte{filepath.Clean(composePath): []byte(compose)}
	plan.Steps = []Step{{Kind: StepPauseSource, App: "api", Ref: "api"}, step, {Kind: StepPushImage, App: "api", Ref: "api"}}
	plan.RecoveryCommand = "sudo bort status --run missing-pin"
	plan.ResumeFrom = 2
	plan.TargetIdentities = map[string]TargetIdentity{"api": {
		ProjectID:      "project-1",
		EnvironmentID:  "environment-1",
		ComposeID:      "compose-1",
		ComposeAppName: "stack-1",
	}}
	var failure StepProgress
	onProgress := func(progress StepProgress) {
		if progress.Status == StepStatusError {
			failure = progress
		}
	}
	plan.OnProgress = &onProgress

	seed := &applyContext{plan: plan, cache: map[string]*appCache{}}
	seed.entry("api").SourcePauseRecorded = true
	seed.entry("api").SourcePausedContainers = []sourcePausedContainer{{
		ID:         "src-id",
		Stopped:    true,
		StartedAt:  "2026-01-01T00:00:00Z",
		FinishedAt: "2026-01-02T00:00:00Z",
	}}
	if err := seed.persistSourcePauseState(); err != nil {
		t.Fatal(err)
	}
	if err := seed.recordMigratedVolumeMount("api", migratedVolumeMount{Service: "web", Target: "/data", VolumeName: staged.VolumeName}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"image inspect example/web":                  []byte(`[{}]`),
		"volume inspect " + staged.VolumeName:        ownedStagingVolumeInspect(plan, staged),
		"inspect --type container src-id":            stoppedSourceInspect("2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z"),
		"ps -a --filter volume=" + staged.VolumeName: []byte(""),
	}}
	if failBeforePin {
		runner.outputErrs = map[string]error{"image inspect example/web": errors.New("image unavailable")}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/project.one":
			_ = json.NewEncoder(w).Encode(Project{ProjectID: "project-1", Environments: []ProjectEnvironment{{EnvironmentID: "environment-1"}}})
		case "/api/compose.one":
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: "compose-1", EnvironmentID: "environment-1", AppName: "stack-1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	err := (&Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}).Apply(context.Background(), plan)
	if err == nil || !isUnsafeSourceResumeError(err) || !strings.Contains(err.Error(), "missing after state transfer") || !strings.Contains(err.Error(), "`sudo bort status --run missing-pin`") {
		t.Fatalf("expected a missing transferred-state pin to block source recovery, got %v", err)
	}
	if fakeOutputCalled(runner, "start", "src-id") {
		t.Fatalf("source restarted after its transferred-state pin disappeared: calls=%v", runner.outputArgs)
	}
	if !failure.AuthorityRecoveryRequired {
		t.Fatalf("missing-pin failure was not marked for authority recovery: %#v", failure)
	}
}

func TestSyncVolumeReplayRefusesMissingPinAfterDurableTransfer(t *testing.T) {
	_, plan, step, staged := stagedSyncFixture(t)
	seed := &applyContext{plan: plan, cache: map[string]*appCache{}}
	if err := seed.recordMigratedVolumeMount("api", migratedVolumeMount{Service: staged.Service, Target: staged.Target, VolumeName: staged.VolumeName}); err != nil {
		t.Fatal(err)
	}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	if err := actx.loadMigratedVolumeMounts(); err != nil {
		t.Fatal(err)
	}
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect src-vol":              []byte(`[{"Name":"src-vol"}]`),
		"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
	}}

	err := (&Client{Docker: runner}).applySyncVolume(context.Background(), actx, step)
	if err == nil || !isUnsafeSourceResumeError(err) || !strings.Contains(err.Error(), "missing after state transfer") {
		t.Fatalf("expected same-transfer replay to refuse pin recreation, got %v", err)
	}
	for _, args := range runner.outputArgs {
		if len(args) > 1 && args[0] == "run" && args[1] == "-d" {
			t.Fatalf("same-transfer replay recreated the missing pin: calls=%v", runner.outputArgs)
		}
	}
}

func TestLaterStagedTransferRefusesMissingAppPin(t *testing.T) {
	app, plan, firstStep, first := stagedSyncFixture(t)
	secondSource := preparer.VolumeResource{
		Service:             "worker",
		Type:                "volume",
		Name:                "src-cache",
		Target:              "/cache",
		SourceContainerID:   "src-id",
		SourceContainerName: "coolify-web",
	}
	app.Resources.Volumes = append(app.Resources.Volumes, secondSource)
	secondStep := Step{Kind: StepSyncVolume, App: "api", Ref: "volume:worker -> /cache"}
	plan = stagedPlan(t, app, plan.RunDir, Step{Kind: StepPauseSource, App: "api"}, firstStep, secondStep)
	second, ok := stagedVolumeFor(plan, "api", secondSource)
	if !ok {
		t.Fatal("second volume is not staged")
	}
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect src-cache":            []byte(`[{"Name":"src-cache"}]`),
		"volume inspect " + first.VolumeName:  ownedStagingVolumeInspect(plan, first),
		"volume inspect " + second.VolumeName: ownedStagingVolumeInspect(plan, second),
	}}
	client := &Client{Docker: runner}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	actx.entry("api").MigratedVolumeMounts = map[string]migratedVolumeMount{
		migratedMountKey(first.Service, first.Target): {Service: first.Service, Target: first.Target, VolumeName: first.VolumeName},
	}

	err := client.applySyncVolume(context.Background(), actx, secondStep)
	if err == nil || !strings.Contains(err.Error(), "missing after state transfer") {
		t.Fatalf("expected later transfer to refuse a missing app pin, got %v", err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("later transfer copied or cleared data without the original pin: runs=%#v", runner.runs)
	}
	for _, args := range runner.outputArgs {
		if len(args) > 1 && args[0] == "run" && args[1] == "-d" {
			t.Fatalf("later transfer recreated the missing app pin: calls=%v", runner.outputArgs)
		}
	}
}

func TestAcquireStagingVolumePinHoldsEveryVolumeUntilExplicitRelease(t *testing.T) {
	_, plan, _, first := stagedSyncFixture(t)
	second := first
	second.Service = "worker"
	second.Target = "/work"
	second.VolumeName += "-worker"
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + first.VolumeName:  ownedStagingVolumeInspect(plan, first),
		"volume inspect " + second.VolumeName: ownedStagingVolumeInspect(plan, second),
	}}
	volumes := []stagedVolume{first, second}
	pin, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", volumes, true)
	if err != nil {
		t.Fatalf("acquireStagingVolumePin: %v", err)
	}
	var runCalls []string
	for _, args := range runner.outputArgs {
		if len(args) > 1 && args[0] == "run" && args[1] == "-d" {
			runCalls = append(runCalls, strings.Join(args, " "))
		}
	}
	if len(runCalls) != 1 || !strings.Contains(runCalls[0], "--restart unless-stopped") || !strings.Contains(runCalls[0], "-v "+first.VolumeName+":/bort-volume/0:ro") || !strings.Contains(runCalls[0], "-v "+second.VolumeName+":/bort-volume/1:ro") {
		t.Fatalf("expected one persistent read-only helper mounting every staging volume, got %v", runCalls)
	}
	if err := releaseStagingVolumePin(context.Background(), runner, plan, volumes, pin); err != nil {
		t.Fatalf("releaseStagingVolumePin: %v", err)
	}
	if !fakeOutputCalled(runner, "rm", "-f", pin.containerID) {
		t.Fatalf("expected explicit staging volume pin release, calls=%v", runner.outputArgs)
	}
}

func TestRequireStagingVolumeAttachmentSetsBatchesMultipleVolumes(t *testing.T) {
	volumes := []stagedVolume{{VolumeName: "stage-web"}, {VolumeName: "stage-worker"}}
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"ps -a --filter volume=stage-web --filter volume=stage-worker --format {{.ID}}": []byte("pin-id\ntarget-id\n"),
		"inspect --type container pin-id target-id": []byte(`[
			{"Id":"pin-id","Mounts":[{"Type":"volume","Name":"stage-web"},{"Type":"volume","Name":"stage-worker"}]},
			{"Id":"target-id","Mounts":[{"Type":"volume","Name":"stage-web"}]}
		]`),
	}}
	allowed := map[string][]string{
		"stage-web":    {"pin-id", "target-id"},
		"stage-worker": {"pin-id"},
	}
	if err := requireStagingVolumeAttachmentSets(context.Background(), runner, volumes, allowed); err != nil {
		t.Fatal(err)
	}
	if !fakeOutputCalled(runner, "ps", "-a", "--filter", "volume=stage-web", "--filter", "volume=stage-worker", "--format", "{{.ID}}") {
		t.Fatalf("multiple volume attachments were not discovered in one Docker query: %v", runner.outputArgs)
	}
	for _, volume := range volumes {
		if fakeOutputCalled(runner, "ps", "-a", "--filter", "volume="+volume.VolumeName, "--format", "{{.ID}}") {
			t.Fatalf("volume %s was queried separately: %v", volume.VolumeName, runner.outputArgs)
		}
	}
}

func TestReleaseStagingVolumePinsRequiresAuthorityAttachmentSet(t *testing.T) {
	for _, targetAuthority := range []bool{false, true} {
		name := "source"
		if targetAuthority {
			name = "target"
		}
		t.Run(name, func(t *testing.T) {
			_, plan, _, staged := stagedSyncFixture(t)
			plan.RunID = "run-digest"
			staged, _ = stagedVolumeFor(plan, "api", plan.Prepare.Apps[0].Resources.Volumes[0])
			runner := &fakeDockerRunner{outputs: map[string][]byte{
				"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
			}}
			pin, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
			if err != nil {
				t.Fatal(err)
			}
			client := &Client{Docker: runner}
			if targetAuthority {
				const targetID = "target-id"
				client = targetAuthorityTestClient(t, runner, &plan)
				runner.outputs["ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}"] = []byte(targetID + "\n")
				runner.outputs["inspect --type container "+targetID] = []byte(`[{"Id":"` + targetID + `","Name":"/web","Config":{"Labels":{"com.docker.compose.service":"web","com.docker.compose.project":"stack-1"}},"State":{"Running":true,"Status":"running"},"Mounts":[{"Type":"volume","Name":"` + staged.VolumeName + `","Destination":"/data","RW":true}]}]`)
				runner.activeComposeProjects = map[string][]string{"stack-1": {targetID}}
				state := &applyContext{plan: plan, cache: map[string]*appCache{}}
				if err := state.recordMigratedVolumeMount("api", migratedVolumeMount{Service: "web", Target: "/data", VolumeName: staged.VolumeName}); err != nil {
					t.Fatal(err)
				}
			}

			if err := client.ReleaseStagingVolumePins(context.Background(), plan, targetAuthority); err != nil {
				t.Fatalf("release pins under %s authority: %v", name, err)
			}
			if _, ok := runner.activePins[pin.containerID]; ok {
				t.Fatalf("verified pin remained after %s-authority cleanup: %#v", name, runner.activePins)
			}
			if err := client.ReleaseStagingVolumePins(context.Background(), plan, targetAuthority); err != nil {
				t.Fatalf("repeated %s-authority cleanup was not idempotent: %v", name, err)
			}
		})
	}
}

func TestReleaseStagingVolumePinsBoundsDockerCallsByCallerDeadline(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	base := fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
	}}
	if _, err := acquireStagingVolumePin(context.Background(), &base, plan, "api", []stagedVolume{staged}, true); err != nil {
		t.Fatal(err)
	}
	base.outputArgs = nil
	runner := &deadlineRecordingRunner{fakeDockerRunner: base}
	deadline := time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if err := (&Client{Docker: runner}).ReleaseStagingVolumePins(ctx, plan, false); err != nil {
		t.Fatal(err)
	}
	if len(runner.deadlines) == 0 {
		t.Fatal("pin release made no Docker validation call")
	}
	for _, got := range runner.deadlines {
		if !got.Equal(deadline) {
			t.Fatalf("Docker deadline = %s, want caller deadline %s", got, deadline)
		}
	}
	for _, want := range [][]string{
		{"ps", "-a", "--filter", "label=bort.staging-pin=true", "--filter", "label=bort.run-id=run1", "--format", "{{.ID}}"},
		{"inspect", "--type", "container", "pin-id"},
		{"volume", "inspect", staged.VolumeName},
		{"ps", "-a", "--filter", "volume=" + staged.VolumeName, "--format", "{{.ID}}"},
	} {
		if !fakeOutputCalled(&runner.fakeDockerRunner, want...) {
			t.Fatalf("deadline test did not reach Docker call %v: %v", want, runner.outputArgs)
		}
	}
}

func TestReleaseTargetAuthorityPinRequiresDurableRecordAndTargetAttachment(t *testing.T) {
	for _, tc := range []struct {
		name         string
		recordMount  bool
		targetMounts string
		want         string
	}{
		{name: "missing durable record", targetMounts: `[{"Type":"volume","Name":"%s","Destination":"/data","RW":true}]`, want: "no complete durable migrated-volume record"},
		{name: "missing target attachment", recordMount: true, targetMounts: `[]`, want: "no longer has migrated volume mounted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, plan, _, staged := stagedSyncFixture(t)
			plan.RunID = "run-digest"
			staged, _ = stagedVolumeFor(plan, "api", plan.Prepare.Apps[0].Resources.Volumes[0])
			const targetID = "target-id"
			targetMounts := tc.targetMounts
			if strings.Contains(targetMounts, "%s") {
				targetMounts = fmt.Sprintf(targetMounts, staged.VolumeName)
			}
			runner := &fakeDockerRunner{outputs: map[string][]byte{
				"volume inspect " + staged.VolumeName:                                      ownedStagingVolumeInspect(plan, staged),
				"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": []byte(targetID + "\n"),
				"inspect --type container " + targetID:                                     []byte(fmt.Sprintf(`[{"Id":"%s","Name":"/web","Config":{"Labels":{"com.docker.compose.service":"web","com.docker.compose.project":"stack-1"}},"State":{"Running":true,"Status":"running"},"Mounts":%s}]`, targetID, targetMounts)),
			}, activeComposeProjects: map[string][]string{"stack-1": {targetID}}}
			client := targetAuthorityTestClient(t, runner, &plan)
			pin, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
			if err != nil {
				t.Fatal(err)
			}
			if tc.recordMount {
				state := &applyContext{plan: plan, cache: map[string]*appCache{}}
				if err := state.recordMigratedVolumeMount("api", migratedVolumeMount{Service: "web", Target: "/data", VolumeName: staged.VolumeName}); err != nil {
					t.Fatal(err)
				}
			}

			err = client.ReleaseStagingVolumePins(context.Background(), plan, true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q rejection, got %v", tc.want, err)
			}
			if _, ok := runner.activePins[pin.containerID]; !ok {
				t.Fatalf("target-authority validation removed the pin after %s", tc.name)
			}
		})
	}
}

func TestReleaseTargetAuthorityPinRejectsCrossMountedStagingVolume(t *testing.T) {
	app, plan, firstStep, first := stagedSyncFixture(t)
	app.Resources.Volumes = append(app.Resources.Volumes, preparer.VolumeResource{Service: "worker", Type: "volume", Name: "worker-data", Target: "/work"})
	secondStep := Step{Kind: StepSyncVolume, App: "api", Ref: "volume:worker -> /work"}
	plan = stagedPlan(t, app, plan.RunDir, Step{Kind: StepPauseSource, App: "api"}, firstStep, secondStep)
	plan.RunID = "run-digest"
	volumes := stagedVolumesForApp(plan, "api")
	if len(volumes) != 2 {
		t.Fatalf("staged volumes = %#v, want two", volumes)
	}
	first, _ = stagedVolumeFor(plan, "api", app.Resources.Volumes[0])
	second, _ := stagedVolumeFor(plan, "api", app.Resources.Volumes[1])
	const webID, workerID = "web-id", "worker-id"
	webInspect := fmt.Sprintf(`{"Id":"%s","Name":"/web","Config":{"Labels":{"com.docker.compose.service":"web","com.docker.compose.project":"stack-1"}},"State":{"Running":true,"Status":"running"},"Mounts":[{"Type":"volume","Name":"%s","Destination":"/data","RW":true},{"Type":"volume","Name":"%s","Destination":"/stolen","RW":true}]}`, webID, first.VolumeName, second.VolumeName)
	workerInspect := fmt.Sprintf(`{"Id":"%s","Name":"/worker","Config":{"Labels":{"com.docker.compose.service":"worker","com.docker.compose.project":"stack-1"}},"State":{"Running":true,"Status":"running"},"Mounts":[{"Type":"volume","Name":"%s","Destination":"/work","RW":true}]}`, workerID, second.VolumeName)
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + first.VolumeName:                                       ownedStagingVolumeInspect(plan, first),
		"volume inspect " + second.VolumeName:                                      ownedStagingVolumeInspect(plan, second),
		"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": []byte(webID + "\n" + workerID + "\n"),
		"inspect --type container " + webID:                                        []byte("[" + webInspect + "]"),
		"inspect --type container " + workerID:                                     []byte("[" + workerInspect + "]"),
		"inspect --type container " + webID + " " + workerID:                       []byte("[" + webInspect + "," + workerInspect + "]"),
	}, activeComposeProjects: map[string][]string{"stack-1": {webID, workerID}}}
	client := targetAuthorityTestClient(t, runner, &plan)
	pin, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", volumes, true)
	if err != nil {
		t.Fatal(err)
	}
	state := &applyContext{plan: plan, cache: map[string]*appCache{}}
	for _, mount := range []migratedVolumeMount{
		{Service: "web", Target: "/data", VolumeName: first.VolumeName},
		{Service: "worker", Target: "/work", VolumeName: second.VolumeName},
	} {
		if err := state.recordMigratedVolumeMount("api", mount); err != nil {
			t.Fatal(err)
		}
	}

	err = client.ReleaseStagingVolumePins(context.Background(), plan, true)
	if err == nil || !strings.Contains(err.Error(), "unexpected container "+webID) || !strings.Contains(err.Error(), second.VolumeName) {
		t.Fatalf("cross-mounted writer was not rejected per volume: %v", err)
	}
	if _, ok := runner.activePins[pin.containerID]; !ok {
		t.Fatal("cross-volume validation released the pin")
	}
}

func TestReleaseSourceAuthorityPinRefusesTargetAttachment(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
	}}
	pin, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
	if err != nil {
		t.Fatal(err)
	}
	const targetID = "target-id"
	runner.outputs["inspect --type container "+targetID] = []byte(`[{"Id":"` + targetID + `","Name":"/web","Mounts":[{"Type":"volume","Name":"` + staged.VolumeName + `","Destination":"/data","RW":true}]}]`)
	runner.activeComposeProjects = map[string][]string{"stack-1": {targetID}}

	err = (&Client{Docker: runner}).ReleaseStagingVolumePins(context.Background(), plan, false)
	if err == nil || !strings.Contains(err.Error(), "unexpected container "+targetID) {
		t.Fatalf("source authority removed a pin while the target remained attached: %v", err)
	}
	if _, ok := runner.activePins[pin.containerID]; !ok {
		t.Fatal("source-authority cleanup removed the pin despite a target attachment")
	}
}

func TestReleaseSourceAuthorityRefusesPinMountDrift(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
	}}
	pin, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
	if err != nil {
		t.Fatal(err)
	}
	container := runner.activePins[pin.containerID]
	container.Mounts[0].RW = true
	runner.activePins[pin.containerID] = container

	err = (&Client{Docker: runner}).ReleaseStagingVolumePins(context.Background(), plan, false)
	if err == nil || !strings.Contains(err.Error(), "unexpected or writable volume mount") {
		t.Fatalf("pin mount drift was not rejected: %v", err)
	}
	if _, ok := runner.activePins[pin.containerID]; !ok {
		t.Fatal("drifted pin was removed")
	}
}

func TestReleaseSourceAuthorityWithoutPinStillRefusesTargetAttachment(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	plan.StagingTransferApps = []string{"api"}
	const targetID = "target-id"
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect " + staged.VolumeName:  ownedStagingVolumeInspect(plan, staged),
			"inspect --type container " + targetID: []byte(`[{"Id":"` + targetID + `","Name":"/web","Mounts":[{"Type":"volume","Name":"` + staged.VolumeName + `","Destination":"/data","RW":true}]}]`),
		},
		activeComposeProjects: map[string][]string{"stack-1": {targetID}},
	}

	err := (&Client{Docker: runner}).ReleaseStagingVolumePins(context.Background(), plan, false)
	if err == nil || !strings.Contains(err.Error(), "unexpected container "+targetID) {
		t.Fatalf("source authority bypassed attachment validation because the pin was absent: %v", err)
	}
}

func TestReleaseTargetAuthorityWithoutPinRequiresDurableTransferRecord(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	plan.StagingTransferApps = []string{"api"}
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
	}}

	err := (&Client{Docker: runner}).ReleaseStagingVolumePins(context.Background(), plan, true)
	if err == nil || !strings.Contains(err.Error(), "no complete durable migrated-volume record") {
		t.Fatalf("target authority bypassed durable transfer validation because the pin was absent: %v", err)
	}
}

func TestReleaseSourceAuthoritySkipsUncreatedVolumesAfterPartialTransfer(t *testing.T) {
	app, plan, firstStep, first := stagedSyncFixture(t)
	app.Resources.Volumes = append(app.Resources.Volumes, preparer.VolumeResource{Service: "worker", Type: "volume", Name: "worker-data", Target: "/work"})
	plan = stagedPlan(t, app, plan.RunDir, Step{Kind: StepPauseSource, App: "api"}, firstStep, Step{Kind: StepSyncVolume, App: "api", Ref: "volume:worker -> /work"})
	plan.StagingTransferApps = []string{"api"}
	volumes := stagedVolumesForApp(plan, "api")
	first, _ = stagedVolumeFor(plan, "api", app.Resources.Volumes[0])
	second, _ := stagedVolumeFor(plan, "api", app.Resources.Volumes[1])
	runner := &fakeDockerRunner{
		outputs:    map[string][]byte{"volume inspect " + first.VolumeName: ownedStagingVolumeInspect(plan, first)},
		outputErrs: map[string]error{"volume inspect " + second.VolumeName: errors.New("Error response from daemon: volume " + second.VolumeName + " not found")},
	}

	if err := (&Client{Docker: runner}).ReleaseStagingVolumePins(context.Background(), plan, false); err != nil {
		t.Fatalf("source recovery dead-ended on an uncreated later volume: %v (planned=%#v)", err, volumes)
	}
	if !fakeOutputCalled(runner, "ps", "-a", "--filter", "volume="+first.VolumeName, "--format", "{{.ID}}") {
		t.Fatalf("source recovery skipped attachment validation for the created volume: %v", runner.outputArgs)
	}
	for _, args := range runner.outputArgs {
		if strings.Contains(strings.Join(args, " "), "volume="+second.VolumeName) {
			t.Fatalf("source recovery queried attachments for the uncreated volume: %v", args)
		}
	}
}

func TestReleaseSourceAuthorityHonorsLegacyTransferStartEvidence(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	path, err := migratedVolumeMountsPath(plan.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := json.Marshal(migratedVolumeMountsState{APIVersion: migratedVolumeMountsLegacyAPIVersion, StartedApps: []string{"api"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	const targetID = "target-id"
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect " + staged.VolumeName:  ownedStagingVolumeInspect(plan, staged),
			"inspect --type container " + targetID: []byte(`[{"Id":"` + targetID + `","Name":"/web","Mounts":[{"Type":"volume","Name":"` + staged.VolumeName + `","Destination":"/data","RW":true}]}]`),
		},
		activeComposeProjects: map[string][]string{"stack-1": {targetID}},
	}

	err = (&Client{Docker: runner}).ReleaseStagingVolumePins(context.Background(), plan, false)
	if err == nil || !strings.Contains(err.Error(), "unexpected container "+targetID) {
		t.Fatalf("legacy transfer-start evidence did not trigger attachment validation: %v", err)
	}
}

func TestExactStagingVolumeAttachmentsAcceptsShortAndFullContainerIDs(t *testing.T) {
	shortID := "123456789abc"
	fullID := shortID + strings.Repeat("d", 52)
	for _, ids := range [][2]string{{shortID, fullID}, {fullID, shortID}} {
		if err := requireExactStagingVolumeAttachments("data", []string{ids[0]}, []string{ids[1]}); err != nil {
			t.Fatalf("equivalent Docker IDs %q and %q did not match: %v", ids[0], ids[1], err)
		}
	}
}

func TestStagingVolumePinRejectsUnexpectedAttachment(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	attachmentCommand := "ps -a --filter volume=" + staged.VolumeName + " --format {{.ID}}"
	runner := &sequencedOutputRunner{
		fakeDockerRunner: fakeDockerRunner{outputs: map[string][]byte{
			"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
		}},
		sequences: map[string][][]byte{
			attachmentCommand: {[]byte("pin-id\nforeign-writer\n")},
		},
	}
	pin, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
	if err != nil {
		t.Fatalf("acquireStagingVolumePin: %v", err)
	}
	err = requireStagingVolumeAttachments(context.Background(), runner, []stagedVolume{staged}, []string{pin.containerID})
	if err == nil || !strings.Contains(err.Error(), "unexpected container foreign-writer") {
		t.Fatalf("expected a foreign attachment to block handoff, got %v", err)
	}
}

type ambiguousPinCreateRunner struct {
	*fakeDockerRunner
	failCreate bool
}

func (r *ambiguousPinCreateRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) > 1 && args[0] == "run" && args[1] == "-d" && slices.Contains(args, stagingVolumePinLabel+"=true") && r.failCreate {
		r.failCreate = false
		_, _ = r.fakeDockerRunner.Output(ctx, args...)
		return nil, errors.New("docker response lost after creating pin")
	}
	return r.fakeDockerRunner.Output(ctx, args...)
}

func TestAcquireStagingVolumePinAdoptsAmbiguousCreateAndRetry(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	runner := &ambiguousPinCreateRunner{
		fakeDockerRunner: &fakeDockerRunner{outputs: map[string][]byte{
			"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
		}},
		failCreate: true,
	}
	pin, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
	if err != nil || pin.containerID != "pin-id" {
		t.Fatalf("expected ambiguous create to adopt the running pin, pin=%#v err=%v", pin, err)
	}
	if len(runner.activePins) != 1 || fakeOutputCalled(runner.fakeDockerRunner, "rm", "-f", "pin-id") {
		t.Fatalf("adopted pin was not preserved: active=%v calls=%v", runner.activePins, runner.outputArgs)
	}
	second, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
	if err != nil || second.containerID != pin.containerID {
		t.Fatalf("adopted pin blocked ordinary retry: first=%#v second=%#v err=%v", pin, second, err)
	}
	runCalls := 0
	for _, args := range runner.outputArgs {
		if len(args) > 1 && args[0] == "run" && args[1] == "-d" {
			runCalls++
		}
	}
	if runCalls != 1 {
		t.Fatalf("ambiguous create and retry ran %d pin containers, want 1; calls=%v", runCalls, runner.outputArgs)
	}
}

func TestAcquireStagingVolumePinRestartsStoppedExactPinInPlace(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
	}}
	pin, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
	if err != nil {
		t.Fatalf("create staging volume pin: %v", err)
	}
	stopped := runner.activePins[pin.containerID]
	stopped.State.Running = false
	stopped.State.Status = "exited"
	runner.activePins[pin.containerID] = stopped
	runner.outputArgs = nil

	restarted, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
	if err != nil || restarted != pin {
		t.Fatalf("expected stopped exact pin to restart in place, pin=%#v restarted=%#v err=%v", pin, restarted, err)
	}
	if !runner.activePins[pin.containerID].State.Running || !fakeOutputCalled(runner, "start", pin.containerID) {
		t.Fatalf("stopped pin was not restarted: active=%#v calls=%v", runner.activePins, runner.outputArgs)
	}
	for _, args := range runner.outputArgs {
		if len(args) > 1 && args[0] == "run" && args[1] == "-d" || len(args) > 1 && args[0] == "rm" {
			t.Fatalf("stopped exact pin was replaced instead of restarted: calls=%v", runner.outputArgs)
		}
	}
}

func TestAcquireStagingVolumePinFailedRestartPreservesStoppedPin(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
	}}
	pin, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
	if err != nil {
		t.Fatalf("create staging volume pin: %v", err)
	}
	stopped := runner.activePins[pin.containerID]
	stopped.State.Running = false
	stopped.State.Status = "exited"
	runner.activePins[pin.containerID] = stopped
	runner.outputArgs = nil
	runner.outputErrs = map[string]error{"start " + pin.containerID: errors.New("restart failed")}

	_, err = acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
	if err == nil || !strings.Contains(err.Error(), "restart stopped staging volume pin") {
		t.Fatalf("expected stopped pin restart failure, got %v", err)
	}
	if active, ok := runner.activePins[pin.containerID]; !ok || active.State.Running {
		t.Fatalf("failed restart did not preserve the stopped pin: active=%#v", runner.activePins)
	}
	for _, args := range runner.outputArgs {
		if len(args) > 1 && args[0] == "run" && args[1] == "-d" || len(args) > 1 && args[0] == "rm" {
			t.Fatalf("failed restart replaced or removed the stopped pin: calls=%v", runner.outputArgs)
		}
	}
}

type sourceRecoveryPinRunner struct {
	*fakeDockerRunner
	sourceRunning bool
}

func (r *sourceRecoveryPinRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) == 4 && args[0] == "inspect" && args[1] == "--type" && args[2] == "container" && args[3] == "src-id" {
		r.outputArgs = append(r.outputArgs, append([]string{}, args...))
		return []byte(fmt.Sprintf(`[{"Id":"src-id","Name":"/coolify-web","State":{"Running":%t,"Status":"exited"}}]`, r.sourceRunning)), nil
	}
	if len(args) == 2 && args[0] == "start" && args[1] == "src-id" {
		r.outputArgs = append(r.outputArgs, append([]string{}, args...))
		r.sourceRunning = true
		return []byte("src-id\n"), nil
	}
	return r.fakeDockerRunner.Output(ctx, args...)
}

func TestBestEffortResumePreservesVerifiedPreHandoffPin(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	runner := &sourceRecoveryPinRunner{fakeDockerRunner: &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
	}}}
	pin, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
	if err != nil {
		t.Fatal(err)
	}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}, stagingVolumePins: map[string]stagingVolumePin{"api": pin}}
	entry := actx.entry("api")
	entry.SourcePauseRecorded = true
	entry.SourcePausedContainers = []sourcePausedContainer{{ID: "src-id", Stopped: true}}
	client := &Client{Docker: runner}

	if err := client.bestEffortResume(context.Background(), actx, plan, len(plan.Steps), pausedSources{"api": true}, false, true); err != nil {
		t.Fatalf("resume source with pre-handoff pin: %v", err)
	}
	if !runner.sourceRunning {
		t.Fatal("source was not resumed")
	}
	if _, ok := runner.activePins[pin.containerID]; !ok {
		t.Fatalf("verified pre-handoff pin was removed before a retry could reuse it: %#v", runner.activePins)
	}
	if cached, ok := actx.stagingVolumePins["api"]; !ok || cached != pin {
		t.Fatalf("preserved pin was not cached for retry: %#v", actx.stagingVolumePins)
	}
	pinRuns := 0
	for _, args := range runner.outputArgs {
		if len(args) > 1 && args[0] == "run" && args[1] == "-d" {
			pinRuns++
		}
		if len(args) > 1 && args[0] == "rm" && args[1] == "-f" {
			t.Fatalf("source recovery removed the preserved pin: %v", runner.outputArgs)
		}
	}
	if pinRuns != 1 {
		t.Fatalf("source recovery recreated the preserved pin: run calls=%d all calls=%v", pinRuns, runner.outputArgs)
	}
}

func TestBestEffortResumeKeepsSourceStoppedWhenPinGuardIsUnverified(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	runner := &sourceRecoveryPinRunner{fakeDockerRunner: &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
	}}}
	pin, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
	if err != nil {
		t.Fatal(err)
	}
	const targetID = "unexpected-target"
	runner.outputs["inspect --type container "+targetID] = []byte(`[{"Id":"` + targetID + `","Name":"/web","Mounts":[{"Type":"volume","Name":"` + staged.VolumeName + `","Destination":"/data","RW":true}]}]`)
	runner.activeComposeProjects = map[string][]string{"stack-1": {targetID}}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}, stagingVolumePins: map[string]stagingVolumePin{"api": pin}}
	entry := actx.entry("api")
	entry.SourcePauseRecorded = true
	entry.SourcePausedContainers = []sourcePausedContainer{{ID: "src-id", Stopped: true}}
	client := &Client{Docker: runner}

	err = client.bestEffortResume(context.Background(), actx, plan, len(plan.Steps), pausedSources{"api": true}, false, true)
	if err == nil || !strings.Contains(err.Error(), "pin could not be verified before restart") {
		t.Fatalf("expected unverified pin guard to block source restart, got %v", err)
	}
	if runner.sourceRunning || fakeOutputCalled(runner.fakeDockerRunner, "start", "src-id") {
		t.Fatalf("source restarted with an unverified pin guard: running=%t calls=%v", runner.sourceRunning, runner.outputArgs)
	}
	if _, ok := runner.activePins[pin.containerID]; !ok {
		t.Fatal("unverified pin guard was removed")
	}
}

func TestFailedStagingVolumeHandoffReportsGuardVerification(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
	}}
	pin, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true)
	if err != nil {
		t.Fatal(err)
	}
	verified := failedStagingVolumeHandoff(runner, plan, "api", []stagedVolume{staged}, pin, "handoff failed", errors.New("deploy error"))
	if !strings.Contains(verified.Error(), "verified and preserved") {
		t.Fatalf("verified guard was not reported accurately: %v", verified)
	}
	runner.outputs["volume inspect "+staged.VolumeName] = []byte(`[{"Name":"` + staged.VolumeName + `","Labels":{"bort.run-id":"other-run"}}]`)
	unverified := failedStagingVolumeHandoff(runner, plan, "api", []stagedVolume{staged}, pin, "handoff failed", errors.New("deploy error"))
	if !strings.Contains(unverified.Error(), "could not verify the read-only staging-volume guard") || strings.Contains(unverified.Error(), "verified and preserved") {
		t.Fatalf("unverified guard was reported inaccurately: %v", unverified)
	}
}

type handoffPinRunner struct {
	*fakeDockerRunner
	pinCommand    string
	pinID         string
	targetID      string
	targetInspect string
	pinned        bool
	sawTarget     bool
}

func (r *handoffPinRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	if key == r.targetInspect {
		if !r.pinned {
			return nil, errors.New("target attached after staging volume pin was released")
		}
		r.sawTarget = true
	}
	if strings.HasPrefix(key, "ps -a --filter volume=") && r.sawTarget {
		return []byte(r.pinID + "\n" + r.targetID + "\n"), nil
	}
	out, err := r.fakeDockerRunner.Output(ctx, args...)
	if err != nil {
		return nil, err
	}
	if key == r.pinCommand {
		r.pinned = true
	}
	if key == "rm -f "+r.pinID {
		if !r.sawTarget {
			return nil, errors.New("staging volume pin released before target attachment was verified")
		}
		r.pinned = false
	}
	return out, nil
}

type completedDriftHandoffRunner struct {
	*fakeDockerRunner
	targetID      string
	targetVisible bool
	targetStopped bool
}

func (r *completedDriftHandoffRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	if strings.HasPrefix(key, "ps -a --filter volume=") {
		out, err := r.fakeDockerRunner.Output(ctx, args...)
		if err != nil || !r.targetVisible {
			return out, err
		}
		return append(out, []byte("\n"+r.targetID+"\n")...), nil
	}
	out, err := r.fakeDockerRunner.Output(ctx, args...)
	if err != nil {
		return nil, err
	}
	if key == "stop "+r.targetID {
		r.targetStopped = true
	}
	if key == "inspect --type container "+r.targetID && r.targetStopped {
		out = []byte(strings.ReplaceAll(strings.ReplaceAll(string(out), `"Running":true`, `"Running":false`), `"Status":"running"`, `"Status":"exited"`))
	}
	return out, nil
}

func TestApplyPushImagePinsStagingVolumeThroughTargetValidation(t *testing.T) {
	app, plan, _, staged := stagedSyncFixture(t)
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  web:\n    image: example/web\n    volumes:\n      - data:/data\nvolumes:\n  data:\n"
	composePath := filepath.Join(appDir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	app.Directory = "api"
	app.TargetResources = &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{ComposePath: "compose.yaml"}}}
	plan.Prepare = preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{app}}
	plan.BundleFiles = map[string][]byte{filepath.Clean(composePath): []byte(compose)}

	var deploymentTitle string
	deploys := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.update":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.deploy":
			deploys++
			var request deployComposeRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			deploymentTitle = request.Title
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.one":
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: "c1", AppName: "stack-1", Deployments: []Deployment{{Title: deploymentTitle, Status: "done"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	const targetID = "abcdef123456"
	pinCommand := "run -d --name " + stagingVolumePinName(plan, []stagedVolume{staged}) + " --network none --read-only --restart unless-stopped --label " + stagingVolumePinLabel + "=true --label " + stagingVolumeRunIDLabel + "=" + stagingOwner(plan) + " --label " + stagingVolumeAppLabel + "=api -v " + staged.VolumeName + ":/bort-volume/0:ro " + volumeCopyImage + " sh -c while :; do sleep 2147483647; done"
	targetInspect := "inspect --type container " + targetID
	runner := &handoffPinRunner{
		fakeDockerRunner: &fakeDockerRunner{
			outputs: map[string][]byte{
				"image inspect example/web": []byte(`[{}]`),
				"ps --format {{.Names}}":    []byte("dokploy-postgres\n"),
				pinCommand:                  []byte("handoff-pin\n"),
				"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
				"volume inspect " + staged.VolumeName:                                            ownedStagingVolumeInspect(plan, staged),
				"rm -f handoff-pin":                                                              []byte("handoff-pin\n"),
				"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}":       []byte(targetID + "\n"),
				targetInspect: []byte(`[{"Id":"` + targetID + `","Name":"/web","Config":{"Labels":{"com.docker.compose.service":"web","com.docker.compose.project":"stack-1"}},"State":{"Running":true,"Status":"running"},"Mounts":[{"Type":"volume","Name":"` + staged.VolumeName + `","Destination":"/data","RW":true}]}]`),
			},
			runOutputs: map[string][]byte{
				"exec -i dokploy-postgres psql -U dokploy -d dokploy -v ON_ERROR_STOP=1 -At": {},
			},
		},
		pinCommand:    pinCommand,
		pinID:         "handoff-pin",
		targetID:      targetID,
		targetInspect: targetInspect,
	}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	entry := actx.entry("api")
	entry.ComposeID = "c1"
	entry.ComposeAppName = "stack-1"
	entry.MigratedVolumeMounts = map[string]migratedVolumeMount{
		migratedMountKey("web", "/data"): {Service: "web", Target: "/data", VolumeName: staged.VolumeName},
	}
	if _, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true); err != nil {
		t.Fatalf("seed transferred-state pin: %v", err)
	}

	if err := client.applyPushImage(context.Background(), actx, Step{Kind: StepPushImage, App: "api", Ref: "api"}); err != nil {
		t.Fatalf("applyPushImage: %v", err)
	}
	if !runner.sawTarget || runner.pinned {
		t.Fatalf("expected target validation while pinned followed by release, sawTarget=%t pinned=%t", runner.sawTarget, runner.pinned)
	}
	if _, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true); err != nil {
		t.Fatalf("seed interrupted-handoff pin: %v", err)
	}
	runner.sawTarget = true
	err := client.applyPushImage(context.Background(), actx, Step{Kind: StepPushImage, App: "api", Ref: "api"})
	if err == nil || !isUnsafeSourceResumeError(err) || !mutationResponseMayHaveSucceeded(err) || !strings.Contains(err.Error(), "previous target attachment may have survived") || !runner.pinned {
		t.Fatalf("a resumed push with a surviving target attachment must preserve the pin and keep the source stopped, pinned=%t err=%v", runner.pinned, err)
	}
	if deploys != 1 {
		t.Fatalf("a surviving target attachment must block another deployment, deploys=%d", deploys)
	}
	if err := releaseStagingVolumePin(context.Background(), runner, plan, []stagedVolume{staged}, stagingVolumePin{name: stagingVolumePinName(plan, []stagedVolume{staged}), containerID: runner.pinID}); err != nil {
		t.Fatalf("release preserved test pin: %v", err)
	}
	delete(actx.stagingVolumePins, "api")
	runner.sawTarget = false
	if _, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true); err != nil {
		t.Fatalf("seed transferred-state pin for release failure: %v", err)
	}
	runner.outputErrs = map[string]error{"rm -f handoff-pin": errors.New("docker response lost while releasing pin")}
	err = client.applyPushImage(context.Background(), actx, Step{Kind: StepPushImage, App: "api", Ref: "api"})
	if err == nil || !isUnsafeSourceResumeError(err) || !mutationResponseMayHaveSucceeded(err) || !strings.Contains(err.Error(), "paused source applications remain stopped") || !strings.Contains(err.Error(), "bort status") {
		t.Fatalf("a pin-release failure after target validation must keep the source stopped, got %v", err)
	}
}

func TestApplyPushImageKeepsSourceStoppedAfterCompletedDeploymentMountDrift(t *testing.T) {
	app, plan, _, staged := stagedSyncFixture(t)
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  web:\n    image: example/web\n    volumes:\n      - data:/data\nvolumes:\n  data:\n"
	composePath := filepath.Join(appDir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	app.Directory = "api"
	app.TargetResources = &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{ComposePath: "compose.yaml"}}}
	plan.Prepare = preparer.Result{BundleDir: bundleDir, Apps: []preparer.AppPlan{app}}
	plan.BundleFiles = map[string][]byte{filepath.Clean(composePath): []byte(compose)}

	const targetID = "drifted-web"
	runner := &completedDriftHandoffRunner{
		fakeDockerRunner: &fakeDockerRunner{
			outputs: map[string][]byte{
				"image inspect example/web": []byte(`[{}]`),
				"ps --format {{.Names}}":    []byte("dokploy-postgres\n"),
				"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
				"volume inspect " + staged.VolumeName:                                            ownedStagingVolumeInspect(plan, staged),
				"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}":       []byte(targetID + "\n"),
				"inspect --type container " + targetID:                                           []byte(`[{"Id":"` + targetID + `","Name":"/web","Config":{"Labels":{"com.docker.compose.service":"web","com.docker.compose.project":"stack-1"}},"State":{"Running":true,"Status":"running"},"Mounts":[{"Type":"volume","Name":"fresh-vol","Destination":"/data","RW":true}]}]`),
				"stop " + targetID:                                                               []byte(targetID + "\n"),
			},
			runOutputs: map[string][]byte{
				"exec -i dokploy-postgres psql -U dokploy -d dokploy -v ON_ERROR_STOP=1 -At": {},
			},
		},
		targetID: targetID,
	}
	var deploymentTitle string
	deploys := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.update":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/compose.deploy":
			deploys++
			var request deployComposeRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			deploymentTitle = request.Title
			runner.targetVisible = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/compose.one":
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: "c1", AppName: "stack-1", Deployments: []Deployment{{Title: deploymentTitle, Status: "done"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: runner}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	entry := actx.entry("api")
	entry.ComposeID = "c1"
	entry.ComposeAppName = "stack-1"
	entry.MigratedVolumeMounts = map[string]migratedVolumeMount{
		migratedMountKey("web", "/data"): {Service: "web", Target: "/data", VolumeName: staged.VolumeName},
	}
	if _, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true); err != nil {
		t.Fatalf("seed transferred-state pin: %v", err)
	}

	err := client.applyPushImage(context.Background(), actx, Step{Kind: StepPushImage, App: "api", Ref: "api"})
	if err == nil || !isUnsafeSourceResumeError(err) || !mutationResponseMayHaveSucceeded(err) || !strings.Contains(err.Error(), "changed from migrated volume") {
		t.Fatalf("a completed deployment with mount drift must keep the source stopped, got %v", err)
	}
	if !runner.targetStopped {
		t.Fatalf("the drifted target was not stopped, calls=%#v", runner.outputArgs)
	}
	if len(runner.activePins) != 1 {
		t.Fatalf("the staging-volume pin must remain active after an unsafe completed deployment, active=%#v", runner.activePins)
	}
	if deploys != 1 {
		t.Fatalf("mount drift test observed %d deploy requests, want 1", deploys)
	}
}

func TestSyncVolumeToStagingCreatesOwnedVolumeAndRecordsMount(t *testing.T) {
	app, plan, step, _ := stagedSyncFixture(t)
	plan.RunID = "run-digest"
	staged, ok := stagedVolumeFor(plan, "api", app.Resources.Volumes[0])
	if !ok {
		t.Fatal("volume not staged after setting the run ID")
	}
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect src-vol":              []byte(`[{"Name":"src-vol"}]`),
			"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
			"volume create":                       []byte(staged.VolumeName + "\n"),
			"create --network none -v " + staged.VolumeName + ":/volume " + volumeCopyImage + " true": []byte("pin-id\n"),
			"rm -f pin-id":                               []byte("pin-id\n"),
			"inspect --type container src-id":            stoppedSourceInspect("2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z"),
			"ps -a --filter volume=" + staged.VolumeName: []byte(""),
		},
		outputErrs: map[string]error{
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: errors.New("Error response from daemon: get " + staged.VolumeName + ": no such volume"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan}

	if err := client.applySyncVolume(context.Background(), actx, step); err != nil {
		t.Fatalf("applySyncVolume: %v", err)
	}
	var created []string
	for _, args := range runner.outputArgs {
		if len(args) > 2 && args[0] == "volume" && args[1] == "create" {
			created = args
		}
	}
	joined := strings.Join(created, " ")
	if created == nil || !strings.Contains(joined, "--label bort.run=run1") || !strings.Contains(joined, "--label bort.run-id=run-digest") || !strings.HasSuffix(joined, staged.VolumeName) {
		t.Fatalf("expected labeled staging volume creation, got %v", created)
	}
	if len(runner.runs) != 1 || !strings.Contains(strings.Join(runner.runs[0].Args, " "), "src-vol:/from:ro -v "+staged.VolumeName+":/to") {
		t.Fatalf("expected one copy into the staging volume, got %#v", runner.runs)
	}
	mount, ok := actx.entry("api").MigratedVolumeMounts[migratedMountKey("web", "/data")]
	if !ok || mount.VolumeName != staged.VolumeName {
		t.Fatalf("expected staging mount recorded, got %#v", actx.entry("api").MigratedVolumeMounts)
	}
	if len(runner.activePins) != 1 {
		t.Fatalf("sync must leave the app-wide staging-volume pin active for deployment handoff, active=%#v", runner.activePins)
	}
}

func TestSyncVolumeToStagingRefusesCopyWhoseFlushFails(t *testing.T) {
	app, plan, step, _ := stagedSyncFixture(t)
	plan.RunID = "run-digest"
	staged, ok := stagedVolumeFor(plan, "api", app.Resources.Volumes[0])
	if !ok {
		t.Fatal("volume not staged after setting the run ID")
	}
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect src-vol":              []byte(`[{"Name":"src-vol"}]`),
			"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
			"volume create":                       []byte(staged.VolumeName + "\n"),
			"create --network none -v " + staged.VolumeName + ":/volume " + volumeCopyImage + " true": []byte("pin-id\n"),
			"rm -f pin-id":                               []byte("pin-id\n"),
			"inspect --type container src-id":            stoppedSourceInspect("2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z"),
			"ps -a --filter volume=" + staged.VolumeName: []byte(""),
		},
		outputErrs: map[string]error{
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: errors.New("Error response from daemon: get " + staged.VolumeName + ": no such volume"),
		},
		runErr: errors.New("fsync: /to/data/db.sqlite: Input/output error"),
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan}

	err := client.applySyncVolume(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("expected the flush failure to fail the copy, got %v", err)
	}
	if len(runner.runs) != 1 {
		t.Fatalf("expected one copy attempt, got %#v", runner.runs)
	}
	script := runner.runs[0].Args[len(runner.runs[0].Args)-1]
	if !strings.HasPrefix(script, "set -o pipefail; find /to -mindepth 1 -delete && ") || !strings.Contains(script, "tar xpf - -C /to && find /to") || !strings.HasSuffix(script, "-print0 | xargs -0 fsync") {
		t.Fatalf("copy must clear /to without shell globbing, fsync every file and directory under /to, and propagate any batch failure before succeeding, got %q", script)
	}
	if _, ok := actx.entry("api").MigratedVolumeMounts[migratedMountKey("web", "/data")]; ok {
		t.Fatalf("an unflushed copy must not be recorded as transferred: %#v", actx.entry("api").MigratedVolumeMounts)
	}
}

func TestSyncVolumeToStagingRefusesForeignOwnedVolume(t *testing.T) {
	_, plan, step, staged := stagedSyncFixture(t)
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect " + staged.VolumeName: stagingVolumeInspect(plan, staged, "other-run"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan}
	err := client.applySyncVolume(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "label bort.run-id=\"other-run\", want \"run1\"") {
		t.Fatalf("expected ownership error, got %v", err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("must not copy into a foreign volume, got %#v", runner.runs)
	}
}

func TestSyncVolumeToStagingRejectsSourceRestartDuringCopy(t *testing.T) {
	_, plan, step, staged := stagedSyncFixture(t)
	runner := &sequencedOutputRunner{
		fakeDockerRunner: fakeDockerRunner{
			outputs: map[string][]byte{
				"volume inspect src-vol":              []byte(`[{"Name":"src-vol"}]`),
				"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
				"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
				"ps -a --filter volume=" + staged.VolumeName:                                     []byte(""),
			},
		},
		sequences: map[string][][]byte{
			"inspect --type container src-id": {
				stoppedSourceInspect("2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z"),
				stoppedSourceInspect("2026-01-03T00:00:00Z", "2026-01-03T00:00:05Z"),
			},
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan}
	actx.entry("api").MigratedVolumeMounts = map[string]migratedVolumeMount{
		migratedMountKey("web", "/data"): {Service: "web", Target: "/data", VolumeName: staged.VolumeName},
	}
	if _, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{staged}, true); err != nil {
		t.Fatalf("seed transferred-state pin: %v", err)
	}

	err := client.applySyncVolume(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "ran during state copy") {
		t.Fatalf("expected restart detection, got %v", err)
	}
	if len(runner.runs) != 1 {
		t.Fatalf("copy must have run once before the post-check, got %#v", runner.runs)
	}
	if _, ok := actx.entry("api").MigratedVolumeMounts[migratedMountKey("web", "/data")]; ok {
		t.Fatalf("stale mount record must be forgotten before a failed copy")
	}
}

func stagedSharedSyncFixture(t *testing.T) (Plan, []Step, []stagedVolume) {
	t.Helper()
	app := preparer.AppPlan{Name: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{
		{ServiceName: "web", ContainerID: "src-web-id", ContainerName: "coolify-web"},
		{ServiceName: "worker", ContainerID: "src-worker-id", ContainerName: "coolify-worker"},
	}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", Name: "src-shared", Target: "/data", SourceContainerID: "src-web-id", SourceContainerName: "coolify-web"},
		{Service: "worker", Type: "volume", Name: "src-shared", Target: "/work", SourceContainerID: "src-worker-id", SourceContainerName: "coolify-worker"},
		{Service: "web", Type: "volume", Name: "src-uploads", Target: "/uploads", SourceContainerID: "src-web-id", SourceContainerName: "coolify-web"},
	}
	steps := []Step{
		{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
		{Kind: StepSyncVolume, App: "api", Ref: "volume:worker -> /work"},
		{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /uploads"},
	}
	plan := stagedPlan(t, app, t.TempDir(), append([]Step{{Kind: StepPauseSource, App: "api"}}, steps...)...)
	staged := stagedVolumesForApp(plan, "api")
	if len(staged) != 3 {
		t.Fatalf("expected all mounts to stage, got %#v", staged)
	}
	return plan, steps, staged
}

func sourceContainerInspects(args [][]string) int {
	count := 0
	for _, inspect := range args {
		if len(inspect) > 3 && inspect[0] == "inspect" && inspect[1] == "--type" && inspect[2] == "container" &&
			(slices.Contains(inspect, "src-web-id") || slices.Contains(inspect, "src-worker-id")) {
			count++
		}
	}
	return count
}

func TestSyncVolumeToStagingCopiesSharedSourceOnce(t *testing.T) {
	plan, steps, staged := stagedSharedSyncFixture(t)
	if staged[0].VolumeName != staged[1].VolumeName || !staged[0].Shared || !staged[1].Shared {
		t.Fatalf("expected one shared staging volume, got %#v", staged)
	}
	if staged[2].Shared || staged[2].VolumeName == staged[0].VolumeName {
		t.Fatalf("expected the unrelated upload mount to keep its own staging volume, got %#v", staged)
	}
	shared := staged[0].VolumeName
	uploads := staged[2].VolumeName
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect " + shared:               sharedStagingVolumeInspect(plan, staged[0]),
			"volume inspect " + uploads:              stagingVolumeInspect(plan, staged[2], "run1"),
			"volume create":                          []byte(shared + "\n"),
			"volume inspect src-shared":              []byte(`[{"Name":"src-shared"}]`),
			"volume inspect src-uploads":             []byte(`[{"Name":"src-uploads"}]`),
			"inspect --type container src-web-id":    []byte(`[{"Id":"src-web-id","Name":"/coolify-web","State":{"Running":false,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"2026-01-02T00:00:00Z"}}]`),
			"inspect --type container src-worker-id": []byte(`[{"Id":"src-worker-id","Name":"/coolify-worker","State":{"Running":false,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"2026-01-02T00:00:00Z"}}]`),
		},
		outputErrs: map[string]error{
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + shared:  errors.New("Error response from daemon: get " + shared + ": no such volume"),
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + uploads: errors.New("Error response from daemon: get " + uploads + ": no such volume"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan}

	if err := client.applySyncVolume(context.Background(), actx, steps[2]); err != nil {
		t.Fatalf("applySyncVolume(uploads): %v", err)
	}
	if len(runner.runs) != 1 || !strings.Contains(strings.Join(runner.runs[0].Args, " "), "src-uploads:/from:ro -v "+uploads+":/to") {
		t.Fatalf("expected one copy into the private staging volume, got %#v", runner.runs)
	}
	if err := client.applySyncVolume(context.Background(), actx, steps[0]); err != nil {
		t.Fatalf("applySyncVolume(web): %v", err)
	}
	if len(runner.runs) != 2 || !strings.Contains(strings.Join(runner.runs[1].Args, " "), "src-shared:/from:ro -v "+shared+":/to") {
		t.Fatalf("the shared mount must copy despite a recorded transfer into another staging volume, got %#v", runner.runs)
	}
	var sharedCreate []string
	for _, args := range runner.outputArgs {
		if len(args) > 2 && args[0] == "volume" && args[1] == "create" && strings.HasSuffix(strings.Join(args, " "), shared) {
			sharedCreate = args
		}
	}
	joined := strings.Join(sharedCreate, " ")
	if !strings.Contains(joined, "--label bort.source=src-shared") || !strings.HasSuffix(joined, shared) {
		t.Fatalf("expected the shared staging volume to be created with its source label, got %v", sharedCreate)
	}

	inspectsBeforeWorker := sourceContainerInspects(runner.outputArgs)
	if err := client.applySyncVolume(context.Background(), actx, steps[1]); err != nil {
		t.Fatalf("applySyncVolume(worker): %v", err)
	}
	if len(runner.runs) != 2 {
		t.Fatalf("the second mount of a shared source must not copy again, got %#v", runner.runs)
	}
	if got := sourceContainerInspects(runner.outputArgs); got != inspectsBeforeWorker {
		t.Fatalf("the skipped mount must not re-inspect quiesce targets, got %d source inspects, want %d", got, inspectsBeforeWorker)
	}
	for _, mount := range []struct{ service, target string }{{"web", "/data"}, {"worker", "/work"}} {
		record, ok := actx.entry("api").MigratedVolumeMounts[migratedMountKey(mount.service, mount.target)]
		if !ok || record.VolumeName != shared {
			t.Fatalf("expected %s:%s to be recorded on the shared staging volume, got %#v", mount.service, mount.target, actx.entry("api").MigratedVolumeMounts)
		}
	}

	if err := client.applySyncVolume(context.Background(), actx, steps[0]); err != nil {
		t.Fatalf("applySyncVolume(web) retry: %v", err)
	}
	if len(runner.runs) != 2 {
		t.Fatalf("a retried mount of a shared source must not copy again, got %#v", runner.runs)
	}
	if len(runner.activePins) != 1 {
		t.Fatalf("the shared staging volume pin must stay active for deployment handoff, active=%#v", runner.activePins)
	}
}

func TestPauseStepRerunInvalidatesSharedVolumeRecords(t *testing.T) {
	plan, steps, staged := stagedSharedSyncFixture(t)
	shared := staged[0].VolumeName
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect " + shared:               sharedStagingVolumeInspect(plan, staged[0]),
			"volume inspect " + staged[2].VolumeName: stagingVolumeInspect(plan, staged[2], "run1"),
			"volume inspect src-shared":              []byte(`[{"Name":"src-shared"}]`),
			"inspect --type container src-web-id":    []byte(`[{"Id":"src-web-id","Name":"/coolify-web","State":{"Running":false,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"2026-01-02T00:00:00Z"}}]`),
			"inspect --type container src-worker-id": []byte(`[{"Id":"src-worker-id","Name":"/coolify-worker","State":{"Running":false,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"2026-01-02T00:00:00Z"}}]`),
		},
	}
	if _, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", staged, true); err != nil {
		t.Fatalf("seed staging volume pin: %v", err)
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan}
	entry := actx.entry("api")
	entry.MigratedVolumeMounts = map[string]migratedVolumeMount{
		migratedMountKey("web", "/data"):    {Service: "web", Target: "/data", VolumeName: shared},
		migratedMountKey("worker", "/work"): {Service: "worker", Target: "/work", VolumeName: shared},
	}
	entry.StagingTransferStarted = true

	if err := client.applyStep(context.Background(), actx, plan.Steps[0]); err != nil {
		t.Fatalf("applyStep(pause): %v", err)
	}
	if len(entry.MigratedVolumeMounts) != 0 {
		t.Fatalf("records from the previous attempt must not survive a re-executed pause, got %#v", entry.MigratedVolumeMounts)
	}
	if !entry.StagingTransferStarted {
		t.Fatal("staging transfer evidence must survive the invalidation")
	}
	path, err := migratedVolumeMountsPath(plan.RunDir)
	if err != nil {
		t.Fatalf("migrated volume state path: %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated volume state: %v", err)
	}
	var state migratedVolumeMountsState
	if err := json.Unmarshal(contents, &state); err != nil {
		t.Fatalf("decode migrated volume state: %v", err)
	}
	if len(state.Apps["api"]) != 0 {
		t.Fatalf("persisted records must not survive a re-executed pause, got %#v", state.Apps["api"])
	}
	if !slices.Contains(state.StartedApps, "api") {
		t.Fatalf("persisted staging transfer evidence must survive, got %#v", state.StartedApps)
	}

	if err := client.applySyncVolume(context.Background(), actx, steps[0]); err != nil {
		t.Fatalf("applySyncVolume(web): %v", err)
	}
	if len(runner.runs) != 1 || !strings.Contains(strings.Join(runner.runs[0].Args, " "), "src-shared:/from:ro -v "+shared+":/to") {
		t.Fatalf("stale records must not suppress the re-copy, got %#v", runner.runs)
	}
}

func TestSyncVolumeToStagingRefusesRunningSource(t *testing.T) {
	_, plan, step, staged := stagedSyncFixture(t)
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect src-vol":              []byte(`[{"Name":"src-vol"}]`),
			"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
			"ps -a --filter volume=" + staged.VolumeName:                                     []byte(""),
			"inspect --type container src-id":                                                []byte(`[{"Id":"src-id","Name":"/coolify-web","State":{"Running":true}}]`),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan}
	err := client.applySyncVolume(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "is running while its state is being copied") {
		t.Fatalf("expected running-source refusal, got %v", err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("must not copy from a running source, got %#v", runner.runs)
	}
}

func TestApplySyncVolumeRefusesBindMountInStagedApp(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "web", Type: "bind", Source: "/srv/data", Target: "/data"}}
	step := Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"}
	plan := stagedPlan(t, app, t.TempDir(), step)
	runner := &fakeDockerRunner{}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan}
	err := client.applySyncVolume(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "only named volumes are transferred before deploy") {
		t.Fatalf("expected bind mount refusal, got %v", err)
	}
	if len(runner.outputArgs) != 0 || len(runner.runs) != 0 {
		t.Fatalf("must not touch docker, got outputs=%v runs=%v", runner.outputArgs, runner.runs)
	}
}

func TestApplyRefusesStagedBindMountBeforePausingSource(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", Source: "src-vol", Target: "/named", SourceContainerID: "src-id"},
		{Service: "web", Type: "bind", Source: "/srv/data", Target: "/data", SourceContainerID: "src-id"},
	}
	plan := stagedPlan(t, app, t.TempDir(),
		Step{Kind: StepPauseSource, App: "api", Ref: "api"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /named"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
	)
	runner := &fakeDockerRunner{}
	client := &Client{Docker: runner}
	err := client.Apply(context.Background(), plan)
	if !errors.Is(err, ErrNotImplemented) || !strings.Contains(err.Error(), "choose a recreate or managed data store strategy or change the source compose before live apply") {
		t.Fatalf("expected pre-live bind mount refusal, got %v", err)
	}
	if len(runner.outputArgs) != 0 || len(runner.runs) != 0 {
		t.Fatalf("source was touched before the bind mount refusal: outputs=%v runs=%v", runner.outputArgs, runner.runs)
	}
}

func stagedRestoreFixture(t *testing.T) (Plan, Step, stagedVolume) {
	t.Helper()
	runDir := t.TempDir()
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  db:\n    image: postgres:16\n    ports:\n      - \"5432:5432\"\n    environment:\n      POSTGRES_PASSWORD: ${DB_PASSWORD}\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n  web:\n    image: example/web\nvolumes:\n  pgdata:\n"
	env := "DB_PASSWORD=s3cret\n"
	composePath := filepath.Join(appDir, "compose.yaml")
	envPath := filepath.Join(appDir, ".env")
	if err := os.WriteFile(composePath, []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	app := preparer.AppPlan{
		Name:      "api",
		Directory: "api",
		TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{
			ComposeApp: preparer.DokployComposeApp{ComposePath: "compose.yaml"},
			EnvFiles:   []preparer.DokployEnvFile{{Path: ".env"}},
		}},
	}
	app.Resources.DataStores = []preparer.DataStoreResource{{Kind: "postgres", Service: "db", Strategy: "migrate"}}
	app.Resources.Volumes = []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}}
	step := Step{Kind: StepRestoreDataStore, App: "api", Ref: "data-store:db"}
	plan := stagedPlan(t, app, runDir, Step{Kind: StepPauseSource, App: "api"}, Step{Kind: StepDumpDataStore, App: "api", Ref: step.Ref}, step)
	plan.Prepare.BundleDir = bundleDir
	plan.BundleFiles = map[string][]byte{filepath.Clean(composePath): []byte(compose), filepath.Clean(envPath): []byte(env)}

	dumpPath, err := dataStoreDumpPath(plan, "api", step.Ref)
	if err != nil {
		t.Fatalf("dataStoreDumpPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dumpPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dumpPath, []byte("pg-dump"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged := stagedVolumesForService(plan, "api", "db")
	if len(staged) != 1 {
		t.Fatalf("expected one staged volume for db, got %#v", staged)
	}
	return plan, step, staged[0]
}

func TestRestoreDataStoreToStagingRunsIsolatedComposeProject(t *testing.T) {
	plan, step, staged := stagedRestoreFixture(t)
	project := stagingProjectName(plan, "api", "db")
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
			"volume inspect " + staged.VolumeName:                                            ownedStagingVolumeInspect(plan, staged),
			"compose -p " + project:                                                          []byte("stg-id\n"),
			"inspect --type container stg-id":                                                []byte(`[{"Id":"stg-id","Name":"/` + project + `-db-1","Config":{"Env":["POSTGRES_USER=bob","POSTGRES_PASSWORD=s3cret","POSTGRES_DB=app"]},"State":{"Running":true,"Status":"running"},"Mounts":[{"Type":"volume","Name":"` + staged.VolumeName + `","Destination":"/var/lib/postgresql/data","RW":true}]}]`),
			"ps -a --filter volume=" + staged.VolumeName:                                     []byte(""),
			"exec stg-id rm -f":                                                              []byte(""),
		},
		runOutputs: map[string][]byte{
			"exec -i stg-id pg_restore -l": []byte("271; 1259 100 TABLE public widgets bob\n"),
		},
	}
	client := stagingCompatibleClient(t, runner, true, "")
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan, stagingEnvFormat: stagingEnvFormatKeepInterpolation}
	actx.entry("api").ComposeAppName = "stack-1"

	if err := client.applyRestoreDataStore(context.Background(), actx, step); err != nil {
		t.Fatalf("applyRestoreDataStore: %v", err)
	}

	stageDir := filepath.Join(plan.RunDir, "stage", "api", "db")
	composeOut, err := os.ReadFile(filepath.Join(stageDir, "compose.yaml"))
	if err != nil {
		t.Fatalf("staging compose not written: %v", err)
	}
	doc := decodeCompose(t, string(composeOut))
	if services := doc["services"].(map[string]any); len(services) != 1 || services["db"] == nil {
		t.Fatalf("staging compose must contain only db, got %#v", services)
	}
	if def := composeVolumeDef(t, doc, "pgdata"); def["name"] != staged.VolumeName {
		t.Fatalf("staging compose must mount the staging volume, got %#v", def)
	}
	envOut, err := os.ReadFile(filepath.Join(stageDir, ".env"))
	if err != nil {
		t.Fatalf("staging env not written: %v", err)
	}
	for _, want := range []string{"COMPOSE_PROJECT_NAME=\"stack-1\"", "DB_PASSWORD=\"s3cret\""} {
		if !strings.Contains(string(envOut), want) {
			t.Fatalf("staging env missing %s:\n%s", want, envOut)
		}
	}

	composePath := filepath.Join(stageDir, "compose.yaml")
	var composeRuns []string
	upIndex, restoreIndex, lastDownIndex := -1, -1, -1
	for index, run := range runner.runs {
		joined := strings.Join(run.Args, " ")
		if run.Args[0] != "compose" {
			if strings.Contains(joined, " pg_restore -w ") {
				restoreIndex = index
			}
			continue
		}
		prefix := "compose -p " + project + " --env-file " + filepath.Join(stageDir, ".env") + " -f " + composePath + " "
		if !strings.HasPrefix(joined, prefix) {
			t.Fatalf("compose must target the Bort staging project with its env file, got %v", run.Args)
		}
		subcommand := strings.TrimPrefix(joined, prefix)
		composeRuns = append(composeRuns, subcommand)
		if strings.HasPrefix(subcommand, "up ") {
			upIndex = index
		} else if strings.HasPrefix(subcommand, "down") {
			lastDownIndex = index
		}
	}
	wantCompose := []string{"down --remove-orphans", "up -d --no-build --no-deps db", "down --remove-orphans"}
	if strings.Join(composeRuns, "|") != strings.Join(wantCompose, "|") {
		t.Fatalf("compose lifecycle mismatch\n got: %v\nwant: %v", composeRuns, wantCompose)
	}
	if restoreIndex < 0 || !(upIndex < restoreIndex && restoreIndex < lastDownIndex) {
		t.Fatalf("pg_restore must run between compose up and the final down, runs=%#v", runner.runs)
	}

	clearIndex := -1
	for index, run := range runner.runs {
		if strings.Contains(strings.Join(run.Args, " "), "find /volume -mindepth 1 -delete && sync") {
			clearIndex = index
			break
		}
	}
	if clearIndex < 0 || upIndex < 0 || clearIndex >= upIndex {
		t.Fatalf("staging volume must be cleared under its pin before restore, clear=%d up=%d runs=%#v", clearIndex, upIndex, runner.runs)
	}
	if fakeOutputCalled(runner, "volume", "rm", "-f", staged.VolumeName) {
		t.Fatalf("restore must preserve the owned staging volume identity, outputs=%v", runner.outputArgs)
	}
	mount, ok := actx.entry("api").MigratedVolumeMounts[migratedMountKey("db", "/var/lib/postgresql/data")]
	if !ok || mount.VolumeName != staged.VolumeName {
		t.Fatalf("expected staging mount recorded after restore, got %#v", actx.entry("api").MigratedVolumeMounts)
	}
	if len(runner.activePins) != 1 {
		t.Fatalf("restore must leave the app-wide staging-volume pin active for deployment handoff, active=%#v", runner.activePins)
	}
}

func TestRestoreDataStoreToStagingLeavesUnrelatedAppVolumeAttachedOnlyToPin(t *testing.T) {
	plan, step, databaseVolume := stagedRestoreFixture(t)
	app := &plan.Prepare.Apps[0]
	uploads := preparer.VolumeResource{Service: "web", Type: "volume", Name: "src-uploads", Target: "/uploads"}
	app.Resources.Volumes = append(app.Resources.Volumes, uploads)
	plan.Steps = slices.Insert(plan.Steps, len(plan.Steps)-1, Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /uploads"})
	uploadsVolume, ok := stagedVolumeFor(plan, "api", uploads)
	if !ok {
		t.Fatal("uploads volume is not staged")
	}
	project := stagingProjectName(plan, "api", "db")
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect " + databaseVolume.VolumeName: ownedStagingVolumeInspect(plan, databaseVolume),
			"volume inspect " + uploadsVolume.VolumeName:  ownedStagingVolumeInspect(plan, uploadsVolume),
			"compose -p " + project:                       []byte("stg-id\n"),
			"inspect --type container stg-id":             []byte(`[{"Id":"stg-id","Name":"/` + project + `-db-1","Config":{"Env":["POSTGRES_USER=bob","POSTGRES_PASSWORD=[REDACTED:password]","POSTGRES_DB=app"]},"State":{"Running":true,"Status":"running"},"Mounts":[{"Type":"volume","Name":"` + databaseVolume.VolumeName + `","Destination":"/var/lib/postgresql/data","RW":true}]}]`),
			"exec stg-id rm -f":                           []byte(""),
		},
		runOutputs: map[string][]byte{
			"exec -i stg-id pg_restore -l": []byte("271; 1259 100 TABLE public widgets bob\n"),
		},
	}
	client := stagingCompatibleClient(t, runner, true, "")
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan, stagingEnvFormat: stagingEnvFormatKeepInterpolation}
	actx.entry("api").ComposeAppName = "stack-1"

	if err := client.applyRestoreDataStore(context.Background(), actx, step); err != nil {
		t.Fatalf("restore rejected the unrelated uploads volume: %v", err)
	}
	if len(runner.activePins) != 1 {
		t.Fatalf("app-wide pin was not preserved across restore: %#v", runner.activePins)
	}
	var pin dockerContainer
	for _, candidate := range runner.activePins {
		pin = candidate
	}
	mounted := map[string]bool{}
	for _, mount := range pin.Mounts {
		if mount.Type == "volume" {
			mounted[mount.Name] = !mount.RW
		}
	}
	if !mounted[databaseVolume.VolumeName] || !mounted[uploadsVolume.VolumeName] || len(mounted) != 2 {
		t.Fatalf("app-wide pin mounts = %#v, want both staging volumes read-only", pin.Mounts)
	}
	attachments, err := stagingVolumeAttachmentSets(context.Background(), runner, []stagedVolume{databaseVolume, uploadsVolume})
	if err != nil {
		t.Fatal(err)
	}
	for _, volume := range []stagedVolume{databaseVolume, uploadsVolume} {
		if got := attachments[volume.VolumeName]; len(got) != 1 || !stagingContainerIDsMatch(got[0], pin.ID) {
			t.Fatalf("staging volume %s attachments = %v, want only pin %s", volume.VolumeName, got, pin.ID)
		}
	}
}

func TestRestoreDataStoreReplayRefusesMissingPinAfterDurableTransfer(t *testing.T) {
	plan, step, staged := stagedRestoreFixture(t)
	seed := &applyContext{plan: plan, cache: map[string]*appCache{}}
	if err := seed.recordMigratedVolumeMount("api", migratedVolumeMount{Service: staged.Service, Target: staged.Target, VolumeName: staged.VolumeName}); err != nil {
		t.Fatal(err)
	}
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}, stagingEnvFormat: stagingEnvFormatKeepInterpolation}
	if err := actx.loadMigratedVolumeMounts(); err != nil {
		t.Fatal(err)
	}
	actx.entry("api").ComposeID = "c1"
	actx.entry("api").ComposeAppName = "stack-1"
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
	}}
	client := stagingCompatibleClient(t, runner, true, "")

	err := client.applyRestoreDataStore(context.Background(), actx, step)
	if err == nil || !isUnsafeSourceResumeError(err) || !strings.Contains(err.Error(), "missing after state transfer") {
		t.Fatalf("expected same-restore replay to refuse pin recreation, got %v", err)
	}
	for _, run := range runner.runs {
		joined := strings.Join(run.Args, " ")
		if strings.Contains(joined, "find /volume -mindepth 1 -delete") {
			t.Fatalf("same-restore replay cleared data: runs=%#v", runner.runs)
		}
	}
	for _, args := range runner.outputArgs {
		if len(args) > 1 && args[0] == "run" && args[1] == "-d" {
			t.Fatalf("same-restore replay recreated the missing pin: outputs=%#v", runner.outputArgs)
		}
	}
	if len(runner.activePins) != 0 {
		t.Fatalf("same-restore replay left an unexpected pin active: %#v", runner.activePins)
	}
}

func TestRestoreDataStoreToStagingRefusesToClearForeignVolume(t *testing.T) {
	plan, step, staged := stagedRestoreFixture(t)
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect " + staged.VolumeName:        stagingVolumeInspect(plan, staged, "other-run"),
			"ps -a --filter volume=" + staged.VolumeName: []byte(""),
		},
	}
	client := stagingCompatibleClient(t, runner, true, "")
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan, stagingEnvFormat: stagingEnvFormatKeepInterpolation}
	actx.entry("api").ComposeAppName = "stack-1"

	err := client.applyRestoreDataStore(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "label bort.run-id=\"other-run\", want \"run1\"") {
		t.Fatalf("expected foreign volume refusal, got %v", err)
	}
	for _, run := range runner.runs {
		if strings.Contains(strings.Join(run.Args, " "), "find /volume -mindepth 1 -delete") {
			t.Fatalf("restore cleared a volume it does not own: %#v", runner.runs)
		}
	}
}

func TestRestoreDataStoreToStagingStopsProjectAndSkipsRecordOnFailure(t *testing.T) {
	plan, step, staged := stagedRestoreFixture(t)
	project := stagingProjectName(plan, "api", "db")
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
			"volume inspect " + staged.VolumeName:                                            ownedStagingVolumeInspect(plan, staged),
			"ps -a --filter volume=" + staged.VolumeName:                                     []byte(""),
			"compose -p " + project:                                                          []byte("stg-a\nstg-b\n"),
		},
	}
	client := stagingCompatibleClient(t, runner, true, "")
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan, stagingEnvFormat: stagingEnvFormatKeepInterpolation}
	actx.entry("api").ComposeAppName = "stack-1"

	err := client.applyRestoreDataStore(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "has 2 containers for service db") {
		t.Fatalf("expected ambiguous container error, got %v", err)
	}
	last := runner.runs[len(runner.runs)-1].Args
	if last[0] != "compose" || !strings.Contains(strings.Join(last, " "), " down --remove-orphans") {
		t.Fatalf("staging project must be stopped after a failed restore, last run %v", last)
	}
	if len(actx.entry("api").MigratedVolumeMounts) != 0 {
		t.Fatalf("failed restore must not record mounts, got %#v", actx.entry("api").MigratedVolumeMounts)
	}
}

type failingStagingUpRunner struct {
	*fakeDockerRunner
}

func (r *failingStagingUpRunner) Run(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	if err := r.fakeDockerRunner.Run(ctx, stdin, stdout, args...); err != nil {
		return err
	}
	if slices.Contains(args, "up") {
		return errors.New("docker compose response lost after start")
	}
	return nil
}

func TestRestoreDataStoreToStagingStopsProjectAfterAmbiguousStartFailure(t *testing.T) {
	plan, step, staged := stagedRestoreFixture(t)
	project := stagingProjectName(plan, "api", "db")
	runner := &failingStagingUpRunner{fakeDockerRunner: &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
		"volume inspect " + staged.VolumeName:                                            ownedStagingVolumeInspect(plan, staged),
		"ps -a --filter volume=" + staged.VolumeName:                                     []byte(""),
	}}}
	client := stagingCompatibleClient(t, runner, true, "")
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan, stagingEnvFormat: stagingEnvFormatKeepInterpolation}
	actx.entry("api").ComposeAppName = "stack-1"

	err := client.applyRestoreDataStore(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "docker compose response lost after start") {
		t.Fatalf("expected staging start failure, got %v", err)
	}
	var composeRuns []string
	for _, run := range runner.runs {
		joined := strings.Join(run.Args, " ")
		if strings.HasPrefix(joined, "compose -p "+project+" ") {
			composeRuns = append(composeRuns, joined)
		}
	}
	if len(composeRuns) != 3 || !strings.Contains(composeRuns[1], " up -d ") || !strings.HasSuffix(composeRuns[2], " down --remove-orphans") {
		t.Fatalf("failed staging start must be followed by teardown, runs=%v", composeRuns)
	}
	if len(actx.entry("api").MigratedVolumeMounts) != 0 {
		t.Fatalf("failed staging start must not record mounts, got %#v", actx.entry("api").MigratedVolumeMounts)
	}
}

func TestComposeFileForApplyRewritesVolumesOnlyAfterStagedTransfer(t *testing.T) {
	plan, _, staged := stagedRestoreFixture(t)
	client := &Client{Docker: &fakeDockerRunner{}}
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan}

	before, err := client.composeFileForApply(context.Background(), actx, "api")
	if err != nil {
		t.Fatalf("composeFileForApply: %v", err)
	}
	if strings.Contains(before, staged.VolumeName) {
		t.Fatalf("compose must not reference staging volumes before transfer:\n%s", before)
	}
	err = requireStagedStateTransferred(actx, "api")
	if err == nil || !strings.Contains(err.Error(), "db:/var/lib/postgresql/data not transferred") {
		t.Fatalf("expected incomplete-state refusal, got %v", err)
	}

	if err := actx.recordMigratedVolumeMount("api", migratedVolumeMount{Service: "db", Target: "/var/lib/postgresql/data", VolumeName: "wrong-volume"}); err != nil {
		t.Fatal(err)
	}
	if err := requireStagedStateTransferred(actx, "api"); err == nil {
		t.Fatalf("a mount recorded under a different volume name must not count as transferred")
	}

	if err := actx.recordMigratedVolumeMount("api", migratedVolumeMount{Service: "db", Target: "/var/lib/postgresql/data", VolumeName: staged.VolumeName}); err != nil {
		t.Fatal(err)
	}
	if err := requireStagedStateTransferred(actx, "api"); err != nil {
		t.Fatalf("requireStagedStateTransferred after transfer: %v", err)
	}
	after, err := client.composeFileForApply(context.Background(), actx, "api")
	if err != nil {
		t.Fatalf("composeFileForApply: %v", err)
	}
	def := composeVolumeDef(t, decodeCompose(t, after), "pgdata")
	if def["name"] != staged.VolumeName || def["external"] != true {
		t.Fatalf("compose must hand the staging volume to Dokploy after transfer, got %#v", def)
	}
}

func TestAppStateIsStagedRequiresPushAfterStateSteps(t *testing.T) {
	steps := []Step{
		{Kind: StepPushImage, App: "legacy"},
		{Kind: StepSyncVolume, App: "legacy", Ref: "volume:web -> /data"},
		{Kind: StepSyncVolume, App: "staged", Ref: "volume:web -> /data"},
		{Kind: StepPushImage, App: "staged"},
		{Kind: StepPushImage, App: "stateless"},
	}
	plan := Plan{Steps: steps}
	if appStateIsStaged(plan, "legacy") {
		t.Fatalf("push before state work is in-place, not staged")
	}
	if !appStateIsStaged(plan, "staged") {
		t.Fatalf("state work before push must be staged")
	}
	if appStateIsStaged(plan, "stateless") {
		t.Fatalf("apps without state work are not staged")
	}
}

func TestSyncVolumeToStagingRefusesAttachedVolumeBeforeCopy(t *testing.T) {
	_, plan, step, staged := stagedSyncFixture(t)
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect src-vol":              []byte(`[{"Name":"src-vol"}]`),
			"volume inspect " + staged.VolumeName: ownedStagingVolumeInspect(plan, staged),
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
			"ps -a --filter volume=" + staged.VolumeName:                                     []byte("deployed-id\n"),
			"inspect --type container src-id":                                                stoppedSourceInspect("2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan}
	err := client.applySyncVolume(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "attached to unexpected container deployed-id") {
		t.Fatalf("expected attached-volume refusal, got %v", err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("must not copy into an attached staging volume, got %#v", runner.runs)
	}
}

func TestStagingOwnershipUsesRunIDOverRunName(t *testing.T) {
	_, plan, step, _ := stagedSyncFixture(t)
	plan.RunID = "run-digest"
	staged, ok := stagedVolumeFor(plan, "api", plan.Prepare.Apps[0].Resources.Volumes[0])
	if !ok {
		t.Fatal("volume not staged")
	}
	sameName := stagedPlan(t, plan.Prepare.Apps[0], plan.RunDir, Step{Kind: StepPauseSource, App: "api"}, step)
	if other, _ := stagedVolumeFor(sameName, "api", plan.Prepare.Apps[0].Resources.Volumes[0]); other.VolumeName == staged.VolumeName {
		t.Fatalf("runs with the same name but different identity must not share staging volume %s", staged.VolumeName)
	}
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect " + staged.VolumeName: stagingVolumeInspect(plan, staged, "run1"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan}
	err := client.applySyncVolume(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "bort.run-id=\"run1\"") {
		t.Fatalf("expected run-id ownership refusal, got %v", err)
	}
}

func TestRestoreDataStoreToStagingRefusesUnstagedDataDir(t *testing.T) {
	plan, step, staged := stagedRestoreFixture(t)
	project := stagingProjectName(plan, "api", "db")
	for name, tc := range map[string]struct {
		inspect string
		volumes []preparer.VolumeResource
		want    string
	}{
		"bind at PGDATA": {
			inspect: `"Config":{"Env":["POSTGRES_USER=bob","POSTGRES_DB=app","PGDATA=/srv/pg"]},"Mounts":[{"Type":"bind","Source":"/host/pg","Destination":"/srv/pg","RW":true},{"Type":"volume","Name":"` + staged.VolumeName + `","Destination":"/var/lib/postgresql/data","RW":true}]`,
			want:    "not a staged volume",
		},
		"nothing at PGDATA": {
			inspect: `"Config":{"Env":["POSTGRES_USER=bob","POSTGRES_DB=app"]},"Mounts":[{"Type":"volume","Name":"` + staged.VolumeName + `","Destination":"/backups","RW":true}]`,
			want:    "not mounted from a staged volume",
		},
		"source bind inside image PGDATA": {
			inspect: `"Config":{"Env":["POSTGRES_USER=bob","POSTGRES_DB=app"]},"Mounts":[{"Type":"volume","Name":"` + staged.VolumeName + `","Destination":"/var/lib/postgresql/data","RW":true}]`,
			volumes: []preparer.VolumeResource{{Service: "db", Type: "bind", Source: "/srv/wal", Target: "/var/lib/postgresql/data/pg_wal", ReadWrite: true}},
			want:    "sits inside postgres data directory",
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan := plan
			plan.Prepare.Apps = []preparer.AppPlan{plan.Prepare.Apps[0]}
			plan.Prepare.Apps[0].Resources.Volumes = append(append([]preparer.VolumeResource{}, plan.Prepare.Apps[0].Resources.Volumes...), tc.volumes...)
			inspect := tc.inspect
			runner := &fakeDockerRunner{
				outputs: map[string][]byte{
					"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
					"volume inspect " + staged.VolumeName:                                            ownedStagingVolumeInspect(plan, staged),
					"ps -a --filter volume=" + staged.VolumeName:                                     []byte(""),
					"compose -p " + project:                                                          []byte("stg-id\n"),
					"inspect --type container stg-id":                                                []byte(`[{"Id":"stg-id","Name":"/` + project + `-db-1",` + inspect + `,"State":{"Running":true,"Status":"running"}}]`),
				},
			}
			client := stagingCompatibleClient(t, runner, true, "")
			actx := &applyContext{cache: map[string]*appCache{}, plan: plan, stagingEnvFormat: stagingEnvFormatKeepInterpolation}
			actx.entry("api").ComposeAppName = "stack-1"
			err := client.applyRestoreDataStore(context.Background(), actx, step)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected data dir refusal mentioning %q, got %v", tc.want, err)
			}
			for _, run := range runner.runs {
				if strings.Contains(strings.Join(run.Args, " "), "pg_restore") {
					t.Fatalf("pg_restore must not run into an unstaged data dir: %v", run.Args)
				}
			}
			if len(actx.entry("api").MigratedVolumeMounts) != 0 {
				t.Fatalf("refused restore must not record mounts, got %#v", actx.entry("api").MigratedVolumeMounts)
			}
		})
	}
}

func TestRequireStagedPostgresDataDirAcceptsParentVolumeMount(t *testing.T) {
	var container dockerContainer
	container.Config.Env = []string{"PGDATA=/var/lib/postgresql/data/pgdata"}
	container.Mounts = []dockerMount{{Type: "volume", Name: "staged-vol", Destination: "/var/lib/postgresql/data"}}
	if err := requireStagedPostgresDataDir(container, []stagedVolume{{VolumeName: "staged-vol"}}); err != nil {
		t.Fatalf("parent volume mount must satisfy PGDATA: %v", err)
	}
	container.Mounts = append(container.Mounts, dockerMount{Type: "bind", Source: "/host", Destination: "/var/lib/postgresql/data/pgdata"})
	if err := requireStagedPostgresDataDir(container, []stagedVolume{{VolumeName: "staged-vol"}}); err == nil {
		t.Fatal("the deepest mount at PGDATA wins; a bind there must be refused")
	}
}

func TestRewriteComposeStagedVolumesRefusesSharedKey(t *testing.T) {
	compose := "services:\n  web:\n    volumes:\n      - shared:/data\n  worker:\n    volumes:\n      - shared:/work\nvolumes:\n  shared:\n"
	staged := []stagedVolume{
		{Service: "web", Target: "/data", VolumeName: "bort-web"},
		{Service: "worker", Target: "/work", VolumeName: "bort-worker"},
	}
	_, err := rewriteComposeStagedVolumes(compose, staged)
	if err == nil || !strings.Contains(err.Error(), "compose volume shared is mounted by web:/data and worker:/work") {
		t.Fatalf("expected shared key refusal, got %v", err)
	}
}

func TestValidatePlanReadyForLiveApplyStagesSharedVolumeWithinApp(t *testing.T) {
	runDir := t.TempDir()
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  web:\n    image: example/web\n    volumes:\n      - shared:/data\n  worker:\n    image: example/worker\n    volumes:\n      - shared:/work\nvolumes:\n  shared:\n"
	composePath := filepath.Join(appDir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	app := preparer.AppPlan{
		Name:            "api",
		Directory:       "api",
		TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{ComposePath: "compose.yaml"}}},
	}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", Name: "src-shared", Target: "/data"},
		{Service: "worker", Type: "volume", Name: "src-shared", Target: "/work"},
	}
	plan := stagedPlan(t, app, runDir,
		Step{Kind: StepCreateVolume, App: "api", Ref: "shared"},
		Step{Kind: StepPauseSource, App: "api"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:worker -> /work"},
	)
	plan.Prepare.BundleDir = bundleDir
	plan.BundleFiles = map[string][]byte{filepath.Clean(composePath): []byte(compose)}
	err := validatePlanReadyForLiveApply(plan)
	if err != nil {
		t.Fatalf("expected a shared source volume within one app to stage as one volume, got %v", err)
	}
	staged := stagedVolumesForApp(plan, "api")
	if len(staged) != 2 {
		t.Fatalf("expected both mounts to stage, got %#v", staged)
	}
	if staged[0].VolumeName != staged[1].VolumeName {
		t.Fatalf("expected one shared staging volume, got %q and %q", staged[0].VolumeName, staged[1].VolumeName)
	}
	if want := stagingSharedVolumeName(plan, "api", "src-shared"); staged[0].VolumeName != want {
		t.Fatalf("expected shared staging volume %q, got %q", want, staged[0].VolumeName)
	}
	out, err := rewriteComposeStagedVolumes(compose, staged)
	if err != nil {
		t.Fatalf("rewriteComposeStagedVolumes: %v", err)
	}
	def := composeVolumeDef(t, decodeCompose(t, out), "shared")
	if def["name"] != staged[0].VolumeName || def["external"] != true {
		t.Fatalf("expected compose volume shared to hand off the shared staging volume, got %#v", def)
	}
}

func stagedRestoreSharedVolumePlan(t *testing.T, compose string, volumes []preparer.VolumeResource, steps ...Step) Plan {
	t.Helper()
	runDir := t.TempDir()
	bundleDir := t.TempDir()
	appDir := filepath.Join(bundleDir, "api")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(appDir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	app := preparer.AppPlan{
		Name:            "api",
		Directory:       "api",
		TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{ComposePath: "compose.yaml"}}},
	}
	app.Resources.Volumes = volumes
	app.Resources.DataStores = []preparer.DataStoreResource{{Kind: "postgres", Service: "db", Strategy: "migrate"}}
	plan := stagedPlan(t, app, runDir, steps...)
	plan.Prepare.BundleDir = bundleDir
	plan.BundleFiles = map[string][]byte{filepath.Clean(composePath): []byte(compose)}
	return plan
}

func TestValidateStagedTransferRefusesRestoreSharingStagedVolume(t *testing.T) {
	compose := "services:\n  db:\n    image: postgres:16\n    volumes:\n      - shared:/var/lib/postgresql/data\n  web:\n    image: example/web\n    volumes:\n      - shared:/data\nvolumes:\n  shared:\n"
	plan := stagedRestoreSharedVolumePlan(t, compose,
		[]preparer.VolumeResource{
			{Service: "db", Type: "volume", Name: "src-shared", Target: "/var/lib/postgresql/data"},
			{Service: "web", Type: "volume", Name: "src-shared", Target: "/data"},
		},
		Step{Kind: StepCreateVolume, App: "api", Ref: "shared"},
		Step{Kind: StepPauseSource, App: "api"},
		Step{Kind: StepRestoreDataStore, App: "api", Ref: "data-store:db"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
	)
	err := ValidateStagedTransfer(plan)
	if err == nil || !strings.Contains(err.Error(), "a data store restore replaces the whole staged volume") {
		t.Fatalf("expected a shared-restore refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "db /var/lib/postgresql/data and web /data in app api") {
		t.Fatalf("expected the refusal to name both mounts, got %v", err)
	}
}

func TestValidateStagedTransferRefusesStagedVolumeSharedWithUntransferredMount(t *testing.T) {
	compose := "services:\n  web:\n    image: example/web\n    volumes:\n      - shared:/data\n  worker:\n    image: example/worker\n    volumes:\n      - shared:/work\n  cache:\n    image: redis:7\n    volumes:\n      - shared:/redis\nvolumes:\n  shared:\n"
	plan := stagedRestoreSharedVolumePlan(t, compose,
		[]preparer.VolumeResource{
			{Service: "web", Type: "volume", Name: "src-shared", Target: "/data"},
			{Service: "worker", Type: "volume", Name: "src-shared", Target: "/work"},
			{Service: "cache", Type: "volume", Name: "src-shared", Target: "/redis"},
		},
		Step{Kind: StepCreateVolume, App: "api", Ref: "shared"},
		Step{Kind: StepPauseSource, App: "api"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:worker -> /work"},
	)
	plan.Prepare.Apps[0].Resources.DataStores = []preparer.DataStoreResource{{Kind: "redis", Service: "cache", Strategy: "recreate"}}
	err := ValidateStagedTransfer(plan)
	if err == nil || !strings.Contains(err.Error(), "also mounted by cache /redis in app api, which live apply does not transfer") {
		t.Fatalf("expected a refusal for the untransferred sibling mount, got %v", err)
	}
}

func TestValidateStagedTransferAllowsRestoreSharingWithinStoreService(t *testing.T) {
	compose := "services:\n  db:\n    image: postgres:16\n    volumes:\n      - shared:/var/lib/postgresql/data\n      - shared:/backups\nvolumes:\n  shared:\n"
	plan := stagedRestoreSharedVolumePlan(t, compose,
		[]preparer.VolumeResource{
			{Service: "db", Type: "volume", Name: "src-shared", Target: "/var/lib/postgresql/data"},
			{Service: "db", Type: "volume", Name: "src-shared", Target: "/backups"},
		},
		Step{Kind: StepCreateVolume, App: "api", Ref: "shared"},
		Step{Kind: StepPauseSource, App: "api"},
		Step{Kind: StepRestoreDataStore, App: "api", Ref: "data-store:db"},
	)
	if err := ValidateStagedTransfer(plan); err != nil {
		t.Fatalf("expected a store's own mounts of one volume to share its staging volume, got %v", err)
	}
}

func TestValidatePlanReadyForLiveApplyRefusesSourceVolumeSharedAcrossApps(t *testing.T) {
	runDir := t.TempDir()
	bundleDir := t.TempDir()
	apps := []preparer.AppPlan{}
	steps := []Step{}
	bundleFiles := map[string][]byte{}
	for _, name := range []string{"api", "worker"} {
		appDir := filepath.Join(bundleDir, name)
		if err := os.MkdirAll(appDir, 0o700); err != nil {
			t.Fatal(err)
		}
		compose := "services:\n  main:\n    image: example/" + name + "\n    volumes:\n      - data:/data\nvolumes:\n  data:\n    name: src-shared\n"
		composePath := filepath.Join(appDir, "compose.yaml")
		if err := os.WriteFile(composePath, []byte(compose), 0o600); err != nil {
			t.Fatal(err)
		}
		bundleFiles[filepath.Clean(composePath)] = []byte(compose)
		app := preparer.AppPlan{
			Name:            name,
			Directory:       name,
			TargetResources: &preparer.TargetResources{Dokploy: &preparer.DokployResources{ComposeApp: preparer.DokployComposeApp{ComposePath: "compose.yaml"}}},
		}
		app.Resources.Volumes = []preparer.VolumeResource{{Service: "main", Type: "volume", Name: "src-shared", Target: "/data"}}
		apps = append(apps, app)
		steps = append(steps,
			Step{Kind: StepCreateVolume, App: name, Ref: "data"},
			Step{Kind: StepPauseSource, App: name},
			Step{Kind: StepSyncVolume, App: name, Ref: "volume:main -> /data"},
			Step{Kind: StepPushImage, App: name},
		)
	}
	plan := Plan{
		RunName:     "run1",
		RunDir:      runDir,
		Prepare:     preparer.Result{BundleDir: bundleDir, Apps: apps},
		Steps:       steps,
		BundleFiles: bundleFiles,
	}
	err := validatePlanReadyForLiveApply(plan)
	if err == nil || !strings.Contains(err.Error(), "source volume src-shared is mounted by api main:/data and worker main:/data") || !strings.Contains(err.Error(), "choose a recreate or managed data store strategy or change the source compose before live apply") {
		t.Fatalf("expected cross-app shared source volume refusal before live apply starts, got %v", err)
	}
	plan.Prepare.Apps[1].Resources.Volumes[0].Name = "src-worker"
	if err := validatePlanReadyForLiveApply(plan); err != nil {
		t.Fatalf("distinct source volumes were refused: %v", err)
	}
}

func TestStagedVolumesForAppStagesEachSourceVolumeOnce(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", Name: "src-shared", Target: "/data"},
		{Service: "worker", Type: "volume", Name: "src-shared", Target: "/work"},
		{Service: "web", Type: "volume", Target: "/cache"},
	}
	plan := stagedPlan(t, app, t.TempDir(),
		Step{Kind: StepPauseSource, App: "api"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:worker -> /work"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /cache"},
	)
	staged := stagedVolumesForApp(plan, "api")
	if len(staged) != 3 {
		t.Fatalf("expected three staged mounts, got %#v", staged)
	}
	if staged[0].VolumeName != staged[1].VolumeName {
		t.Fatalf("expected the shared source to stage as one volume, got %q and %q", staged[0].VolumeName, staged[1].VolumeName)
	}
	if want := stagingSharedVolumeName(plan, "api", "src-shared"); staged[0].VolumeName != want || staged[1].VolumeName != want {
		t.Fatalf("expected shared staging volume %q, got %q and %q", want, staged[0].VolumeName, staged[1].VolumeName)
	}
	if !staged[0].Shared || !staged[1].Shared {
		t.Fatalf("expected the shared mounts to be marked shared, got %#v", staged[:2])
	}
	if want := stagingVolumeName(plan, "api", "web", "/cache"); staged[2].VolumeName != want || staged[2].Shared {
		t.Fatalf("expected the unnamed source to keep its per-mount staging volume %q, got %#v", want, staged[2])
	}
}

func TestSharedStagingVolumeOwnershipUsesSourceLabel(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", Name: "src-shared", Target: "/data"},
		{Service: "worker", Type: "volume", Name: "src-shared", Target: "/work"},
	}
	plan := stagedPlan(t, app, t.TempDir(),
		Step{Kind: StepPauseSource, App: "api"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:worker -> /work"},
	)
	staged := stagedVolumesForApp(plan, "api")
	wantLabels := []string{"bort.run=run1", "bort.run-id=run1", "bort.app=api", "bort.source=src-shared"}
	if got := stagingVolumeLabels(plan, "api", staged[0]); !reflect.DeepEqual(got, wantLabels) {
		t.Fatalf("expected shared staging labels %v, got %v", wantLabels, got)
	}
	if got := stagingVolumeLabels(plan, "api", staged[1]); !reflect.DeepEqual(got, wantLabels) {
		t.Fatalf("expected shared staging labels %v, got %v", wantLabels, got)
	}

	ownedState, _ := json.Marshal([]stagingVolumeState{{Name: staged[0].VolumeName, Labels: map[string]string{
		"bort.run":    "run1",
		"bort.run-id": "run1",
		"bort.app":    "api",
		"bort.source": "other-src",
	}}})
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + staged[0].VolumeName: ownedState,
	}}
	err := requireStagingVolumeOwned(context.Background(), runner, plan, "api", staged[0])
	if err == nil || !strings.Contains(err.Error(), "is not the staged copy of source volume src-shared for app api in run \"run1\"") {
		t.Fatalf("expected a shared ownership refusal naming the source volume, got %v", err)
	}
}

func TestRequireStagingVolumesOwnedInspectsSharedNameOnce(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "web", Type: "volume", Name: "src-shared", Target: "/data"},
		{Service: "worker", Type: "volume", Name: "src-shared", Target: "/work"},
	}
	plan := stagedPlan(t, app, t.TempDir(),
		Step{Kind: StepPauseSource, App: "api"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:web -> /data"},
		Step{Kind: StepSyncVolume, App: "api", Ref: "volume:worker -> /work"},
	)
	staged := stagedVolumesForApp(plan, "api")
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + staged[0].VolumeName: sharedStagingVolumeInspect(plan, staged[0]),
	}}
	if err := requireStagingVolumesOwned(context.Background(), runner, plan, "api", staged); err != nil {
		t.Fatalf("requireStagingVolumesOwned: %v", err)
	}
	var inspected []string
	for _, args := range runner.outputArgs {
		if len(args) >= 3 && args[0] == "volume" && args[1] == "inspect" && args[2] != "--format" {
			inspected = append(inspected, args[2:]...)
		}
	}
	if len(inspected) != 1 || inspected[0] != staged[0].VolumeName {
		t.Fatalf("expected one ownership inspect of the shared staging volume, got %v", inspected)
	}
}

func TestStagingVolumePinBindsSharedNameOnce(t *testing.T) {
	_, plan, _, _ := stagedSyncFixture(t)
	shared := stagedVolume{Service: "web", Target: "/data", VolumeName: "bort-run1-api-shared-aaaaaaaa", Source: preparer.VolumeResource{Name: "src-shared"}, Shared: true}
	other := stagedVolume{Service: "web", Target: "/cache", VolumeName: "bort-run1-api-web-bbbbbbbb"}
	if stagingVolumePinName(plan, []stagedVolume{shared, shared, other}) != stagingVolumePinName(plan, []stagedVolume{shared, other}) {
		t.Fatal("expected duplicate staged volumes to produce one pin name")
	}
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + shared.VolumeName: sharedStagingVolumeInspect(plan, shared),
		"volume inspect " + other.VolumeName:  stagingVolumeInspect(plan, other, "run1"),
	}}
	pin, err := acquireStagingVolumePin(context.Background(), runner, plan, "api", []stagedVolume{shared, shared, other}, true)
	if err != nil {
		t.Fatalf("acquireStagingVolumePin: %v", err)
	}
	created, ok := runner.activePins[pin.containerID]
	if !ok {
		t.Fatalf("expected pin %s to be active, got %#v", pin.containerID, runner.activePins)
	}
	wantMounts := []dockerMount{
		{Type: "volume", Name: shared.VolumeName, Destination: "/bort-volume/0", RW: false},
		{Type: "volume", Name: other.VolumeName, Destination: "/bort-volume/1", RW: false},
	}
	if !reflect.DeepEqual(created.Mounts, wantMounts) {
		t.Fatalf("expected the shared staging volume to be bound once, got %#v", created.Mounts)
	}
}

func TestValidatePlannedPostgresDataDirsRefusesUnstagedLayoutsBeforeLiveApply(t *testing.T) {
	for name, tc := range map[string]struct {
		compose string
		volumes []preparer.VolumeResource
		env     string
		want    string
	}{
		"bind mount data dir": {
			compose: "services:\n  db:\n    image: postgres:16\n    volumes:\n      - ./pg:/var/lib/postgresql/data\nvolumes: {}\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "bind", Source: "/srv/pg", Target: "/var/lib/postgresql/data"}},
			want:    "mounts no named volume",
		},
		"no data dir mount": {
			compose: "services:\n  db:\n    image: postgres:16\n",
			want:    "mounts no named volume",
		},
		"bind data dir beside unrelated named volume": {
			compose: "services:\n  db:\n    image: postgres:16\n    environment:\n      PGDATA: /pg/data\n    volumes:\n      - backups:/backups\n      - ./pg:/pg/data\nvolumes:\n  backups:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-backups", Target: "/backups"}, {Service: "db", Type: "bind", Source: "/srv/pg", Target: "/pg/data"}},
			want:    "bind \"/srv/pg\" at /pg/data",
		},
		"null PGDATA defers to preflight": {
			compose: "services:\n  db:\n    image: postgres:16\n    environment:\n      PGDATA: null\n    volumes:\n      - pgdata:/var/lib/postgresql/data\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}},
		},
		"list-form PGDATA beside named volume": {
			compose: "services:\n  db:\n    image: postgres:16\n    environment:\n      - PGDATA=/pg/data\n    volumes:\n      - pgdata:/var/lib/postgresql/data\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}},
			want:    "/pg/data",
		},
		"bind inside PGDATA": {
			compose: "services:\n  db:\n    image: postgres:16\n    environment:\n      PGDATA: /var/lib/postgresql/data\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n      - ./wal:/var/lib/postgresql/data/pg_wal\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}, {Service: "db", Type: "bind", Source: "/srv/wal", Target: "/var/lib/postgresql/data/pg_wal", ReadWrite: true}},
			want:    "bind \"/srv/wal\" at /var/lib/postgresql/data/pg_wal",
		},
		"read-only config file inside PGDATA": {
			compose: "services:\n  db:\n    image: postgres:16\n    environment:\n      PGDATA: /var/lib/postgresql/data\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n      - ./pg_hba.conf:/var/lib/postgresql/data/pg_hba.conf:ro\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}, {Service: "db", Type: "bind", Source: "/srv/pg_hba.conf", Target: "/var/lib/postgresql/data/pg_hba.conf"}},
		},
		"writable config file inside PGDATA": {
			compose: "services:\n  db:\n    image: postgres:16\n    environment:\n      PGDATA: /var/lib/postgresql/data\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n      - ./postgresql.conf:/var/lib/postgresql/data/postgresql.conf\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}, {Service: "db", Type: "bind", Source: "/srv/postgresql.conf", Target: "/var/lib/postgresql/data/postgresql.conf", ReadWrite: true}},
			want:    "bind \"/srv/postgresql.conf\" at /var/lib/postgresql/data/postgresql.conf",
		},
		"named volume inside PGDATA": {
			compose: "services:\n  db:\n    image: postgres:16\n    environment:\n      PGDATA: /var/lib/postgresql/data\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n      - wal:/var/lib/postgresql/data/pg_wal\nvolumes:\n  pgdata:\n  wal:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}, {Service: "db", Type: "volume", Name: "src-wal", Target: "/var/lib/postgresql/data/pg_wal"}},
		},
		"named volume outside PGDATA": {
			compose: "services:\n  db:\n    image: postgres:16\n    environment:\n      PGDATA: /var/lib/postgresql/data\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n      - backups:/backups\nvolumes:\n  pgdata:\n  backups:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}, {Service: "db", Type: "volume", Name: "src-backups", Target: "/backups"}},
			want:    "named volume \"src-backups\" at /backups",
		},
		"named volume beside custom PGDATA": {
			compose: "services:\n  db:\n    image: postgres:16\n    environment:\n      PGDATA: /pg/data\n    volumes:\n      - pgdata:/var/lib/postgresql/data\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}},
			want:    "/pg/data",
		},
		"custom PGDATA from env file": {
			compose: "services:\n  db:\n    image: postgres:16\n    environment:\n      - PGDATA=${PG_DIR}\n    volumes:\n      - pgdata:/var/lib/postgresql/data\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}},
			env:     "PG_DIR=/pg/data\n",
		},
		"interpolated PGDATA that cleans to a literal": {
			compose: "services:\n  db:\n    image: postgres:16\n    environment:\n      PGDATA: ${PGROOT}/../data\n    volumes:\n      - pgdata:/var/lib/postgresql/data\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}},
			env:     "PGROOT=/var/lib/postgresql/child\n",
		},
		"interpolated bind target": {
			compose: "services:\n  db:\n    image: postgres:16\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n      - ./other:${BIND_TARGET}\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}, {Service: "db", Type: "bind", Source: "/srv/other", Target: "/backups"}},
			env:     "BIND_TARGET=/backups\n",
			want:    "\"${BIND_TARGET}\" at an interpolated path",
		},
		"interpolated long-form bind target": {
			compose: "services:\n  db:\n    image: postgres:16\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n      - type: bind\n        source: ./other\n        target: $BIND_TARGET\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}, {Service: "db", Type: "bind", Source: "/srv/other", Target: "/backups"}},
			env:     "BIND_TARGET=/backups\n",
			want:    "\"$BIND_TARGET\" at an interpolated path",
		},
		"interpolated bind source with literal target": {
			compose: "services:\n  db:\n    image: postgres:16\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n      - ${BACKUP_DIR}:/backups\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}, {Service: "db", Type: "bind", Source: "/srv/other", Target: "/backups"}},
			env:     "BACKUP_DIR=/srv/other\n",
		},
		"defaulted bind source with interpolated target": {
			compose: "services:\n  db:\n    image: postgres:16\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n      - ${BACKUP_DIR:-./backups}:${BACKUP_TARGET}\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}, {Service: "db", Type: "bind", Source: "/srv/other", Target: "/backups"}},
			env:     "BACKUP_TARGET=/backups\n",
			want:    "at an interpolated path",
		},
		"defaulted bind source with literal target": {
			compose: "services:\n  db:\n    image: postgres:16\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n      - ${BACKUP_DIR:-$PWD/backups}:/backups\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}, {Service: "db", Type: "bind", Source: "/srv/other", Target: "/backups"}},
		},
		"custom PGDATA under named volume": {
			compose: "services:\n  db:\n    image: postgres:16\n    environment:\n      PGDATA: /var/lib/postgresql/data/pgdata\n    volumes:\n      - pgdata:/var/lib/postgresql/data\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}},
		},
		"bind mount shadows named volume": {
			compose: "services:\n  db:\n    image: postgres:16\n    volumes:\n      - pgdata:/var/lib/postgresql\n      - ./pg:/var/lib/postgresql/data\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pg", Target: "/var/lib/postgresql"}, {Service: "db", Type: "bind", Source: "/srv/pg", Target: "/var/lib/postgresql/data"}},
		},
		"custom PGDATA through merge key": {
			compose: "x-pg: &pg\n  image: postgres:16\n  environment:\n    PGDATA: /pg/data\nservices:\n  db:\n    <<: *pg\n    volumes:\n      - pgdata:/var/lib/postgresql/data\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}},
			want:    "/pg/data",
		},
		"custom PGDATA through aliased environment": {
			compose: "x-env: &env\n  PGDATA: /pg/data\nservices:\n  db:\n    image: postgres:16\n    environment: *env\n    volumes:\n      - pgdata:/var/lib/postgresql/data\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}},
			want:    "/pg/data",
		},
		"custom PGDATA through default env_file": {
			compose: "services:\n  db:\n    image: postgres:16\n    env_file: .env\n    volumes:\n      - pgdata:/var/lib/postgresql/data\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}},
			env:     "PGDATA=/pg/data\n",
		},
		"environment wins over env_file": {
			compose: "services:\n  db:\n    image: postgres:16\n    env_file: .env\n    environment:\n      PGDATA: /var/lib/postgresql/data/pgdata\n    volumes:\n      - pgdata:/var/lib/postgresql/data\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data"}},
			env:     "PGDATA=/pg/data\n",
		},
		"trailing slash on named volume target": {
			compose: "services:\n  db:\n    image: postgres:16\n    environment:\n      PGDATA: /var/lib/postgresql/data\n    volumes:\n      - pgdata:/var/lib/postgresql/data/\nvolumes:\n  pgdata:\n",
			volumes: []preparer.VolumeResource{{Service: "db", Type: "volume", Name: "src-pgdata", Target: "/var/lib/postgresql/data/"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan, _, _ := stagedRestoreFixture(t)
			composePath := filepath.Clean(filepath.Join(plan.Prepare.BundleDir, "api", "compose.yaml"))
			plan.BundleFiles[composePath] = []byte(tc.compose)
			if tc.env != "" {
				plan.BundleFiles[filepath.Clean(filepath.Join(plan.Prepare.BundleDir, "api", ".env"))] = []byte(tc.env)
			}
			plan.Prepare.Apps[0].Resources.Volumes = tc.volumes
			if err := validatePlanReadyForLiveApply(plan); err != nil {
				t.Fatalf("the planned data dir gate must not block Apply of a started run: %v", err)
			}
			err := ValidatePlannedPostgresDataDirs(plan)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err == nil || !errors.Is(err, ErrNotImplemented) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "before live apply") {
				t.Fatalf("expected data dir refusal mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestPauseSourcePreflightsStagedRestoreOnCreatedContainer(t *testing.T) {
	for name, tc := range map[string]struct {
		env     string
		volumes []preparer.VolumeResource
		want    string
	}{
		"image PGDATA outside staged volume": {env: `"PGDATA=/pg/data"`, want: "/pg/data"},
		"source bind nested under staged volume": {
			env:     `"PGDATA=/var/lib/postgresql/data/pgdata"`,
			volumes: []preparer.VolumeResource{{Service: "db", Type: "bind", Source: "/srv/pg", Target: "/var/lib/postgresql/data/pgdata", ReadWrite: true}},
			want:    "bind \"/srv/pg\" at /var/lib/postgresql/data/pgdata",
		},
		"data dir on staged volume": {env: `"POSTGRES_USER=bob"`},
	} {
		t.Run(name, func(t *testing.T) {
			plan, _, staged := stagedRestoreFixture(t)
			plan.Prepare.Apps[0].Resources.Volumes = append(append(plan.Prepare.Apps[0].Resources.Volumes, tc.volumes...), preparer.VolumeResource{Service: "web", Type: "volume", SourceContainerID: "web-id"})
			project := stagingProjectName(plan, "api", "db")
			runner := &fakeDockerRunner{
				outputs: map[string][]byte{
					"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
					"volume inspect " + staged.VolumeName:                                            ownedStagingVolumeInspect(plan, staged),
					"compose -p " + project:                                                          []byte("stg-id\n"),
					"inspect --type container stg-id":                                                []byte(`[{"Id":"stg-id","Name":"/` + project + `-db-1","Config":{"Env":[` + tc.env + `]},"State":{"Running":false,"Status":"created"},"Mounts":[{"Type":"volume","Name":"` + staged.VolumeName + `","Destination":"/var/lib/postgresql/data","RW":true}]}]`),
					"inspect --type container web-id":                                                []byte(`[{"Id":"web-id","Name":"/web","State":{"Running":true,"Status":"running"}}]`),
					"stop web-id":                                                                    []byte("web-id\n"),
				},
			}
			client := stagingCompatibleClient(t, runner, true, "")
			actx := &applyContext{cache: map[string]*appCache{}, plan: plan, stagingEnvFormat: stagingEnvFormatKeepInterpolation}
			actx.entry("api").ComposeAppName = "stack-1"

			err := client.preflightStagedRestores(context.Background(), actx, "api")
			if err == nil {
				err = client.applyStep(context.Background(), actx, Step{Kind: StepPauseSource, App: "api"})
			}

			stageDir := filepath.Join(plan.RunDir, "stage", "api", "db")
			prefix := "compose -p " + project + " --env-file " + filepath.Join(stageDir, ".env") + " -f " + filepath.Join(stageDir, "compose.yaml") + " "
			var composeRuns []string
			for _, run := range runner.runs {
				joined := strings.Join(run.Args, " ")
				if run.Args[0] != "compose" {
					continue
				}
				if !strings.HasPrefix(joined, prefix) {
					t.Fatalf("compose must target the Bort staging project with its env file, got %v", run.Args)
				}
				composeRuns = append(composeRuns, strings.TrimPrefix(joined, prefix))
			}
			wantCompose := []string{"down --remove-orphans", "create --no-build db", "down --remove-orphans"}
			if strings.Join(composeRuns, "|") != strings.Join(wantCompose, "|") {
				t.Fatalf("compose lifecycle mismatch\n got: %v\nwant: %v", composeRuns, wantCompose)
			}
			if fakeOutputCalled(runner, "volume", "rm", "-f", staged.VolumeName) {
				t.Fatalf("preflight must not recreate the staging volume, calls=%v", runner.outputArgs)
			}
			if tc.want == "" {
				if err != nil {
					t.Fatalf("applyStep(pause_source): %v", err)
				}
				if !fakeOutputCalled(runner, "stop", "web-id") {
					t.Fatalf("pause must stop the source after a clean preflight, calls=%v", runner.outputArgs)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "did not stop the source") || !strings.Contains(err.Error(), "create a new run") {
				t.Fatalf("expected preflight refusal mentioning %q, got %v", tc.want, err)
			}
			var refusal stagedRestorePreflightError
			if !errors.As(err, &refusal) || !refusal.requiresNewRun {
				t.Fatalf("a layout refusal must require a new run, got %v", err)
			}
			if fakeOutputCalled(runner, "stop", "web-id") {
				t.Fatalf("preflight refusal must not stop the source, calls=%v", runner.outputArgs)
			}
			if actx.entry("api").SourcePauseRecorded {
				t.Fatalf("preflight refusal must not record a source pause: %#v", actx.entry("api"))
			}
		})
	}
}

type failingCreateRunner struct {
	*fakeDockerRunner
}

func (r *failingCreateRunner) Run(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	if err := r.fakeDockerRunner.Run(ctx, stdin, stdout, args...); err != nil {
		return err
	}
	if slices.Contains(args, "create") {
		return errors.New("image pull failed")
	}
	return nil
}

func TestPauseSourcePreflightStopsStagingProjectAfterFailedCreate(t *testing.T) {
	plan, _, staged := stagedRestoreFixture(t)
	plan.Prepare.Apps[0].Resources.Volumes = append(plan.Prepare.Apps[0].Resources.Volumes, preparer.VolumeResource{Service: "web", Type: "volume", SourceContainerID: "web-id"})
	runner := &failingCreateRunner{fakeDockerRunner: &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
			"volume inspect " + staged.VolumeName:                                            ownedStagingVolumeInspect(plan, staged),
		},
	}}
	client := stagingCompatibleClient(t, runner, true, "")
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan, stagingEnvFormat: stagingEnvFormatKeepInterpolation}
	actx.entry("api").ComposeAppName = "stack-1"

	err := client.preflightStagedRestores(context.Background(), actx, "api")
	if err == nil || !strings.Contains(err.Error(), "image pull failed") || !strings.Contains(err.Error(), "did not stop the source") {
		t.Fatalf("expected create failure to abort the pause, got %v", err)
	}
	var refusal stagedRestorePreflightError
	if errors.As(err, &refusal) {
		t.Fatalf("a transient create failure must stay retryable, got %v", err)
	}
	var composeRuns []string
	for _, run := range runner.runs {
		if run.Args[0] == "compose" {
			composeRuns = append(composeRuns, strings.Join(run.Args[len(run.Args)-2:], " "))
		}
	}
	if last := composeRuns[len(composeRuns)-1]; last != "down --remove-orphans" || len(composeRuns) != 3 {
		t.Fatalf("a failed create must still stop the staging project, got %v", composeRuns)
	}
	if fakeOutputCalled(runner.fakeDockerRunner, "stop", "web-id") {
		t.Fatalf("create failure must not stop the source, calls=%v", runner.outputArgs)
	}
}

func TestApplyPreflightRefusalSkipsSourceResumeWhenNothingWasPaused(t *testing.T) {
	for name, tc := range map[string]applyPreflightRefusalCase{
		"layout refusal":             {want: "/pg/data", requiresNewRun: true},
		"transient create failure":   {transient: true, want: "image pull failed"},
		"repairable Dokploy setting": {composeCommand: "docker compose up -d", want: "clear it in Dokploy and resume"},
	} {
		t.Run(name, func(t *testing.T) {
			testApplyPreflightRefusal(t, tc)
		})
	}
}

func TestApplyPreflightRefusalResumesEarlierPartialPause(t *testing.T) {
	for name, tc := range map[string]applyPreflightRefusalCase{
		"resume succeeds": {want: "/pg/data", requiresNewRun: true, earlierPartialPause: true},
		"resume fails":    {want: "/pg/data", requiresNewRun: true, earlierPartialPause: true, resumeFails: true},
		"no named volume": {want: "has no named volume to stage", requiresNewRun: true, earlierPartialPause: true, plan: func(plan *Plan) {
			plan.Prepare.Apps[0].Resources.Volumes[0].Type = "bind"
		}},
		"interpolated mount target outside the data dir": {want: "\"${WAL_TARGET:-/backups}\" at an interpolated path", requiresNewRun: true, earlierPartialPause: true, plan: func(plan *Plan) {
			app := &plan.Prepare.Apps[0]
			app.Resources.Volumes = append(app.Resources.Volumes, preparer.VolumeResource{Service: "db", Type: "bind", Source: "/srv/wal", Target: "/backups", ReadWrite: true})
			for path, contents := range plan.BundleFiles {
				if filepath.Base(path) == "compose.yaml" {
					plan.BundleFiles[path] = []byte(strings.Replace(string(contents), "      - pgdata:/var/lib/postgresql/data\n", "      - pgdata:/var/lib/postgresql/data\n      - /srv/wal:${WAL_TARGET:-/backups}\n", 1))
				}
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			testApplyPreflightRefusal(t, tc)
		})
	}
}

type applyPreflightRefusalCase struct {
	transient           bool
	want                string
	requiresNewRun      bool
	earlierPartialPause bool
	resumeFails         bool
	composeCommand      string
	plan                func(*Plan)
}

func testApplyPreflightRefusal(t *testing.T, tc applyPreflightRefusalCase) {
	t.Helper()
	transient, want, requiresNewRun := tc.transient, tc.want, tc.requiresNewRun
	plan, _, staged := stagedRestoreFixture(t)
	plan.Steps = append([]Step{{Kind: StepCreateProject, App: "api"}, {Kind: StepCreateService, App: "api"}}, plan.Steps...)
	plan.ResumeFrom = 2
	plan.TargetIdentities = map[string]TargetIdentity{"api": {ProjectID: "proj-1", EnvironmentID: "env-1", ComposeID: "comp-1", ComposeAppName: "stack-1"}}
	plan.Prepare.Apps[0].Resources.Volumes = append(plan.Prepare.Apps[0].Resources.Volumes, preparer.VolumeResource{Service: "web", Type: "volume", SourceContainerID: "web-id"})
	if tc.plan != nil {
		tc.plan(&plan)
	}
	project := stagingProjectName(plan, "api", "db")
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
			"volume inspect " + staged.VolumeName:                                            ownedStagingVolumeInspect(plan, staged),
			"compose -p " + project:                                                          []byte("stg-id\n"),
			"inspect --type container stg-id":                                                []byte(`[{"Id":"stg-id","Name":"/` + project + `-db-1","Config":{"Env":["PGDATA=/pg/data"]},"State":{"Running":false,"Status":"created"},"Mounts":[{"Type":"volume","Name":"` + staged.VolumeName + `","Destination":"/var/lib/postgresql/data","RW":true}]}]`),
			"inspect --type container web-id":                                                []byte(`[{"Id":"web-id","Name":"/web","State":{"Running":true,"Status":"running"}}]`),
		},
	}
	if tc.earlierPartialPause {
		runner.outputs["inspect --type container web-id"] = []byte(`[{"Id":"web-id","Name":"/web","State":{"Running":false,"Status":"exited"}}]`)
		runner.outputs["start web-id"] = []byte("web-id\n")
		if tc.resumeFails {
			runner.outputErrs = map[string]error{"start web-id": errors.New("daemon unreachable")}
		}
		paused := &applyContext{plan: plan, cache: map[string]*appCache{}}
		paused.entry("api").SourcePauseRecorded = true
		paused.entry("api").SourcePausedContainers = []sourcePausedContainer{{ID: "web-id", Stopped: true}}
		if err := paused.persistSourcePauseState(); err != nil {
			t.Fatal(err)
		}
	}
	createEnvFile := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "secret" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/api/settings.getDokployVersion":
			_, _ = w.Write([]byte(`"v0.31.0"`))
		case "/api/project.one":
			_ = json.NewEncoder(w).Encode(Project{ProjectID: "proj-1", Environments: []ProjectEnvironment{{EnvironmentID: "env-1", Name: "production"}}})
		case "/api/compose.one":
			_ = json.NewEncoder(w).Encode(Compose{ComposeID: r.URL.Query().Get("composeId"), AppName: "stack-1", EnvironmentID: "env-1", CreateEnvFile: &createEnvFile, Command: tc.composeCommand})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var progress []StepProgress
	onProgress := func(p StepProgress) { progress = append(progress, p) }
	plan.OnProgress = &onProgress
	var docker dockerRunner = runner
	if transient {
		docker = &failingCreateRunner{fakeDockerRunner: runner}
	}
	client := &Client{BaseURL: server.URL, Token: "secret", HTTPClient: server.Client(), Docker: docker}

	err := client.Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "did not stop the source") {
		t.Fatalf("expected preflight refusal, got %v", err)
	}
	if errors.As(err, new(stagedRestorePreflightError)) != requiresNewRun {
		t.Fatalf("preflight error classified as layout refusal = %v, want %v: %v", !requiresNewRun, requiresNewRun, err)
	}
	if strings.Contains(err.Error(), "remains stopped after failed apply") != tc.resumeFails || strings.Contains(err.Error(), "was not durably recorded") {
		t.Fatalf("resume failure reported = %v, want %v: %v", strings.Contains(err.Error(), "remains stopped after failed apply"), tc.resumeFails, err)
	}
	var first, last *StepProgress
	resumed := false
	for i, p := range progress {
		if p.Step.Kind == StepResumeSource {
			if !tc.earlierPartialPause {
				t.Fatalf("unexpected resume progress after a preflight refusal: %#v", progress)
			}
			resumed = resumed || p.Status == StepStatusOK
		}
		if p.Index != 2 || p.Status == StepStatusStarted {
			continue
		}
		if first == nil {
			first = &progress[i]
		}
		last = &progress[i]
	}
	if first == nil || first.Step.Kind != StepPauseSource || first.Status != StepStatusError || first.RequiresNewRun != (requiresNewRun && !tc.earlierPartialPause) {
		t.Fatalf("the refusal must not claim a new run while owned source containers may still be stopped, got %#v", first)
	}
	if tc.resumeFails {
		if last == nil || last.Step.Kind != StepResumeSource || last.Status != StepStatusError || last.RequiresNewRun {
			t.Fatalf("a failed cleanup resume must stay the last record at the pause index so status keeps retrying the resume, got %#v", last)
		}
	} else if last == nil || last.Step.Kind != StepPauseSource || last.Status != StepStatusError || last.RequiresNewRun != requiresNewRun {
		t.Fatalf("the last record at the pause index must be the refusal (ledgers keep one record per index), got %#v", last)
	}
	if fakeOutputCalled(runner, "stop", "web-id") {
		t.Fatalf("preflight refusal must not stop the source, calls=%v", runner.outputArgs)
	}
	wantResumed := tc.earlierPartialPause && !tc.resumeFails
	if fakeOutputCalled(runner, "start", "web-id") != tc.earlierPartialPause || resumed != wantResumed {
		t.Fatalf("source restart after refusal = %v (resume progress %v), want %v/%v: calls=%v", fakeOutputCalled(runner, "start", "web-id"), resumed, tc.earlierPartialPause, wantResumed, runner.outputArgs)
	}
}

func TestReleaseTargetAuthorityPinsSkipsDokployAPIWithoutStagedState(t *testing.T) {
	_, plan, _, _ := stagedSyncFixture(t)
	plan.TargetIdentities = map[string]TargetIdentity{"api": {ProjectID: "project-1", EnvironmentID: "env-1", ComposeID: "compose-1", ComposeAppName: "stack-1"}}
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"ps -a --filter label=bort.staging-pin=true --filter label=bort.run-id=run1 --format {{.ID}}": nil,
	}}
	if err := (&Client{Docker: runner}).ReleaseStagingVolumePins(context.Background(), plan, true); err != nil {
		t.Fatalf("target finalization without staged state needed the Dokploy API: %v", err)
	}
}

func TestRequireSourceMountsStageDataDirRefusesAuxiliaryVolumeWithDefaultPGDATA(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "db", Type: "volume", Name: "pgdata", Target: "/var/lib/postgresql/data", ReadWrite: true},
		{Service: "db", Type: "volume", Name: "backups", Target: "/backups", ReadWrite: true},
	}
	staged := []stagedVolume{
		{Service: "db", Target: "/var/lib/postgresql/data", VolumeName: "bort-pgdata"},
		{Service: "db", Target: "/backups", VolumeName: "bort-backups"},
	}
	err := requireSourceMountsStageDataDir(app, "db", postgresDataDir(dockerContainer{}), staged)
	if !errors.Is(err, ErrNotImplemented) || !strings.Contains(err.Error(), `"backups" at /backups`) {
		t.Fatalf("default-PGDATA layout must refuse the auxiliary volume before pause_source, got %v", err)
	}
}

func TestReleaseTargetAuthorityWithoutPinSkipsLiveAttachmentChecks(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	plan.StagingTransferApps = []string{"api"}
	plan.TargetIdentities = map[string]TargetIdentity{"api": {ProjectID: "project-1", EnvironmentID: "env-1", ComposeID: "compose-1", ComposeAppName: "stack-1"}}
	state := &applyContext{plan: plan, cache: map[string]*appCache{}}
	if err := state.recordMigratedVolumeMount("api", migratedVolumeMount{Service: "web", Target: "/data", VolumeName: staged.VolumeName}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"ps -a --filter label=bort.staging-pin=true --filter label=bort.run-id=run1 --format {{.ID}}": nil,
	}}

	if err := (&Client{Docker: runner}).ReleaseStagingVolumePins(context.Background(), plan, true); err != nil {
		t.Fatalf("target finalization with no pin left depended on live target state: %v", err)
	}
	for _, args := range runner.outputArgs {
		if len(args) > 0 && args[0] != "ps" || strings.Contains(strings.Join(args, " "), "volume=") {
			t.Fatalf("target finalization with no pin left inspected live attachments: %v", runner.outputArgs)
		}
	}
}

func TestIncompleteStagingTransfersRequiresEveryStagedVolumeRecord(t *testing.T) {
	app, plan, firstStep, _ := stagedSyncFixture(t)
	app.Resources.Volumes = append(app.Resources.Volumes, preparer.VolumeResource{Service: "worker", Type: "volume", Name: "worker-data", Target: "/work"})
	plan = stagedPlan(t, app, plan.RunDir, Step{Kind: StepPauseSource, App: "api"}, firstStep, Step{Kind: StepSyncVolume, App: "api", Ref: "volume:worker -> /work"})
	plan.StagingTransferApps = []string{"api"}
	first, _ := stagedVolumeFor(plan, "api", app.Resources.Volumes[0])
	second, _ := stagedVolumeFor(plan, "api", app.Resources.Volumes[1])
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	for i, volume := range []stagedVolume{first, second} {
		if incomplete, err := IncompleteStagingTransfers(plan); err != nil || len(incomplete) != 1 || incomplete[0] != "api" {
			t.Fatalf("after %d of 2 volume records: incomplete = %v, %v; want [api]", i, incomplete, err)
		}
		if err := actx.recordMigratedVolumeMount("api", migratedVolumeMount{Service: volume.Service, Target: volume.Target, VolumeName: volume.VolumeName}); err != nil {
			t.Fatal(err)
		}
	}
	if incomplete, err := IncompleteStagingTransfers(plan); err != nil || len(incomplete) != 0 {
		t.Fatalf("fully recorded transfer reported incomplete: %v, %v", incomplete, err)
	}
}

func TestValidateTargetAuthorityRequiresHandoffEvidenceWhenPinIsMissing(t *testing.T) {
	_, plan, _, staged := stagedSyncFixture(t)
	plan.StagingTransferApps = []string{"api"}
	plan.RunID = "run-digest"
	staged, _ = stagedVolumeFor(plan, "api", plan.Prepare.Apps[0].Resources.Volumes[0])
	const targetID = "target-id"
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect " + staged.VolumeName:                                      ownedStagingVolumeInspect(plan, staged),
		"ps -a --filter label=com.docker.compose.project=stack-1 --format {{.ID}}": []byte(targetID + "\n"),
		"inspect --type container " + targetID:                                     []byte(`[{"Id":"` + targetID + `","Name":"/web","Config":{"Labels":{"com.docker.compose.service":"web","com.docker.compose.project":"stack-1"}},"State":{"Running":true,"Status":"running"},"Mounts":[{"Type":"volume","Name":"fresh-data","Destination":"/data","RW":true}]}]`),
	}, activeComposeProjects: map[string][]string{"stack-1": {targetID}}}
	client := targetAuthorityTestClient(t, runner, &plan)
	actx := &applyContext{plan: plan, cache: map[string]*appCache{}}
	if err := actx.recordMigratedVolumeMount("api", migratedVolumeMount{Service: "web", Target: "/data", VolumeName: staged.VolumeName}); err != nil {
		t.Fatal(err)
	}

	if err := client.ValidateStagingVolumePins(context.Background(), plan, true); err == nil || !strings.Contains(err.Error(), "fresh-data") {
		t.Fatalf("target validation accepted a missing pin without handoff evidence while the target mounts fresh state: %v", err)
	}
	plan.HandedOffApps = []string{"api"}
	if err := client.ValidateStagingVolumePins(context.Background(), plan, true); err != nil {
		t.Fatalf("target validation refused a recorded handoff whose pin was released: %v", err)
	}
}

func TestRequireSourceMountsStageDataDirRefusesUnmeasuredAnonymousVolume(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.Volumes = []preparer.VolumeResource{
		{Service: "db", Type: "volume", Name: "pgdata", Target: "/pgdata", ReadWrite: true},
		{Service: "db", Type: "volume", Name: strings.Repeat("ab", 32), Target: "/var/lib/postgresql/data", ReadWrite: true},
	}
	staged := []stagedVolume{{Service: "db", Target: "/pgdata", VolumeName: "bort-pgdata"}, {Service: "db", Target: "/var/lib/postgresql/data", VolumeName: "bort-anon"}}
	if err := requireSourceMountsStageDataDir(app, "db", "/pgdata", staged); err == nil || !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("anonymous volume outside PGDATA with unknown contents was accepted: %v", err)
	}
}
