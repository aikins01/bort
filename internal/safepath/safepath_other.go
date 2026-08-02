//go:build windows

package safepath

import (
	"fmt"
	"os"
)

func openPrivateDirNoFollow(path string, _ bool) (*os.File, error) {
	return nil, fmt.Errorf("secure private directory access for %s is unavailable on Windows", path)
}

func createPrivateFileNoFollow(*os.File, string, os.FileMode) (*os.File, error) {
	return nil, fmt.Errorf("secure private file creation is unavailable on Windows")
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
