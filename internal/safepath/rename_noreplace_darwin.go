//go:build darwin

package safepath

import "golang.org/x/sys/unix"

func renamePrivateFileNoReplace(dirFD int, from, to string) error {
	return unix.RenameatxNp(dirFD, from, dirFD, to, unix.RENAME_EXCL)
}
