//go:build !windows

package cli

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

var (
	dokployLiveOperationLockPath   = defaultDokployLiveOperationLockPath
	dokployTrafficOwnerPath        = defaultDokployTrafficOwnerPath
	dokployLiveOperationsSupported = func() bool {
		return runtime.GOOS == "linux"
	}
)

func defaultDokployLiveOperationLockPath() (string, error) {
	return filepath.Join("/var/lib/bort", "dokploy-live.lock"), nil
}

func prepareDokployLiveOperationLockPath(path string) error {
	return ensurePrivateHostDirectory(filepath.Dir(path), "state")
}

func dokployInstallationRecoveryPath() (string, error) {
	path, err := dokployLiveOperationLockPath()
	if err != nil {
		return "", err
	}
	return path + ".install-recovery-required", nil
}

func markDokployInstallationRecoveryRequired(recovery dokployInstallationRecovery) error {
	if err := validateDokployInstallationRecovery(recovery); err != nil {
		return err
	}
	path, err := dokployInstallationRecoveryPath()
	if err != nil {
		return err
	}
	if err := prepareDokployLiveOperationLockPath(path); err != nil {
		return err
	}
	existing, found, err := readDokployInstallationRecoveryMarker(path)
	if err != nil {
		return err
	}
	if found {
		if existing.Identity != recovery.Identity {
			return fmt.Errorf("interrupted Dokploy installation was started with different immutable options; rerun the exact original command: %s", existing.Command)
		}
		if sameDokployInstallationRecovery(existing, recovery) || existing.Phase == dokployInstallAPIKey && recovery.Phase == dokployInstallInstalling {
			return nil
		}
		existingIntent := existing
		existingIntent.APIKeyCreatedID = ""
		recoveryIntent := recovery
		recoveryIntent.APIKeyCreatedID = ""
		updatingCreatedKey := existing.Phase == dokployInstallAPIKey && recovery.Phase == dokployInstallAPIKey && sameDokployInstallationRecovery(existingIntent, recoveryIntent)
		if !updatingCreatedKey && (existing.Phase != dokployInstallInstalling || recovery.Phase != dokployInstallAPIKey) {
			return fmt.Errorf("interrupted Dokploy installation recovery state cannot move from %q to %q", existing.Phase, recovery.Phase)
		}
	}
	contents, err := json.Marshal(recovery)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(contents, '\n'), 0o600)
}

func readDokployInstallationRecovery() (dokployInstallationRecovery, bool, error) {
	path, err := dokployInstallationRecoveryPath()
	if err != nil {
		return dokployInstallationRecovery{}, false, err
	}
	return readDokployInstallationRecoveryMarker(path)
}

func dokployInstallationRecoveryRequired() (bool, error) {
	path, err := dokployInstallationRecoveryPath()
	if err != nil {
		return false, err
	}
	_, found, err := readDokployInstallationRecoveryMarker(path)
	return found, err
}

func dokployInstallationRecoveryCommand() (string, bool, error) {
	path, err := dokployInstallationRecoveryPath()
	if err != nil {
		return "", false, err
	}
	recovery, found, err := readDokployInstallationRecoveryMarker(path)
	return recovery.Command, found, err
}

func readDokployInstallationRecoveryMarker(path string) (dokployInstallationRecovery, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return dokployInstallationRecovery{}, false, nil
	}
	if err != nil {
		return dokployInstallationRecovery{}, false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return dokployInstallationRecovery{}, false, fmt.Errorf("Dokploy installation recovery marker %s must be a single-link mode 0600 regular file owned by uid %d", path, os.Geteuid())
	}
	contents, err := readFileNoFollow(path)
	if err != nil {
		return dokployInstallationRecovery{}, false, err
	}
	var recovery dokployInstallationRecovery
	if err := json.Unmarshal(contents, &recovery); err != nil {
		return dokployInstallationRecovery{}, false, fmt.Errorf("Dokploy installation recovery marker %s is malformed", path)
	}
	canonical, err := json.Marshal(recovery)
	if err != nil || !bytes.Equal(contents, append(canonical, '\n')) || validateDokployInstallationRecovery(recovery) != nil {
		return dokployInstallationRecovery{}, false, fmt.Errorf("Dokploy installation recovery marker %s is malformed", path)
	}
	return recovery, true, nil
}

