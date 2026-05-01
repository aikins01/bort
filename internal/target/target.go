package target

import (
	"context"

	"github.com/aikins01/bort/internal/manifest"
)

type PrepareOptions struct {
	DryRun bool
}

type Preparer interface {
	Prepare(context.Context, manifest.Manifest, PrepareOptions) error
}
