//go:build !windows

package safepath

import (
	"os"
	"syscall"
)

func openNoFollowRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

func openNoFollowWrite(path string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
}
