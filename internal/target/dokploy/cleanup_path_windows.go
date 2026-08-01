//go:build windows

package dokploy

import (
	"fmt"
)

func sourcePurgePathAbsentNoFollow(path string, allowPlatform bool) (bool, error) {
	if err := ValidateSourcePurgePath(path, allowPlatform); err != nil {
		return false, err
	}
	return pathAbsentNoFollow(path)
}

func pathAbsentNoFollow(path string) (bool, error) {
	return false, fmt.Errorf("source path absence verification for %q is unavailable on Windows; run cleanup purge on the source host", path)
}
