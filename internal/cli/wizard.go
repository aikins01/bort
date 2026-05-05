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

	"github.com/aikins01/bort/internal/target/dokploy"
	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

// runWizard drives the interactive flow after a migration run is created.
// it loops over open decisions, persisting each answer to .bort/state.json
// and re-planning, then offers an apply preview when everything is green.
func runWizard(ctx context.Context, run loadedMigrationRun, stdin io.Reader, stdout, stderr io.Writer) error {
	current := run
	for {
		decisions := openRunDecisions(current)
		if len(decisions) == 0 {
			break
		}
		decision := decisions[0]
		handled, err := promptWizardDecision(ctx, current, decision, stdout)
		if err != nil {
			return err
		}
		if !handled {
			break
		}
		refreshed, err := refreshMigrationRun(current.Run.RunDir)
		if err != nil {
			return err
		}
		current = refreshed
	}

	writeAppFirstCockpit(stdout, current)
	fmt.Fprintf(stdout, "Live apply is explicit. Run `%s` when you are ready.\n", liveApplyCommand(current))
	return nil
}

// confirmCommitApply gates the wizard's source-retirement step behind an
// explicit confirm. it runs only after the live apply succeeded so the
// operator can poke at the target before stopping the source — this is
// the last reversible moment without a manual docker start.
func confirmCommitApply(run loadedMigrationRun) (bool, error) {
	plan := dokploy.PlanForCommit(run.Prepare, run.Cutover)
	confirm := false
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Retire source containers now?").
			Description(fmt.Sprintf("Will stop %d source container group(s) plus coolify-proxy. Source is left in place — `docker start` recovers it.", len(plan.Steps)-1)).
			Affirmative("Retire").
			Negative("Skip").
			Value(&confirm),
	))
	if err := form.Run(); err != nil {
		return false, err
	}
	return confirm, nil
}

func promptWizardDecision(ctx context.Context, run loadedMigrationRun, decision runDecision, stdout io.Writer) (bool, error) {
	statePath := defaultStatePath()
	state, err := readBortState(statePath)
	if err != nil {
		return false, err
	}
	switch decision.Kind {
	case "environment":
		return promptEnvDecision(ctx, decision, state, statePath, stdout)
	case "data_stores":
		return promptDataStoreDecision(ctx, decision, state, statePath, stdout)
	case "routes":
		return promptRoutesDecision(decision, stdout)
	}
	fmt.Fprintf(stdout, "%s.\n%s.\nOpen %s for details and re-run when resolved.\n", decisionAction(decision), decisionReason(decision), runArtifactPath(run.Run.RunDir, run.Run.Artifacts.Decisions))
	return false, nil
}

func promptEnvDecision(_ context.Context, decision runDecision, state bortState, statePath string, stdout io.Writer) (bool, error) {
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
	state = setAppEnv(state, app, values)
	if err := writeBortState(statePath, state); err != nil {
		return false, err
	}
	fmt.Fprintf(stdout, "Recorded %d env value(s) for %s.\n", len(values), app)
	return true, nil
}

func promptDataStoreDecision(_ context.Context, decision runDecision, state bortState, statePath string, stdout io.Writer) (bool, error) {
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
		state = setAppDataStrategy(state, item.app, item.store, strategy)
		if err := writeBortState(statePath, state); err != nil {
			return updated, err
		}
		fmt.Fprintf(stdout, "Recorded strategy %s for %s/%s.\n", strategy, item.app, item.store)
		updated = true
	}
	return updated, nil
}

func promptRoutesDecision(decision runDecision, stdout io.Writer) (bool, error) {
	confirm := false
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Confirm route plan").
			Description(decisionAction(decision) + "\n" + decisionReason(decision)).
			Affirmative("Continue").
			Negative("Skip for now").
			Value(&confirm),
	))
	if err := form.Run(); err != nil {
		return false, err
	}
	if !confirm {
		fmt.Fprintln(stdout, "Routes left as-is; rerun the wizard after editing the bundle if needed.")
		return false, nil
	}
	return false, nil
}

