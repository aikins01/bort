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
)

const (
	guideEnvRedacted = "redacted"
	guideEnvInclude  = "include-values"
)

type guidedSetup struct {
	Source       string
	Target       string
	EnvMode      string
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
		run, err := loadMigrationRun(runRef)
		if err != nil {
			return err
		}
		writeMigrationCockpitText(stdout, summarizeMigrationRun(run))
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
		writeMigrationCockpitText(stdout, summarizeMigrationRun(run))
		return nil
	}

	if canPromptGuide(stdin) {
		setup, err := promptGuidedSetup(stdin, stdout, time.Now().UTC())
		if err != nil {
			return err
		}
		run, err := createGuidedMigrationRun(ctx, setup)
		if err != nil {
			return err
		}
		writeMigrationCockpitText(stdout, summarizeMigrationRun(run))
		return nil
	}

	return writeGuideStart(stdout)
}

func promptGuidedSetup(stdin io.Reader, stdout io.Writer, now time.Time) (guidedSetup, error) {
	reader := bufio.NewReader(stdin)
	fmt.Fprintln(stdout, "Migration setup")
	fmt.Fprintln(stdout, "All actions are local dry-runs until live execution is implemented.")
	fmt.Fprintln(stdout)

	sourceName, err := promptChoice(reader, stdout, "Source", []guideChoice{
		{Value: "coolify-local", Label: "Coolify on this server (local Docker labels, no API writes)"},
		{Value: "coolify", Label: "Coolify API (requires BORT_COOLIFY_URL and BORT_COOLIFY_TOKEN)"},
		{Value: "docker", Label: "Local Docker"},
		{Value: "manifest", Label: "Existing manifest"},
	}, "coolify-local")
	if err != nil {
		return guidedSetup{}, err
	}

	target, err := promptChoice(reader, stdout, "Target", []guideChoice{{Value: "dokploy", Label: "Dokploy"}}, "dokploy")
	if err != nil {
		return guidedSetup{}, err
	}

	envMode, err := promptChoice(reader, stdout, "Environment", []guideChoice{
		{Value: guideEnvRedacted, Label: "Redact env values and fill private templates later"},
		{Value: guideEnvInclude, Label: "Include known env values in private local files"},
	}, guideEnvRedacted)
	if err != nil {
		return guidedSetup{}, err
	}
	if envMode == guideEnvInclude {
		fmt.Fprintln(stdout, "This may read environment values and secrets into local 0600 files. Values will not be printed.")
		confirmed, err := promptLine(reader, stdout, "Type include to continue, or press Enter to use redacted mode")
		if err != nil {
			return guidedSetup{}, err
		}
		if !strings.EqualFold(strings.TrimSpace(confirmed), "include") {
			envMode = guideEnvRedacted
		}
	}

	defaultName := defaultGuideRunName(sourceName, now)
	runName, err := promptLineDefault(reader, stdout, "Run name", defaultName)
	if err != nil {
		return guidedSetup{}, err
	}

	setup := guidedSetup{Source: sourceName, Target: target, EnvMode: envMode, RunName: runName}
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
	if _, err := exporter.Export(m, exporter.Options{OutputDir: bundleDir, IncludeEnvValues: setup.EnvMode == guideEnvInclude}); err != nil {
		return loadedMigrationRun{}, err
	}

	return createMigrationRun(migrationRunOptions{
		BundleDir:                bundleDir,
		Target:                   setup.Target,
		RunRef:                   runDir,
		Source:                   setup.Source,
		EnvMode:                  setup.EnvMode,
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
		IncludeEnvValues: setup.EnvMode == guideEnvInclude,
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
	fmt.Fprintf(stdout, "%s:\n", label)
	for index, choice := range choices {
		marker := ""
		if choice.Value == defaultValue {
			marker = " [default]"
		}
		fmt.Fprintf(stdout, "  %d. %s%s\n", index+1, choice.Label, marker)
	}

	for {
		answer, err := promptLine(reader, stdout, label)
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
		fmt.Fprintf(stdout, "Choose 1-%d.\n", len(choices))
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
	_, err := fmt.Fprint(w, `bort migrates self-hosted apps between PaaS platforms.

No local migration run or default bundle was found.

Start with a local bundle:
  bort scan --output manifest.json
  bort export --manifest manifest.json --output-dir bort-bundle
  bort

If you already have a bundle elsewhere:
  bort migrate --bundle <bundle-dir>

Power-user commands are still available with:
  bort help
`)
	return err
}
