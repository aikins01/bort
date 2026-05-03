package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
)

// runData records the migration strategy for a data store in .bort/state.json.
//
//	bort data <app> <store> --recreate|--migrate|--managed
//
// Exactly one strategy flag must be supplied. The recorded strategy is read
// back by `bort` so the user's choice persists across runs.
func runData(_ context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: bort data <app> <store> --recreate|--migrate|--managed")
		return fmt.Errorf("data requires <app> and <store>")
	}
	app := strings.TrimSpace(args[0])
	store := strings.TrimSpace(args[1])
	if app == "" || store == "" {
		return fmt.Errorf("app and store names are required")
	}
	fs := flag.NewFlagSet("data", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var recreate, migrate, managed bool
	fs.BoolVar(&recreate, "recreate", false, "recreate the data store empty on the target")
	fs.BoolVar(&migrate, "migrate", false, "migrate existing data to the target")
	fs.BoolVar(&managed, "managed", false, "point the app at an externally managed store")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}

	strategy, err := pickDataStrategy(recreate, migrate, managed)
	if err != nil {
		return err
	}

	statePath := defaultStatePath()
	state, err := readBortState(statePath)
	if err != nil {
		return err
	}
	state = setAppDataStrategy(state, app, store, strategy)
	if err := writeBortState(statePath, state); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Recorded data strategy %q for %s/%s in .bort/state.json.\n", strategy, app, store)
	return nil
}

func pickDataStrategy(recreate, migrate, managed bool) (string, error) {
	count := 0
	chosen := ""
	if recreate {
		count++
		chosen = dataStrategyRecreate
	}
	if migrate {
		count++
		chosen = dataStrategyMigrate
	}
	if managed {
		count++
		chosen = dataStrategyManaged
	}
	switch count {
	case 0:
		return "", fmt.Errorf("pick exactly one strategy: --recreate, --migrate, or --managed")
	case 1:
		return chosen, nil
	default:
		return "", fmt.Errorf("pick exactly one strategy: --recreate, --migrate, or --managed (got multiple)")
	}
}
