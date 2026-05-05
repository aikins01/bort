package localdocker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/source"
)

func TestRoutesFromLabelsExtractsHostsAndServicePort(t *testing.T) {
	labels := map[string]string{
		"traefik.http.routers.web.rule":                          "Host(`app.example.com`, `www.example.com`) || PathPrefix(`/`)",
		"traefik.http.routers.web.service":                       "app-svc",
		"traefik.http.services.app-svc.loadbalancer.server.port": "3000",
	}

	routes := routesFromLabels("web-1", labels)
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}

	seen := map[string]bool{}
	for _, route := range routes {
		seen[route.Host] = true
		if route.ServiceName != "web-1" {
			t.Fatalf("expected service name web-1, got %q", route.ServiceName)
		}
		if route.Port != "3000" {
			t.Fatalf("expected port 3000, got %q", route.Port)
		}
	}

	if !seen["app.example.com"] || !seen["www.example.com"] {
		t.Fatalf("expected app.example.com and www.example.com, got %#v", seen)
	}
}

func TestCaddyRoutesFromLabelsExtractsHostsAndPort(t *testing.T) {
	labels := map[string]string{
		"caddy_ingress_network":               "coolify",
		"caddy_0":                             "https://app.example.com",
		"caddy_0.handle_path.0_reverse_proxy": "{{upstreams 3000}}",
		"caddy_1":                             "https://api.example.com/v1",
		"caddy_1.handle_path.1_reverse_proxy": "{{upstreams}}",
		"traefik.http.routers.web.rule":       "Host(`legacy.example.com`)",
		"traefik.http.routers.web.service":    "app-svc",
		"traefik.http.services.app-svc.loadbalancer.server.port": "8080",
	}

	caddyRoutes := caddyRoutesFromLabels("web-1", labels)
	if len(caddyRoutes) != 2 {
		t.Fatalf("expected 2 caddy routes, got %#v", caddyRoutes)
	}
	byHost := map[string]manifest.Route{}
	for _, route := range caddyRoutes {
		byHost[route.Host] = route
	}
	if got, ok := byHost["app.example.com"]; !ok || got.Port != "3000" || got.Source != "caddy_0" {
		t.Fatalf("expected app.example.com on port 3000 from caddy_0, got %#v", got)
	}
	if got, ok := byHost["api.example.com"]; !ok || got.Port != "" || got.Source != "caddy_1" {
		t.Fatalf("expected api.example.com with empty port from caddy_1, got %#v", got)
	}
}

func TestCaddyRoutesFromLabelsIgnoresInvalidValues(t *testing.T) {
	labels := map[string]string{
		"caddy_0":               "https://192.168.1.1",
		"caddy_1":               "",
		"caddy_ingress_network": "coolify",
	}
	if got := caddyRoutesFromLabels("svc", labels); len(got) != 0 {
		t.Fatalf("expected no routes, got %#v", got)
	}
}

func TestEnvVarsRedactValuesByDefault(t *testing.T) {
	vars := envVars([]string{"PASSWORD=s3cr3t", "PORT=3000"}, false)
	if len(vars) != 2 {
		t.Fatalf("expected 2 env vars, got %d", len(vars))
	}

	for _, env := range vars {
		if env.ValueKnown {
			t.Fatalf("expected %s to be redacted", env.Name)
		}
		if env.Name == "PASSWORD" && !env.Sensitive {
			t.Fatal("expected PASSWORD to be marked sensitive")
		}
	}
}

func TestEnvVarsCanIncludeValues(t *testing.T) {
	vars := envVars([]string{"PORT=3000"}, true)
	if len(vars) != 1 {
		t.Fatalf("expected 1 env var, got %d", len(vars))
	}
	if !vars[0].ValueKnown || vars[0].Value != "3000" {
		t.Fatalf("expected PORT value to be included, got %#v", vars[0])
	}
}

func TestInspectContainersChunksIDs(t *testing.T) {
	ids := make([]string, 205)
	for i := range ids {
		ids[i] = fmt.Sprintf("container-%03d", i)
	}

	inspectCalls := []int{}
	scanner := &Scanner{
		runCommand: func(_ context.Context, args ...string) ([]byte, error) {
			switch {
			case len(args) == 2 && args[0] == "ps" && args[1] == "-aq":
				return []byte(strings.Join(ids, "\n")), nil
			case len(args) > 0 && args[0] == "inspect":
				inspectCalls = append(inspectCalls, len(args)-1)
				return inspectContainersJSON(t, args[1:]), nil
			default:
				return nil, fmt.Errorf("unexpected docker args: %v", args)
			}
		},
	}

	containers, err := scanner.inspectContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != len(ids) {
		t.Fatalf("expected %d containers, got %d", len(ids), len(containers))
	}
	wantCalls := []int{100, 100, 5}
	if fmt.Sprint(inspectCalls) != fmt.Sprint(wantCalls) {
		t.Fatalf("expected inspect chunks %v, got %v", wantCalls, inspectCalls)
	}
}

