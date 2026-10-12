package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

func (service *SyncRepoService) ResolveRemoteBranchContext(ctx context.Context, in inbound.SyncInput, branch string) (domain.ContextQueryView, error) {
	if domain.ValidateBranchName(branch) != nil {
		return domain.ContextQueryView{}, domain.ErrInvalidRef
	}
	repo, err := service.repoID(ctx, in)
	if err != nil {
		return domain.ContextQueryView{}, err
	}
	reader, ok := service.remote.(outbound.ContextQueryReader)
	if !ok {
		return domain.ContextQueryView{}, fmt.Errorf("server context query unavailable")
	}
	selection := domain.ContextSelection{Branch: branch, Position: branch, Scope: "current"}
	view, err := reader.QueryContext(ctx, repo, selection)
	if err != nil {
		return domain.ContextQueryView{}, err
	}
	if err := domain.ValidateContextQuery(repo, selection, view); err != nil {
		return domain.ContextQueryView{}, err
	}
	if view.Branch != branch || view.Inclusion == nil || view.Inclusion.BranchID == "" || view.Inclusion.SnapshotID != view.Position || domain.ValidateContentHash(view.Position) != nil ||
		(in.ExpectedBranchID != "" && view.Inclusion.BranchID != in.ExpectedBranchID) {
		return domain.ContextQueryView{}, domain.ErrSelectionChanged
	}
	return view, nil
}
