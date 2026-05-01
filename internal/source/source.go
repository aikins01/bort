package source

import (
	"context"

	"github.com/aikins01/bort/internal/manifest"
)

type ScanOptions struct {
	IncludeEnvValues bool
}

type Scanner interface {
	Scan(context.Context, ScanOptions) (manifest.Manifest, error)
}
