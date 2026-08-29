package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type BranchLifecycleService struct {
	gitCtx outbound.GitContext
	store  outbound.SessionStore
}

func NewBranchLifecycleService(gitCtx outbound.GitContext, store outbound.SessionStore) *BranchLifecycleService {
	return &BranchLifecycleService{gitCtx: gitCtx, store: store}
}

func (s *BranchLifecycleService) Archive(ctx context.Context, in inbound.BranchArchiveInput) (inbound.BranchArchiveOutput, error) {
	if err := domain.ValidateBranchName(in.Branch); err != nil {
		return inbound.BranchArchiveOutput{}, err
	}
	repoID := in.RepoID
	if repoID == "" {
		repo, err := s.gitCtx.CurrentRepo(ctx, in.Cwd)
		if err != nil {
			return inbound.BranchArchiveOutput{}, err
		}
		repoID = repo.ID
	}
	event, err := s.store.ArchiveBranchRef(ctx, repoID, in.Branch)
	if err != nil {
		return inbound.BranchArchiveOutput{}, err
	}
	parsed, ok, err := domain.ParseBranchLifecycleRef(event)
	if err != nil || !ok || parsed.State != domain.BranchArchived {
		if err != nil {
			return inbound.BranchArchiveOutput{}, err
		}
		return inbound.BranchArchiveOutput{}, domain.ErrInvalidRef
	}
	return inbound.BranchArchiveOutput{Branch: in.Branch, Target: parsed.Target, Event: event}, nil
}

// Rename transfers only the mutable branch projection. The destination is
// activated before the source is archived, so an interrupted rename leaves a
// recoverable duplicate rather than a missing branch. A force-renamed
// destination is archived first, preserving its former context as an
// immutable lifecycle root.
func (s *BranchLifecycleService) Rename(ctx context.Context, in inbound.BranchRenameInput) (inbound.BranchRenameOutput, error) {
	if err := domain.ValidateBranchName(in.From); err != nil {
		return inbound.BranchRenameOutput{}, err
	}
	if err := domain.ValidateBranchName(in.To); err != nil {
		return inbound.BranchRenameOutput{}, err
	}
	if in.From == in.To {
		return inbound.BranchRenameOutput{}, domain.ErrInvalidRef
	}
	repoID := in.RepoID
	if repoID == "" {
		repo, err := s.gitCtx.CurrentRepo(ctx, in.Cwd)
		if err != nil {
			return inbound.BranchRenameOutput{}, err
		}
		repoID = repo.ID
	}

	source, err := s.store.GetRef(ctx, repoID, domain.RefBranch, in.From)
	sourceArchived := false
	if errors.Is(err, domain.ErrNotFound) {
		refs, listErr := s.store.ListRefs(ctx, repoID)
		if listErr != nil {
			return inbound.BranchRenameOutput{}, listErr
		}
		latest, ok, stateErr := domain.LatestBranchLifecycle(refs, in.From)
		if stateErr != nil {
			return inbound.BranchRenameOutput{}, stateErr
		}
		if !ok || latest.State != domain.BranchArchived {
			return inbound.BranchRenameOutput{}, domain.ErrNotFound
		}
		source = domain.Ref{Kind: domain.RefBranch, Name: in.From, RepoID: repoID, Target: latest.Target}
		sourceArchived = true
	} else if err != nil {
		return inbound.BranchRenameOutput{}, err
	}

	destination, destErr := s.store.GetRef(ctx, repoID, domain.RefBranch, in.To)
	switch {
	case destErr == nil && destination.Target != source.Target:
		if _, err := s.store.ArchiveBranchRef(ctx, repoID, in.To); err != nil {
			return inbound.BranchRenameOutput{}, err
		}
		destErr = domain.ErrNotFound
	case destErr != nil && !errors.Is(destErr, domain.ErrNotFound):
		return inbound.BranchRenameOutput{}, destErr
	}
	if errors.Is(destErr, domain.ErrNotFound) {
		_, createErr := s.store.CreateBranchRef(ctx, domain.Ref{
			Kind: domain.RefBranch, Name: in.To, RepoID: repoID, Target: source.Target,
		})
		if errors.Is(createErr, domain.ErrBranchExists) {
			current, getErr := s.store.GetRef(ctx, repoID, domain.RefBranch, in.To)
			if getErr != nil || current.Target != source.Target {
				return inbound.BranchRenameOutput{}, createErr
			}
		} else if createErr != nil {
			return inbound.BranchRenameOutput{}, createErr
		}
	}
	if !sourceArchived {
		if _, err := s.store.ArchiveBranchRef(ctx, repoID, in.From); err != nil {
			return inbound.BranchRenameOutput{}, err
		}
	}
	return inbound.BranchRenameOutput{From: in.From, To: in.To, Target: source.Target}, nil
}

var _ inbound.BranchLifecycle = (*BranchLifecycleService)(nil)
