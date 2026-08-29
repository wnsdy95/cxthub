package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// TagService implements the TagRef inbound port for git tags.
//
// A tag is an immutable ref pointing to a snapshot. Only creation is supported, and reassignment of the same name is rejected
// (same as git; also rejected on server push — movement requires --force).
type TagService struct {
	gitCtx outbound.GitContext
	store  outbound.SessionStore
}

// NewTagService creates a TagService.
func NewTagService(gitCtx outbound.GitContext, store outbound.SessionStore) *TagService {
	return &TagService{gitCtx: gitCtx, store: store}
}

// Tag applies a tag to the snapshot pointed to by the specified ref (or HEAD if empty).
func (s *TagService) Tag(ctx context.Context, in inbound.TagInput) (inbound.TagOutput, error) {
	if in.Name == "" {
		return inbound.TagOutput{}, fmt.Errorf("tag name is required: cxt tag <name> [ref]")
	}
	if strings.HasPrefix(in.Name, domain.BranchLifecycleTagPrefix) {
		return inbound.TagOutput{}, fmt.Errorf("%w: %s is reserved for branch lifecycle events", domain.ErrInvalidRef, domain.BranchLifecycleTagPrefix)
	}
	repo, err := s.gitCtx.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return inbound.TagOutput{}, err
	}
	// Immutable rule: reject if it already exists (same as git tag).
	if existing, err := s.store.GetRef(ctx, string(repo.ID), domain.RefTag, in.Name); err == nil && existing.Target != "" {
		return inbound.TagOutput{}, fmt.Errorf("tag %q already exists (target: %s)", in.Name, existing.Target)
	}
	ref := in.Ref
	if ref == "" {
		ref = "HEAD"
	}
	target, err := resolveRef(ctx, s.store, string(repo.ID), ref)
	if err != nil {
		return inbound.TagOutput{}, err
	}
	if err := s.store.PutRef(ctx, domain.Ref{Kind: domain.RefTag, Name: in.Name, RepoID: string(repo.ID), Target: target}); err != nil {
		return inbound.TagOutput{}, err
	}
	return inbound.TagOutput{Name: in.Name, Target: target}, nil
}

// Tags returns the list of tags in the current repo.
func (s *TagService) Tags(ctx context.Context, cwd string) ([]domain.Ref, error) {
	repo, err := s.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return nil, err
	}
	refs, err := s.store.ListRefs(ctx, string(repo.ID))
	if err != nil {
		return nil, err
	}
	var tags []domain.Ref
	for _, r := range refs {
		if r.Kind == domain.RefTag {
			if _, lifecycle, err := domain.ParseBranchLifecycleRef(r); err != nil {
				return nil, err
			} else if lifecycle {
				continue
			}
			tags = append(tags, r)
		}
	}
	return tags, nil
}

// Ensure TagService implements inbound.TagRef.
var _ inbound.TagRef = (*TagService)(nil)
