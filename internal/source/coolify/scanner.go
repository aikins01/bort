package coolify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/source"
)

type Scanner struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
	Now        func() time.Time
}

func NewScanner(baseURL, token string) (*Scanner, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("--coolify-url or BORT_COOLIFY_URL is required for --source coolify")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("--coolify-token or BORT_COOLIFY_TOKEN is required for --source coolify")
	}

	return &Scanner{
		BaseURL:    baseURL,
		Token:      token,
		HTTPClient: http.DefaultClient,
		Now:        time.Now,
	}, nil
}

func (s *Scanner) Scan(ctx context.Context, opts source.ScanOptions) (manifest.Manifest, error) {
	if s.HTTPClient == nil {
		s.HTTPClient = http.DefaultClient
	}
	if s.Now == nil {
		s.Now = time.Now
	}

	result := manifest.New(manifest.Source{Platform: "coolify", Hostname: hostFromBaseURL(s.BaseURL)}, s.Now())

	resources := []resourceKind{
		{Runtime: "application", ListPath: "/api/v1/applications", DetailPath: "/api/v1/applications/%s", EnvsPath: "/api/v1/applications/%s/envs", StoragesPath: "/api/v1/applications/%s/storages"},
		{Runtime: "service", ListPath: "/api/v1/services", DetailPath: "/api/v1/services/%s", EnvsPath: "/api/v1/services/%s/envs", StoragesPath: "/api/v1/services/%s/storages"},
		{Runtime: "database", ListPath: "/api/v1/databases", DetailPath: "/api/v1/databases/%s", EnvsPath: "/api/v1/databases/%s/envs", StoragesPath: "/api/v1/databases/%s/storages"},
	}

	for _, kind := range resources {
		apps, warnings, err := s.scanKind(ctx, kind, opts)
		if err != nil {
			result.Warnings = append(result.Warnings, manifest.Warning{Code: "coolify.scan_failed", Message: err.Error()})
			continue
		}
		result.Apps = append(result.Apps, apps...)
		result.Warnings = append(result.Warnings, warnings...)
	}

	sort.Slice(result.Apps, func(i, j int) bool {
		if result.Apps[i].Runtime == result.Apps[j].Runtime {
			return result.Apps[i].Name < result.Apps[j].Name
		}
		return result.Apps[i].Runtime < result.Apps[j].Runtime
	})

	return result, nil
}

type resourceKind struct {
	Runtime      string
	ListPath     string
	DetailPath   string
	EnvsPath     string
	StoragesPath string
}

func (s *Scanner) scanKind(ctx context.Context, kind resourceKind, opts source.ScanOptions) ([]manifest.App, []manifest.Warning, error) {
	items, err := s.getList(ctx, kind.ListPath)
	if err != nil {
		return nil, nil, fmt.Errorf("list Coolify %s resources: %w", kind.Runtime, err)
	}

	apps := make([]manifest.App, 0, len(items))
	warnings := []manifest.Warning{}
	for _, item := range items {
		uuid := getString(item, "uuid", "id")
		resource := item
		if uuid != "" && kind.DetailPath != "" {
			detail, err := s.getObject(ctx, fmt.Sprintf(kind.DetailPath, url.PathEscape(uuid)))
			if err != nil {
				warnings = append(warnings, manifest.Warning{Code: "coolify.detail_failed", Message: fmt.Sprintf("%s %s: %v", kind.Runtime, uuid, err)})
			} else {
				resource = mergeObjects(resource, detail)
			}
		}

		app := appFromResource(kind.Runtime, resource)
		if uuid != "" {
			app.Environment = s.envsFor(ctx, kind, uuid, opts, &warnings)
			app.Storages = s.storagesFor(ctx, kind, uuid, opts, &warnings)
		}
		apps = append(apps, app)
	}

	return apps, warnings, nil
}

func (s *Scanner) envsFor(ctx context.Context, kind resourceKind, uuid string, opts source.ScanOptions, warnings *[]manifest.Warning) []manifest.EnvVar {
	items, err := s.getList(ctx, fmt.Sprintf(kind.EnvsPath, url.PathEscape(uuid)))
	if err != nil {
		*warnings = append(*warnings, manifest.Warning{Code: "coolify.envs_failed", Message: fmt.Sprintf("%s %s: %v", kind.Runtime, uuid, err)})
		return nil
	}
	return envVars(items, opts.IncludeEnvValues)
}

func (s *Scanner) storagesFor(ctx context.Context, kind resourceKind, uuid string, opts source.ScanOptions, warnings *[]manifest.Warning) []manifest.Storage {
	items, err := s.getList(ctx, fmt.Sprintf(kind.StoragesPath, url.PathEscape(uuid)))
	if err != nil {
		*warnings = append(*warnings, manifest.Warning{Code: "coolify.storages_failed", Message: fmt.Sprintf("%s %s: %v", kind.Runtime, uuid, err)})
		return nil
	}
	return storages(items, opts.IncludeEnvValues)
}

