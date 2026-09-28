//go:build !windows

package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aikins01/bort/internal/safepath"
)

func dokployInstallerSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

func configureDokployInstallerCommand(cmd *exec.Cmd, operationLock, authSecret *os.File) int {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	nextFD := 3
	if operationLock != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, operationLock)
		nextFD++
	}
	authSecretFD := 0
	if authSecret != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, authSecret)
		authSecretFD = nextFD
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
	return authSecretFD
}

func prepareDokployAuthSecretEscrow(path string) (*os.File, string, error) {
	dir, err := safepath.OpenExistingPrivateDirNoFollow(filepath.Dir(path))
	if err != nil {
		return nil, "", fmt.Errorf("open authentication-secret escrow directory: %w", err)
	}
	defer dir.Close()
	name := filepath.Base(path)
	file, err := dir.OpenFile(name)
	if errors.Is(err, os.ErrNotExist) {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return nil, "", fmt.Errorf("generate authentication-secret escrow: %w", err)
		}
		contents := append([]byte(hex.EncodeToString(secret[:])), '\n')
		if err := dir.WriteFileAtomicNew(name, contents, 0o600); err != nil {
			return nil, "", fmt.Errorf("create authentication-secret escrow: %w", err)
		}
		file, err = dir.OpenFile(name)
	}
	if err != nil {
		return nil, "", fmt.Errorf("open authentication-secret escrow: %w", err)
	}
	valid := false
	defer func() {
		if !valid {
			_ = file.Close()
		}
	}()
	before, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return nil, "", fmt.Errorf("authentication-secret escrow must be a single-link mode 0600 regular file owned by uid %d", os.Geteuid())
	}
	if before.Size() != 65 {
		return nil, "", fmt.Errorf("authentication-secret escrow is %d bytes, not the 65 bytes of one 64-lowercase-hex key and newline", before.Size())
	}
	contents, err := io.ReadAll(io.LimitReader(file, 66))
	if err != nil {
		return nil, "", err
	}
	if len(contents) != 65 || contents[64] != '\n' || strings.ToLower(string(contents[:64])) != string(contents[:64]) {
		return nil, "", fmt.Errorf("authentication-secret escrow must contain exactly one 64-lowercase-hex key and newline")
	}
	if decoded, err := hex.DecodeString(string(contents[:64])); err != nil || len(decoded) != 32 {
		return nil, "", fmt.Errorf("authentication-secret escrow must contain exactly one 64-lowercase-hex key and newline")
	}
	after, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		return nil, "", fmt.Errorf("authentication-secret escrow changed during validation")
	}
	if err := dir.ValidatePath(); err != nil {
		return nil, "", err
	}
	snapshot, writer, err := os.Pipe()
	if err != nil {
		return nil, "", err
	}
	written, writeErr := writer.Write(contents)
	closeErr := writer.Close()
	if writeErr != nil || written != len(contents) || closeErr != nil {
		_ = snapshot.Close()
		var shortWriteErr error
		if written != len(contents) {
			shortWriteErr = io.ErrShortWrite
		}
		return nil, "", errors.Join(writeErr, closeErr, shortWriteErr)
	}
	digest := sha256.Sum256(contents)
	if err := file.Close(); err != nil {
		_ = snapshot.Close()
		return nil, "", err
	}
	valid = true
	return snapshot, hex.EncodeToString(digest[:]), nil
}

func verifyDokployAuthSecretEscrow(path, expectedDigest string) error {
	dir, err := safepath.OpenExistingPrivateDirNoFollow(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("reopen authentication-secret escrow directory after installation: %w", err)
	}
	defer dir.Close()
	name := filepath.Base(path)
	file, err := dir.OpenFile(name)
	if err != nil {
		return fmt.Errorf("reopen authentication-secret escrow after installation: %w", err)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || before.Size() != 65 {
		return fmt.Errorf("authentication-secret escrow security properties changed during installation; installation recovery remains required")
	}
	contents, err := io.ReadAll(io.LimitReader(file, 66))
	if err != nil {
		return err
	}
	if len(contents) != 65 {
		return fmt.Errorf("authentication-secret escrow changed during post-install verification; installation recovery remains required")
	}
	after, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("authentication-secret escrow changed during post-install verification; installation recovery remains required")
	}
	if err := dir.ValidatePath(); err != nil {
		return err
	}
	current, err := dir.OpenFile(name)
	if err != nil {
		return fmt.Errorf("rebind authentication-secret escrow path after installation: %w", err)
	}
	defer current.Close()
	currentInfo, err := current.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(after, currentInfo) {
		return fmt.Errorf("authentication-secret escrow path changed during installation; installation recovery remains required")
	}
	currentStat, ok := currentInfo.Sys().(*syscall.Stat_t)
	if !ok || currentInfo.Mode().Perm() != 0o600 || currentStat.Uid != uint32(os.Geteuid()) || currentStat.Nlink != 1 {
		return fmt.Errorf("authentication-secret escrow security properties changed during installation; installation recovery remains required")
	}
	digest := sha256.Sum256(contents)
	if hex.EncodeToString(digest[:]) != expectedDigest {
		return fmt.Errorf("authentication-secret escrow changed during installation; installation recovery remains required")
	}
	return nil
}
