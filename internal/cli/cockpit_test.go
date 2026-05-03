package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/preparer"
)

func TestFillEnvFileWritesPrivateValuesWithoutPrintingThem(t *testing.T) {
	workDir := t.TempDir()
	envPath := filepath.Join(workDir, ".env.api")
	templatePath := filepath.Join(workDir, ".env.api.example")
	if err := os.WriteFile(envPath, []byte("API_TOKEN=\nOTHER=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writePipe.WriteString("secret value\n"); err != nil {
		t.Fatal(err)
	}
	if err := writePipe.Close(); err != nil {
		t.Fatal(err)
	}
	defer readPipe.Close()

	var stdout bytes.Buffer
	if err := fillEnvFile(&stdout, readPipe, envHint{Path: filepath.ToSlash(envPath), TemplatePath: filepath.ToSlash(templatePath), MissingKeys: []string{"API_TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "secret value") {
		t.Fatalf("env prompt printed the secret value:\n%s", stdout.String())
	}

	contents, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "API_TOKEN=\"secret value\"") || !strings.Contains(string(contents), "OTHER=old") {
		t.Fatalf("unexpected env file contents: %q", string(contents))
	}
	template, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(template), "secret value") || !strings.Contains(string(template), "API_TOKEN=") {
		t.Fatalf("unexpected env template contents: %q", string(template))
	}
	assertFileMode(t, envPath, 0o600)
	assertFileMode(t, templatePath, 0o600)
}

func TestFillEnvFileRedactedModeCreatesPrivateFileWithoutChangingTemplateValues(t *testing.T) {
	workDir := t.TempDir()
	envPath := filepath.Join(workDir, ".env.api")
	templatePath := filepath.Join(workDir, ".env.api.example")
	if err := os.WriteFile(templatePath, []byte("API_TOKEN=\nOTHER=\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	readPipe := secretInput(t, "redacted-secret\n")
	var stdout bytes.Buffer
	if err := fillEnvFile(&stdout, readPipe, envHint{Path: filepath.ToSlash(envPath), TemplatePath: filepath.ToSlash(templatePath), MissingKeys: []string{"API_TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "redacted-secret") {
		t.Fatalf("env prompt printed the secret value:\n%s", stdout.String())
	}

	contents, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "API_TOKEN=redacted-secret") || !strings.Contains(string(contents), "OTHER=") {
		t.Fatalf("unexpected private env file contents: %q", string(contents))
	}
	template, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(template) != "API_TOKEN=\nOTHER=\n" {
		t.Fatalf("expected template to remain value-less, got %q", string(template))
	}
	assertFileMode(t, envPath, 0o600)
	assertFileMode(t, templatePath, 0o600)
}

func TestSafeRunArtifactPathRejectsEscapes(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "run")
	path, err := safeRunArtifactPath(runDir, "nested/progress.json")
	if err != nil {
		t.Fatalf("expected nested progress path to be allowed: %v", err)
	}
	if err := containedPath(runDir, path); err != nil {
		t.Fatalf("expected %s to remain inside %s: %v", path, runDir, err)
	}

	for _, artifact := range []string{"", "../progress.json", "nested/../../progress.json", filepath.Join(t.TempDir(), "progress.json")} {
		if _, err := safeRunArtifactPath(runDir, artifact); err == nil {
			t.Fatalf("expected artifact path %q to be rejected", artifact)
		}
	}
}

func TestReadRunProgressIgnoresStaleOrMismatchedProgress(t *testing.T) {
	workDir := t.TempDir()
	runDir := filepath.Join(workDir, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	progressPath := filepath.Join(runDir, "progress.json")
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	run := migrationRun{Name: "marketmap", RunDir: filepath.ToSlash(runDir), UpdatedAt: now, DryRun: true}
	decision := runDecision{
		Kind: "cutover",
		Items: []runDecisionItem{{
			Stage:       "cutover",
			App:         "api",
			Code:        "route.confirm",
			ResourceRef: "route:api.example.com",
			Message:     "confirm cutover route",
			Readiness:   preparer.ReadinessNeedsDecision,
		}},
	}

	stale := markDecisionDone(emptyRunProgress(run), decision, progressStatusResolved, "old", now.Add(-time.Minute))
	if err := writeRunProgress(progressPath, stale); err != nil {
		t.Fatal(err)
	}
	progress, err := readRunProgress(progressPath, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress.Decisions) != 0 {
		t.Fatalf("expected stale progress to be ignored, got %#v", progress.Decisions)
	}

	fresh := markDecisionDone(emptyRunProgress(run), decision, progressStatusResolved, "new", now.Add(time.Minute))
	if err := writeRunProgress(progressPath, fresh); err != nil {
		t.Fatal(err)
	}
	progress, err = readRunProgress(progressPath, run)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Decisions["cutover"].Status != progressStatusResolved {
		t.Fatalf("expected fresh matching progress to be loaded, got %#v", progress.Decisions)
	}

	mismatchedName := fresh
	mismatchedName.RunName = "other"
	if err := writeRunProgress(progressPath, mismatchedName); err != nil {
		t.Fatal(err)
	}
	progress, err = readRunProgress(progressPath, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress.Decisions) != 0 {
		t.Fatalf("expected wrong-run-name progress to be ignored, got %#v", progress.Decisions)
	}

	mismatchedDir := fresh
	mismatchedDir.RunDir = filepath.ToSlash(filepath.Join(workDir, "other-run"))
	if err := writeRunProgress(progressPath, mismatchedDir); err != nil {
		t.Fatal(err)
	}
	progress, err = readRunProgress(progressPath, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress.Decisions) != 0 {
		t.Fatalf("expected wrong-run-dir progress to be ignored, got %#v", progress.Decisions)
	}
}

func TestRunContinueWithoutInteractiveStdinJustPrintsCockpit(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	bundleDir := filepath.Join(workDir, "bort-bundle")
	writeTestBundle(t, bundleDir, manifest.Manifest{
		Source: manifest.Source{Platform: "docker"},
		Apps: []manifest.App{
			{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api:latest"}}, Routes: []manifest.Route{{Host: "api.example.com", ServiceName: "api"}}},
		},
	})
	runCommand(t, runMigrate, []string{"--bundle", bundleDir, "--run", "marketmap", "--observation-window", "0", "--rollback-window", "0"})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := RunWithInput(context.Background(), []string{"continue"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("continue failed: %v\nstderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Run: marketmap") {
		t.Fatalf("expected cockpit header, got:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "How do you want to handle it?") {
		t.Fatalf("did not expect interactive prompt without an interactive stdin:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(workDir, ".bort", "runs", "marketmap", "progress.json")); err == nil {
		t.Fatalf("did not expect progress.json to be written without any local progress")
	}
}

func secretInput(t *testing.T, value string) *os.File {
	t.Helper()
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writePipe.WriteString(value); err != nil {
		readPipe.Close()
		writePipe.Close()
		t.Fatal(err)
	}
	if err := writePipe.Close(); err != nil {
		readPipe.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { readPipe.Close() })
	return readPipe
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("expected %s mode %o, got %o", path, want, got)
	}
}
