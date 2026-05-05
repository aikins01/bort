package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aikins01/bort/internal/exporter"
	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/source"
	"github.com/charmbracelet/huh"
)

type guidedSetup struct {
	Source       string
	Target       string
	RunName      string
	ManifestPath string
}

type guideChoice struct {
	Value string
	Label string
}

type guidePromptInput interface {
	io.Reader
	guidePromptInput()
}

func runGuide(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	if runRef, ok := latestRunRef(); ok {
		if applyRunActive(runRef) {
			run, err := loadMigrationRun(runRef)
			if err != nil {
				return err
			}
			if isRealTTY(stdin, stdout) {
				return executeWithProgress(ctx, run, stdout, stderr)
			}
			writeAppFirstCockpit(stdout, run)
			return nil
		}
		run, err := refreshGuideRun(ctx, runRef, stdin, stdout)
		if err != nil {
			return err
		}
		if isRealTTY(stdin, stdout) && readyToOfferLiveApply(run) {
			return runWizard(ctx, run, stdin, stdout, stderr)
		}
		writeAppFirstCockpit(stdout, run)
		return nil
	}

	if defaultBundleExists() {
		run, err := createMigrationRun(migrationRunOptions{
			BundleDir:                "bort-bundle",
			Target:                   "dokploy",
			ObservationWindowSeconds: gateway.DefaultObservationWindowSeconds,
			RollbackWindowSeconds:    gateway.DefaultRollbackWindowSeconds,
		})
		if err != nil {
			return err
		}
		writeAppFirstCockpit(stdout, run)
		return nil
	}

	if canPromptGuideWithOutput(stdin, stdout) {
		var setup guidedSetup
		var err error
		if isRealTTY(stdin, stdout) {
			setup, err = promptGuidedSetupHuh(time.Now().UTC())
		} else {
			reader := bufio.NewReader(stdin)
			setup, err = promptGuidedSetupWithReader(reader, stdout, time.Now().UTC())
		}
		if err != nil {
			return err
		}
		var run loadedMigrationRun
		if isRealTTY(stdin, stdout) {
			run, err = runWizardScan(ctx, setup, stdout)
		} else {
			run, err = createGuidedMigrationRun(ctx, setup)
		}
		if err != nil {
			return err
		}
		if isRealTTY(stdin, stdout) {
			return runWizard(ctx, run, stdin, stdout, stderr)
		}
		writeAppFirstCockpit(stdout, run)
		return nil
	}

	return writeGuideStart(stdout)
}

func readyToOfferLiveApply(run loadedMigrationRun) bool {
	return overallAppHealth(appsFromRun(run)) == appHealthReady
}

func refreshGuideRun(ctx context.Context, runRef string, stdin io.Reader, stdout io.Writer) (loadedMigrationRun, error) {
	runDir := filepath.FromSlash(runRef)
	existing, err := readRunMetadata(filepath.Join(runDir, "run.json"))
	if err != nil {
		return loadedMigrationRun{}, err
	}
	if shouldAutoRescanRun(existing) && !runHasAppliedSteps(existing) {
		if isRealTTY(stdin, stdout) {
			stopStatus := startScanStatus(stdout)
			err = refreshRunSourceBundle(ctx, existing)
			stopStatus(err)
		} else {
			err = refreshRunSourceBundle(ctx, existing)
		}
		if err != nil {
			return loadedMigrationRun{}, err
		}
	}
	return refreshMigrationRun(runRef)
}

func shouldAutoRescanRun(run migrationRun) bool {
	sourceName := strings.TrimSpace(run.Source)
	if sourceName == "" || sourceName == "manifest" {
		return false
	}
	runDir := filepath.FromSlash(run.RunDir)
	bundleDir := filepath.FromSlash(run.BundleDir)
	if runDir == "" || bundleDir == "" {
		return false
	}
	return containedPath(runDir, bundleDir) == nil
}

func runHasAppliedSteps(run migrationRun) bool {
	run.Artifacts = run.Artifacts.withDefaults()
	runDir := filepath.FromSlash(run.RunDir)
	if runDir == "" {
		return false
	}
	appliedPath, err := safeRunArtifactPath(runDir, run.Artifacts.Applied)
	if err != nil {
		return false
	}
	applied, err := readRunApplied(appliedPath, run)
	return err == nil && len(applied.Steps) > 0
}

func refreshRunSourceBundle(ctx context.Context, run migrationRun) error {
	runDir := filepath.FromSlash(run.RunDir)
	bundleDir := filepath.FromSlash(run.BundleDir)
	setup := guidedSetup{Source: run.Source, Target: run.Target, ManifestPath: run.ManifestPath}
	m, _, err := guidedManifest(ctx, setup, runDir)
	if err != nil {
		return err
	}
	if err := containedPath(runDir, bundleDir); err != nil {
		return err
	}
	if err := os.RemoveAll(bundleDir); err != nil {
		return err
	}
	_, err = exporter.Export(m, exporter.Options{OutputDir: bundleDir, AppName: run.AppName, IncludeEnvValues: true})
	return err
}

