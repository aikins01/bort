package localdocker

import "testing"

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
