package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/aikins01/bort/internal/preparer"
)

func writeAppFirstCockpit(w io.Writer, run loadedMigrationRun) {
	apps := appsFromRun(run)
	summary := summarizeMigrationRun(run)
	phase := migrationRunPhase(run)
	st := newStyler(w)

	ready, blocked := 0, 0
	for _, app := range apps {
		switch app.Health {
		case appHealthReady:
			ready++
		case appHealthBlocked:
			blocked++
		}
	}

	header := fmt.Sprintf("%s %s %s · %d of %d ready",
		runSourceLabel(summary.Run),
		st.muted("→"),
		st.emph(summary.Run.Target),
		ready, len(apps),
	)
	if blocked > 0 {
		header += " · " + st.glyph(fmt.Sprintf("%d blocked", blocked), sevBad)
	}
	header += " " + st.pill(migrationRunPhaseLabel(phase), severityForMigrationRunPhase(phase))
	fmt.Fprintln(w, header)
	fmt.Fprintln(w)

	if len(apps) == 0 {
		fmt.Fprintln(w, "No apps in this run.")
		return
	}

	for i, app := range apps {
		if i > 0 {
			fmt.Fprintln(w, st.muted(strings.Repeat("─", 60)))
		}
		nameLine := fmt.Sprintf("%s  %s",
			st.glyph(healthGlyph(app.Health), severityForHealth(app.Health)),
			st.emph(app.Name),
		)
		if st.color {
			nameLine += " " + st.pill(string(app.Health), severityForHealth(app.Health))
		}
		fmt.Fprintln(w, nameLine)
		for _, line := range app.Resources {
			fmt.Fprintf(w, "    %s  %-9s %s\n",
				st.glyph(line.Status, severityForGlyph(line.Status)),
				line.Label,
				st.muted(line.Detail),
			)
		}
		if len(app.Issues) > 0 {
			fmt.Fprintln(w, "    "+st.muted("Issues:"))
			for _, issue := range app.Issues {
				fmt.Fprintf(w, "      %s %s\n",
					st.glyph(severityGlyph(issue.Severity), severityForReadiness(issue.Severity)),
					issue.Title,
				)
				if issue.Detail != "" {
					fmt.Fprintf(w, "          %s\n", st.muted(issue.Detail))
				}
				if fix := issue.FixCommand(app.Name); fix != "" {
					fmt.Fprintf(w, "          %s\n", st.fix(fix))
				} else if next := issue.NextStep(); next != "" {
					fmt.Fprintf(w, "          %s\n", st.muted("next: "+next))
				}
			}
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, st.muted(strings.Repeat("─", 60)))
	parts := []string{fmt.Sprintf("%d apps", len(apps))}
	if ready > 0 {
		parts = append(parts, st.glyph(fmt.Sprintf("%d ready", ready), sevGood))
	}
	if needsWork := len(apps) - ready - blocked; needsWork > 0 {
		parts = append(parts, st.glyph(fmt.Sprintf("%d needs work", needsWork), sevWarn))
	}
	if blocked > 0 {
		parts = append(parts, st.glyph(fmt.Sprintf("%d blocked", blocked), sevBad))
	}
	fmt.Fprintf(w, "%s %s\n", st.emph("Plan:"), strings.Join(parts, " · "))
	if line := appliedFooter(run.Applied); line != "" {
		fmt.Fprintln(w, st.muted(line))
	}
	downstreamBlockers := openDownstreamBlockingDecisions(run)
	if len(downstreamBlockers) > 0 {
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Downstream blockers: %d open", len(downstreamBlockers))))
		writeCockpitDecisions(w, st, downstreamBlockers)
	}
	reviewDecisions := openReviewDecisions(run)
	if len(reviewDecisions) > 0 {
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Review-only decisions: %d open (non-blocking before live apply)", len(reviewDecisions))))
		writeCockpitDecisions(w, st, reviewDecisions)
	}
	switch phase {
	case "lock-error":
		fmt.Fprintln(w, st.muted("Live apply lock state could not be verified. Inspect the run's apply.lock before retrying."))
	case "applying":
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Live apply is running. Run `%s` to attach to its current status.", liveApplyCommand(run))))
	case "partial":
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Live apply is incomplete. Run `%s` to resume safely.", liveApplyCommand(run))))
	case "applied":
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Target is live. Verify it through the rollback window, then run `%s` to retire the source.", runScopedCommand(run, "commit --apply"))))
	case "committed":
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("Target accepted and source containers retired. Run `%s` to audit leftovers.", runScopedCommand(run, "cleanup"))))
	case "purged":
		fmt.Fprintln(w, st.muted("Migration complete. Target resources and source-control credentials were preserved."))
	case "planning":
		fmt.Fprintln(w, st.muted(issueActionFooter(apps)))
	default:
		fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("All app inputs ready. Run `%s` to apply, or run `%s` interactively to continue.", liveApplyCommand(run), bortCommand(""))))
	}
}

func writeCockpitDecisions(w io.Writer, st *styler, decisions []runDecision) {
	limit := min(len(decisions), 3)
	for _, decision := range decisions[:limit] {
		fmt.Fprintf(w, "  %s %s: %s (%d item(s))\n", decision.Kind, decision.Readiness, decision.Action, decision.Count)
	}
	if remaining := len(decisions) - limit; remaining > 0 {
		fmt.Fprintf(w, "  %s\n", st.muted(fmt.Sprintf("and %d more", remaining)))
	}
}

