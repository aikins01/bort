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
func runEnv(_ context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: bort env <app> KEY=value [KEY=value ...]")
		return fmt.Errorf("env requires <app> and at least one KEY=value")
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
	state, err := readBortState(statePath)
	if err != nil {
		return err
	}
	state = setAppEnv(state, app, values)
	if err := writeBortState(statePath, state); err != nil {
		return err
	}

	keys := sortedAppEnvKeys(values)
	fmt.Fprintf(stdout, "Recorded %d env value(s) for %s: %s\n", len(values), app, strings.Join(keys, ", "))
	fmt.Fprintln(stdout, "Stored in .bort/state.json (mode 0600). Values are never printed.")
	return nil
}
