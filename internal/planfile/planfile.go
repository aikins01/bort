package planfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aikins01/bort/internal/planutil"
)

func Read(path string, out any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := json.NewDecoder(file).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func CheckAPIVersion(path, got, want string) error {
	if got != want {
		return fmt.Errorf("%s has apiVersion %q, want %q", path, got, want)
	}
	return nil
}

func CheckDryRun(path string, dryRun bool) error {
	if !dryRun {
		return fmt.Errorf("%s is not a dry-run plan artifact", path)
	}
	return nil
}

func CheckBundle(path, got, want string) error {
	if strings.TrimSpace(want) == "" || samePath(got, want) {
		return nil
	}
	return fmt.Errorf("%s was created for bundle %q, want %q", path, got, want)
}

func CheckTarget(path, got, want string) error {
	if strings.TrimSpace(want) == "" || strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(want)) {
		return nil
	}
	return fmt.Errorf("%s was created for target %q, want %q", path, got, want)
}

func MatchApp(name, directory, appName string) bool {
	if appName == "" {
		return true
	}
	if name == appName || directory == appName {
		return true
	}
	slug := planutil.Slug(name)
	return slug != "" && slug == planutil.Slug(appName)
}

func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && absA == absB
}
