package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCleanRelativeArtifactRejectsVolumeName(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("volume names only meaningful on windows")
	}
	if _, err := cleanRelativeArtifact(`C:..\windows\system32`); err == nil {
		t.Fatalf("expected volume-named path to be rejected")
	}
}

func TestReadFileNoFollowRefusesSymlinkAtFinalComponent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("O_NOFOLLOW fallback is best-effort on windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "secret")
	if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readFileNoFollow(link); err == nil {
		t.Fatalf("expected readFileNoFollow to refuse symlink, got nil")
	}
}

func TestWriteFileNoFollowRefusesSymlinkAtFinalComponent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("O_NOFOLLOW fallback is best-effort on windows")
	}
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if err := writeFileNoFollow(link, []byte("redirected"), 0o600); err == nil {
		t.Fatalf("expected writeFileNoFollow to refuse symlink, got nil")
	}
	contents, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "untouched" {
		t.Fatalf("expected victim file untouched, got %q", contents)
	}
}

func TestContainedPathRejectsSymlinkParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	hop := filepath.Join(root, "hop")
	if err := os.Symlink(outside, hop); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(hop, "file.txt")
	if err := containedPath(root, target); err == nil {
		t.Fatalf("expected containedPath to detect symlink-parent escape, got nil")
	}
}

func TestReplaceComposeEnvExampleDoesNotCorruptSubstrings(t *testing.T) {
	yaml := strings.Join([]string{
		"services:",
		"  api:",
		"    env_file:",
		"      - .env.api.example",
		"      - .env.api.example.bak",
		"    # references .env.api.example",
		"    image: app:latest",
	}, "\n") + "\n"
	got := replaceComposeEnvExample(yaml, ".env.api.example", ".env.api")
	if !strings.Contains(got, "      - .env.api\n") {
		t.Fatalf("expected list-item to be rewritten, got:\n%s", got)
	}
	if !strings.Contains(got, ".env.api.example.bak") {
		t.Fatalf("expected .bak suffix to be preserved, got:\n%s", got)
	}
}

func TestReplaceComposeEnvExampleRewritesInlineValue(t *testing.T) {
	yaml := "    env_file: .env.api.example\n"
	got := replaceComposeEnvExample(yaml, ".env.api.example", ".env.api")
	if got != "    env_file: .env.api\n" {
		t.Fatalf("unexpected rewrite: %q", got)
	}
}