func (s *Scanner) getList(ctx context.Context, path string) ([]map[string]any, error) {
	var payload any
	if err := s.get(ctx, path, &payload); err != nil {
		return nil, err
	}
	return asObjectList(payload), nil
}

func (s *Scanner) getObject(ctx context.Context, path string) (map[string]any, error) {
	var payload map[string]any
	if err := s.get(ctx, path, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (s *Scanner) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.Token)

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GET %s returned %s", path, resp.Status)
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func appFromResource(runtime string, resource map[string]any) manifest.App {
	uuid := getString(resource, "uuid", "id")
	name := getString(resource, "name")
	if name == "" {
		name = uuid
	}

	app := manifest.App{
		ID:        "coolify:" + runtime + ":" + uuid,
		Name:      name,
		Platform:  "coolify",
		Runtime:   runtime,
		BuildPack: getString(resource, "build_pack", "buildPack"),
		Status:    getString(resource, "status"),
		Git:       gitSource(resource),
		Compose:   composeSource(resource),
		Metadata:  metadata(resource, "uuid", "id", "server_uuid", "project_uuid", "environment_name", "environment_uuid", "service_type", "type"),
		Services: []manifest.Service{
			{
				ID:     uuid,
				Name:   name,
				Image:  imageName(resource),
				Status: getString(resource, "status"),
			},
		},
	}

	app.Routes = routes(resource, name)
	return app
}

func gitSource(resource map[string]any) *manifest.GitSource {
	git := &manifest.GitSource{
		Repository:         getString(resource, "git_repository", "gitRepository", "git_full_url", "gitFullUrl"),
		Branch:             getString(resource, "git_branch", "gitBranch"),
		CommitSHA:          getString(resource, "git_commit_sha", "gitCommitSha"),
		BaseDirectory:      getString(resource, "base_directory", "baseDirectory"),
		DockerfileLocation: getString(resource, "dockerfile_location", "dockerfileLocation"),
		ComposeLocation:    getString(resource, "docker_compose_location", "dockerComposeLocation"),
	}
	if *git == (manifest.GitSource{}) {
		return nil
	}
	return git
}

func composeSource(resource map[string]any) *manifest.ComposeSource {
	compose := &manifest.ComposeSource{
		Raw:      getString(resource, "docker_compose_raw", "dockerComposeRaw"),
		Resolved: getString(resource, "docker_compose", "dockerCompose"),
		Domains:  composeDomains(resource),
	}
	if compose.Raw == "" && compose.Resolved == "" && len(compose.Domains) == 0 {
		return nil
	}
	return compose
}

func composeDomains(resource map[string]any) map[string]manifest.ComposeDomain {
	raw := getString(resource, "docker_compose_domains", "dockerComposeDomains")
	if raw == "" {
		return nil
	}

	var domains map[string]manifest.ComposeDomain
	if err := json.Unmarshal([]byte(raw), &domains); err == nil {
		return domains
	}

	var generic map[string]map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return nil
	}

	domains = map[string]manifest.ComposeDomain{}
	for name, value := range generic {
		domains[name] = manifest.ComposeDomain{Domain: getString(value, "domain", "fqdn")}
	}
	return domains
}

func envVars(items []map[string]any, includeValues bool) []manifest.EnvVar {
	vars := make([]manifest.EnvVar, 0, len(items))
	for _, item := range items {
		name := getString(item, "key", "name")
		if name == "" {
			continue
		}

		value := getString(item, "real_value", "realValue", "value")
		env := manifest.EnvVar{Name: name, Sensitive: isSensitiveName(name) || getBool(item, "is_shown_once", "isShownOnce")}
		if includeValues {
			env.Value = value
			env.ValueKnown = value != ""
		}
		vars = append(vars, env)
	}
	sort.Slice(vars, func(i, j int) bool { return vars[i].Name < vars[j].Name })
	return vars
}

func storages(items []map[string]any, includeValues bool) []manifest.Storage {
	storages := make([]manifest.Storage, 0, len(items))
	for _, item := range items {
		storage := manifest.Storage{
			ID:        getString(item, "uuid", "id"),
			Name:      getString(item, "name"),
			Type:      storageType(item),
			Source:    firstNonEmpty(getString(item, "host_path", "hostPath"), getString(item, "fs_path", "fsPath")),
			Target:    getString(item, "mount_path", "mountPath"),
			Directory: getBool(item, "is_directory", "isDirectory"),
			Metadata:  metadata(item, "resource_type", "resource_id", "is_preview_suffix_enabled", "chown", "chmod"),
		}
		if storage.Name == "" {
			storage.Name = getString(item, "volume_name", "volumeName")
		}
		if includeValues {
			storage.Content = getString(item, "content")
		}
		storages = append(storages, storage)
	}
	sort.Slice(storages, func(i, j int) bool {
		if storages[i].Target == storages[j].Target {
			return storages[i].Name < storages[j].Name
		}
		return storages[i].Target < storages[j].Target
	})
	return storages
}

