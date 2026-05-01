package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/manifest"
)

func TestWritePlanFlagsMissingDeployableMetadata(t *testing.T) {
	m := manifest.Manifest{
		Source: manifest.Source{Platform: "coolify", Hostname: "example.com"},
		Apps: []manifest.App{
			{
				Name:     "image app",
				Platform: "coolify",
				Services: []manifest.Service{{Name: "web", Image: "example/web:latest"}},
				Routes:   []manifest.Route{{Host: "web.example.com"}},
			},
			{
				Name:      "source app",
				Platform:  "coolify",
				BuildPack: "dockercompose",
				Git:       &manifest.GitSource{Repository: "example/repo", ComposeLocation: "/compose.yaml"},
				Services:  []manifest.Service{{Name: "web"}},
				Routes:    []manifest.Route{{Host: "source.example.com"}},
			},
			{
				Name:     "missing app",
				Platform: "coolify",
				Services: []manifest.Service{{Name: "web"}},
				Routes:   []manifest.Route{{Host: "missing.example.com"}},
			},
		},
	}

	var out bytes.Buffer
	if err := writePlan(&out, m, "dokploy"); err != nil {
		t.Fatal(err)
	}
	plan := out.String()
	for _, want := range []string{
		"[green] image app",
		"deploy: image metadata captured",
		"[yellow] source app",
		"deploy: source build metadata only; run server-local scan or repository export before migration",
		"[red] missing app",
		"deploy: missing image or raw compose; server-local scan is required before migration",
	} {
		if !strings.Contains(plan, want) {
			t.Fatalf("expected plan to contain %q, got:\n%s", want, plan)
		}
	}
}