func confirmApply(run loadedMigrationRun) (bool, error) {
	confirm := false
	planned := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover)
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Apply this migration to dokploy now?").
			Description(fmt.Sprintf("Will execute %d step(s) against %s. You can also skip and run `%s` later.", len(planned.Steps), run.Run.Target, liveApplyCommand(run))).
			Affirmative("Apply").
			Negative("Skip").
			Value(&confirm),
	))
	if err := form.Run(); err != nil {
		return false, err
	}
	return confirm, nil
}

var errDokploySetupSkipped = errors.New("dokploy setup skipped")

func promptInstallAndBootstrapDokploy(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, defaultURL string, reason error) error {
	install := false
	if strings.TrimSpace(defaultURL) == "" {
		defaultURL = "http://127.0.0.1:3030"
	}
	description := fmt.Sprintf("Bort will set up Dokploy in same-VPS shadow mode at %s and keep Coolify on :80/:443 until cutover. You may be asked to choose a Coolify admin and enter its password.", defaultURL)
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
		fmt.Fprintln(stdout, "Skipped Dokploy setup. Run `bort` again when you're ready.")
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

func executeWithProgress(ctx context.Context, run loadedMigrationRun, stdout, stderr io.Writer) error {
	planned := dokploy.PlanFromArtifacts(run.Prepare, run.Sync, run.Cutover)
	total := len(planned.Steps)
	if total == 0 {
		return applyLiveMigration(ctx, run, stderr, nil)
	}

	bar := progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage())
	bar.Width = 40
	model := &progressModel{bar: bar, total: total, done: completedApplyPrefix(planned.Steps, run.Applied)}
	program := tea.NewProgram(model, tea.WithOutput(stderr))

	var applyErr error
	go func() {
		applyErr = applyLiveMigration(ctx, run, stderr, func(p dokploy.StepProgress) {
			program.Send(progressTick{progress: p})
		})
		program.Send(progressDone{err: applyErr})
	}()

	if _, err := program.Run(); err != nil && !errors.Is(err, tea.ErrInterrupted) {
		return err
	}
	if applyErr != nil {
		return applyErr
	}
	return nil
}

type progressTick struct {
	progress dokploy.StepProgress
}

type progressDone struct {
	err error
}

type progressModel struct {
	bar     progress.Model
	total   int
	done    int
	current dokploy.StepProgress
	err     error
	closed  bool
}

func (m *progressModel) Init() tea.Cmd {
	if m.total <= 0 || m.done <= 0 {
		return nil
	}
	percent := float64(m.done) / float64(m.total)
	if percent > 1 {
		percent = 1
	}
	return m.bar.SetPercent(percent)
}

func (m *progressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case progressTick:
		m.current = msg.progress
		if m.total > 0 && msg.progress.Index >= 0 && msg.progress.Index < m.total {
			done := msg.progress.Index
			if msg.progress.Status == dokploy.StepStatusOK || msg.progress.Status == dokploy.StepStatusSkipped {
				done++
			}
			if done > m.done {
				m.done = done
			}
		}
		percent := 0.0
		if m.total > 0 {
			percent = float64(m.done) / float64(m.total)
			if percent > 1 {
				percent = 1
			}
		}
		return m, m.bar.SetPercent(percent)
	case progressDone:
		m.err = msg.err
		m.closed = true
		return m, tea.Quit
	case progress.FrameMsg:
		newModel, cmd := m.bar.Update(msg)
		m.bar = newModel.(progress.Model)
		return m, cmd
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *progressModel) View() string {
	var b strings.Builder
	b.WriteString(m.bar.View())
	b.WriteString("\n")
	if m.current.Step.Kind != "" {
		b.WriteString(fmt.Sprintf("  %d/%d  %s %s/%s", m.done, m.total, m.current.Status, m.current.Step.App, m.current.Step.Kind))
	}
	if m.closed {
		if m.err != nil {
			b.WriteString("\n  failed: ")
			b.WriteString(m.err.Error())
		} else {
			b.WriteString("\n  done")
		}
	}
	b.WriteString("\n")
	return b.String()
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
