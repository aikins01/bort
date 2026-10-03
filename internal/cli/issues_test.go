package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/preparer"
)

func TestClassifyItemMapsCodesToIssueKinds(t *testing.T) {
	cases := []struct {
		code string
		want issueKind
	}{
		{"env.missing_value", issueKindEnv},
		{"data_store.unknown", issueKindData},
		{"domain.host_missing", issueKindRoute},
		{"routes.none", issueKindRoute},
		{"linked_resource.confirm", issueKindLink},
		{"external_requirement.unresolved", issueKindLink},
		{"volume.bind_mount_review", issueKindHostFiles},
		{"cutover.observation_pending", issueKindReview},
		{"random.code", issueKindReview},
	}
	for _, tc := range cases {
		got := classifyItem(runDecisionItem{Code: tc.code})
		if got != tc.want {
			t.Errorf("classifyItem(%q) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

func TestAppHealthFromIssuesPicksWorstSeverity(t *testing.T) {
	if got := appHealthFromIssues(nil); got != appHealthReady {
		t.Errorf("expected ready for empty issues, got %q", got)
	}
	needsWork := []appIssue{{Severity: preparer.ReadinessNeedsDecision}}
	if got := appHealthFromIssues(needsWork); got != appHealthNeedsWork {
		t.Errorf("expected needs work, got %q", got)
	}
	blocked := []appIssue{
		{Severity: preparer.ReadinessNeedsInput},
		{Severity: preparer.ReadinessBlocked},
	}
	if got := appHealthFromIssues(blocked); got != appHealthBlocked {
		t.Errorf("expected blocked, got %q", got)
	}
}

func TestIssuesForAppGroupsAndOrders(t *testing.T) {
	items := []runDecisionItem{
		{Code: "env.missing_value", Readiness: preparer.ReadinessNeedsInput, App: "x"},
		{Code: "env.unredacted", Readiness: preparer.ReadinessNeedsDecision, App: "x"},
		{Code: "data_store.unknown", Readiness: preparer.ReadinessNeedsDecision, App: "x"},
	}
	issues := issuesForApp(items)
	if len(issues) != 2 {
		t.Fatalf("expected 2 grouped issues, got %d", len(issues))
	}
	// env should sort first because severity is needs_input (worse than decision)
	if issues[0].Kind != issueKindEnv {
		t.Errorf("expected env issue first, got %q", issues[0].Kind)
	}
	if len(issues[0].Items) != 2 {
		t.Errorf("expected env issue to contain 2 items, got %d", len(issues[0].Items))
	}
}

func TestFixCommandOnlyReturnsCommandsThatPersistState(t *testing.T) {
	envIssue := appIssue{Kind: issueKindEnv, Items: []runDecisionItem{{ResourceRef: "env:.env.example", Evidence: []string{"FIRST_KEY"}}}}
	if got := envIssue.FixCommand("api"); got != "bort env api FIRST_KEY=value" {
		t.Fatalf("unexpected env fix command: %q", got)
	}
	manyKeys := []string{"A", "B", "C", "D", "E", "F", "G"}
	manyEnvIssue := appIssue{Kind: issueKindEnv, Items: []runDecisionItem{{ResourceRef: "env:.env.example", Evidence: manyKeys}}}
	if got := manyEnvIssue.FixCommand("api"); got != "bort env api A=value B=value C=value D=value E=value   # +2 more key(s)" {
		t.Fatalf("unexpected truncated env fix command: %q", got)
	}
	dataIssue := appIssue{Kind: issueKindData, Items: []runDecisionItem{{ResourceRef: "data-store:postgres"}}}
	if got := dataIssue.FixCommand("api"); got != "bort data api postgres --migrate   # or choose --recreate / --managed" {
		t.Fatalf("unexpected data fix command: %q", got)
	}
	for _, issue := range []appIssue{{Kind: issueKindRoute}, {Kind: issueKindLink}} {
		if got := issue.FixCommand("api"); got != "" {
			t.Fatalf("expected no canned fix for %s, got %q", issue.Kind, got)
		}
	}
}

func TestRouteIssueNextStepDistinguishesBlockedMappings(t *testing.T) {
	wantBlocked := "if live execution has started, run `" + bortCommand("status") + "`, complete the required recovery, then correct the source or input bundle and create a new run; otherwise correct the route-to-Compose-service mapping in the app's topology.json in the run's bundle, keep its routes.json consistent, and re-plan with `" + bortCommand("migrate --run <run>") + "`"
	for _, blocked := range []appIssue{
		{Kind: issueKindRoute, Items: []runDecisionItem{{Code: preparer.GateRoutesNone}, {Code: preparer.GateDomainServiceMissing}}},
		{Kind: issueKindRoute, Items: []runDecisionItem{{Code: preparer.GateDomainServiceNotInCompose}}},
	} {
		if got := blocked.NextStep(); got != wantBlocked {
			t.Fatalf("unexpected blocked route guidance: %q", got)
		}
	}
	review := appIssue{Kind: issueKindRoute, Items: []runDecisionItem{{Code: preparer.GateRoutesNone}}}
	if got := review.NextStep(); got != "confirm the route host and service in Dokploy before live apply" {
		t.Fatalf("unexpected review route guidance: %q", got)
	}
	run := loadedMigrationRun{Run: migrationRun{Name: "demo", RunDir: filepath.Join(t.TempDir(), "demo")}}
	blocked := appIssue{Kind: issueKindRoute, Items: []runDecisionItem{{Code: preparer.GateDomainServiceMissing}}}
	got := blocked.NextStepForRun(run)
	if !strings.Contains(got, "`"+runScopedCommand(run, "status")+"`") || !strings.Contains(got, "`"+runScopedCommand(run, "migrate")+"`") {
		t.Fatalf("run-specific route guidance lost its run context: %q", got)
	}

	for _, tc := range []struct {
		name string
		run  migrationRun
		want string
	}{
		{name: "source-created run edits its snapshot", run: migrationRun{Source: "coolify-local", BundleDir: "/runs/demo/bundle-2", SourceBundleDir: "/stale/input"}, want: "/runs/demo/bundle-2"},
		{name: "input-bundle run edits the original bundle", run: migrationRun{BundleDir: "/runs/demo/bundle-2", SourceBundleDir: "/work/bort-bundle"}, want: "/work/bort-bundle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run.Name, tc.run.RunDir = run.Run.Name, run.Run.RunDir
			got := blocked.NextStepForRun(loadedMigrationRun{Run: tc.run})
			if !strings.Contains(got, "topology.json in `"+tc.want+"`") {
				t.Fatalf("route guidance should name re-plan bundle %s: %q", tc.want, got)
			}
		})
	}
}

func TestFixCommandShellQuotesUntrustedNames(t *testing.T) {
	envIssue := appIssue{Kind: issueKindEnv}
	if got := envIssue.FixCommand("My App"); got != "bort env 'My App' KEY=value" {
		t.Fatalf("unexpected env fix command for spaces: %q", got)
	}
	if got := envIssue.FixCommand("api;rm -rf /"); got != `bort env 'api;rm -rf /' KEY=value` {
		t.Fatalf("unexpected env fix command for shell metacharacters: %q", got)
	}
	keyIssue := appIssue{Kind: issueKindEnv, Items: []runDecisionItem{{Evidence: []string{"NAME WITH SPACE", "OK_KEY"}}}}
	got := keyIssue.FixCommand("api")
	if !strings.Contains(got, "OK_KEY=value") || strings.Contains(got, "NAME WITH SPACE") {
		t.Fatalf("expected unsafe key replaced with KEY=value, got %q", got)
	}
}

func TestFixAndLiveCommandsPreserveSudoLifecycle(t *testing.T) {
	t.Setenv("SUDO_UID", "501")
	issue := appIssue{Kind: issueKindEnv, Items: []runDecisionItem{{Code: "env.values_required", ResourceRef: "env:KEY"}}}
	if got := issue.FixCommand("api"); got != "sudo bort env api KEY=value" {
		t.Fatalf("expected sudo fix command, got %q", got)
	}
	if got := liveApplyCommand(loadedMigrationRun{Run: migrationRun{Name: "demo"}}); got != "sudo bort migrate --live --run demo" {
		t.Fatalf("expected sudo live command, got %q", got)
	}
	route := appIssue{Kind: issueKindRoute, Items: []runDecisionItem{{Code: preparer.GateDomainServiceMissing}}}
	if got := route.NextStep(); !strings.Contains(got, "`sudo bort status`") || !strings.Contains(got, "`sudo bort migrate --run <run>`") {
		t.Fatalf("expected sudo route recovery commands, got %q", got)
	}
}
