package coolifylocal

import (
	"context"

	"github.com/aikins01/bort/internal/manifest"
	"github.com/aikins01/bort/internal/source"
	"github.com/aikins01/bort/internal/source/localdocker"
)

type Scanner struct {
	Docker source.Scanner
}

func NewScanner() *Scanner {
	return &Scanner{Docker: localdocker.NewScanner()}
}

func (s *Scanner) Scan(ctx context.Context, opts source.ScanOptions) (manifest.Manifest, error) {
	docker := s.Docker
	if docker == nil {
		docker = localdocker.NewScanner()
	}

	result, err := docker.Scan(ctx, opts)
	if err != nil {
		return manifest.Manifest{}, err
	}
	result.Source.Platform = "coolify-local"
	return result, nil
}