// isRealTTY reports whether stdin and stdout are both attached to an
// actual terminal device (not a *guidePromptInput test fake), which is
// the only situation where the huh-based prompt can render correctly.
func isRealTTY(stdin io.Reader, stdout io.Writer) bool {
	inFile, ok := stdin.(*os.File)
	if !ok {
		return false
	}
	outFile, ok := stdout.(*os.File)
	if !ok {
		return false
	}
	return isInteractiveTerminal(inFile) && isInteractiveTerminal(outFile)
}

func promptGuidedSetupHuh(now time.Time) (guidedSetup, error) {
	source := "coolify-local"
	manifestPath := "manifest.json"

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Where are your apps?").
				Description("All actions are local dry-runs. bort runs on the same server as the source PaaS.").
				Options(
					huh.NewOption("Coolify on this server (reads local Docker labels)", "coolify-local"),
					huh.NewOption("Coolify API (uses BORT_COOLIFY_URL/TOKEN)", "coolify"),
					huh.NewOption("Local Docker", "docker"),
					huh.NewOption("Existing manifest file", "manifest"),
				).
				Value(&source),
		),
		huh.NewGroup(
			huh.NewInput().
				Title("Manifest path").
				Placeholder("manifest.json").
				Value(&manifestPath),
		).WithHideFunc(func() bool { return source != "manifest" }),
	)

	if err := form.Run(); err != nil {
		return guidedSetup{}, err
	}

	setup := guidedSetup{
		Source:  source,
		Target:  "dokploy",
		RunName: defaultGuideRunName(source, now),
	}
	if source == "manifest" {
		setup.ManifestPath = strings.TrimSpace(manifestPath)
		if setup.ManifestPath == "" {
			setup.ManifestPath = "manifest.json"
		}
	}
	return setup, nil
}

func promptGuidedSetupWithReader(reader *bufio.Reader, stdout io.Writer, now time.Time) (guidedSetup, error) {
	st := newStyler(stdout)
	fmt.Fprintln(stdout, st.emph("Migration setup"))
	fmt.Fprintln(stdout, st.muted("All actions are local dry-runs. bort runs on the same server as the source PaaS."))
	fmt.Fprintln(stdout)

	sourceName, err := promptChoice(reader, stdout, "Where are your apps?", []guideChoice{
		{Value: "coolify-local", Label: "Coolify on this server (reads local Docker labels)"},
		{Value: "coolify", Label: "Coolify API (uses BORT_COOLIFY_URL/TOKEN)"},
		{Value: "docker", Label: "Local Docker"},
		{Value: "manifest", Label: "Existing manifest file"},
	}, "coolify-local")
	if err != nil {
		return guidedSetup{}, err
	}

	setup := guidedSetup{
		Source:  sourceName,
		Target:  "dokploy",
		RunName: defaultGuideRunName(sourceName, now),
	}
	if sourceName == "manifest" {
		manifestPath, err := promptLineDefault(reader, stdout, "Manifest path", "manifest.json")
		if err != nil {
			return guidedSetup{}, err
		}
		setup.ManifestPath = manifestPath
	}
	return setup, nil
}

func createGuidedMigrationRun(ctx context.Context, setup guidedSetup) (loadedMigrationRun, error) {
	runDir, _ := newRunDir(setup.RunName, "", "", time.Now().UTC())
	if err := ensurePrivateRunDir(runDir); err != nil {
		return loadedMigrationRun{}, err
	}

	m, manifestPath, err := guidedManifest(ctx, setup, runDir)
	if err != nil {
		return loadedMigrationRun{}, err
	}

	bundleDir := filepath.Join(runDir, "bundle")
	if _, err := exporter.Export(m, exporter.Options{OutputDir: bundleDir, IncludeEnvValues: true}); err != nil {
		return loadedMigrationRun{}, err
	}

	return createMigrationRun(migrationRunOptions{
		BundleDir:                bundleDir,
		Target:                   setup.Target,
		RunRef:                   runDir,
		Source:                   setup.Source,
		ManifestPath:             manifestPath,
		ObservationWindowSeconds: gateway.DefaultObservationWindowSeconds,
		RollbackWindowSeconds:    gateway.DefaultRollbackWindowSeconds,
	})
}

