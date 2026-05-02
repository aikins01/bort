package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/aikins01/bort/internal/preparer"
)

func runPrepare(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var bundleDir string
	var target string
	var appName string
	var format string

	fs.StringVar(&bundleDir, "bundle", "bort-bundle", "migration bundle directory")
	fs.StringVar(&target, "target", "dokploy", "target platform")
	fs.StringVar(&appName, "app", "", "optional app name to prepare")
	fs.StringVar(&format, "format", "text", "output format: text, json")

	if err := fs.Parse(args); err != nil {
		return err
	}

	result, err := preparer.Plan(preparer.Options{BundleDir: bundleDir, Target: target, AppName: appName})
	if err != nil {
		return err
	}

	switch format {
	case "text":
		writePrepareText(stdout, result)
	case "json":
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(result); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported prepare format %q", format)
	}

	return nil
}

func writePrepareText(w io.Writer, result preparer.Result) {
	fmt.Fprintf(w, "Prepare plan: %s -> %s\n", result.BundleDir, result.Target)
	fmt.Fprintf(w, "Status: %s\n\n", result.Status)

	for _, app := range result.Apps {
		fmt.Fprintf(w, "[%s] %s\n", app.Status, app.Name)
		fmt.Fprintf(w, "  readiness: %s\n", app.Readiness)
		if app.Resources.App.Type != "" {
			fmt.Fprintf(w, "  target shell: %s %s from %s\n", app.Resources.App.Readiness, app.Resources.App.Type, app.Resources.App.ComposePath)
		}
		if app.TargetResources != nil && app.TargetResources.Dokploy != nil {
			dokploy := app.TargetResources.Dokploy
			fmt.Fprintf(w, "  dokploy dry-run: compose app %s (%s), %d domains, %d env files, %d volumes\n", dokploy.ComposeApp.Name, dokploy.ComposeApp.Readiness, len(dokploy.Domains), len(dokploy.EnvFiles), len(dokploy.Volumes))
		}
		if len(app.Gates) > 0 {
			fmt.Fprintln(w, "  gates:")
			for _, gate := range app.Gates {
				fmt.Fprintf(w, "    %s %s: %s\n", gate.Severity, gate.Code, gate.Message)
			}
		}
		if len(app.Actions) == 0 {
			fmt.Fprintln(w, "  no actions")
		} else {
			for _, action := range app.Actions {
				fmt.Fprintf(w, "  %s %s: %s\n", action.Severity, action.Kind, action.Message)
			}
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "Dry run only: no resources were created or changed.")
}
