package coolifylocal

import (
	"context"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/source"
)

func TestScanMarksSourceAsCoolifyLocal(t *testing.T) {
	scanner := &Scanner{Docker: fakeScanner{}}

	result, err := scanner.Scan(context.Background(), source.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Source.Platform != "coolify-local" {
		t.Fatalf("expected coolify-local source, got %#v", result.Source)
	}
	if len(result.Apps) != 1 || result.Apps[0].Name != "api" {
		t.Fatalf("expected docker scan apps to pass through, got %#v", result.Apps)
	}
}

type fakeScanner struct{}

func (fakeScanner) Scan(context.Context, source.ScanOptions) (manifest.Manifest, error) {
	return manifest.Manifest{
		APIVersion:  manifest.APIVersion,
		Kind:        "MigrationManifest",
		GeneratedAt: time.Unix(0, 0),
		Source:      manifest.Source{Platform: "docker", Hostname: "coolify-host"},
		Apps:        []manifest.App{{Name: "api", Services: []manifest.Service{{Name: "api", Image: "example/api"}}}},
	}, nil
}