func routes(resource map[string]any, serviceName string) []manifest.Route {
	routes := []manifest.Route{}
	for _, fqdn := range splitCSV(getString(resource, "fqdn", "domains")) {
		routes = append(routes, routeFromURL(fqdn, serviceName, "fqdn"))
	}

	if compose := composeSource(resource); compose != nil {
		for name, domain := range compose.Domains {
			for _, fqdn := range splitCSV(domain.Domain) {
				route := routeFromURL(fqdn, name, "docker_compose_domains")
				routes = append(routes, route)
			}
		}
	}

	return dedupeRoutes(routes)
}

func routeFromURL(raw, serviceName, source string) manifest.Route {
	route := manifest.Route{ServiceName: serviceName, Source: source}
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Host != "" {
		route.Host = parsed.Hostname()
		route.Port = parsed.Port()
		return route
	}

	host := strings.TrimPrefix(strings.TrimPrefix(raw, "http://"), "https://")
	if before, _, found := strings.Cut(host, "/"); found {
		host = before
	}
	route.Host = host
	if h, p, err := netSplitHostPort(host); err == nil {
		route.Host = h
		route.Port = p
	}
	return route
}

func dedupeRoutes(routes []manifest.Route) []manifest.Route {
	seen := map[string]manifest.Route{}
	for _, route := range routes {
		if route.Host == "" {
			continue
		}
		seen[route.Host+"|"+route.ServiceName+"|"+route.Port] = route
	}

	deduped := make([]manifest.Route, 0, len(seen))
	for _, route := range seen {
		deduped = append(deduped, route)
	}
	sort.Slice(deduped, func(i, j int) bool { return deduped[i].Host < deduped[j].Host })
	return deduped
}

func asObjectList(payload any) []map[string]any {
	switch value := payload.(type) {
	case []any:
		items := make([]map[string]any, 0, len(value))
		for _, item := range value {
			if object, ok := item.(map[string]any); ok {
				items = append(items, object)
			}
		}
		return items
	case map[string]any:
		for _, key := range []string{"data", "items", "resources"} {
			if items := asObjectList(value[key]); len(items) > 0 {
				return items
			}
		}
		return []map[string]any{value}
	default:
		return nil
	}
}

func mergeObjects(first, second map[string]any) map[string]any {
	merged := map[string]any{}
	for key, value := range first {
		merged[key] = value
	}
	for key, value := range second {
		merged[key] = value
	}
	return merged
}

func getString(item map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := item[key].(type) {
		case string:
			return value
		case float64:
			return fmt.Sprintf("%.0f", value)
		case bool:
			if value {
				return "true"
			}
			return "false"
		}
	}
	return ""
}

func getBool(item map[string]any, keys ...string) bool {
	for _, key := range keys {
		switch value := item[key].(type) {
		case bool:
			return value
		case string:
			return value == "true" || value == "1"
		}
	}
	return false
}

func metadata(item map[string]any, keys ...string) map[string]string {
	values := map[string]string{}
	for _, key := range keys {
		if value := getString(item, key); value != "" {
			values[key] = value
		}
	}
	if len(values) == 0 {
		return nil
	}
	return values
}

func imageName(resource map[string]any) string {
	image := getString(resource, "image")
	if image != "" {
		return image
	}
	name := getString(resource, "docker_registry_image_name", "dockerRegistryImageName")
	tag := getString(resource, "docker_registry_image_tag", "dockerRegistryImageTag")
	if name == "" {
		return ""
	}
	if tag == "" {
		return name
	}
	return name + ":" + tag
}

func storageType(item map[string]any) string {
	if getString(item, "fs_path", "fsPath") != "" || getString(item, "content") != "" {
		return "file"
	}
	if getString(item, "host_path", "hostPath") != "" {
		return "bind"
	}
	return "volume"
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	items := []string{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			items = append(items, part)
		}
	}
	return items
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func hostFromBaseURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return raw
	}
	return parsed.Hostname()
}

func netSplitHostPort(host string) (string, string, error) {
	parsed, err := url.Parse("scheme://" + host)
	if err != nil || parsed.Hostname() == "" || parsed.Port() == "" {
		return "", "", fmt.Errorf("no host port")
	}
	return parsed.Hostname(), parsed.Port(), nil
}

func isSensitiveName(name string) bool {
	lower := strings.ToLower(name)
	for _, token := range []string{"password", "passwd", "secret", "token", "key", "credential"} {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return false
}
