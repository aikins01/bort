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
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/preparer"
	"gopkg.in/yaml.v3"
)

type sequencedOutputRunner struct {
	fakeDockerRunner
	sequences map[string][][]byte
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
	if len(dbVolumes) != 2 || dbVolumes[0] != "pgdata:/var/lib/postgresql/data" {
		t.Fatalf("service mounts must be left untouched, got %#v", dbVolumes)
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
		"braced reference in db":     {compose: strings.Replace(original, "${DB_PASSWORD}", "${COMPOSE_PROJECT_NAME}_db", 1), refuse: true},
		"bare reference in db":       {compose: strings.Replace(original, "${DB_PASSWORD}", "$COMPOSE_PROJECT_NAME", 1), refuse: true},
		"longer variable name in db": {compose: strings.Replace(original, "${DB_PASSWORD}", "${COMPOSE_PROJECT_NAME_SUFFIX}", 1), refuse: false},
		"reference in other service": {compose: strings.Replace(original, "image: example/web\n", "image: example/web\n    environment:\n      APP: ${COMPOSE_PROJECT_NAME}\n", 1), refuse: false},
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

func TestSyncVolumeToStagingCreatesOwnedVolumeAndRecordsMount(t *testing.T) {
	app, plan, step, _ := stagedSyncFixture(t)
	plan.RunID = "run-digest"
	staged, ok := stagedVolumeFor(plan, "api", app.Resources.Volumes[0])
	if !ok {
		t.Fatal("volume not staged after setting the run ID")
	}
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect src-vol":                     []byte(`[{"Name":"src-vol"}]`),
			"volume inspect " + staged.VolumeName:        []byte(`[{"Name":"` + staged.VolumeName + `"}]`),
			"volume create":                              []byte(staged.VolumeName + "\n"),
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
			"volume inspect src-vol":                     []byte(`[{"Name":"src-vol"}]`),
			"volume inspect " + staged.VolumeName:        []byte(`[{"Name":"` + staged.VolumeName + `"}]`),
			"volume create":                              []byte(staged.VolumeName + "\n"),
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
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("other-run\n"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan}
	err := client.applySyncVolume(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "not owned by run \"run1\"") {
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
				"volume inspect " + staged.VolumeName: []byte(`[{"Name":"` + staged.VolumeName + `"}]`),
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

func TestSyncVolumeToStagingRefusesRunningSource(t *testing.T) {
	_, plan, step, staged := stagedSyncFixture(t)
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect src-vol":              []byte(`[{"Name":"src-vol"}]`),
			"volume inspect " + staged.VolumeName: []byte(`[{"Name":"` + staged.VolumeName + `"}]`),
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

type stagingVolumeStateRunner struct {
	*fakeDockerRunner
	volumeName string
	present    bool
}

func (r *stagingVolumeStateRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	out, err := r.fakeDockerRunner.Output(ctx, args...)
	key := strings.Join(args, " ")
	switch {
	case key == "volume rm -f "+r.volumeName:
		r.present = false
	case args[0] == "volume" && args[1] == "create" && args[len(args)-1] == r.volumeName:
		r.present = true
	case strings.HasPrefix(key, "volume inspect --format") && strings.HasSuffix(key, " "+r.volumeName) && !r.present:
		return nil, errors.New("Error response from daemon: get " + r.volumeName + ": no such volume")
	}
	return out, err
}

func TestRestoreDataStoreToStagingRunsIsolatedComposeProject(t *testing.T) {
	plan, step, staged := stagedRestoreFixture(t)
	project := stagingProjectName(plan, "api", "db")
	fake := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
			"volume rm -f " + staged.VolumeName:                                              []byte(staged.VolumeName + "\n"),
			"volume create":                                                                  []byte(staged.VolumeName + "\n"),
			"compose -p " + project:                                                          []byte("stg-id\n"),
			"inspect --type container stg-id":                                                []byte(`[{"Id":"stg-id","Name":"/` + project + `-db-1","Config":{"Env":["POSTGRES_USER=bob","POSTGRES_PASSWORD=s3cret","POSTGRES_DB=app"]},"State":{"Running":true,"Status":"running"},"Mounts":[{"Type":"volume","Name":"` + staged.VolumeName + `","Destination":"/var/lib/postgresql/data","RW":true}]}]`),
			"ps -a --filter volume=" + staged.VolumeName:                                     []byte(""),
			"exec stg-id rm -f":                                                              []byte(""),
		},
		runOutputs: map[string][]byte{
			"exec -i stg-id pg_restore -l": []byte("271; 1259 100 TABLE public widgets bob\n"),
		},
	}
	runner := &stagingVolumeStateRunner{fakeDockerRunner: fake, volumeName: staged.VolumeName, present: true}
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

	removed, recreated := false, false
	recreateIndex, lookupIndex := -1, -1
	for index, args := range runner.outputArgs {
		joined := strings.Join(args, " ")
		if joined == "volume rm -f "+staged.VolumeName {
			removed = true
		}
		if removed && !recreated && strings.HasPrefix(joined, "volume create ") && strings.HasSuffix(joined, " "+staged.VolumeName) {
			if !strings.Contains(joined, " --label bort.run-id="+stagingOwner(plan)+" ") {
				t.Fatalf("recreated staging volume must carry the run label, got %v", args)
			}
			recreated = true
			recreateIndex = index
		}
		if lookupIndex < 0 && strings.HasPrefix(joined, "compose -p "+project+" ") {
			lookupIndex = index
		}
	}
	if !removed || !recreated {
		t.Fatalf("staging volume must be recreated before restore, outputs=%v", runner.outputArgs)
	}
	if lookupIndex < 0 || lookupIndex < recreateIndex {
		t.Fatalf("staging container lookup must follow the volume recreate (recreate=%d lookup=%d), outputs=%v", recreateIndex, lookupIndex, runner.outputArgs)
	}
	mount, ok := actx.entry("api").MigratedVolumeMounts[migratedMountKey("db", "/var/lib/postgresql/data")]
	if !ok || mount.VolumeName != staged.VolumeName {
		t.Fatalf("expected staging mount recorded after restore, got %#v", actx.entry("api").MigratedVolumeMounts)
	}
}

func TestRestoreDataStoreToStagingRefusesToRemoveForeignVolume(t *testing.T) {
	plan, step, staged := stagedRestoreFixture(t)
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("other-run\n"),
			"volume rm -f " + staged.VolumeName:                                              []byte(staged.VolumeName + "\n"),
			"ps -a --filter volume=" + staged.VolumeName:                                     []byte(""),
		},
	}
	client := stagingCompatibleClient(t, runner, true, "")
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan, stagingEnvFormat: stagingEnvFormatKeepInterpolation}
	actx.entry("api").ComposeAppName = "stack-1"

	err := client.applyRestoreDataStore(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "not owned by run") {
		t.Fatalf("expected foreign volume refusal, got %v", err)
	}
	if fakeOutputCalled(runner, "volume", "rm", "-f", staged.VolumeName) {
		t.Fatalf("restore removed a volume it does not own: %#v", runner.outputArgs)
	}
}

