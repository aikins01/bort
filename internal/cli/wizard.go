package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aikins01/bort/internal/preparer"
	"github.com/charmbracelet/huh"
)

func runWizard(ctx context.Context, run loadedMigrationRun, stdin io.Reader, stdout, stderr io.Writer) error {
	current := run
	if len(current.Applied.Steps) > 0 {
		writeAppFirstCockpit(stdout, current)
		return nil
	}
	for {
		decisions, reviewOnly := nextWizardDecisions(current)
		if len(decisions) == 0 {
			break
		}
		decision := decisions[0]
		var handled, replan bool
		var err error
		if reviewOnly {
			handled, err = promptReviewDecision(current, decision, stdout)
		} else {
			handled, replan, err = promptWizardDecision(ctx, current, decision, stdout)
		}
		if err != nil {
			return err
		}
		if !handled {
			break
		}
		var refreshed loadedMigrationRun
		if replan {
			refreshed, err = refreshMigrationRun(current.Run.RunDir)
		} else {
			refreshed, err = loadMigrationRun(current.Run.RunDir)
		}
		if err != nil {
			return err
		}
		current = refreshed
	}

	writeAppFirstCockpit(stdout, current)
	fmt.Fprintf(stdout, "Live apply is explicit. Run `%s` when you are ready.\n", liveApplyCommand(current))
	return nil
}

func nextWizardDecisions(run loadedMigrationRun) ([]runDecision, bool) {
	if decisions := openSetupDecisions(run); len(decisions) > 0 {
		return decisions, false
	}
	return openReviewDecisions(run), true
}

func promptWizardDecision(ctx context.Context, run loadedMigrationRun, decision runDecision, stdout io.Writer) (bool, bool, error) {
	statePath := defaultStatePath()
	switch decision.Kind {
	case "environment":
		handled, err := promptEnvDecision(ctx, decision, statePath, stdout)
		return handled, handled, err
	case "data_stores":
		handled, err := promptDataStoreDecision(ctx, decision, statePath, stdout)
		return handled, handled, err
	}
	if decision.Readiness != preparer.ReadinessNeedsDecision {
		fmt.Fprintf(stdout, "%s.\n%s.\nOpen %s for details and re-run when resolved.\n", decisionAction(decision), decisionReason(decision), runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Decisions))
		return false, false, nil
	}
	handled, err := promptReviewDecision(run, decision, stdout)
	return handled, false, err
}

func promptEnvDecision(_ context.Context, decision runDecision, statePath string, stdout io.Writer) (bool, error) {
	app := firstApp(decision)
	if app == "" {
		return false, nil
	}
	keys := envKeysFromItems(decision.Items)
	hint := envInputHint(keys)
	var input string
	form := huh.NewForm(huh.NewGroup(
		huh.NewText().
			Title(fmt.Sprintf("Environment values for %s", app)).
			Description(hint + "\nPaste KEY=value lines, one per line. Leave blank to skip.").
			Value(&input),
	))
	if err := form.Run(); err != nil {
		return false, err
	}
	values := parseEnvBlock(input)
	if len(values) == 0 {
		fmt.Fprintln(stdout, "No values entered; skipping.")
		return false, nil
	}
	if err := mutateBortState(statePath, func(state *bortState) bool {
		*state = setAppEnv(*state, app, values)
		return true
	}); err != nil {
		return false, err
	}
	fmt.Fprintf(stdout, "Recorded %d env value(s) for %s.\n", len(values), app)
	return true, nil
}

func promptDataStoreDecision(_ context.Context, decision runDecision, statePath string, stdout io.Writer) (bool, error) {
	stores := storesFromDataItems(decision.Items)
	if len(stores) == 0 {
		return false, nil
	}
	updated := false
	for _, item := range stores {
		strategy := dataStrategyMigrate
		form := huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("Strategy for %s/%s", item.app, item.store)).
				Options(
					huh.NewOption("migrate (dump+restore from source)", dataStrategyMigrate),
					huh.NewOption("recreate (fresh empty store on target)", dataStrategyRecreate),
					huh.NewOption("managed (point app at an external service)", dataStrategyManaged),
				).
				Value(&strategy),
		))
		if err := form.Run(); err != nil {
			return updated, err
		}
		if err := mutateBortState(statePath, func(state *bortState) bool {
			*state = setAppDataStrategy(*state, item.app, item.store, strategy)
			return true
		}); err != nil {
			return updated, err
		}
		fmt.Fprintf(stdout, "Recorded strategy %s for %s/%s.\n", strategy, item.app, item.store)
		updated = true
	}
	return updated, nil
}

