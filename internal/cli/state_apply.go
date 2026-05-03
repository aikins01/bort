package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/preparer"
)

// merges recorded env values into per-app private env files inside the bundle.
// default bundles that only have env examples are
// materialized into private env files before merging.
//
// run this before planning so the preparer's missing-values scan sees the
// merged contents.
func applyStateEnvToBundle(state bortState, bundleDir string) (int, error) {
	if bundleDir == "" || len(state.Apps) == 0 {
		return 0, nil
	}
	index, hasIndex, err := readBundleIndex(bundleDir)
	if err != nil {
		return 0, err
	}
	appDirs := bundleAppDirs(index)
	indexChanged := false
	touched := 0
	for app, data := range state.Apps {
		if len(data.Env) == 0 {
			continue
		}
		appDir := bundleAppDir(bundleDir, app, appDirs)
		materialized, err := materializePrivateEnvFiles(appDir)
		if err != nil {
			return touched, err
		}
		if materialized && markBundleAppPrivateEnv(&index, app) {
			indexChanged = true
		}
		envFiles, err := privateEnvFiles(appDir)
		if err != nil {
			return touched, err
		}
		if len(envFiles) == 0 {
			continue
		}
		for _, envPath := range envFiles {
			templatePath := envPath + ".example"
			toFill, err := envValuesToFill(envPath, templatePath, data.Env)
			if err != nil {
				return touched, err
			}
			if len(toFill) == 0 {
				continue
			}
			if err := writeEnvFileValues(envPath, templatePath, toFill); err != nil {
				return touched, err
			}
			touched++
		}
	}
	if hasIndex && indexChanged {
		if err := writeJSONArtifact(filepath.Join(bundleDir, "index.json"), index); err != nil {
			return touched, err
		}
	}
	return touched, nil
}

func readBundleIndex(bundleDir string) (exporter.Summary, bool, error) {
	contents, err := os.ReadFile(filepath.Join(bundleDir, "index.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return exporter.Summary{}, false, nil
		}
		return exporter.Summary{}, false, err
	}
	var summary exporter.Summary
	if err := json.Unmarshal(contents, &summary); err != nil {
		return exporter.Summary{}, false, err
	}
	return summary, true, nil
}

func bundleAppDirs(index exporter.Summary) map[string]string {
	dirs := map[string]string{}
	for _, app := range index.Apps {
		if app.Directory == "" {
			continue
		}
		dirs[app.Name] = app.Directory
		dirs[app.Directory] = app.Directory
	}
	return dirs
}

func bundleAppDir(bundleDir, app string, dirs map[string]string) string {
	if dir := dirs[app]; dir != "" {
		return filepath.Join(bundleDir, filepath.FromSlash(dir))
	}
	return filepath.Join(bundleDir, app)
}

func markBundleAppPrivateEnv(index *exporter.Summary, appName string) bool {
	changed := false
	for i := range index.Apps {
		app := &index.Apps[i]
		if app.Name != appName && app.Directory != appName {
			continue
		}
		if !app.PrivateEnvValues {
			app.PrivateEnvValues = true
			changed = true
		}
	}
	return changed
}

func materializePrivateEnvFiles(appDir string) (bool, error) {
	entries, err := os.ReadDir(appDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	materialized := false
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".env") || !strings.HasSuffix(name, ".example") {
			continue
		}
		examplePath := filepath.Join(appDir, name)
		privatePath := filepath.Join(appDir, strings.TrimSuffix(name, ".example"))
		if _, err := os.Lstat(privatePath); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return materialized, err
		}
		contents, err := readFileNoFollow(examplePath)
		if err != nil {
			return materialized, err
		}
		if err := writeFileNoFollow(privatePath, contents, 0o600); err != nil {
			return materialized, err
		}
		materialized = true
	}
	if materialized {
		if err := rewriteComposeEnvFileExamples(appDir); err != nil {
			return materialized, err
		}
	}
	return materialized, nil
}

func rewriteComposeEnvFileExamples(appDir string) error {
	composePath := filepath.Join(appDir, "compose.yaml")
	contents, err := readFileNoFollow(composePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	updated := string(contents)
	entries, err := os.ReadDir(appDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".env") || !strings.HasSuffix(name, ".example") {
			continue
		}
		updated = replaceComposeEnvExample(updated, name, strings.TrimSuffix(name, ".example"))
	}
	if updated == string(contents) {
		return nil
	}
	return writeFileNoFollow(composePath, []byte(updated), 0o600)
}

// replaceComposeEnvExample swaps oldName for newName inside compose YAML
// only when oldName appears as a standalone path token. The boundary
// classes match the characters that delimit a path value in YAML
// (whitespace, separators, list markers, quotes, equals). Substring
// matches inside longer filenames or comments are left alone.
func replaceComposeEnvExample(yaml, oldName, newName string) string {
	pattern := `(^|[\s/:'"\[,=])` + regexp.QuoteMeta(oldName) + `($|[\s/:'"\],])`
	re := regexp.MustCompile(pattern)
	return re.ReplaceAllString(yaml, "${1}"+newName+"${2}")
}