func TestHealthcheckFromConfig(t *testing.T) {
	hc := healthcheckFromConfig(&healthcheckConfig{
		Test:        []string{"CMD", "curl", "http://localhost"},
		Interval:    int64(30 * time.Second),
		Timeout:     int64(5 * time.Second),
		Retries:     3,
		StartPeriod: int64(10 * time.Second),
	})
	if hc == nil {
		t.Fatal("expected healthcheck, got nil")
	}
	if hc.Interval != "30s" || hc.Timeout != "5s" || hc.StartPeriod != "10s" {
		t.Fatalf("unexpected durations: %+v", hc)
	}
	if hc.Retries != 3 || len(hc.Test) != 3 {
		t.Fatalf("unexpected test/retries: %+v", hc)
	}

	if got := healthcheckFromConfig(nil); got != nil {
		t.Fatalf("expected nil for missing healthcheck, got %+v", got)
	}
	if got := healthcheckFromConfig(&healthcheckConfig{Test: []string{}}); got != nil {
		t.Fatalf("expected nil for empty test, got %+v", got)
	}
}

func TestInspectImageDigestsAggregatesUnique(t *testing.T) {
	containers := []containerInspect{
		{Image: "sha256:aaa"},
		{Image: "sha256:aaa"},
		{Image: "sha256:bbb"},
		{Image: ""},
	}

	calls := 0
	scanner := &Scanner{
		runCommand: func(_ context.Context, args ...string) ([]byte, error) {
			if len(args) < 2 || args[0] != "image" || args[1] != "inspect" {
				return nil, fmt.Errorf("unexpected docker args: %v", args)
			}
			calls++
			ids := args[2:]
			items := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				items = append(items, map[string]any{
					"Id":          id,
					"RepoDigests": []string{"registry.example.com/app@" + id},
				})
			}
			encoded, err := json.Marshal(items)
			if err != nil {
				t.Fatal(err)
			}
			return encoded, nil
		},
	}

	digests, err := scanner.inspectImageDigests(context.Background(), containers)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 docker call, got %d", calls)
	}
	if digests["sha256:aaa"] != "registry.example.com/app@sha256:aaa" {
		t.Fatalf("missing digest for aaa: %#v", digests)
	}
	if digests["sha256:bbb"] != "registry.example.com/app@sha256:bbb" {
		t.Fatalf("missing digest for bbb: %#v", digests)
	}
	if _, ok := digests[""]; ok {
		t.Fatal("expected empty image id to be skipped")
	}
}

func TestParseDuBytesAndInt64(t *testing.T) {
	got, err := parseDuBytes([]byte("12345\t/var/lib/docker/volumes/foo/_data\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 12345 {
		t.Fatalf("expected 12345, got %d", got)
	}
	if _, err := parseInt64("not-a-number"); err == nil {
		t.Fatal("expected error for non-numeric")
	}
	if _, err := parseDuBytes([]byte("")); err == nil {
		t.Fatal("expected error for empty du output")
	}
}

func TestScanPopulatesNewServiceFields(t *testing.T) {
	scanner := &Scanner{
		Now: func() time.Time { return time.Unix(0, 0).UTC() },
		runCommand: func(_ context.Context, args ...string) ([]byte, error) {
			switch {
			case len(args) == 2 && args[0] == "ps" && args[1] == "-aq":
				return []byte("c1\n"), nil
			case len(args) > 0 && args[0] == "inspect":
				return []byte(`[{
					"Id":"c1","Name":"/api","Image":"sha256:abc",
					"Config":{"Image":"app:1","Env":["PORT=80"],"Labels":{},"Healthcheck":{"Test":["CMD","ok"],"Interval":1000000000,"Retries":2}},
					"State":{"Status":"running"},
					"Mounts":[{"Type":"volume","Name":"data","Source":"/var/lib/docker/volumes/data/_data","Destination":"/data","RW":true}],
					"NetworkSettings":{"Ports":{},"Networks":{}}
				}]`), nil
			case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
				return []byte(`[{"Id":"sha256:abc","RepoDigests":["registry/app@sha256:dead"]}]`), nil
			case len(args) == 3 && args[0] == "volume" && args[1] == "ls" && args[2] == "-q":
				return []byte("data\n"), nil
			case len(args) >= 2 && args[0] == "volume" && args[1] == "inspect":
				return []byte(`[{"Name":"data","Driver":"local","Mountpoint":"/var/lib/docker/volumes/data/_data","Scope":"local"}]`), nil
			case len(args) == 3 && args[0] == "network" && args[1] == "ls" && args[2] == "-q":
				return []byte("n1\n"), nil
			case len(args) >= 2 && args[0] == "network" && args[1] == "inspect":
				return []byte(`[{"Id":"n1","Name":"net","Driver":"bridge","Scope":"local"}]`), nil
			}
			return nil, fmt.Errorf("unexpected docker args: %v", args)
		},
		measureCommand: func(_ context.Context, mountpoint string) (int64, int64, error) {
			if mountpoint != "/var/lib/docker/volumes/data/_data" {
				t.Fatalf("unexpected mountpoint %q", mountpoint)
			}
			return 4096, 12, nil
		},
	}

	m, err := scanner.Scan(context.Background(), source.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Apps) != 1 || len(m.Apps[0].Services) != 1 {
		t.Fatalf("unexpected apps: %+v", m.Apps)
	}
	svc := m.Apps[0].Services[0]
	if svc.ImageID != "sha256:abc" {
		t.Fatalf("expected image id sha256:abc, got %q", svc.ImageID)
	}
	if svc.ImageDigest != "registry/app@sha256:dead" {
		t.Fatalf("expected digest, got %q", svc.ImageDigest)
	}
	if svc.Healthcheck == nil || svc.Healthcheck.Interval != "1s" || svc.Healthcheck.Retries != 2 {
		t.Fatalf("unexpected healthcheck: %+v", svc.Healthcheck)
	}
	if len(m.Volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(m.Volumes))
	}
	v := m.Volumes[0]
	if v.SizeBytes != 4096 || v.FileCount != 12 {
		t.Fatalf("unexpected volume metrics: %+v", v)
	}
}