func validateDokployInstallationRecovery(recovery dokployInstallationRecovery) error {
	if recovery.APIVersion != dokployInstallRecoveryAPIVersion {
		return fmt.Errorf("unsupported Dokploy installation recovery marker version")
	}
	if recovery.Phase != dokployInstallInstalling && recovery.Phase != dokployInstallAPIKey {
		return fmt.Errorf("unsupported Dokploy installation recovery phase")
	}
	if recovery.CommandPrefix != "bort" && recovery.CommandPrefix != "sudo bort" {
		return fmt.Errorf("unsupported Dokploy installation recovery command prefix")
	}
	if recovery.EndpointMode != "vip" && recovery.EndpointMode != "dnsrr" {
		return fmt.Errorf("unsupported Dokploy installation endpoint mode")
	}
	if normalized, err := normalizeSwarmAddressPool(recovery.AddressPool); err != nil || normalized != recovery.AddressPool {
		return fmt.Errorf("unsupported Dokploy installation address pool")
	}
	if err := validateDokployAPIKeyName(recovery.APIKeyName); err != nil {
		return fmt.Errorf("unsupported Dokploy installation API-key name")
	}
	if recovery.Phase == dokployInstallInstalling && len(recovery.APIKeyBaselineIDs) != 0 {
		return fmt.Errorf("Dokploy installation recovery baseline is only valid during API-key recovery")
	}
	if recovery.Phase == dokployInstallInstalling && recovery.APIKeyCreatedID != "" {
		return fmt.Errorf("Dokploy installation recovery created key is only valid during API-key recovery")
	}
	if strings.ContainsAny(recovery.APIKeyCreatedID, "\r\n\x00") {
		return fmt.Errorf("Dokploy installation recovery created API-key identity is malformed")
	}
	for index, id := range recovery.APIKeyBaselineIDs {
		if id == "" || strings.ContainsAny(id, "\r\n\x00") || index > 0 && recovery.APIKeyBaselineIDs[index-1] >= id {
			return fmt.Errorf("Dokploy installation recovery API-key baseline is malformed")
		}
	}
	if decoded, err := hex.DecodeString(recovery.Identity); err != nil || len(decoded) != 32 || recovery.Identity != strings.ToLower(recovery.Identity) {
		return fmt.Errorf("Dokploy installation recovery identity must be 64 lowercase hexadecimal characters")
	}
	if !sameDokployInstallationRecovery(recovery, canonicalDokployInstallationRecovery(recovery)) {
		return fmt.Errorf("Dokploy installation recovery options are not canonical")
	}
	if recovery.Identity != dokployInstallRecoveryIdentity(recovery) {
		return fmt.Errorf("Dokploy installation recovery identity does not match its immutable options")
	}
	if recovery.Command != dokployInstallRecoveryCommand(recovery) || recovery.Command == "" || recovery.Command != strings.TrimSpace(recovery.Command) || strings.ContainsAny(recovery.Command, "\r\n\x00") {
		return fmt.Errorf("Dokploy installation recovery command is malformed")
	}
	return nil
}

func clearDokployInstallationRecoveryRequired() error {
	path, err := dokployInstallationRecoveryPath()
	if err != nil {
		return err
	}
	blocked, err := dokployInstallationRecoveryRequired()
	if err != nil || !blocked {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

func defaultDokployTrafficOwnerPath() (string, error) {
	return filepath.Join("/var/lib/bort", "dokploy-traffic-owner.json"), nil
}

func prepareDokployTrafficOwnerPath(path string) error {
	return ensurePrivateHostDirectory(filepath.Dir(path), "state")
}

func ensurePrivateHostDirectory(dir, kind string) error {
	created := false
	if err := os.Mkdir(dir, 0o700); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("create host Dokploy %s directory %s: %w%s", kind, dir, err, hostDirectoryProvisioningHint(dir))
		}
	} else {
		created = true
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect host Dokploy %s directory %s: %w", kind, dir, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("host Dokploy %s directory %s must be a mode 0700 directory owned by uid %d%s", kind, dir, os.Geteuid(), hostDirectoryProvisioningHint(dir))
	}
	if !created {
		return nil
	}
	parent, err := os.Open(filepath.Dir(dir))
	if err != nil {
		return fmt.Errorf("open parent of host Dokploy %s directory %s: %w", kind, dir, err)
	}
	if err := parent.Sync(); err != nil {
		_ = parent.Close()
		return fmt.Errorf("sync parent of host Dokploy %s directory %s: %w", kind, dir, err)
	}
	if err := parent.Close(); err != nil {
		return fmt.Errorf("close parent of host Dokploy %s directory %s: %w", kind, dir, err)
	}
	return nil
}

func hostDirectoryProvisioningHint(dir string) string {
	if os.Geteuid() == 0 {
		return ""
	}
	return fmt.Sprintf(`; provision it once with: sudo install -d -m 700 -o "$(id -u)" -g "$(id -g)" %s`, shellQuote(dir))
}
