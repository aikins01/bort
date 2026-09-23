package cli

import (
	"os"
	"testing"
)

// bortCommand output must stay deterministic in tests, even when go test is
// invoked via sudo.
func TestMain(m *testing.M) {
	os.Setenv("SUDO_UID", "")
	os.Exit(m.Run())
}
