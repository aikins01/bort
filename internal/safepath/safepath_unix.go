//go:build !windows

package safepath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func openPrivateDirNoFollow(path string, create bool) (*os.File, error) {
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	parentFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	if err := validatePrivateDirDescriptor(parentFD, string(filepath.Separator)); err != nil {
		_ = unix.Close(parentFD)
		return nil, err
	}
	currentPath := string(filepath.Separator)
	for _, part := range parts {
		if part == "" {
			continue
		}
		componentPath := filepath.Join(currentPath, part)
		nextFD, err := unix.Openat(parentFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if create && errors.Is(err, unix.ENOENT) {
			mkdirErr := unix.Mkdirat(parentFD, part, 0o700)
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(parentFD)
				return nil, fmt.Errorf("create private directory component %s: %w", componentPath, mkdirErr)
			}
			if mkdirErr == nil {
				if syncErr := unix.Fsync(parentFD); syncErr != nil {
					_ = unix.Close(parentFD)
					return nil, fmt.Errorf("sync private directory parent after creating %s: %w", componentPath, syncErr)
				}
			}
			nextFD, err = unix.Openat(parentFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if err != nil {
			_ = unix.Close(parentFD)
			return nil, fmt.Errorf("open private directory component %s without following links: %w", componentPath, err)
		}
		if err := validatePrivateDirDescriptor(nextFD, componentPath); err != nil {
			_ = unix.Close(parentFD)
			_ = unix.Close(nextFD)
			return nil, err
		}
		if err := unix.Close(parentFD); err != nil {
			_ = unix.Close(nextFD)
			return nil, err
		}
		parentFD = nextFD
		currentPath = componentPath
	}
	dir := os.NewFile(uintptr(parentFD), path)
	if dir == nil {
		_ = unix.Close(parentFD)
		return nil, fmt.Errorf("open private directory %s", path)
	}
	return dir, nil
}

func validatePrivateDirDescriptor(fd int, path string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&0o022 != 0 {
		return fmt.Errorf("private directory ancestor %s is writable by group or others", path)
	}
	if !trustedPrivateDirOwner(stat.Uid) {
		return fmt.Errorf("private directory ancestor %s is owned by untrusted uid %d", path, stat.Uid)
	}
	return nil
}

func trustedPrivateDirOwner(uid uint32) bool {
	euid := uint32(os.Geteuid())
	if uid == 0 || uid == euid {
		return true
	}
	if euid != 0 {
		return false
	}
	sudoUID, err := strconv.ParseUint(strings.TrimSpace(os.Getenv("SUDO_UID")), 10, 32)
	return err == nil && uid == uint32(sudoUID)
}

func createPrivateFileNoFollow(dir *os.File, name string, mode os.FileMode) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("create private file %s", name)
	}
	return file, nil
}

func writePrivateFileAtomicNoFollow(dir *os.File, name string, data []byte, mode os.FileMode) error {
	tmpName, err := privateTempName()
	if err != nil {
		return err
	}
	tmp, err := createPrivateFileNoFollow(dir, tmpName, mode)
	if err != nil {
		return err
	}
	removeTmp := true
	defer func() {
		_ = tmp.Close()
		if removeTmp {
			_ = removePrivateFileNoFollow(dir, tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := unix.Renameat(int(dir.Fd()), tmpName, int(dir.Fd()), name); err != nil {
		return err
	}
	removeTmp = false
	return unix.Fsync(int(dir.Fd()))
}

func removePrivateFileNoFollow(dir *os.File, name string) error {
	err := unix.Unlinkat(int(dir.Fd()), name, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

func syncPrivateDir(dir *os.File) error {
	return unix.Fsync(int(dir.Fd()))
}

func openNoFollowRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

func openNoFollowWrite(path string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
}

func syncParentDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