func TestScanIgnoresDokployTargetComposeStacks(t *testing.T) {
	scanner := &Scanner{
		Now: func() time.Time { return time.Unix(0, 0).UTC() },
		runCommand: func(_ context.Context, args ...string) ([]byte, error) {
			switch {
			case len(args) == 2 && args[0] == "ps" && args[1] == "-aq":
				return []byte("source\ntarget\ndokploy\ndokploy-db\ndokploy-traefik\n"), nil
			case len(args) > 0 && args[0] == "inspect":
				return []byte(`[
					{"Id":"source","Name":"/source-api","Image":"sha256:source","Config":{"Image":"app:1","Labels":{"com.docker.compose.project":"source-api","com.docker.compose.project.working_dir":"/data/coolify/app/source-api"}},"State":{"Status":"running"},"Mounts":[],"NetworkSettings":{"Ports":{},"Networks":{}}},
					{"Id":"target","Name":"/compose-noisy-source-api-1","Image":"sha256:target","Config":{"Image":"app:1","Labels":{"com.docker.compose.project":"compose-noisy","com.docker.compose.project.working_dir":"/etc/dokploy/compose/compose-noisy/code"}},"State":{"Status":"running"},"Mounts":[],"NetworkSettings":{"Ports":{},"Networks":{}}},
					{"Id":"dokploy","Name":"/dokploy.1.task","Image":"sha256:dokploy","Config":{"Image":"dokploy/dokploy:latest","Labels":{"com.docker.swarm.service.name":"dokploy"}},"State":{"Status":"running"},"Mounts":[],"NetworkSettings":{"Ports":{},"Networks":{}}},
					{"Id":"dokploy-db","Name":"/dokploy-postgres.1.task","Image":"sha256:db","Config":{"Image":"postgres:16","Labels":{"com.docker.swarm.service.name":"dokploy-postgres"}},"State":{"Status":"running"},"Mounts":[],"NetworkSettings":{"Ports":{},"Networks":{}}},
					{"Id":"dokploy-traefik","Name":"/dokploy-traefik","Image":"sha256:traefik","Config":{"Image":"traefik:v3.6.7","Labels":{}},"State":{"Status":"created"},"Mounts":[],"NetworkSettings":{"Ports":{},"Networks":{}}}
				]`), nil
			case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
				return []byte(`[]`), nil
			case len(args) == 3 && args[0] == "volume" && args[1] == "ls" && args[2] == "-q":
				return []byte(""), nil
			case len(args) == 3 && args[0] == "network" && args[1] == "ls" && args[2] == "-q":
				return []byte(""), nil
			}
			return nil, fmt.Errorf("unexpected docker args: %v", args)
		},
	}

	m, err := scanner.Scan(context.Background(), source.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Apps) != 1 || m.Apps[0].Name != "source-api" {
		t.Fatalf("expected only source app, got %+v", m.Apps)
	}
}

func inspectContainersJSON(t *testing.T, ids []string) []byte {
	t.Helper()
	items := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		items = append(items, map[string]any{
			"Id": id,
			"Config": map[string]any{
				"Labels": map[string]string{},
			},
			"State": map[string]any{
				"Status": "running",
			},
			"NetworkSettings": map[string]any{
				"Ports":    map[string]any{},
				"Networks": map[string]any{},
			},
		})
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
