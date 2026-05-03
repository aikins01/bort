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
	overall := overallAppHealth(apps)
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
	header += " " + st.pill("DRY RUN", sevWarn)
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
	if overall != appHealthReady {
		fmt.Fprintln(w, st.muted("Run the shown `fix:` commands, then re-run `bort` to recheck."))
	} else {
		fmt.Fprintln(w, st.muted("All app inputs ready. Use `bort migrate` to refresh dry-run artifacts (live execution not yet implemented)."))
	}
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
