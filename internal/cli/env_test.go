package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunEnvRecordsValuesInState(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	var stdout, stderr bytes.Buffer
	if err := RunWithInput(context.Background(), []string{"env", "api", "API_TOKEN=secret", "DB_URL=postgres://x"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("env failed: %v\nstderr:\n%s", err, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Recorded 2 env value(s) for api") {
		t.Fatalf("expected confirmation message, got:\n%s", out)
	}
	if strings.Contains(out, "secret") || strings.Contains(out, "postgres") {
		t.Fatalf("env command echoed value back:\n%s", out)
	}

	statePath := filepath.Join(workDir, ".bort", "state.json")
	state, err := readBortState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	app, ok := state.Apps["api"]
	if !ok {
		t.Fatalf("expected app entry for api, got %#v", state.Apps)
	}
	if app.Env["API_TOKEN"] != "secret" || app.Env["DB_URL"] != "postgres://x" {
		t.Fatalf("unexpected env values: %#v", app.Env)
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected state.json mode 0600, got %o", info.Mode().Perm())
	}
}

func TestRunEnvMergesAcrossInvocations(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	var stdout, stderr bytes.Buffer
	if err := RunWithInput(context.Background(), []string{"env", "api", "FIRST=1"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := RunWithInput(context.Background(), []string{"env", "api", "SECOND=2"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	state, err := readBortState(filepath.Join(workDir, ".bort", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if state.Apps["api"].Env["FIRST"] != "1" || state.Apps["api"].Env["SECOND"] != "2" {
		t.Fatalf("expected merged env, got %#v", state.Apps["api"].Env)
	}
}

func TestRunEnvRequiresKeyValue(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	var stdout, stderr bytes.Buffer
	err := RunWithInput(context.Background(), []string{"env", "api"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatalf("expected error when no KEY=value supplied")
	}
}
