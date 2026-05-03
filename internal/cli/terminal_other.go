//go:build !darwin && !freebsd && !linux && !netbsd && !openbsd

package cli

import (
	"bufio"
	"os"
	"strings"
)

func isTerminalFile(file *os.File) bool {
	return false
}

func readSecretLine(file *os.File) (string, error) {
	line, err := bufio.NewReader(file).ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}