func promptReviewDecision(run loadedMigrationRun, decision runDecision, stdout io.Writer) (bool, error) {
	confirm := false
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title(decisionAction(decision)).
			Description(decisionAction(decision) + "\n" + decisionReason(decision)).
			Affirmative("Reviewed").
			Negative("Not yet").
			Value(&confirm),
	))
	if err := form.Run(); err != nil {
		return false, err
	}
	if !confirm {
		return false, nil
	}
	if err := recordReviewDecision(run, decision, time.Now().UTC()); err != nil {
		return false, err
	}
	fmt.Fprintf(stdout, "Review recorded. You can re-run `%s` at any time to revisit the plan.\n", bortCommand(""))
	return true, nil
}

func recordReviewDecision(run loadedMigrationRun, decision runDecision, at time.Time) error {
	operationLock, err := acquireRunOperationLock(run.Run.RunDir)
	if err != nil {
		return err
	}
	defer operationLock.Release()
	current, err := loadMigrationRun(run.Run.RunDir)
	if err != nil {
		return err
	}
	if run.Run.Artifacts.withDefaults() != current.Run.Artifacts.withDefaults() || !run.Run.UpdatedAt.Equal(current.Run.UpdatedAt) {
		return fmt.Errorf("migration run plan changed while review was open; review the current plan and confirm again")
	}
	progress := markReviewDecisionDone(current, decision, at)
	path, err := safeRunArtifactPath(current.Run.RunDir, current.Run.Artifacts.Progress)
	if err != nil {
		return err
	}
	if err := writeRunProgress(path, progress); err != nil {
		return err
	}
	return nil
}

func markReviewDecisionDone(run loadedMigrationRun, decision runDecision, at time.Time) runProgress {
	progress := markDecisionDone(run.Progress, decision, progressStatusResolved, "confirmed in guided review", at)
	selected := map[string]struct{}{}
	for _, item := range decision.Items {
		selected[progressItemKey(item)] = struct{}{}
	}
	for _, current := range openRunDecisions(run) {
		if current.Kind != decision.Kind {
			continue
		}
		for _, item := range current.Items {
			if _, ok := selected[progressItemKey(item)]; ok {
				continue
			}
			state := progress.Decisions[decision.Kind]
			state.Status = progressStatusOpen
			state.Note = ""
			progress.Decisions[decision.Kind] = state
			return progress
		}
	}
	return progress
}

var errDokploySetupSkipped = errors.New("dokploy setup skipped")

func promptInstallAndBootstrapDokploy(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, defaultURL string, reason error) error {
	install := false
	if strings.TrimSpace(defaultURL) == "" {
		defaultURL = "http://127.0.0.1:3030"
	}
	description := fmt.Sprintf("Bort will set up Dokploy in same-VPS shadow mode at %s. This writes system configuration, creates Docker resources, initializes Swarm when needed, and may disable Docker live-restore and reload Docker. Coolify keeps :80/:443 until Bort switches traffic. You may be asked to choose a Coolify admin and enter its password.", defaultURL)
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Install Dokploy now?").
			Description(description).
			Affirmative("Install").
			Negative("Cancel").
			Value(&install),
	))
	if err := form.Run(); err != nil {
		return err
	}
	if !install {
		fmt.Fprintf(stdout, "Skipped Dokploy setup. Run `%s` again when you're ready.\n", bortCommand(""))
		return errDokploySetupSkipped
	}
	return promptInlineInitTargetWithOptions(ctx, stdin, stdout, stderr, inlineInitTargetOptions{Install: true, DefaultURL: defaultURL})
}

func promptInlineInitTarget(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	return promptInlineInitTargetWithOptions(ctx, stdin, stdout, stderr, inlineInitTargetOptions{})
}

type inlineInitTargetOptions struct {
	Install    bool
	DefaultURL string
}

func promptInlineInitTargetWithOptions(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, opts inlineInitTargetOptions) error {
	var url, email string
	if state, err := readBortState(defaultStatePath()); err == nil {
		if creds, ok := state.Targets["dokploy"]; ok {
			url = creds.URL
			email = creds.AdminEmail
		}
	}
	if strings.TrimSpace(opts.DefaultURL) != "" {
		url = opts.DefaultURL
	}
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Dokploy base URL").Placeholder("http://127.0.0.1:3030").Value(&url),
		huh.NewInput().Title("Coolify admin email").Placeholder("press enter to choose from detected admins").Value(&email),
	))
	if err := form.Run(); err != nil {
		return err
	}
	args := []string{}
	if opts.Install {
		args = append(args, "--install")
	}
	if url != "" {
		args = append(args, "--dokploy-url", url)
	}
	if email != "" {
		args = append(args, "--coolify-email", email)
	}
	fmt.Fprintln(stdout, "Setting up Dokploy...")
	return runInitTarget(ctx, args, stdin, stdout, stderr)
}

