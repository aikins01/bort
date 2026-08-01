package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// runEnv records environment variable values for an app in .bort/state.json.
//
//	bort env <app> KEY=value [KEY=value ...]
//
// Values are stored locally and never printed back. The command does not
// modify the source apps; subsequent `bort` runs read the recorded values
// instead of asking again.
func runEnv(_ context.Context, args []string, stdout, _ io.Writer) error {
	usage := "usage: " + bortCommand("env <app> KEY=value [KEY=value ...]")
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stdout, usage)
		return nil
	}
	if len(args) < 2 {
		return fmt.Errorf("%s", usage)
	}
	app := strings.TrimSpace(args[0])
	if app == "" {
		return fmt.Errorf("app name is required")
	}
	values, err := parseKeyValueArgs(args[1:])
	if err != nil {
		return err
	}
	if err := validateStateApp(app); err != nil {
		return err
	}

	statePath := defaultStatePath()
	if err := mutateBortState(statePath, func(state *bortState) bool {
		*state = setAppEnv(*state, app, values)
		return true
	}); err != nil {
		return err
	}

	st := newStyler(stdout)
	keys := sortedAppEnvKeys(values)
	fmt.Fprintf(stdout, "%s Recorded %s for %s: %s\n",
		st.glyph("✓", sevGood),
		pluralize(len(values), "env value", "env values"),
		st.emph(app),
		strings.Join(keys, ", "),
	)
	fmt.Fprintf(stdout, "%s\n", st.muted(fmt.Sprintf("Stored in .bort/state.json (mode 0600). Run `%s` to recheck.", bortCommand(""))))
	return nil
}

func pluralize(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
