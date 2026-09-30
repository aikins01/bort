//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

func dokployInstallerSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}

func configureDokployInstallerCommand(*exec.Cmd, *os.File, *os.File) int { return 0 }

func prepareDokployAuthSecretEscrow(string) (*os.File, string, error) {
	return nil, "", errors.New("Dokploy installation is unavailable on Windows")
}

func verifyDokployAuthSecretEscrow(string, string) error {
	return errors.New("Dokploy installation is unavailable on Windows")
}
