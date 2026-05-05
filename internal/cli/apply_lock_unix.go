//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
)

var errApplyAlreadyRunning = errors.New("live apply already running")

type applyLock struct {
	file *os.File
}

func acquireApplyLock(path string) (*applyLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errApplyAlreadyRunning
		}
		return nil, err
	}
	if err := file.Truncate(0); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, 0); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := fmt.Fprintln(file, strconv.Itoa(os.Getpid())); err != nil {
		file.Close()
		return nil, err
	}
	return &applyLock{file: file}, nil
}

func (l *applyLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
}

func applyLockPIDRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
