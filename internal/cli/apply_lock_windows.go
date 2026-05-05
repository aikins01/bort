//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

var errApplyAlreadyRunning = errors.New("live apply already running")

type applyLock struct {
	file *os.File
}

func acquireApplyLock(path string) (*applyLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, errApplyAlreadyRunning
		}
		return nil, err
	}
	if _, err := fmt.Fprintln(file, os.Getpid()); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &applyLock{file: file}, nil
}

func (l *applyLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	path := l.file.Name()
	_ = l.file.Close()
	_ = os.Remove(path)
}

func applyLockPIDRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	const (
		processQueryLimitedInformation = 0x1000
		stillActive                    = 259
	)
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	var code uint32
	if err := syscall.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	return code == stillActive
}
