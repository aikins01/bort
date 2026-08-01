package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const stateAPIVersion = "bort.state/v1alpha1"

const (
	stateLockRetryInterval = 10 * time.Millisecond
	stateLockWait          = 5 * time.Second
)

var errBortStateActive = errors.New("workspace state update already in progress")

const (
	dataStrategyRecreate = "recreate"
	dataStrategyMigrate  = "migrate"
	dataStrategyManaged  = "managed"
)

type bortState struct {
	APIVersion string                       `json:"apiVersion"`
	UpdatedAt  time.Time                    `json:"updatedAt"`
	CurrentRun string                       `json:"currentRun,omitempty"`
	Apps       map[string]appStateData      `json:"apps,omitempty"`
	Targets    map[string]targetCredentials `json:"targets,omitempty"`
}

type targetCredentials struct {
	URL            string    `json:"url"`
	Token          string    `json:"token"`
	AdminEmail     string    `json:"adminEmail,omitempty"`
	BootstrappedAt time.Time `json:"bootstrappedAt"`
	APIKeyName     string    `json:"apiKeyName,omitempty"`
}

type appStateData struct {
	Env  map[string]string         `json:"env,omitempty"`
	Data map[string]dataStoreState `json:"data,omitempty"`
}

type dataStoreState struct {
	Strategy  string    `json:"strategy"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// defaultStatePath returns the path to the user-edited state file relative
// to the current working directory. This is intentionally cwd-relative for
// now so it sits next to .bort/runs/. Running bort from a different directory
// uses a different state file. TODO: revisit when a workspace/source root
// layer is introduced.
func defaultStatePath() string {
	return filepath.Join(".bort", "state.json")
}

func readBortState(path string) (bortState, error) {
	state := emptyBortState()
	contents, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return bortState{}, err
	}
	if len(contents) == 0 {
		return state, nil
	}
	if err := json.Unmarshal(contents, &state); err != nil {
		return bortState{}, fmt.Errorf("read %s: %w", path, err)
	}
	if state.APIVersion != "" && state.APIVersion != stateAPIVersion {
		return bortState{}, fmt.Errorf("%s has unsupported apiVersion %q (want %q)", path, state.APIVersion, stateAPIVersion)
	}
	if state.Apps == nil {
		state.Apps = map[string]appStateData{}
	}
	return state, nil
}

func writeBortState(path string, state bortState) error {
	if state.APIVersion == "" {
		state.APIVersion = stateAPIVersion
	}
	if state.Apps == nil {
		state.Apps = map[string]appStateData{}
	}
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now().UTC()
	}
	if err := ensurePrivateStateDir(path); err != nil {
		return err
	}
	contents, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	if err := writeFileAtomic(path, contents, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func ensurePrivateStateDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

func acquireBortStateLock(path string) (*applyLock, error) {
	if err := ensurePrivateStateDir(path); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(filepath.Dir(path), "state.lock")
	deadline := time.Now().Add(stateLockWait)
	for {
		lock, err := acquireApplyLock(lockPath)
		if !errors.Is(err, errApplyAlreadyRunning) {
			return lock, err
		}
		if !time.Now().Before(deadline) {
			return nil, errBortStateActive
		}
		time.Sleep(stateLockRetryInterval)
	}
}

func mutateBortState(path string, mutate func(*bortState) bool) error {
	lock, err := acquireBortStateLock(path)
	if err != nil {
		return err
	}
	defer lock.Release()
	state, err := readBortState(path)
	if err != nil {
		return err
	}
	if !mutate(&state) {
		return nil
	}
	return writeBortState(path, state)
}

func emptyBortState() bortState {
	return bortState{
		APIVersion: stateAPIVersion,
		Apps:       map[string]appStateData{},
	}
}

func setAppEnv(state bortState, app string, values map[string]string) bortState {
	if state.Apps == nil {
		state.Apps = map[string]appStateData{}
	}
	entry := state.Apps[app]
	if entry.Env == nil {
		entry.Env = map[string]string{}
	}
	for key, value := range values {
		entry.Env[key] = value
	}
	state.Apps[app] = entry
	state.UpdatedAt = time.Now().UTC()
	return state
}

func setAppDataStrategy(state bortState, app, store, strategy string) bortState {
	if state.Apps == nil {
		state.Apps = map[string]appStateData{}
	}
	entry := state.Apps[app]
	if entry.Data == nil {
		entry.Data = map[string]dataStoreState{}
	}
	entry.Data[store] = dataStoreState{Strategy: strategy, UpdatedAt: time.Now().UTC()}
	state.Apps[app] = entry
	state.UpdatedAt = time.Now().UTC()
	return state
}

func setTargetCredentials(state bortState, target string, creds targetCredentials) bortState {
	if state.Targets == nil {
		state.Targets = map[string]targetCredentials{}
	}
	state.Targets[target] = creds
	state.UpdatedAt = time.Now().UTC()
	return state
}

func rememberCurrentRun(run migrationRun) error {
	ref := filepath.ToSlash(filepath.Clean(run.RunDir))
	if ref == "" || ref == "." {
		return fmt.Errorf("cannot remember migration run with empty directory")
	}
	return mutateBortState(defaultStatePath(), func(state *bortState) bool {
		if state.CurrentRun == ref {
			return false
		}
		state.CurrentRun = ref
		state.UpdatedAt = time.Now().UTC()
		return true
	})
}

func currentRunRef() (string, bool, error) {
	state, err := readBortState(defaultStatePath())
	if err != nil {
		return "", false, err
	}
	ref := strings.TrimSpace(state.CurrentRun)
	return ref, ref != "", nil
}

func parseKeyValueArgs(args []string) (map[string]string, error) {
	values := map[string]string{}
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("expected KEY=value, got %q", arg)
		}
		values[key] = value
	}
	if len(values) == 0 {
		return nil, errors.New("no KEY=value pairs provided")
	}
	return values, nil
}

func sortedAppEnvKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