type appStorePair struct {
	app   string
	store string
}

func storesFromDataItems(items []runDecisionItem) []appStorePair {
	seen := map[appStorePair]struct{}{}
	pairs := []appStorePair{}
	for _, item := range items {
		store := strings.TrimPrefix(item.ResourceRef, "data-store:")
		if store == "" || store == item.ResourceRef {
			continue
		}
		key := appStorePair{app: item.App, store: store}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		pairs = append(pairs, key)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].app != pairs[j].app {
			return pairs[i].app < pairs[j].app
		}
		return pairs[i].store < pairs[j].store
	})
	return pairs
}

func firstApp(decision runDecision) string {
	if len(decision.Apps) > 0 {
		return decision.Apps[0]
	}
	for _, item := range decision.Items {
		if item.App != "" {
			return item.App
		}
	}
	return ""
}

func envInputHint(keys []string) string {
	if len(keys) == 0 {
		return "Enter KEY=value pairs (no specific keys requested)."
	}
	const limit = 8
	shown := keys
	more := 0
	if len(shown) > limit {
		shown = shown[:limit]
		more = len(keys) - limit
	}
	hint := "Required keys: " + strings.Join(shown, ", ")
	if more > 0 {
		hint += fmt.Sprintf(" (+%d more)", more)
	}
	return hint
}

func parseEnvBlock(input string) map[string]string {
	values := map[string]string{}
	for _, raw := range strings.Split(input, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		values[key] = strings.TrimSpace(value)
	}
	return values
}

// runWizardScan wraps the guided manifest fetch with a status message
// so the wizard does not look frozen during a slow Coolify scan.
func runWizardScan(ctx context.Context, setup guidedSetup, stdout io.Writer) (loadedMigrationRun, error) {
	if !canAnimateStatus(stdout) {
		fmt.Fprintln(stdout, "~ Scanning source...")
		return createGuidedMigrationRun(ctx, setup)
	}
	stopStatus := startScanStatus(stdout)
	run, err := createGuidedMigrationRun(ctx, setup)
	stopStatus(err)
	return run, err
}

const scanStatusFrameCount = 3

func canAnimateStatus(w io.Writer) bool {
	file, ok := w.(*os.File)
	return ok && isInteractiveTerminal(file)
}

func startScanStatus(w io.Writer) func(error) {
	done := make(chan error, 1)
	stopped := make(chan struct{})
	st := newStyler(w)
	go func() {
		defer close(stopped)
		defer fmt.Fprint(w, "\x1b[?25h")
		fmt.Fprint(w, "\x1b[?25l")
		frame := 0
		prevHeight := renderScanStatusFrame(w, st, frame, 0)
		ticker := time.NewTicker(180 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case err := <-done:
				renderScanStatusDone(w, st, err, prevHeight)
				return
			case <-ticker.C:
				frame = (frame + 1) % scanStatusFrameCount
				prevHeight = renderScanStatusFrame(w, st, frame, prevHeight)
			}
		}
	}()
	return func(err error) {
		done <- err
		<-stopped
	}
}

func renderScanStatusFrame(w io.Writer, st *styler, frame, prevHeight int) int {
	if prevHeight > 0 {
		fmt.Fprintf(w, "\r\x1b[%dA\x1b[%dM", prevHeight, prevHeight)
	}
	lines := scanStatusLines(st, frame)
	for _, line := range lines {
		fmt.Fprintf(w, "%s\n", line)
	}
	return len(lines)
}

func renderScanStatusDone(w io.Writer, st *styler, err error, prevHeight int) {
	if prevHeight > 0 {
		fmt.Fprintf(w, "\r\x1b[%dA\x1b[%dM", prevHeight, prevHeight)
	}
	line := st.glyph("✓", sevGood) + " Scanned source."
	if err != nil {
		line = st.glyph("!", sevBad) + " Scan failed."
	}
	fmt.Fprintf(w, "%s\n", line)
}

func scanStatusLines(st *styler, frame int) []string {
	count := frame%scanStatusFrameCount + 1
	lines := make([]string, count)
	for i := range lines {
		if i == count-1 {
			lines[i] = st.glyph("~", sevDim) + " Scanning source..."
		} else {
			lines[i] = st.glyph("~", sevDim)
		}
	}
	return lines
}
