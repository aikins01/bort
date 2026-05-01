package localdocker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
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