// returns values for keys that are empty in the env file or belong to its template.
// non-empty values in the env file are skipped so manual edits survive.
func envValuesToFill(envPath, templatePath string, values map[string]string) (map[string]string, error) {
	existing, existingOK, err := readEnvAssignments(envPath)
	if err != nil {
		return nil, err
	}
	template, templateOK, err := readEnvAssignments(templatePath)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for key, value := range values {
		current, present := existing[key]
		switch {
		case present && current == "":
			out[key] = value
		case present:
			continue
		case templateOK:
			if _, ok := template[key]; ok {
				out[key] = value
			}
		case !existingOK:
			out[key] = value
		}
	}
	return out, nil
}

func readEnvAssignments(path string) (map[string]string, bool, error) {
	contents, err := readFileNoFollow(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, false, nil
		}
		return nil, false, err
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(contents), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		values[key] = strings.TrimSpace(value)
	}
	return values, true, nil
}

// patches an in-memory prepare result with user-recorded answers so the linear
// status view counts resolved setup issues as resolved.
func applyStateOverridesToPrepare(state bortState, prepare *preparer.Result) {
	if prepare == nil || len(state.Apps) == 0 {
		return
	}
	changed := false
	for i := range prepare.Apps {
		app := &prepare.Apps[i]
		entry, ok := state.Apps[app.Name]
		if !ok {
			continue
		}
		appChanged := false
		if len(entry.Env) > 0 && envResourcesComplete(app.Resources.EnvFiles) {
			before := len(app.Gates)
			app.Gates = filterGates(app.Gates, func(g preparer.Gate) bool {
				return g.Code != "env.values_redacted"
			})
			if len(app.Gates) != before {
				appChanged = true
			}
		}
		if len(entry.Data) == 0 {
			if appChanged {
				recomputeAppReadiness(app)
				changed = true
			}
			continue
		}
		resolved := map[string]struct{}{}
		for j := range app.Resources.DataStores {
			store := &app.Resources.DataStores[j]
			strategy, key, ok := lookupDataStoreStrategy(entry.Data, store)
			if !ok {
				continue
			}
			store.Strategy = strategy.Strategy
			store.Readiness = preparer.ReadinessReadyToCreate
			for _, ref := range []string{key, store.Service, store.Kind} {
				if ref != "" {
					resolved["data-store:"+ref] = struct{}{}
				}
			}
		}
		if len(resolved) == 0 {
			if appChanged {
				recomputeAppReadiness(app)
				changed = true
			}
			continue
		}
		app.Gates = filterGates(app.Gates, func(g preparer.Gate) bool {
			switch g.Code {
			case "data_store.prepare_required", "data_store.confirm_strategy", "data_store.manual_review":
				_, hit := resolved[g.ResourceRef]
				return !hit
			default:
				return true
			}
		})
		appChanged = true
		recomputeAppReadiness(app)
		changed = true
	}
	if changed {
		recomputePrepareStatus(prepare)
	}
}

func envResourcesComplete(files []preparer.EnvFileResource) bool {
	if len(files) == 0 {
		return false
	}
	for _, file := range files {
		if len(file.MissingValues) > 0 {
			return false
		}
	}
	return true
}

// lookupDataStoreStrategy resolves a recorded strategy for the given store
// by trying the service name first, then the kind. The matching key is
// returned alongside the strategy so the caller can compose the gate's
// resourceRef ("data-store:<key>") for filtering.
func lookupDataStoreStrategy(stored map[string]dataStoreState, store *preparer.DataStoreResource) (dataStoreState, string, bool) {
	for _, candidate := range []string{store.Service, store.Kind} {
		if candidate == "" {
			continue
		}
		if strategy, ok := stored[candidate]; ok {
			return strategy, candidate, true
		}
	}
	return dataStoreState{}, "", false
}

func filterGates(gates []preparer.Gate, keep func(preparer.Gate) bool) []preparer.Gate {
	out := gates[:0]
	for _, g := range gates {
		if keep(g) {
			out = append(out, g)
		}
	}
	return out
}

func recomputeAppReadiness(app *preparer.AppPlan) {
	app.Readiness = preparer.ReadinessReadyToCreate
	for _, gate := range app.Gates {
		app.Readiness = preparer.WorseReadiness(app.Readiness, gate.Readiness)
	}
	app.Status = preparer.StatusFromReadiness(app.Readiness)
}

func recomputePrepareStatus(prepare *preparer.Result) {
	prepare.Status = preparer.StatusGreen
	for _, app := range prepare.Apps {
		prepare.Status = preparer.WorseStatus(prepare.Status, app.Status)
	}
}

func privateEnvFiles(appDir string) ([]string, error) {
	entries, err := os.ReadDir(appDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	paths := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".env") || strings.HasSuffix(name, ".example") {
			continue
		}
		paths = append(paths, filepath.Join(appDir, name))
	}
	return paths, nil
}
