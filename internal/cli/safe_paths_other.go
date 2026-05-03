//go:build windows

package cli

import (
	"fmt"
	"os"
)

// best-effort fallback for platforms that lack O_NOFOLLOW. Lstat first to
// reject pre-existing symlinks at the final component; this is non-atomic
// and TOCTOU-vulnerable but matches the historical bort behavior on
// platforms where atomic symlink-rejection isn't available.
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
