//go:build !linux && !darwin && !windows

package safepath

import "fmt"

func renamePrivateFileNoReplace(int, string, string) error {
	return fmt.Errorf("secure no-replace rename is unavailable on this platform")
}
