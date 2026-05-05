package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/aikins01/bort/internal/target/dokploy"
)

const appliedAPIVersion = "bort.applied/v1alpha1"

// runApplied is the per-run audit ledger. it captures the outcome of every
// step the live applier walks through so a partial run can be inspected and
// safely retried.
type runApplied struct {
	APIVersion string                `json:"apiVersion"`
	RunName    string                `json:"runName"`
	BundleDir  string                `json:"bundleDir,omitempty"`
	Target     string                `json:"target,omitempty"`
	UpdatedAt  time.Time             `json:"updatedAt"`
	Steps      []appliedStep         `json:"steps,omitempty"`
	Apps       map[string]appliedApp `json:"apps,omitempty"`
}

type appliedStep struct {
	Index     int       `json:"index"`
	Kind      string    `json:"kind"`
	App       string    `json:"app"`
	Ref       string    `json:"ref"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updatedAt"`
	Error     string    `json:"error,omitempty"`
}

type appliedApp struct {
	ProjectID     string `json:"projectId,omitempty"`
	EnvironmentID string `json:"environmentId,omitempty"`
	ComposeID     string `json:"composeId,omitempty"`
}

func readRunApplied(path string, run migrationRun) (runApplied, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newRunApplied(run), nil
		}
		return runApplied{}, err
	}
	if len(contents) == 0 {
		return newRunApplied(run), nil
	}
	var applied runApplied
	if err := json.Unmarshal(contents, &applied); err != nil {
		return runApplied{}, fmt.Errorf("read %s: %w", path, err)
	}
	if applied.APIVersion != "" && applied.APIVersion != appliedAPIVersion {
		return runApplied{}, fmt.Errorf("%s has unsupported apiVersion %q (want %q)", path, applied.APIVersion, appliedAPIVersion)
	}
	if applied.Apps == nil {
		applied.Apps = map[string]appliedApp{}
	}
	return applied, nil
}

func newRunApplied(run migrationRun) runApplied {
	return runApplied{
		APIVersion: appliedAPIVersion,
		RunName:    run.Name,
		BundleDir:  run.BundleDir,
		Target:     run.Target,
		UpdatedAt:  time.Now().UTC(),
		Apps:       map[string]appliedApp{},
	}
}

func writeRunApplied(path string, applied runApplied) error {
	if applied.APIVersion == "" {
		applied.APIVersion = appliedAPIVersion
	}
	applied.UpdatedAt = time.Now().UTC()
	return writeJSONArtifact(path, applied)
}

func recordAppliedStep(applied runApplied, progress dokploy.StepProgress) runApplied {
	step := appliedStep{
		Index:     progress.Index,
		Kind:      string(progress.Step.Kind),
		App:       progress.Step.App,
		Ref:       progress.Step.Ref,
		Status:    string(progress.Status),
		UpdatedAt: time.Now().UTC(),
	}
	if progress.Err != nil {
		step.Error = progress.Err.Error()
	}
	for i := range applied.Steps {
		if applied.Steps[i].Index == progress.Index {
			applied.Steps[i] = step
			return applied
		}
	}
	applied.Steps = append(applied.Steps, step)
	sort.Slice(applied.Steps, func(i, j int) bool { return applied.Steps[i].Index < applied.Steps[j].Index })
	return applied
}

// appliedLedger collects step results from concurrent dokploy.OnProgress
// callbacks and persists them to disk. it is safe for concurrent calls.
type appliedLedger struct {
	mu      sync.Mutex
	path    string
	state   runApplied
	persist func(string, runApplied) error
}

func newAppliedLedger(path string, run migrationRun) (*appliedLedger, error) {
	state, err := readRunApplied(path, run)
	if err != nil {
		return nil, err
	}
	return &appliedLedger{path: path, state: state, persist: writeRunApplied}, nil
}

func (l *appliedLedger) Record(progress dokploy.StepProgress) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state = recordAppliedStep(l.state, progress)
	return l.persist(l.path, l.state)
}

func (l *appliedLedger) Snapshot() runApplied {
	l.mu.Lock()
	defer l.mu.Unlock()
	clone := l.state
	clone.Steps = append([]appliedStep{}, l.state.Steps...)
	clone.Apps = map[string]appliedApp{}
	for k, v := range l.state.Apps {
		clone.Apps[k] = v
	}
	return clone
}

func completedApplyPrefix(steps []dokploy.Step, applied runApplied) int {
	strict := indexedCompletedApplyPrefix(steps, applied)
	sequenced := sequencedCompletedApplyPrefix(steps, applied)
	if sequenced > strict {
		return sequenced
	}
	return strict
}

func indexedCompletedApplyPrefix(steps []dokploy.Step, applied runApplied) int {
	byIndex := map[int]appliedStep{}
	for _, step := range applied.Steps {
		if step.Index < 0 || step.Index >= len(steps) {
			continue
		}
		byIndex[step.Index] = step
	}
	for index, step := range steps {
		recorded, ok := byIndex[index]
		if !ok || !appliedStepCompleted(recorded) || !appliedStepMatches(recorded, step) {
			return index
		}
	}
	return len(steps)
}

func sequencedCompletedApplyPrefix(steps []dokploy.Step, applied runApplied) int {
	completed := make([]appliedStep, 0, len(applied.Steps))
	for _, step := range applied.Steps {
		if step.Index < 0 || !appliedStepCompleted(step) {
			continue
		}
		completed = append(completed, step)
	}
	sort.SliceStable(completed, func(i, j int) bool { return completed[i].Index < completed[j].Index })
	searchFrom := 0
	for index, step := range steps {
		matched := false
		for searchFrom < len(completed) {
			recorded := completed[searchFrom]
			searchFrom++
			if appliedStepMatches(recorded, step) {
				matched = true
				break
			}
		}
		if !matched {
			return index
		}
	}
	return len(steps)
}

func appliedStepCompleted(step appliedStep) bool {
	return step.Status == string(dokploy.StepStatusOK) || step.Status == string(dokploy.StepStatusSkipped)
}

func appliedStepMatches(recorded appliedStep, step dokploy.Step) bool {
	return recorded.Kind == string(step.Kind) && recorded.App == step.App && recorded.Ref == step.Ref
}