func migrationRunPhase(run loadedMigrationRun) string {
	applyActive, applyActiveErr := applyRunActive(run.Run.RunDir)
	switch {
	case run.Run.PurgedAt != nil:
		return "purged"
	case run.Run.CommittedAt != nil:
		return "committed"
	case run.Run.LiveAppliedAt != nil || liveApplySucceeded(run):
		return "applied"
	case applyActiveErr != nil:
		return "lock-error"
	case applyActive:
		return "applying"
	case len(run.Applied.Steps) > 0:
		return "partial"
	case len(openSetupDecisions(run)) > 0:
		return "planning"
	case len(liveApplyBlockingDecisions(run)) == 0:
		return "ready"
	default:
		return "planning"
	}
}

func migrationRunPhaseLabel(phase string) string {
	switch phase {
	case "applied":
		return "TARGET LIVE"
	case "purged":
		return "COMPLETE"
	case "lock-error":
		return "LOCK ERROR"
	default:
		return strings.ToUpper(phase)
	}
}

func severityForMigrationRunPhase(phase string) severity {
	switch phase {
	case "ready", "applied", "committed", "purged":
		return sevGood
	case "partial", "lock-error":
		return sevBad
	default:
		return sevWarn
	}
}

func issueActionFooter(apps []appView) string {
	hasFix, hasNext := false, false
	for _, app := range apps {
		for _, issue := range app.Issues {
			if issue.FixCommand(app.Name) != "" {
				hasFix = true
			} else if issue.NextStep() != "" {
				hasNext = true
			}
		}
	}
	switch {
	case hasFix && hasNext:
		return fmt.Sprintf("Run the shown `fix:` commands and use the `next:` notes as a checklist, then re-run `%s` to recheck.", bortCommand(""))
	case hasFix:
		return fmt.Sprintf("Run the shown `fix:` commands, then re-run `%s` to recheck.", bortCommand(""))
	case hasNext:
		return fmt.Sprintf("Use the `next:` notes as a checklist. Re-run `%s` after changing Coolify/Dokploy settings or the bundle.", bortCommand(""))
	default:
		return fmt.Sprintf("Resolve the issues above, then re-run `%s` to recheck.", bortCommand(""))
	}
}

func appliedFooter(applied runApplied) string {
	if len(applied.Steps) == 0 {
		return ""
	}
	ok, errs := 0, 0
	for _, step := range applied.Steps {
		switch step.Status {
		case "ok", "skipped":
			ok++
		case "error":
			errs++
		}
	}
	parts := []string{fmt.Sprintf("Applied: %d step(s) recorded", len(applied.Steps))}
	if ok > 0 {
		parts = append(parts, fmt.Sprintf("%d ok", ok))
	}
	if errs > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", errs))
	}
	return strings.Join(parts, " · ")
}

func severityForHealth(h appHealth) severity {
	switch h {
	case appHealthReady:
		return sevGood
	case appHealthBlocked:
		return sevBad
	default:
		return sevWarn
	}
}

func severityForGlyph(g string) severity {
	switch g {
	case "✓":
		return sevGood
	case "!":
		return sevWarn
	case "✕":
		return sevBad
	default:
		return sevDim
	}
}

func severityForReadiness(r preparer.Readiness) severity {
	switch r {
	case preparer.ReadinessBlocked:
		return sevBad
	case preparer.ReadinessNeedsInput:
		return sevWarn
	case preparer.ReadinessNeedsDecision:
		return sevDim
	default:
		return sevGood
	}
}

func overallAppHealth(apps []appView) appHealth {
	overall := appHealthReady
	for _, app := range apps {
		if healthRank(app.Health) > healthRank(overall) {
			overall = app.Health
		}
	}
	return overall
}

func healthGlyph(h appHealth) string {
	switch h {
	case appHealthReady:
		return "✓"
	case appHealthBlocked:
		return "✕"
	default:
		return "!"
	}
}

func severityGlyph(r preparer.Readiness) string {
	switch r {
	case preparer.ReadinessBlocked:
		return "✕"
	case preparer.ReadinessNeedsInput:
		return "!"
	case preparer.ReadinessNeedsDecision:
		return "?"
	default:
		return "·"
	}
}

func writeEnvFileValues(path, templatePath string, values map[string]string) error {
	contents, err := readFileNoFollow(path)
	if err != nil {
		if !os.IsNotExist(err) || templatePath == "" {
			return err
		}
		contents, err = readFileNoFollow(templatePath)
		if err != nil {
			return err
		}
	}
	seen := map[string]struct{}{}
	lines := strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
	for index, line := range lines {
		key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if value, hit := values[key]; hit {
			lines[index] = key + "=" + formatEnvValue(value)
			seen[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if _, ok := seen[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		lines = append(lines, key+"="+formatEnvValue(values[key]))
	}
	if err := writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	if path != templatePath && templatePath != "" {
		return ensureEnvTemplateKeys(templatePath, values)
	}
	return nil
}

func ensureEnvTemplateKeys(path string, values map[string]string) error {
	contents, err := readFileNoFollow(path)
	if err != nil {
		if os.IsNotExist(err) {
			keys := make([]string, 0, len(values))
			for key := range values {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			lines := make([]string, 0, len(keys))
			for _, key := range keys {
				lines = append(lines, key+"=")
			}
			return writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
		}
		return err
	}
	seen := map[string]struct{}{}
	lines := strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
	for _, line := range lines {
		key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		key = strings.TrimSpace(key)
		if ok && key != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			seen[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if _, ok := seen[key]; !ok {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	for _, key := range keys {
		lines = append(lines, key+"=")
	}
	return writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

func formatEnvValue(value string) string {
	if value == "" {
		return ""
	}
	if strings.ContainsAny(value, " \t\n\r#'\"") {
		return strconv.Quote(value)
	}
	return value
}
