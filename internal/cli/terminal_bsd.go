//go:build darwin || freebsd || netbsd || openbsd

package cli

import (
	"bufio"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

func isTerminalFile(file *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&termios)), 0, 0, 0)
	return errno == 0
}

func readSecretLine(file *os.File) (string, error) {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&termios)), 0, 0, 0)
	if errno != 0 {
		line, err := bufio.NewReader(file).ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}
	old := termios
	termios.Lflag &^= syscall.ECHO
	_, _, errno = syscall.Syscall6(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(&termios)), 0, 0, 0)
	if errno != 0 {
		return "", errno
	}
	defer syscall.Syscall6(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(&old)), 0, 0, 0)
	line, err := bufio.NewReader(file).ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}