func TestRestoreDataStoreToStagingStopsProjectAndSkipsRecordOnFailure(t *testing.T) {
	plan, step, staged := stagedRestoreFixture(t)
	project := stagingProjectName(plan, "api", "db")
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
			"volume rm -f " + staged.VolumeName:                                              []byte(staged.VolumeName + "\n"),
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
			"volume inspect " + staged.VolumeName: []byte(`[{"Name":"` + staged.VolumeName + `"}]`),
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
			"ps -a --filter volume=" + staged.VolumeName:                                     []byte("deployed-id\n"),
			"inspect --type container src-id":                                                stoppedSourceInspect("2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z"),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: plan}
	err := client.applySyncVolume(context.Background(), actx, step)
	if err == nil || !strings.Contains(err.Error(), "attached to container(s) deployed-id") {
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
			"volume inspect --format {{index .Labels \"bort.run-id\"}} " + staged.VolumeName: []byte("run1\n"),
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
					"volume rm -f " + staged.VolumeName:                                              []byte(staged.VolumeName + "\n"),
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

func TestValidatePlanReadyForLiveApplyRefusesSharedKeyBeforeAnyMutation(t *testing.T) {
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
	if err == nil || !strings.Contains(err.Error(), "compose volume shared is mounted by") || !strings.Contains(err.Error(), "choose a recreate or managed data store strategy or change the source compose before live apply") {
		t.Fatalf("expected shared key refusal before live apply starts, got %v", err)
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
					"compose -p " + project:           []byte("stg-id\n"),
					"inspect --type container stg-id": []byte(`[{"Id":"stg-id","Name":"/` + project + `-db-1","Config":{"Env":[` + tc.env + `]},"State":{"Running":false,"Status":"created"},"Mounts":[{"Type":"volume","Name":"` + staged.VolumeName + `","Destination":"/var/lib/postgresql/data","RW":true}]}]`),
					"inspect --type container web-id": []byte(`[{"Id":"web-id","Name":"/web","State":{"Running":true,"Status":"running"}}]`),
					"stop web-id":                     []byte("web-id\n"),
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
			"compose -p " + project:           []byte("stg-id\n"),
			"inspect --type container stg-id": []byte(`[{"Id":"stg-id","Name":"/` + project + `-db-1","Config":{"Env":["PGDATA=/pg/data"]},"State":{"Running":false,"Status":"created"},"Mounts":[{"Type":"volume","Name":"` + staged.VolumeName + `","Destination":"/var/lib/postgresql/data","RW":true}]}]`),
			"inspect --type container web-id": []byte(`[{"Id":"web-id","Name":"/web","State":{"Running":true,"Status":"running"}}]`),
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
	if strings.Contains(err.Error(), "resume source app") != tc.resumeFails || strings.Contains(err.Error(), "was not durably recorded") {
		t.Fatalf("resume failure reported = %v, want %v: %v", strings.Contains(err.Error(), "resume source app"), tc.resumeFails, err)
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
