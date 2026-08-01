//go:build !windows

package dokploy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func sourcePurgePathAbsentNoFollow(path string, allowPlatform bool) (bool, error) {
	if err := ValidateSourcePurgePath(path, allowPlatform); err != nil {
		return false, err
	}
	return pathAbsentNoFollow(path)
}

func pathAbsentNoFollow(path string) (bool, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) || cleaned == string(filepath.Separator) {
		return false, fmt.Errorf("refusing invalid absolute source path %q", path)
	}
	parts := strings.Split(strings.TrimPrefix(cleaned, string(filepath.Separator)), string(filepath.Separator))
	parentFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	parent := os.NewFile(uintptr(parentFD), string(filepath.Separator))
	if parent == nil {
		_ = unix.Close(parentFD)
		return false, fmt.Errorf("open filesystem root for source purge")
	}
	defer func() { _ = parent.Close() }()

	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false, fmt.Errorf("refusing invalid source purge path component %q", part)
		}
		nextFD, err := unix.Openat(int(parent.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				return true, nil
			}
			return false, fmt.Errorf("open source purge path component %s without following links: %w", part, err)
		}
		next := os.NewFile(uintptr(nextFD), part)
		if next == nil {
			_ = unix.Close(nextFD)
			return false, fmt.Errorf("open source purge path component %s", part)
		}
		if err := parent.Close(); err != nil {
			next.Close()
			return false, err
		}
		parent = next
	}
	return false, nil
}
