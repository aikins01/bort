package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func cleanRelativeArtifact(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("artifact path is empty")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("artifact path %q must be relative", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("artifact path %q escapes the run directory", name)
	}
	return clean, nil
}

func containedPath(root, path string) error {
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return err
	}
	pathAbs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("%s escapes %s", path, root)
	}
	return nil
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write through symlink %s", path)
	}
	return nil
}
