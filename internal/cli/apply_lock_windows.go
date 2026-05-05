//go:build windows

package cli

import (
	"errors"
	"os"
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
	return pid > 0
}
