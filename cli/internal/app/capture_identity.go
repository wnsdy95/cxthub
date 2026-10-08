package app

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func captureDocumentIdentity(ctx context.Context, git outbound.GitContext, repo domain.Repo, explicit domain.DocumentIdentity) (domain.DocumentIdentity, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := explicit.Validate(); err != nil {
		return "", err
	}
	if explicit != domain.DocumentIdentityLegacy {
		return explicit, nil
	}
	policy, ok := git.(outbound.CaptureDocumentPolicy)
	if !ok {
		return domain.DocumentIdentityLegacy, nil
	}
	identity, err := policy.CaptureDocumentIdentity(ctx, repo)
	if err != nil {
		return "", err
	}
	if err := identity.Validate(); err != nil {
		return "", err
	}
	return identity, ctx.Err()
}
