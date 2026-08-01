//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

var errApplyAlreadyRunning = errors.New("live apply already running")

type applyLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func acquireApplyLock(path string) (*applyLock, error) {
	file, err := openLockFile(path, windows.OPEN_ALWAYS)
	if err != nil {
		return nil, err
	}
	lock := &applyLock{file: file}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &lock.overlapped); err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errApplyAlreadyRunning
		}
		return nil, err
	}
	if err := file.Truncate(0); err != nil {
		lock.Release()
		return nil, err
	}
	if _, err := file.Seek(0, 0); err != nil {
		lock.Release()
		return nil, err
	}
	if _, err := fmt.Fprintln(file, os.Getpid()); err != nil {
		lock.Release()
		return nil, err
	}
	return lock, nil
}

func (l *applyLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &l.overlapped)
	_ = l.file.Close()
	l.file = nil
}

func applyLockActive(path string) (bool, error) {
	file, err := openLockFile(path, windows.OPEN_EXISTING)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return false, nil
		}
		return false, err
	}
	defer file.Close()
	var overlapped windows.Overlapped
	err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return true, nil
		}
		return false, err
	}
	if err := windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped); err != nil {
		return false, err
	}
	return false, nil
}

func openLockFile(path string, disposition uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		disposition,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("lock file %s must be a single-link regular file and cannot be a reparse point", path)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("open lock file %s", path)
	}
	return file, nil
}
