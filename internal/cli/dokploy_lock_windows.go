//go:build windows

package cli

import "fmt"

var (
	dokployLiveOperationLockPath   = defaultDokployLiveOperationLockPath
	dokployTrafficOwnerPath        = defaultDokployTrafficOwnerPath
	dokployLiveOperationsSupported = func() bool {
		return false
	}
)

func defaultDokployLiveOperationLockPath() (string, error) {
	return "", fmt.Errorf("host-wide Dokploy live operations are unsupported on Windows")
}

func prepareDokployLiveOperationLockPath(string) error {
	return fmt.Errorf("host-wide Dokploy live operations are unsupported on Windows")
}

func markDokployInstallationRecoveryRequired(dokployInstallationRecovery) error {
	return fmt.Errorf("Dokploy installation recovery is unsupported on Windows")
}

func readDokployInstallationRecovery() (dokployInstallationRecovery, bool, error) {
	return dokployInstallationRecovery{}, false, fmt.Errorf("Dokploy installation recovery is unsupported on Windows")
}

func dokployInstallationRecoveryRequired() (bool, error) {
	return false, fmt.Errorf("Dokploy installation recovery is unsupported on Windows")
}

func dokployInstallationRecoveryCommand() (string, bool, error) {
	return "", false, fmt.Errorf("Dokploy installation recovery is unsupported on Windows")
}

func clearDokployInstallationRecoveryRequired() error {
	return fmt.Errorf("Dokploy installation recovery is unsupported on Windows")
}

func defaultDokployTrafficOwnerPath() (string, error) {
	return "", nil
}

func prepareDokployTrafficOwnerPath(string) error {
	return fmt.Errorf("host-wide Dokploy traffic ownership is unsupported on Windows")
}
