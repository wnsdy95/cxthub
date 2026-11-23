package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type reboundGitRepo struct{ outbound.MetadataStore }

func (s reboundGitRepo) GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error) {
	r, err := s.MetadataStore.GetRepo(ctx, id)
	r.GitRemoteURL = "https://github.com/example/other"
	return r, err
}
