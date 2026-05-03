//go:build !darwin && !freebsd && !linux && !netbsd && !openbsd

package cli

import "os"

func isTerminalFile(file *os.File) bool {
	return false
}
