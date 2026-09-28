package cli

import (
	"errors"
	"time"
)

const (
	applyLockProbeGrace    = 200 * time.Millisecond
	applyLockProbeInterval = 10 * time.Millisecond
)

func acquireApplyLock(path string) (*applyLock, error) {
	deadline := time.Now().Add(applyLockProbeGrace)
	for {
		lock, err := tryAcquireApplyLock(path)
		if !errors.Is(err, errApplyAlreadyRunning) || time.Now().After(deadline) {
			return lock, err
		}
		time.Sleep(applyLockProbeInterval)
	}
}
