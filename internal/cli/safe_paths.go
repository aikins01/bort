package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aikins01/bort/internal/safepath"
)

func cleanRelativeArtifact(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("artifact path is empty")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("artifact path %q must be relative", name)
	}
	if filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("artifact path %q must not contain a volume name", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("artifact path %q escapes the run directory", name)
	}
	return clean, nil
}

func containedPath(root, path string) error {
	return safepath.ContainedPath(root, path)
}

func readFileNoFollow(path string) ([]byte, error) {
	return safepath.ReadFileNoFollow(path)
}

func writeFileNoFollow(path string, data []byte, mode os.FileMode) error {
	return safepath.WriteFileNoFollow(path, data, mode)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	return safepath.WriteFileAtomicNoFollow(path, data, mode)
}
