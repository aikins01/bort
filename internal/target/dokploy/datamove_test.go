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

	"github.com/aikins01/bort/internal/preparer"
)

type fakeDockerRunner struct {
	outputs map[string][]byte
	runs    []fakeDockerRun
	runErr  error
}

type fakeDockerRun struct {
	Args   []string
	Stdin  []byte
	Output string
}

func (f *fakeDockerRunner) Output(_ context.Context, args ...string) ([]byte, error) {
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
	run := fakeDockerRun{Args: append([]string{}, args...)}
	if stdin != nil {
		buf, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		run.Stdin = buf
	}
	if stdout != nil {
		_, _ = stdout.Write([]byte("dump-bytes"))
		run.Output = "dump-bytes"
	}
	f.runs = append(f.runs, run)
	return f.runErr
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
			"volume inspect src-vol": []byte(`[{"Name":"src-vol"}]`),
			"volume inspect dst-vol": []byte(`[{"Name":"dst-vol"}]`),
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
