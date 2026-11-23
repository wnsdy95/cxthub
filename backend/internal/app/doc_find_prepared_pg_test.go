//go:build postgres

package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docPreparedFindReadProbe struct {
	outbound.DocDisplayReader
	missing         func()
	pages, versions int
}

func (probe *docPreparedFindReadProbe) ReadDocDisplayGeneration(ctx context.Context, target domain.DocDisplayTarget) (domain.DocDisplayStringGeneration, error) {
	probe.versions++
	generation, err := probe.DocDisplayReader.ReadDocDisplayGeneration(ctx, target)
	if errors.Is(err, domain.ErrNotFound) && probe.missing != nil {
		probe.missing()
	}
	return generation, err
}

func (probe *docPreparedFindReadProbe) ReadDocDisplayPage(ctx context.Context, target domain.DocDisplayTarget, index int) (domain.DocDisplayPage, error) {
	probe.pages++
	return probe.DocDisplayReader.ReadDocDisplayPage(ctx, target, index)
}
