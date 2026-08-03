package safepath

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadPrivateFileDoesNotCreateMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "nested")
	if _, err := ReadPrivateFile(path, "manifest.json"); err == nil {
		t.Fatal("expected missing private file read to fail")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private read created missing directory: %v", err)
	}
}
