//go:build !windows

package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/target/dokploy"
)

func TestInstallerCancellationKillsDescendantProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo $!; wait")
	configureDokployInstallerCommand(cmd, nil, nil)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatalf("read installer child pid: %v", scanner.Err())
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("installer process group did not stop after cancellation")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("installer descendant %d remained after cancellation: %v", childPID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInstallerDescendantRetainsHostLockAfterAbruptParentExit(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "host.lock")
	lock, err := acquireApplyLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.CommandContext(context.Background(), "sh", "-c", `sleep 30 >/dev/null 2>&1 & echo $! > "$1"`, "test", pidPath)
	configureDokployInstallerCommand(cmd, lock.file, nil)
	if err := cmd.Run(); err != nil {
		lock.Release()
		t.Fatal(err)
	}
	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		lock.Release()
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		lock.Release()
		t.Fatal(err)
	}
	if err := lock.file.Close(); err != nil {
		t.Fatal(err)
	}
	lock.file = nil
	defer syscall.Kill(childPID, syscall.SIGKILL)
	active, err := applyLockActive(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("installer descendant did not retain the host lock after the parent descriptor closed")
	}
	if err := syscall.Kill(childPID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		active, err = applyLockActive(lockPath)
		if err != nil {
			t.Fatal(err)
		}
		if !active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("host lock remained held after the installer descendant exited")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInitTargetInstallTimeoutBlocksHostMutationsUntilRecovery(t *testing.T) {
	resetDokployTrafficOwner(t)
	t.Cleanup(func() { _ = clearDokployInstallationRecoveryRequired() })
	t.Setenv("SUDO_UID", "501")
	t.Setenv(envDokployAuthBackup, filepath.Join(t.TempDir(), "dokploy-auth-secret"))
	installer := &blockingDokployInstaller{}
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		installer:      installer,
		installTimeout: 10 * time.Millisecond,
		newClient: func(string) *dokploy.Client {
			t.Fatal("Dokploy client was created after installer timeout")
			return nil
		},
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}
	t.Setenv(envCoolifyAdminPwd, "right-password")
	err := runInitTargetWith(context.Background(), []string{
		"--install",
		"--coolify-email", "admin@example.com",
		"--dokploy-url", "http://127.0.0.1:3030",
		"--name", "Recovery Admin",
		"--api-key-name", "recovery key",
	}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "install dokploy timed out after 10ms") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected bounded installer timeout, got %v", err)
	}
	initErr := err
	if installer.calls != 1 {
		t.Fatalf("installer calls=%d, want 1", installer.calls)
	}
	lock, lockErr := acquireDokployLiveOperationLock()
	if lock != nil {
		lock.Release()
	}
	recoveryCommand, found, err := dokployInstallationRecoveryCommand()
	if err != nil || !found || !strings.Contains(recoveryCommand, "--install-port 3030") {
		t.Fatalf("recovery command was not recorded: found=%t err=%v command=%q", found, err, recoveryCommand)
	}
	if !strings.Contains(initErr.Error(), "run `"+recoveryCommand+"` to reconcile it") {
		t.Fatalf("installer timeout error did not name the recovery command: %v", initErr)
	}
	if !errors.Is(lockErr, errDokployInstallRecoveryRequired) || !strings.Contains(lockErr.Error(), "run `"+recoveryCommand+"`") {
		t.Fatalf("installer timeout did not block later host mutations with the recovery command: %v", lockErr)
	}
	if active, err := dokployLiveOperationActive(); active || !errors.Is(err, errDokployInstallRecoveryRequired) || !strings.Contains(err.Error(), "run `"+recoveryCommand+"`") {
		t.Fatalf("installer recovery state was not visible after timeout: active=%t err=%v", active, err)
	}
	t.Chdir(t.TempDir())
	for _, test := range []struct {
		name string
		run  func(*strings.Builder) error
	}{
		{name: "guide", run: func(output *strings.Builder) error {
			return runGuide(context.Background(), strings.NewReader(""), output, io.Discard)
		}},
		{name: "status", run: func(output *strings.Builder) error {
			return runStatus(context.Background(), nil, output, io.Discard)
		}},
		{name: "next", run: func(output *strings.Builder) error {
			return runNext(context.Background(), nil, output, io.Discard)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output strings.Builder
			if err := test.run(&output); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "Dokploy installation recovery required") || !strings.Contains(output.String(), "sudo bort init-target --install") || strings.Contains(output.String(), "no current migration run") {
				t.Fatalf("standalone recovery output omitted the exact retry command: %s", output.String())
			}
		})
	}
	blockedRun := loadedMigrationRun{Run: migrationRun{Name: "blocked", RunDir: t.TempDir(), Target: "dokploy"}}
	if phase := migrationRunPhase(blockedRun); phase != "install-recovery" {
		t.Fatalf("installer recovery marker produced phase %q", phase)
	}
	if next := nextSafeStep(blockedRun, nil); !strings.Contains(next.Action, "sudo bort init-target --install") || !strings.Contains(next.Action, "--dokploy-version v0.30.7@sha256:") || !strings.Contains(next.Action, "--auth-secret-backup") || !strings.Contains(next.Action, "--name 'Recovery Admin'") || !strings.Contains(next.Action, "--api-key-name 'recovery key'") {
		t.Fatalf("installer recovery marker produced unsafe next step: %#v", next)
	}
	var cockpit strings.Builder
	writeAppFirstCockpit(&cockpit, blockedRun)
	if !strings.Contains(cockpit.String(), "sudo bort init-target --install") {
		t.Fatalf("installer recovery cockpit omitted the persisted command: %s", cockpit.String())
	}
	recoveryLock, err := acquireDokployInstallRecoveryLock()
	if err != nil {
		t.Fatalf("installer recovery could not reacquire the host lock: %v", err)
	}
	recoveryPath, err := dokployInstallationRecoveryPath()
	if err != nil {
		recoveryLock.Release()
		t.Fatal(err)
	}
	recovery, found, err := readDokployInstallationRecoveryMarker(recoveryPath)
	if err != nil || !found {
		recoveryLock.Release()
		t.Fatalf("read installer recovery identity: found=%t err=%v", found, err)
	}
	if recovery.CommandPrefix != "sudo bort" || recovery.TargetURL != "http://127.0.0.1:3030" || recovery.HostPort != "3030" || recovery.AddressPool != "auto" || recovery.DokployVersion != defaultDokployVersion+"@"+defaultDokployDigest || recovery.EndpointMode != "vip" || recovery.ACMEEmail != "admin@example.com" || recovery.AuthSecretBackup == "" || recovery.AdminName != "Recovery Admin" || recovery.APIKeyName != "recovery key" || !strings.Contains(recovery.Command, "--dokploy-url http://127.0.0.1:3030") {
		recoveryLock.Release()
		t.Fatalf("recovery marker omitted immutable install options: %#v", recovery)
	}
	if err := markDokployInstallationRecoveryRequired(recovery); err != nil {
		recoveryLock.Release()
		t.Fatalf("same installer options did not resume recovery: %v", err)
	}
	different := newDokployInstallationRecovery(recovery.TargetURL, dokployInstallOptions{
		HostPort:         "3031",
		AddrPool:         recovery.AddressPool,
		Version:          recovery.DokployVersion,
		EndpointMode:     recovery.EndpointMode,
		ACMEEmail:        recovery.ACMEEmail,
		AuthSecretBackup: recovery.AuthSecretBackup,
		AdminName:        recovery.AdminName,
		APIKeyName:       recovery.APIKeyName,
	})
	if err := markDokployInstallationRecoveryRequired(different); err == nil || !strings.Contains(err.Error(), "exact original command: "+recovery.Command) {
		recoveryLock.Release()
		t.Fatalf("different installer options were allowed to replace recovery state: %v", err)
	}
	if err := clearDokployInstallationRecoveryRequired(); err != nil {
		recoveryLock.Release()
		t.Fatal(err)
	}
	recoveryLock.Release()
	lock, lockErr = acquireDokployLiveOperationLock()
	if lockErr != nil {
		t.Fatalf("reconciled installer state did not release host mutations: %v", lockErr)
	}
	lock.Release()
}

