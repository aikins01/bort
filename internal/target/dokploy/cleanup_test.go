package dokploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupStalePlatformProjectsBacksUpThenDeletesMetadata(t *testing.T) {
	backupDir := t.TempDir()
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"ps --format {{.Names}}": []byte("dokploy-postgres.1.task\n"),
		},
		runOutputs: map[string][]byte{
			"exec -i dokploy-postgres.1.task psql -U dokploy -d dokploy -v ON_ERROR_STOP=1 -At -F |": []byte("BEGIN\ndeleted|proxy|p1\nCOMMIT\n"),
		},
	}
	client := &Client{Docker: runner}

	result, err := client.CleanupStalePlatformProjects(context.Background(), StalePlatformCleanupOptions{
		ProjectNames: []string{"proxy", "source", "proxy"},
		BackupDir:    backupDir,
		BackupPrefix: "cleanup-test",
	})
	if err != nil {
		t.Fatalf("CleanupStalePlatformProjects: %v", err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0].Name != "proxy" || result.Deleted[0].ProjectID != "p1" {
		t.Fatalf("unexpected deleted projects: %#v", result.Deleted)
	}
	if !strings.HasPrefix(result.BackupPath, filepath.Join(backupDir, "cleanup-test-")) {
		t.Fatalf("unexpected backup path: %s", result.BackupPath)
	}
	backup, err := os.ReadFile(result.BackupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != "dump-bytes" {
		t.Fatalf("expected pg_dump backup contents, got %q", string(backup))
	}
	if len(runner.runs) != 2 {
		t.Fatalf("expected pg_dump and psql runs, got %#v", runner.runs)
	}
	if got := strings.Join(runner.runs[0].Args, " "); !strings.Contains(got, "pg_dump") {
		t.Fatalf("expected backup before delete, got first run %q", got)
	}
	if got := strings.Join(runner.runs[1].Args, " "); !strings.Contains(got, "psql") {
		t.Fatalf("expected psql delete second, got %q", got)
	}
	sql := string(runner.runs[1].Stdin)
	for _, want := range []string{"('proxy')", "('source')", "refusing to delete stale platform projects with domains", "delete from project"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("expected cleanup sql to contain %q, got:\n%s", want, sql)
		}
	}
}

func TestFindDokployPostgresContainerRequiresDokployPostgres(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{"ps --format {{.Names}}": []byte("postgres\n")}}
	if _, err := findDokployPostgresContainer(context.Background(), runner); err == nil {
		t.Fatal("expected missing dokploy postgres to error")
	}
}
