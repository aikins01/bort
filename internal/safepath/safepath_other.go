//go:build windows

package safepath

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func openPrivateDirNoFollow(path string, _ bool) (*os.File, error) {
	return nil, fmt.Errorf("secure private directory access for %s is unavailable on Windows", path)
}

func createPrivateFileNoFollow(*os.File, string, os.FileMode) (*os.File, error) {
	return nil, fmt.Errorf("secure private file creation is unavailable on Windows")
}

func openPrivateFileNoFollow(*os.File, string) (*os.File, error) {
	return nil, fmt.Errorf("secure private file reading is unavailable on Windows")
}

func readPrivateFile(path, name string) ([]byte, error) {
	root, held, err := openPrivateRootNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("private file %s is not regular", name)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("private file %s changed during read", name)
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	afterRead, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !samePrivateFileState(opened, afterRead) {
		return nil, fmt.Errorf("private file %s changed during read", name)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	verification, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	afterVerification, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !samePrivateFileState(afterRead, afterVerification) || !bytes.Equal(contents, verification) {
		return nil, fmt.Errorf("private file %s changed during read", name)
	}
	currentFile, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !currentFile.Mode().IsRegular() || !samePrivateFileState(afterVerification, currentFile) {
		return nil, fmt.Errorf("private file %s changed during read", name)
	}
	currentRoot, current, err := openPrivateRootNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer currentRoot.Close()
	if !os.SameFile(held, current) {
		return nil, fmt.Errorf("private directory path changed during operation")
	}
	return contents, nil
}

func openPrivateRootNoFollow(path string) (*os.Root, os.FileInfo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	volume := filepath.VolumeName(abs)
	if volume == "" {
		return nil, nil, fmt.Errorf("private directory path %s has no volume", path)
	}
	rootPath := volume + string(os.PathSeparator)
	rel, err := filepath.Rel(rootPath, abs)
	if err != nil {
		return nil, nil, err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, nil, fmt.Errorf("private directory path %s escapes volume root", path)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, nil, err
	}
	held, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, nil, err
	}
	if rel == "." {
		return root, held, nil
	}
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		before, err := root.Lstat(part)
		if err != nil {
			root.Close()
			return nil, nil, err
		}
		if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, nil, fmt.Errorf("private directory component %s is not a regular directory", part)
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			root.Close()
			return nil, nil, err
		}
		opened, err := next.Stat(".")
		if err != nil {
			next.Close()
			root.Close()
			return nil, nil, err
		}
		if !os.SameFile(before, opened) {
			next.Close()
			root.Close()
			return nil, nil, fmt.Errorf("private directory component %s changed while opening", part)
		}
		if err := root.Close(); err != nil {
			next.Close()
			return nil, nil, err
		}
		root = next
		held = opened
	}
	return root, held, nil
}

func writePrivateFileAtomicNoFollow(*os.File, string, []byte, os.FileMode) error {
	return fmt.Errorf("secure private file replacement is unavailable on Windows")
}

func removePrivateFileNoFollow(*os.File, string) error {
	return fmt.Errorf("secure private file removal is unavailable on Windows")
}

func syncPrivateDir(*os.File) error {
	return fmt.Errorf("secure private directory sync is unavailable on Windows")
}

func openNoFollowRead(path string) (*os.File, error) {
	if err := rejectFinalSymlink(path); err != nil {
		return nil, err
	}
	return os.Open(path)
}

func openNoFollowWrite(path string, mode os.FileMode) (*os.File, error) {
	if err := rejectFinalSymlink(path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
}

func rejectFinalSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write through symlink %s", path)
	}
	return nil
}

func syncParentDir(string) error {
	return nil
}