func guidedManifest(ctx context.Context, setup guidedSetup, runDir string) (manifest.Manifest, string, error) {
	if setup.Source == "manifest" {
		m, err := readManifestFile(setup.ManifestPath)
		return m, setup.ManifestPath, err
	}

	scanOptions := source.ScanOptions{
		IncludeEnvValues: true,
		Coolify: source.CoolifyOptions{
			BaseURL: os.Getenv("BORT_COOLIFY_URL"),
			Token:   os.Getenv("BORT_COOLIFY_TOKEN"),
		},
	}
	scanner, err := scannerFor(setup.Source, scanOptions)
	if err != nil {
		return manifest.Manifest{}, "", err
	}
	m, err := scanner.Scan(ctx, scanOptions)
	if err != nil {
		return manifest.Manifest{}, "", err
	}
	manifestPath := filepath.Join(runDir, "manifest.json")
	if err := writeJSONArtifact(manifestPath, m); err != nil {
		return manifest.Manifest{}, "", err
	}
	return m, manifestPath, nil
}

func readManifestFile(path string) (manifest.Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return manifest.Manifest{}, err
	}
	defer file.Close()

	var m manifest.Manifest
	if err := json.NewDecoder(file).Decode(&m); err != nil {
		return manifest.Manifest{}, err
	}
	return m, nil
}

func promptChoice(reader *bufio.Reader, stdout io.Writer, label string, choices []guideChoice, defaultValue string) (string, error) {
	st := newStyler(stdout)
	fmt.Fprintln(stdout, st.emph(label))
	defaultIdx := -1
	for index, choice := range choices {
		line := fmt.Sprintf("  %d. %s", index+1, choice.Label)
		if choice.Value == defaultValue {
			defaultIdx = index + 1
			line = st.emph(line) + " " + st.muted("(default)")
		}
		fmt.Fprintln(stdout, line)
	}

	for {
		hint := fmt.Sprintf("[1-%d, default %d]", len(choices), defaultIdx)
		answer, err := promptLine(reader, stdout, st.muted(hint))
		if err != nil {
			return "", err
		}
		answer = strings.TrimSpace(answer)
		if answer == "" {
			return defaultValue, nil
		}
		for index, choice := range choices {
			if answer == fmt.Sprint(index+1) || strings.EqualFold(answer, choice.Value) {
				return choice.Value, nil
			}
		}
		fmt.Fprintln(stdout, st.glyph(fmt.Sprintf("Choose 1-%d.", len(choices)), sevWarn))
	}
}

func promptLineDefault(reader *bufio.Reader, stdout io.Writer, label, defaultValue string) (string, error) {
	prompt := label
	if defaultValue != "" {
		prompt += " [" + defaultValue + "]"
	}
	answer, err := promptLine(reader, stdout, prompt)
	if err != nil {
		return "", err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return defaultValue, nil
	}
	return answer, nil
}

func promptLine(reader *bufio.Reader, stdout io.Writer, label string) (string, error) {
	fmt.Fprintf(stdout, "%s: ", label)
	line, err := reader.ReadString('\n')
	if err != nil && !(err == io.EOF && line != "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func defaultGuideRunName(sourceName string, now time.Time) string {
	name := sourceName
	if name == "" {
		name = "migration"
	}
	return defaultRunName(name, "", now)
}

func canPromptGuide(stdin io.Reader) bool {
	if _, ok := stdin.(guidePromptInput); ok {
		return true
	}
	file, ok := stdin.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0 && isTerminalFile(file)
}

func canPromptGuideWithOutput(stdin io.Reader, stdout io.Writer) bool {
	if _, ok := stdin.(guidePromptInput); ok {
		return true
	}
	if !canPromptGuide(stdin) {
		return false
	}
	file, ok := stdout.(*os.File)
	return ok && isInteractiveTerminal(file)
}

func latestRunRef() (string, bool) {
	entries, err := os.ReadDir(filepath.Join(".bort", "runs"))
	if err != nil {
		return "", false
	}

	runs := []runCandidate{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(".bort", "runs", entry.Name())
		info, err := os.Stat(filepath.Join(path, "run.json"))
		if err != nil {
			continue
		}
		runs = append(runs, runCandidate{Path: path, UpdatedAt: info.ModTime()})
	}
	if len(runs) == 0 {
		return "", false
	}
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].UpdatedAt.After(runs[j].UpdatedAt)
	})
	return runs[0].Path, true
}

type runCandidate struct {
	Path      string
	UpdatedAt time.Time
}

func defaultBundleExists() bool {
	info, err := os.Stat(filepath.Join("bort-bundle", "index.json"))
	return err == nil && !info.IsDir()
}

func writeGuideStart(w io.Writer) error {
	st := newStyler(w)
	fmt.Fprintln(w, st.emph("bort")+" "+st.muted("— migrate self-hosted apps between PaaS platforms"))
	fmt.Fprintln(w)
	fmt.Fprintln(w, st.muted("No local migration run or default bundle was found."))
	fmt.Fprintln(w)
	fmt.Fprintln(w, st.emph("To get started:"))
	for _, cmd := range []string{
		"bort scan --output manifest.json",
		"bort export --manifest manifest.json --output-dir bort-bundle",
		"bort",
	} {
		fmt.Fprintf(w, "  %s\n", st.emph(cmd))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, st.muted("See `bort help` for more."))
	return nil
}
