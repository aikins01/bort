package safepath

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func ContainedPath(root, path string) error {
	rootResolved, err := evalExistingSymlinks(root)
	if err != nil {
		return err
	}
	pathResolved, err := evalExistingSymlinks(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootResolved, pathResolved)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("%s escapes %s", path, root)
	}
	return nil
}

type PrivateDir struct {
	path string
	dir  *os.File
}

func OpenPrivateDirNoFollow(path string) (*PrivateDir, error) {
	return openPrivateDirPathNoFollow(path, true)
}

func openPrivateDirPathNoFollow(path string, create bool) (*PrivateDir, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("private directory path is empty")
	}
	abs := path
	if !filepath.IsAbs(path) {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		cwd, err = filepath.EvalSymlinks(cwd)
		if err != nil {
			return nil, err
		}
		abs = filepath.Join(cwd, path)
	}
	abs = filepath.Clean(abs)
	dir, err := openPrivateDirNoFollow(abs, create)
	if err != nil {
		return nil, err
	}
	info, err := dir.Stat()
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		_ = dir.Close()
		return nil, fmt.Errorf("private directory %s has permissions %o; remove group and other access before retrying", path, info.Mode().Perm())
	}
	return &PrivateDir{path: abs, dir: dir}, nil
}

func (d *PrivateDir) Close() error {
	if d == nil || d.dir == nil {
		return nil
	}
	err := d.dir.Close()
	d.dir = nil
	return err
}

func (d *PrivateDir) CreateFile(name string, mode os.FileMode) (*os.File, error) {
	if err := validatePrivateFileName(name); err != nil {
		return nil, err
	}
	dir, err := d.openFile()
	if err != nil {
		return nil, err
	}
	return createPrivateFileNoFollow(dir, name, mode)
}

func (d *PrivateDir) ReadFile(name string) ([]byte, error) {
	if err := validatePrivateFileName(name); err != nil {
		return nil, err
	}
	dir, err := d.openFile()
	if err != nil {
		return nil, err
	}
	file, err := openPrivateFileNoFollow(dir, name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("private file %s is not regular", name)
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	afterRead, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !samePrivateFileState(info, afterRead) {
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
	current, err := openPrivateFileNoFollow(dir, name)
	if err != nil {
		return nil, err
	}
	defer current.Close()
	currentInfo, err := current.Stat()
	if err != nil {
		return nil, err
	}
	if !currentInfo.Mode().IsRegular() || !samePrivateFileState(afterVerification, currentInfo) {
		return nil, fmt.Errorf("private file %s changed during read", name)
	}
	return contents, nil
}

func samePrivateFileState(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime())
}

func ReadPrivateFile(path, name string) ([]byte, error) {
	if err := validatePrivateFileName(name); err != nil {
		return nil, err
	}
	return readPrivateFile(path, name)
}

func (d *PrivateDir) WriteFileAtomic(name string, data []byte, mode os.FileMode) error {
	if err := validatePrivateFileName(name); err != nil {
		return err
	}
	dir, err := d.openFile()
	if err != nil {
		return err
	}
	if err := writePrivateFileAtomicNoFollow(dir, name, data, mode); err != nil {
		return err
	}
	return d.ValidatePath()
}

func (d *PrivateDir) Remove(name string) error {
	if err := validatePrivateFileName(name); err != nil {
		return err
	}
	dir, err := d.openFile()
	if err != nil {
		return err
	}
	return removePrivateFileNoFollow(dir, name)
}

func (d *PrivateDir) Sync() error {
	dir, err := d.openFile()
	if err != nil {
		return err
	}
	return syncPrivateDir(dir)
}

func (d *PrivateDir) ValidatePath() error {
	dir, err := d.openFile()
	if err != nil {
		return err
	}
	current, err := openPrivateDirNoFollow(d.path, false)
	if err != nil {
		return fmt.Errorf("private directory path changed during operation: %w", err)
	}
	defer current.Close()
	heldInfo, err := dir.Stat()
	if err != nil {
		return err
	}
	currentInfo, err := current.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(heldInfo, currentInfo) {
		return fmt.Errorf("private directory path changed during operation")
	}
	return nil
}

func (d *PrivateDir) openFile() (*os.File, error) {
	if d == nil || d.dir == nil {
		return nil, fmt.Errorf("private directory is closed")
	}
	return d.dir, nil
}

func validatePrivateFileName(name string) error {
	if strings.TrimSpace(name) == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("private file name %q must be a base name", name)
	}
	return nil
}

func privateTempName() (string, error) {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return ".bort-tmp-" + hex.EncodeToString(suffix[:]), nil
}

func ReadFileNoFollow(path string) ([]byte, error) {
	f, err := openNoFollowRead(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func WriteFileNoFollow(path string, data []byte, mode os.FileMode) error {
	f, err := openNoFollowWrite(path, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}

func WriteFileAtomicNoFollow(path string, data []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to overwrite symlink %s", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".bort-tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncParentDir(path)
}

func evalExistingSymlinks(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(abs)
	suffix := ""
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			if suffix == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, suffix), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return current, nil
		}
		suffix = filepath.Join(filepath.Base(current), suffix)
		current = parent
	}
}
