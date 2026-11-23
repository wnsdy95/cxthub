package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type associationStatusReadKey struct{}

type associationStatusReadProbe struct {
	metadata    domain.Repo
	calls       []string
	scopeError  error
	finishError error
}

func (probe *associationStatusReadProbe) ReadRepository(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	probe.calls = append(probe.calls, "scope")
	if probe.scopeError != nil {
		return probe.scopeError
	}
	if repo != probe.metadata.ID {
		return domain.ErrIntegrity
	}
	if err := read(context.WithValue(ctx, associationStatusReadKey{}, probe)); err != nil {
		return err
	}
	return probe.finishError
}

func (probe *associationStatusReadProbe) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	probe.calls = append(probe.calls, "metadata")
	if ctx.Value(associationStatusReadKey{}) != probe || repo != probe.metadata.ID {
		return domain.Repo{}, domain.ErrIntegrity
	}
	return probe.metadata, nil
}
