//go:build linux

package safepath

import "golang.org/x/sys/unix"

func renamePrivateFileNoReplace(dirFD int, from, to string) error {
	return unix.Renameat2(dirFD, from, dirFD, to, unix.RENAME_NOREPLACE)
}
