package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aikins01/bort/internal/preparer"
)

// appHealth is a user-facing health label that hides the underlying
// status/readiness/gate machinery.
type appHealth string

const (
	appHealthReady     appHealth = "ready"
	appHealthNeedsWork appHealth = "needs work"
	appHealthBlocked   appHealth = "blocked"
)

// issueKind groups the underlying gates/decision items into the small
// set of categories a user actually thinks about.
type issueKind string

const (
	issueKindEnv    issueKind = "env"
	issueKindData   issueKind = "data"
	issueKindRoute  issueKind = "route"
	issueKindLink   issueKind = "link"
	issueKindReview issueKind = "review"
)

type appView struct {
	Name      string
	Role      string
	Health    appHealth
	Resources []resourceLine
	Issues    []appIssue
}

type resourceLine struct {
	Label  string
	Status string // "✓" "!" "?" "✕"
	Detail string
}

type appIssue struct {
	Kind     issueKind
	Title    string
	Detail   string
	Severity preparer.Readiness
	Items    []runDecisionItem
}

// FixCommand returns a copy-pasteable shell snippet that resolves this
// issue for the given app. Empty string means there is no canned fix.
func (i appIssue) FixCommand(app string) string {
	quotedApp := shellQuote(app)
	switch i.Kind {
	case issueKindEnv:
		keys := envKeysFromItems(i.Items)
		if len(keys) == 0 {
			return fmt.Sprintf("bort env %s KEY=value", quotedApp)
		}
		const maxKeys = 5
		shown := keys
		more := 0
		if len(keys) > maxKeys {
			shown = keys[:maxKeys]
			more = len(keys) - maxKeys
		}
		parts := make([]string, 0, len(shown))
		for _, key := range shown {
			if isShellSafeIdent(key) {
				parts = append(parts, key+"=value")
			} else {
				parts = append(parts, "KEY=value")
			}
		}
		cmd := fmt.Sprintf("bort env %s %s", quotedApp, strings.Join(parts, " "))
		if more > 0 {
			cmd += fmt.Sprintf("   # +%d more key(s)", more)
		}
		return cmd
	case issueKindData:
		stores := dataStoresFromItems(i.Items)
		if len(stores) == 0 {
			return fmt.Sprintf("bort data %s <store> --migrate   # or choose --recreate / --managed", quotedApp)
		}
		return fmt.Sprintf("bort data %s %s --migrate   # or choose --recreate / --managed", quotedApp, shellQuote(stores[0]))
	default:
		return ""
	}
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '/', r == '@', r == ':', r == '+', r == '=', r == ',':
		default:
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

func isShellSafeIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r == '_':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func envKeysFromItems(items []runDecisionItem) []string {
	seen := map[string]struct{}{}
	keys := []string{}
	for _, item := range items {
		for _, key := range item.Evidence {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func dataStoresFromItems(items []runDecisionItem) []string {
	seen := map[string]struct{}{}
	stores := []string{}
	for _, item := range items {
		ref := strings.TrimPrefix(item.ResourceRef, "data_store:")
		ref = strings.TrimPrefix(ref, "data-store:")
		if ref == "" || ref == item.ResourceRef {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		stores = append(stores, ref)
	}
	sort.Strings(stores)
	return stores
}

// appsFromRun derives the app-first view from a loaded run.
// It walks the prepare output for resource health and the open decisions
// for actionable issues, classifying each item into one of five issue kinds.
func appsFromRun(run loadedMigrationRun) []appView {
	itemsByApp := openItemsByApp(run)

	apps := []appView{}
	for _, app := range run.Prepare.Apps {
		if isPlatformRunApp(app.Role) {
			continue
		}
		view := appView{
			Name:      app.Name,
			Role:      app.Role,
			Resources: resourceLinesForApp(app),
			Issues:    issuesForApp(itemsByApp[app.Name]),
		}
		view.Health = appHealthFromIssues(view.Issues)
		apps = append(apps, view)
	}
	sort.Slice(apps, func(i, j int) bool {
		if healthRank(apps[i].Health) != healthRank(apps[j].Health) {
			return healthRank(apps[i].Health) > healthRank(apps[j].Health)
		}
		return apps[i].Name < apps[j].Name
	})
	return apps
}

func openItemsByApp(run loadedMigrationRun) map[string][]runDecisionItem {
	result := map[string][]runDecisionItem{}
	for _, decision := range openRunDecisions(run) {
		for _, item := range decision.Items {
			if item.Stage != "prepare" {
				continue
			}
			result[item.App] = append(result[item.App], item)
		}
	}
	return result
}

func issuesForApp(items []runDecisionItem) []appIssue {
	if len(items) == 0 {
		return nil
	}
	groups := map[issueKind][]runDecisionItem{}
	for _, item := range items {
		kind := classifyItem(item)
		groups[kind] = append(groups[kind], item)
	}
	issues := make([]appIssue, 0, len(groups))
	for kind, group := range groups {
		issues = append(issues, buildIssue(kind, group))
	}
	sort.Slice(issues, func(i, j int) bool {
		if preparer.ReadinessRank(issues[i].Severity) != preparer.ReadinessRank(issues[j].Severity) {
			return preparer.ReadinessRank(issues[i].Severity) > preparer.ReadinessRank(issues[j].Severity)
		}
		return string(issues[i].Kind) < string(issues[j].Kind)
	})
	return issues
}

func classifyItem(item runDecisionItem) issueKind {
	code := item.Code
	switch {
	case strings.HasPrefix(code, preparer.GateCodePrefixEnv):
		return issueKindEnv
	case strings.HasPrefix(code, preparer.GateCodePrefixDataStore):
		return issueKindData
	case strings.HasPrefix(code, preparer.GateCodePrefixDomain) || code == preparer.GateRoutesNone:
		return issueKindRoute
	case strings.HasPrefix(code, preparer.GateCodePrefixLinkedResource), strings.HasPrefix(code, preparer.GateCodePrefixExternalRequirement):
		return issueKindLink
	default:
		return issueKindReview
	}
}

func buildIssue(kind issueKind, items []runDecisionItem) appIssue {
	issue := appIssue{Kind: kind, Items: items}
	for _, item := range items {
		issue.Severity = preparer.WorseReadiness(issue.Severity, item.Readiness)
	}
	switch kind {
	case issueKindEnv:
		issue.Title = "Fill environment values"
	case issueKindData:
		issue.Title = "Confirm data store strategy"
	case issueKindRoute:
		issue.Title = "Confirm routes"
	case issueKindLink:
		issue.Title = "Confirm support resources"
	default:
		issue.Title = "Review needed"
	}
	issue.Detail = describeIssueItems(items, 3)
	return issue
}

func describeIssueItems(items []runDecisionItem, limit int) string {
	if limit > len(items) {
		limit = len(items)
	}
	parts := make([]string, 0, limit+1)
	for _, item := range items[:limit] {
		text := strings.TrimSpace(item.Message)
		if text == "" {
			text = item.Code
		}
		parts = append(parts, text)
	}
	if len(items) > limit {
		parts = append(parts, fmt.Sprintf("+%d more", len(items)-limit))
	}
	return strings.Join(parts, " · ")
}

func appHealthFromIssues(issues []appIssue) appHealth {
	if len(issues) == 0 {
		return appHealthReady
	}
	for _, issue := range issues {
		if issue.Severity == preparer.ReadinessBlocked {
			return appHealthBlocked
		}
	}
	return appHealthNeedsWork
}

func healthRank(h appHealth) int {
	switch h {
	case appHealthBlocked:
		return 2
	case appHealthNeedsWork:
		return 1
	default:
		return 0
	}
}

// humanBytes formats a byte count as a short, human-friendly string
// (e.g. 6.2 GB, 1 GB) for the cockpit volume line. Uses 1024-based units.
func humanBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	value := float64(bytes) / float64(div)
	if value == float64(int64(value)) {
		return fmt.Sprintf("%d %cB", int64(value), "KMGTPE"[exp])
	}
	return fmt.Sprintf("%.1f %cB", value, "KMGTPE"[exp])
}

func resourceLinesForApp(app preparer.AppPlan) []resourceLine {
	lines := []resourceLine{}

	if app.Resources.App.ComposeMissing {
		lines = append(lines, resourceLine{Label: "Compose", Status: "✕", Detail: "missing"})
	} else {
		path := app.Resources.App.ComposePath
		if path == "" {
			path = "compose.yaml"
		}
		lines = append(lines, resourceLine{Label: "Compose", Status: "✓", Detail: path})
	}

	if len(app.Resources.Domains) > 0 {
		ready := 0
		for _, d := range app.Resources.Domains {
			if d.Readiness == preparer.ReadinessReadyToCreate {
				ready++
			}
		}
		status := "✓"
		if ready < len(app.Resources.Domains) {
			status = "?"
		}
		lines = append(lines, resourceLine{Label: "Routes", Status: status, Detail: fmt.Sprintf("%d total, %d ready", len(app.Resources.Domains), ready)})
	}

	if len(app.Resources.EnvFiles) > 0 {
		missing := 0
		for _, e := range app.Resources.EnvFiles {
			missing += len(e.MissingValues)
		}
		status := "✓"
		detail := fmt.Sprintf("%d file(s)", len(app.Resources.EnvFiles))
		if missing > 0 {
			status = "!"
			detail = fmt.Sprintf("%d missing values across %d file(s)", missing, len(app.Resources.EnvFiles))
		}
		lines = append(lines, resourceLine{Label: "Env", Status: status, Detail: detail})
	}

	if len(app.Resources.DataStores) > 0 {
		unset := 0
		for _, ds := range app.Resources.DataStores {
			if ds.Strategy == "" || ds.Strategy == "manual_review" {
				unset++
			}
		}
		status := "✓"
		detail := fmt.Sprintf("%d store(s)", len(app.Resources.DataStores))
		if unset > 0 {
			status = "?"
			detail = fmt.Sprintf("%d store(s), %d strategy unset", len(app.Resources.DataStores), unset)
		}
		lines = append(lines, resourceLine{Label: "Data", Status: status, Detail: detail})
	}

	if len(app.Resources.Volumes) > 0 {
		binds := 0
		var totalBytes, totalFiles int64
		for _, v := range app.Resources.Volumes {
			if v.Type == "bind" {
				binds++
			}
			totalBytes += v.SizeBytes
			totalFiles += v.FileCount
		}
		status := "✓"
		detail := fmt.Sprintf("%d volume(s)", len(app.Resources.Volumes))
		if totalBytes > 0 {
			detail = fmt.Sprintf("%d volume(s), %s across %d file(s)", len(app.Resources.Volumes), humanBytes(totalBytes), totalFiles)
		}
		if binds > 0 {
			status = "?"
			detail = fmt.Sprintf("%s, %d bind mount(s) to verify", detail, binds)
		}
		lines = append(lines, resourceLine{Label: "Volumes", Status: status, Detail: detail})
	}

	if len(app.Resources.LinkedResources)+len(app.Resources.ExternalRequirements) > 0 {
		unconfirmed := 0
		for _, l := range app.Resources.LinkedResources {
			if l.RequiresConfirmation {
				unconfirmed++
			}
		}
		status := "✓"
		detail := fmt.Sprintf("%d candidate(s)", len(app.Resources.LinkedResources))
		if unconfirmed > 0 {
			status = "?"
			detail = fmt.Sprintf("%d candidate(s), %d need confirm", len(app.Resources.LinkedResources), unconfirmed)
		}
		lines = append(lines, resourceLine{Label: "Linked", Status: status, Detail: detail})
	}
	return lines
}
