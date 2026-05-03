package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aikins01/bort/internal/preparer"
)

// validateStateApp returns an error if a latest run exists and `app`
// does not match any non-platform app in the prepare plan. When no run
// exists yet we accept the input so users can pre-record values before
// the first scan.
func validateStateApp(app string) error {
	run, ok := loadLatestRunForValidation()
	if !ok {
		return nil
	}
	if appExistsInRun(run, app) {
		return nil
	}
	known := knownAppNames(run)
	if len(known) == 0 {
		return fmt.Errorf("app %q not found in latest run %q", app, run.Run.Name)
	}
	return fmt.Errorf("app %q not found in latest run %q; known apps: %s", app, run.Run.Name, strings.Join(known, ", "))
}

// validateStateDataStore validates that `app` exists and `store` matches
// one of the data stores (by Service or Kind) recorded for that app. If
// no run exists yet, the input is accepted.
func validateStateDataStore(app, store string) error {
	run, ok := loadLatestRunForValidation()
	if !ok {
		return nil
	}
	plan, found := findRunApp(run, app)
	if !found {
		known := knownAppNames(run)
		if len(known) == 0 {
			return fmt.Errorf("app %q not found in latest run %q", app, run.Run.Name)
		}
		return fmt.Errorf("app %q not found in latest run %q; known apps: %s", app, run.Run.Name, strings.Join(known, ", "))
	}
	if dataStoreMatches(plan, store) {
		return nil
	}
	known := knownDataStores(plan)
	if len(known) == 0 {
		return fmt.Errorf("data store %q not found for app %q (no data stores detected)", store, app)
	}
	return fmt.Errorf("data store %q not found for app %q; known stores: %s", store, app, strings.Join(known, ", "))
}

func loadLatestRunForValidation() (loadedMigrationRun, bool) {
	ref, ok := latestRunRef()
	if !ok {
		return loadedMigrationRun{}, false
	}
	run, err := loadMigrationRun(ref)
	if err != nil {
		return loadedMigrationRun{}, false
	}
	return run, true
}

func appExistsInRun(run loadedMigrationRun, app string) bool {
	_, ok := findRunApp(run, app)
	return ok
}

func findRunApp(run loadedMigrationRun, app string) (preparer.AppPlan, bool) {
	for _, plan := range run.Prepare.Apps {
		if isPlatformRunApp(plan.Role) {
			continue
		}
		if plan.Name == app {
			return plan, true
		}
	}
	return preparer.AppPlan{}, false
}

func knownAppNames(run loadedMigrationRun) []string {
	names := []string{}
	for _, plan := range run.Prepare.Apps {
		if isPlatformRunApp(plan.Role) {
			continue
		}
		if plan.Name == "" {
			continue
		}
		names = append(names, plan.Name)
	}
	sort.Strings(names)
	return names
}

func dataStoreMatches(plan preparer.AppPlan, store string) bool {
	for _, ds := range plan.Resources.DataStores {
		if ds.Service == store || ds.Kind == store {
			return true
		}
	}
	return false
}

func knownDataStores(plan preparer.AppPlan) []string {
	seen := map[string]struct{}{}
	names := []string{}
	for _, ds := range plan.Resources.DataStores {
		for _, key := range []string{ds.Service, ds.Kind} {
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			names = append(names, key)
		}
	}
	sort.Strings(names)
	return names
}
