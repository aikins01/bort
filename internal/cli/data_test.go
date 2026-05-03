package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunDataRecordsStrategyInState(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	var stdout, stderr bytes.Buffer
	if err := RunWithInput(context.Background(), []string{"data", "api", "postgres", "--migrate"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("data failed: %v\nstderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `Recorded data strategy "migrate" for api/postgres`) {
		t.Fatalf("unexpected output: %s", stdout.String())
	}

	state, err := readBortState(filepath.Join(workDir, ".bort", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, ok := state.Apps["api"].Data["postgres"]
	if !ok || store.Strategy != dataStrategyMigrate {
		t.Fatalf("unexpected data state: %#v", state.Apps)
	}
}

func TestRunDataRequiresExactlyOneStrategy(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	var stdout, stderr bytes.Buffer
	if err := RunWithInput(context.Background(), []string{"data", "api", "postgres"}, strings.NewReader(""), &stdout, &stderr); err == nil {
		t.Fatalf("expected error when no strategy provided")
	}
	stdout.Reset()
	stderr.Reset()
	if err := RunWithInput(context.Background(), []string{"data", "api", "postgres", "--recreate", "--migrate"}, strings.NewReader(""), &stdout, &stderr); err == nil {
		t.Fatalf("expected error when multiple strategies provided")
	}
}

func TestRunDataAcceptsAllStrategies(t *testing.T) {
	for _, tc := range []struct {
		flag string
		want string
	}{
		{"--recreate", dataStrategyRecreate},
		{"--migrate", dataStrategyMigrate},
		{"--managed", dataStrategyManaged},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			workDir := t.TempDir()
			t.Chdir(workDir)
			var stdout, stderr bytes.Buffer
			if err := RunWithInput(context.Background(), []string{"data", "api", "store", tc.flag}, strings.NewReader(""), &stdout, &stderr); err != nil {
				t.Fatalf("data %s failed: %v\nstderr:\n%s", tc.flag, err, stderr.String())
			}
			state, err := readBortState(filepath.Join(workDir, ".bort", "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if got := state.Apps["api"].Data["store"].Strategy; got != tc.want {
				t.Fatalf("expected strategy %q, got %q", tc.want, got)
			}
		})
	}
}
