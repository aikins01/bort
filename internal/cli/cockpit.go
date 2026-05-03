package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/aikins01/bort/internal/preparer"
)

func enterInteractiveCockpit(stdin io.Reader, stdout io.Writer, run loadedMigrationRun) error {
	writeAppFirstCockpit(stdout, run)
	_ = stdin
	return nil
}

func enterInteractiveCockpitWithReader(stdin io.Reader, reader *bufio.Reader, stdout io.Writer, run loadedMigrationRun) error {
	writeAppFirstCockpit(stdout, run)
	_ = stdin
	_ = reader
	return nil
}

// writeAppFirstCockpit renders a linear, terraform-style status view of the
// run: one block per app, listing resource health, the issues that need your
// attention, and the exact bort command that fixes each issue.
func writeAppFirstCockpit(w io.Writer, run loadedMigrationRun) {
	apps := appsFromRun(run)
	summary := summarizeMigrationRun(run)
	overall := overallAppHealth(apps)

	fmt.Fprintf(w, "Migration: %s -> %s\n", runSourceLabel(summary.Run), summary.Run.Target)
	fmt.Fprintf(w, "Run: %s\n", summary.Run.Name)
	ready := 0
	for _, app := range apps {
		if app.Health == appHealthReady {
			ready++
		}
	}
	fmt.Fprintf(w, "Status: %s · %d of %d apps ready\n", overall, ready, len(apps))
	fmt.Fprintln(w)

	if len(apps) == 0 {
		fmt.Fprintln(w, "No apps in this run.")
		return
	}

	for _, app := range apps {
		fmt.Fprintf(w, "%s  %s [%s]\n", healthGlyph(app.Health), app.Name, app.Health)
		for _, line := range app.Resources {
			fmt.Fprintf(w, "    %s  %-9s %s\n", line.Status, line.Label, line.Detail)
		}
		if len(app.Issues) > 0 {
			fmt.Fprintln(w, "    What needs your attention:")
			for _, issue := range app.Issues {
				fmt.Fprintf(w, "      %s %s\n", severityGlyph(issue.Severity), issue.Title)
				if issue.Detail != "" {
					fmt.Fprintf(w, "          %s\n", issue.Detail)
				}
				if fix := issue.FixCommand(app.Name); fix != "" {
					fmt.Fprintf(w, "          fix: %s\n", fix)
				}
			}
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "Dry run only: nothing has been sent to the target.")
	if overall != appHealthReady {
		fmt.Fprintln(w, "Resolve the items above, then `bort` again to recheck. Run any shown `fix:` commands to record local answers.")
	} else {
		fmt.Fprintln(w, "All app setup inputs look ready. Use `bort migrate` to refresh the local dry-run migration plan; live execution is not implemented.")
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

type envHint struct {
	Path         string
	TemplatePath string
	MissingKeys  []string
}

func fillEnvFile(stdout io.Writer, input *os.File, hint envHint) error {
	if hint.Path == hint.TemplatePath && strings.HasSuffix(hint.Path, ".example") {
		return fmt.Errorf("refusing to write env values into template file %s", hint.Path)
	}
	values := map[string]string{}
	for _, key := range hint.MissingKeys {
		fmt.Fprintf(stdout, "Enter value for %s in %s (leave blank to keep missing): ", key, hint.Path)
		value, err := readSecretLine(input)
		if err != nil && !(err == io.EOF && value != "") {
			return err
		}
		fmt.Fprintln(stdout)
		if value == "" {
			continue
		}
		values[key] = value
	}
	if len(values) == 0 {
		return nil
	}
	return writeEnvFileValues(filepath.FromSlash(hint.Path), filepath.FromSlash(hint.TemplatePath), values)
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
	if err := writeFileNoFollow(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
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
			return writeFileNoFollow(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
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
	return writeFileNoFollow(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
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
