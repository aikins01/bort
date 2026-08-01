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
	runRef, currentSelected, ok, err := guideRunRef()
	if err != nil {
		return err
	}
	if ok {
		if !currentSelected {
			run, err := loadMigrationRun(runRef)
			if err != nil {
				return err
			}
			writeAppFirstCockpit(stdout, run)
			fmt.Fprintf(stdout, "This existing run is not selected for mutation. Run `%s` to make it current and refresh its dry-run.\n", bortCommand("migrate --run "+shellQuote(run.Run.Name)))
			return nil
		}
		applyActive, applyActiveErr := applyRunActive(runRef)
		if applyActive || applyActiveErr != nil {
			run, err := loadMigrationRun(runRef)
			if err != nil {
				return err
			}
			if err := rememberCurrentRun(run.Run); err != nil {
				return err
			}
			writeAppFirstCockpit(stdout, run)
			return nil
		}
		run, err := refreshGuideRun(ctx, runRef, stdin, stdout)
		if err != nil {
			return err
		}
		if err := rememberCurrentRun(run.Run); err != nil {
			return err
		}
		if isRealTTY(stdin, stdout) {
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
		if err := rememberCurrentRun(run.Run); err != nil {
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
		if err := rememberCurrentRun(run.Run); err != nil {
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

func refreshGuideRun(ctx context.Context, runRef string, stdin io.Reader, stdout io.Writer) (loadedMigrationRun, error) {
	operationLock, err := acquireRunOperationLock(runRef)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	defer operationLock.Release()
	runDir := filepath.FromSlash(runRef)
	existing, err := readRunMetadata(filepath.Join(runDir, "run.json"))
	if err != nil {
		return loadedMigrationRun{}, err
	}
	hasAppliedSteps, err := runHasAppliedSteps(existing)
	if err != nil {
		return loadedMigrationRun{}, fmt.Errorf("read applied migration progress: %w", err)
	}
	if existing.LiveAppliedAt != nil || existing.CommittedAt != nil || existing.PurgedAt != nil || hasAppliedSteps {
		return loadMigrationRun(runRef)
	}
	if shouldAutoRescanRun(existing) {
		var refreshedBundle, refreshedManifest string
		if isRealTTY(stdin, stdout) {
			stopStatus := startScanStatus(stdout)
			refreshedBundle, refreshedManifest, err = refreshRunSourceBundle(ctx, existing)
			stopStatus(err)
		} else {
			refreshedBundle, refreshedManifest, err = refreshRunSourceBundle(ctx, existing)
		}
		if err != nil {
			return loadedMigrationRun{}, err
		}
		defer os.RemoveAll(refreshedBundle)
		refreshed, err := refreshMigrationRunLockedWithInputs(runRef, refreshedBundle, refreshedManifest)
		if err != nil {
			_ = os.Remove(refreshedManifest)
			return loadedMigrationRun{}, err
		}
		return refreshed, nil
	}
	return refreshMigrationRunLocked(runRef)
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

func runHasAppliedSteps(run migrationRun) (bool, error) {
	run.Artifacts = run.Artifacts.withDefaults()
	runDir := filepath.FromSlash(run.RunDir)
	if runDir == "" {
		return false, fmt.Errorf("migration run has no run directory")
	}
	appliedPath, err := safeRunArtifactPath(runDir, run.Artifacts.Applied)
	if err != nil {
		return false, err
	}
	applied, err := readRunApplied(appliedPath, run)
	if err != nil {
		return false, err
	}
	return len(applied.Steps) > 0, nil
}

func refreshRunSourceBundle(ctx context.Context, run migrationRun) (string, string, error) {
	runDir := filepath.FromSlash(run.RunDir)
	setup := guidedSetup{Source: run.Source, Target: run.Target, ManifestPath: run.ManifestPath}
	m, manifestPath, err := guidedManifest(ctx, setup, runDir)
	if err != nil {
		return "", "", err
	}
	bundleDir, err := exportRunSourceBundle(runDir, m, run.AppName)
	if err != nil {
		_ = os.Remove(manifestPath)
		return "", "", err
	}
	return bundleDir, manifestPath, nil
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
	return createMigrationRunFromSource(ctx, setup, gateway.DefaultObservationWindowSeconds, gateway.DefaultRollbackWindowSeconds)
}

func createMigrationRunFromSource(ctx context.Context, setup guidedSetup, observationWindowSeconds, rollbackWindowSeconds int) (loadedMigrationRun, error) {
	now := time.Now().UTC()
	runDir, runName := newRunDir(setup.RunName, "", "", now)
	if err := ensurePrivateRunDir(runDir); err != nil {
		return loadedMigrationRun{}, err
	}
	operationLock, err := acquireRunOperationLock(runDir)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	defer operationLock.Release()
	if _, err := existingMutableMigrationRun(runDir, runName); err != nil {
		return loadedMigrationRun{}, err
	}

	m, manifestPath, err := guidedManifest(ctx, setup, runDir)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	removeManifest := true
	defer func() {
		if removeManifest {
			_ = os.Remove(manifestPath)
		}
	}()

	bundleDir, err := exportRunSourceBundle(runDir, m, "")
	if err != nil {
		return loadedMigrationRun{}, err
	}
	defer os.RemoveAll(bundleDir)

	run, err := createMigrationRunLocked(migrationRunOptions{
		BundleDir:                bundleDir,
		Target:                   setup.Target,
		RunRef:                   runDir,
		Source:                   setup.Source,
		ManifestPath:             manifestPath,
		ObservationWindowSeconds: observationWindowSeconds,
		RollbackWindowSeconds:    rollbackWindowSeconds,
	}, runDir, runName, now)
	if err != nil {
		return loadedMigrationRun{}, err
	}
	removeManifest = false
	return run, nil
}

func exportRunSourceBundle(runDir string, m manifest.Manifest, appName string) (string, error) {
	bundleDir, err := os.MkdirTemp(runDir, "source-bundle-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(bundleDir, 0o700); err != nil {
		_ = os.RemoveAll(bundleDir)
		return "", err
	}
	if _, err := exporter.Export(m, exporter.Options{OutputDir: bundleDir, AppName: appName, IncludeEnvValues: true}); err != nil {
		_ = os.RemoveAll(bundleDir)
		return "", err
	}
	return bundleDir, nil
}

func guidedManifest(ctx context.Context, setup guidedSetup, runDir string) (manifest.Manifest, string, error) {
	if setup.Source == "manifest" {
		m, err := readManifestFile(setup.ManifestPath)
		if err != nil {
			return manifest.Manifest{}, "", err
		}
		manifestPath, err := writeRunSourceManifest(runDir, m)
		if err != nil {
			return manifest.Manifest{}, "", err
		}
		return m, manifestPath, nil
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
	manifestPath, err := writeRunSourceManifest(runDir, m)
	if err != nil {
		return manifest.Manifest{}, "", err
	}
	return m, manifestPath, nil
}

func writeRunSourceManifest(runDir string, m manifest.Manifest) (string, error) {
	file, err := os.CreateTemp(runDir, "manifest-*.json")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := writeJSONArtifact(path, m); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
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

func guideRunRef() (string, bool, bool, error) {
	current, ok, err := currentRunRef()
	if err != nil {
		return "", false, false, err
	}
	if ok {
		if !migrationRunMetadataExists(current) {
			return "", false, false, fmt.Errorf("current migration run %q no longer exists; run `%s` to select another run", current, bortCommand("migrate --run <name>"))
		}
		return current, true, true, nil
	}
	latest, found := latestRunRef()
	return latest, false, found, nil
}

func selectedRunRef(allowLatest bool) (string, bool, error) {
	current, ok, err := currentRunRef()
	if err != nil {
		return "", false, err
	}
	if ok && migrationRunMetadataExists(current) {
		return current, true, nil
	}
	if ok && !allowLatest {
		return "", false, fmt.Errorf("current migration run %q no longer exists; pass --run to select a run", current)
	}
	if allowLatest {
		latest, found := latestRunRef()
		return latest, found, nil
	}
	return "", false, nil
}

func resolveRunRef(explicit string, allowLatest bool) (string, error) {
	if ref := strings.TrimSpace(explicit); ref != "" {
		return ref, nil
	}
	ref, ok, err := selectedRunRef(allowLatest)
	if err != nil {
		return "", err
	}
	if ok {
		return ref, nil
	}
	return "", fmt.Errorf("no current migration run; run `%s` to start one or pass --run", bortCommand(""))
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
	fmt.Fprintf(w, "%s\n", st.muted(fmt.Sprintf("No migration run was found. Run `%s` in a terminal for guided setup.", bortCommand(""))))
	fmt.Fprintln(w)
	fmt.Fprintln(w, st.emph("For non-interactive setup:"))
	fmt.Fprintf(w, "  %s\n", st.emph(bortCommand("migrate --source coolify-local")))
	fmt.Fprintf(w, "  %s\n", st.muted("or: "+bortCommand("migrate --manifest manifest.json")))
	fmt.Fprintln(w)
	fmt.Fprintln(w, st.muted("This single command scans, exports a private bundle, and creates the current run."))
	return nil
}
