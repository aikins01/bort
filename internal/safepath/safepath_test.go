package safepath

import (
	"bytes"
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

func TestWriteFileAtomicNewDoesNotReplaceExistingFile(t *testing.T) {
	dirPath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dirPath, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := OpenPrivateDirNoFollow(dirPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := dir.WriteFileAtomicNew("secret", []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dir.WriteFileAtomicNew("secret", []byte("second"), 0o600); err == nil {
		t.Fatal("atomic new-file write replaced an existing file")
	}
	contents, err := os.ReadFile(filepath.Join(dirPath, "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, []byte("first")) {
		t.Fatalf("existing file changed after no-replace write: %q", contents)
	}
}
