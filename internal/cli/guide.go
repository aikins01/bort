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
		run, err := refreshMigrationRun(runRef)
		if err != nil {
			return err
		}
		return enterInteractiveCockpit(stdin, stdout, run)
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
		return enterInteractiveCockpit(stdin, stdout, run)
	}

	if canPromptGuideWithOutput(stdin, stdout) {
		reader := bufio.NewReader(stdin)
		setup, err := promptGuidedSetupWithReader(reader, stdout, time.Now().UTC())
		if err != nil {
			return err
		}
		run, err := createGuidedMigrationRun(ctx, setup)
		if err != nil {
			return err
		}
		return enterInteractiveCockpitWithReader(stdin, reader, stdout, run)
	}

	return writeGuideStart(stdout)
}

func promptGuidedSetupWithReader(reader *bufio.Reader, stdout io.Writer, now time.Time) (guidedSetup, error) {
	fmt.Fprintln(stdout, "Migration setup")
	fmt.Fprintln(stdout, "All actions are local dry-runs. bort runs on the same server as the source PaaS.")
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
	_, err := fmt.Fprint(w, `bort migrates self-hosted apps between PaaS platforms.

No local migration run or default bundle was found.

To get started:
  bort scan --output manifest.json
  bort export --manifest manifest.json --output-dir bort-bundle
  bort

See bort help for more.
`)
	return err
}