func TestDokployInstallationRecoveryCommandQuotesImmutableOptions(t *testing.T) {
	t.Setenv("SUDO_UID", "501")
	opts := dokployInstallOptions{
		HostPort:         "3030",
		AddrPool:         "auto",
		Version:          defaultDokployVersion + "@" + defaultDokployDigest,
		EndpointMode:     "dnsrr",
		ACMEEmail:        "admin@example.com",
		AuthSecretBackup: "/private/recovery backups/admin's secret",
		AdminName:        "Admin O'Neil",
		APIKeyName:       "bort recovery key",
	}
	recovery := newDokployInstallationRecovery("http://127.0.0.1:3030", opts)

	args := strings.TrimPrefix(recovery.Command, "sudo bort init-target --install")
	command := exec.Command("/bin/sh", "-c", "set -- "+args+"; printf '%s\\n' \"$@\"")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("parse recovery command: %v", err)
	}
	want := strings.Join([]string{
		"--dokploy-url", "http://127.0.0.1:3030",
		"--install-port", "3030",
		"--swarm-addr-pool", "auto",
		"--dokploy-version", defaultDokployVersion + "@" + defaultDokployDigest,
		"--endpoint-mode", "dnsrr",
		"--coolify-email", "admin@example.com",
		"--auth-secret-backup", "/private/recovery backups/admin's secret",
		"--name", "Admin O'Neil",
		"--api-key-name", "bort recovery key",
		"",
	}, "\n")
	if string(output) != want {
		t.Fatalf("parsed recovery command arguments:\n%s\nwant:\n%s", output, want)
	}
	differentAdmin := opts
	differentAdmin.AdminName = "Another Admin"
	if recovery.Identity == newDokployInstallationRecovery(recovery.TargetURL, differentAdmin).Identity {
		t.Fatal("recovery identity omitted the admin display name")
	}
	differentKey := opts
	differentKey.APIKeyName = "another key"
	if recovery.Identity == newDokployInstallationRecovery(recovery.TargetURL, differentKey).Identity {
		t.Fatal("recovery identity omitted the API-key name")
	}
	differentMode := opts
	differentMode.EndpointMode = "vip"
	if recovery.Identity == newDokployInstallationRecovery(recovery.TargetURL, differentMode).Identity {
		t.Fatal("recovery identity omitted the endpoint mode")
	}
	t.Setenv("SUDO_UID", "")
	directRoot := newDokployInstallationRecovery(recovery.TargetURL, opts)
	if directRoot.CommandPrefix != "bort" || !strings.HasPrefix(directRoot.Command, "bort init-target --install ") || strings.HasPrefix(directRoot.Command, "sudo ") {
		t.Fatalf("direct-root recovery command changed invocation style: %#v", directRoot)
	}
	if directRoot.Identity == recovery.Identity {
		t.Fatal("recovery identity omitted the invocation prefix")
	}
}

