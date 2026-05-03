package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aikins01/bort/internal/planfile"
	"github.com/aikins01/bort/internal/preparer"
)

const progressAPIVersion = "bort.progress/v1alpha1"

const (
	progressStatusOpen     = "open"
	progressStatusResolved = "resolved"
	progressStatusSkipped  = "skipped"
)

type runProgress struct {
	APIVersion string                      `json:"apiVersion"`
	RunName    string                      `json:"runName"`
	RunDir     string                      `json:"runDir"`
	DryRun     bool                        `json:"dryRun"`
	UpdatedAt  time.Time                   `json:"updatedAt"`
	Decisions  map[string]decisionProgress `json:"decisions,omitempty"`
}

type decisionProgress struct {
	Status    string                  `json:"status"`
	Note      string                  `json:"note,omitempty"`
	UpdatedAt time.Time               `json:"updatedAt"`
	Items     map[string]itemProgress `json:"items,omitempty"`
}

type itemProgress struct {
	Status    string    `json:"status"`
	Note      string    `json:"note,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func emptyRunProgress(run migrationRun) runProgress {
	return runProgress{
		APIVersion: progressAPIVersion,
		RunName:    run.Name,
		RunDir:     run.RunDir,
		DryRun:     true,
		Decisions:  map[string]decisionProgress{},
	}
}

func readRunProgress(path string, run migrationRun) (runProgress, error) {
	var progress runProgress
	if err := planfile.Read(path, &progress); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyRunProgress(run), nil
		}
		return runProgress{}, err
	}
	if err := planfile.CheckAPIVersion(path, progress.APIVersion, progressAPIVersion); err != nil {
		return runProgress{}, err
	}
	if !progress.DryRun {
		return runProgress{}, fmt.Errorf("%s is not a dry-run progress artifact", path)
	}
	if progress.RunName != run.Name || progress.RunDir != run.RunDir || progress.UpdatedAt.Before(run.UpdatedAt) {
		return emptyRunProgress(run), nil
	}
	if progress.Decisions == nil {
		progress.Decisions = map[string]decisionProgress{}
	}
	return progress, nil
}

func writeRunProgress(path string, progress runProgress) error {
	if progress.APIVersion == "" {
		progress.APIVersion = progressAPIVersion
	}
	progress.DryRun = true
	if progress.Decisions == nil {
		progress.Decisions = map[string]decisionProgress{}
	}
	return writeJSONArtifact(path, progress)
}

func progressItemKey(item runDecisionItem) string {
	return strings.Join([]string{item.Stage, item.App, item.Code, item.ResourceRef, string(item.Readiness), item.Message}, "\x00")
}

func markDecisionDone(progress runProgress, decision runDecision, status, note string, now time.Time) runProgress {
	if progress.Decisions == nil {
		progress.Decisions = map[string]decisionProgress{}
	}
	dp, ok := progress.Decisions[decision.Kind]
	if !ok {
		dp = decisionProgress{Items: map[string]itemProgress{}}
	}
	if dp.Items == nil {
		dp.Items = map[string]itemProgress{}
	}
	dp.Status = status
	dp.Note = note
	dp.UpdatedAt = now.UTC()
	for _, item := range decision.Items {
		dp.Items[progressItemKey(item)] = itemProgress{Status: status, Note: note, UpdatedAt: now.UTC()}
	}
	progress.Decisions[decision.Kind] = dp
	progress.UpdatedAt = now.UTC()
	return progress
}

func applyProgressToDecisions(decisions []runDecision, progress runProgress) []runDecision {
	if len(progress.Decisions) == 0 {
		return decisions
	}
	filtered := make([]runDecision, 0, len(decisions))
	for _, decision := range decisions {
		dp, ok := progress.Decisions[decision.Kind]
		items := make([]runDecisionItem, 0, len(decision.Items))
		for _, item := range decision.Items {
			if ok {
				if ip, hit := dp.Items[progressItemKey(item)]; hit {
					if ip.Status == progressStatusResolved || ip.Status == progressStatusSkipped {
						continue
					}
				}
			}
			items = append(items, item)
		}
		if len(items) == 0 {
			continue
		}

		decision.Items = items
		decision.Count = len(items)
		decision.Apps = nil
		decision.Codes = nil
		decision.Readiness = preparer.ReadinessReadyToCreate
		for _, item := range items {
			decision.Apps = uniqueAppend(decision.Apps, item.App)
			decision.Codes = uniqueAppend(decision.Codes, item.Code)
			decision.Readiness = preparer.WorseReadiness(decision.Readiness, item.Readiness)
		}
		decision.Apps = sortedStrings(decision.Apps)
		decision.Codes = sortedStrings(decision.Codes)
		decision.Action = decisionAction(decision)
		decision.Reason = decisionReason(decision)
		filtered = append(filtered, decision)
	}
	sortRunDecisions(filtered)
	return filtered
}

func progressSummary(progress runProgress) string {
	if len(progress.Decisions) == 0 {
		return ""
	}
	resolved := 0
	skipped := 0
	for _, decision := range progress.Decisions {
		switch decision.Status {
		case progressStatusResolved:
			resolved++
		case progressStatusSkipped:
			skipped++
		}
	}
	if resolved == 0 && skipped == 0 {
		return ""
	}
	return fmt.Sprintf("Local progress: %d resolved, %d skipped", resolved, skipped)
}
