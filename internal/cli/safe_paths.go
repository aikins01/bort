package cli

import (
	"errors"
	"fmt"
	"io"
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
	if filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("artifact path %q must not contain a volume name", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("artifact path %q escapes the run directory", name)
	}
	return clean, nil
}

// containedPath verifies that path resolves to a location under root, even
// if intermediate directories of either are symlinks. The final path
// component is allowed to not exist yet (we resolve the deepest existing
// ancestor). Combined with openWriteNoFollow at the final write site, this
// closes the parent-symlink and final-symlink traversal vectors.
func containedPath(root, path string) error {
	rootResolved, err := evalExistingSymlinks(root)
	if err != nil {
		return err
	}
	pathResolved, err := evalExistingSymlinks(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootResolved, pathResolved)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("%s escapes %s", path, root)
	}
	return nil
}

// evalExistingSymlinks returns the symlink-resolved absolute form of path.
// path may not exist yet; in that case we walk up to the deepest existing
// ancestor, EvalSymlinks that, and rejoin the non-existent suffix.
func evalExistingSymlinks(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(abs)
	suffix := ""
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			if suffix == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, suffix), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return current, nil
		}
		suffix = filepath.Join(filepath.Base(current), suffix)
		current = parent
	}
}

// readFileNoFollow reads path with O_NOFOLLOW on the final component so a
// symlink swapped in at the destination cannot redirect the read.
func readFileNoFollow(path string) ([]byte, error) {
	f, err := openNoFollowRead(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// writeFileNoFollow writes data to path with O_NOFOLLOW on the final
// component so a symlink at the destination cannot redirect the write.
func writeFileNoFollow(path string, data []byte, mode os.FileMode) error {
	f, err := openNoFollowWrite(path, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