func TestPrepareDokployAuthSecretEscrowReturnsValidatedSnapshotAndDigest(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "dokploy-auth-secret")
	file, digest, err := prepareDokployAuthSecretEscrow(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256(original)
	if digest != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("escrow digest=%q, want %x", digest, wantDigest)
	}
	if file.Name() == path {
		t.Fatal("escrow returned the pathname-addressable file instead of a snapshot")
	}
	fromSnapshot, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(original) != 65 || !bytes.Equal(fromSnapshot, original) {
		t.Fatalf("snapshot=%q, want the 65-byte escrow contents %q", fromSnapshot, original)
	}
}

func TestPrepareDokployAuthSecretEscrowRejectsMissingDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "missing")
	path := filepath.Join(dir, "dokploy-auth-secret")
	if _, _, err := prepareDokployAuthSecretEscrow(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected missing escrow directory refusal, got %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing escrow directory was created: %v", err)
	}
}

func TestDokployAuthSecretValidationAcceptsPipeDescriptor(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "dokploy-auth-secret")
	descriptor, digest, err := prepareDokployAuthSecretEscrow(path)
	if err != nil {
		t.Fatal(err)
	}
	defer descriptor.Close()
	start := strings.Index(dokployShadowInstallScript, "validate_auth_secret_backup() {")
	if start < 0 {
		t.Fatal("missing authentication-secret descriptor validator")
	}
	end := strings.Index(dokployShadowInstallScript[start:], "\n}\n\nprepare_auth_secret_backup()")
	if end < 0 {
		t.Fatal("missing authentication-secret descriptor validator")
	}
	validator := dokployShadowInstallScript[start : start+end+3]
	cmd := exec.CommandContext(context.Background(), bash, "-c", "set -euo pipefail\nAUTH_SECRET_FD=3\nAUTH_SECRET_FD_PATH=/dev/fd/$AUTH_SECRET_FD\nAUTH_SECRET_DIGEST="+digest+"\n"+validator+"\nvalidate_auth_secret_backup\ncat \"$AUTH_SECRET_FD_PATH\"")
	if fd := configureDokployInstallerCommand(cmd, nil, descriptor); fd != 3 {
		t.Fatalf("authentication-secret descriptor fd=%d, want 3", fd)
	}
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("validate pipe-backed authentication-secret descriptor: %v", err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, want) {
		t.Fatalf("validated descriptor bytes=%q, want %q", output, want)
	}
}

func TestPrepareDokployAuthSecretEscrowSnapshotsValidatedBytes(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "dokploy-auth-secret")
	original := []byte(strings.Repeat("a", 64) + "\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	file, digest, err := prepareDokployAuthSecretEscrow(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.WriteFile(path, []byte(strings.Repeat("b", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromDescriptor, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fromDescriptor, original) {
		t.Fatalf("installer descriptor consumed mutable escrow bytes: %q", fromDescriptor)
	}
	if err := verifyDokployAuthSecretEscrow(path, digest); err == nil || !strings.Contains(err.Error(), "changed during installation") {
		t.Fatalf("post-install escrow mutation was not detected: %v", err)
	}
}

func TestPrepareDokployAuthSecretEscrowRejectsHardLink(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "dokploy-auth-secret")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, path+".link"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareDokployAuthSecretEscrow(path); err == nil || !strings.Contains(err.Error(), "single-link") {
		t.Fatalf("expected hard-linked escrow refusal, got %v", err)
	}
}

func TestPrepareDokployAuthSecretEscrowRejectsWrongSizeBeforeReading(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "dokploy-auth-secret")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 1<<40); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareDokployAuthSecretEscrow(path); err == nil || !strings.Contains(err.Error(), "is 1099511627776 bytes, not the 65 bytes") {
		t.Fatalf("expected size refusal before reading the escrow, got %v", err)
	}
}
