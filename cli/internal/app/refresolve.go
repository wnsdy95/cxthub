package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// resolveRef parses a ref string (HEAD/branch name/tag name/sha256:*) into a snapshot ContentHash.
// LoadSession and CheckoutSession share this functionality (backend architecture / compatibility rules).
//
// Parsing order:
//   - "" → "HEAD"
//   - "sha256:*" → used as is
//   - "HEAD" → HEAD ref (symbolic if pointing to a branch, its target is used)
//   - otherwise → branch ref → tag ref lookup order
func resolveRef(ctx context.Context, store outbound.SessionStore, repoID, ref string) (domain.ContentHash, error) {
	if ref == "" {
		ref = "HEAD"
	}
	if strings.HasPrefix(ref, "sha256:") {
		return domain.ContentHash(ref), nil
	}
	if ref == "HEAD" {
		head, err := store.GetRef(ctx, repoID, domain.RefHEAD, "HEAD")
		if err != nil {
			return "", err
		}
		if head.Target != "" {
			return head.Target, nil
		}
		if head.Symbolic != "" {
			b, err := store.GetRef(ctx, repoID, domain.RefBranch, head.Symbolic)
			if errors.Is(err, domain.ErrNotFound) {
				return "", fmt.Errorf("%w: HEAD points to branch %q not found", domain.ErrNotFound, head.Symbolic)
			}
			if err != nil {
				return "", err // permission/ruin/IO — preserve cause (grouping NotFound makes diagnosis impossible)
			}
			return b.Target, nil
		}
		return "", fmt.Errorf("%w: HEAD not found (no saved context)", domain.ErrNotFound)
	}
	b, berr := store.GetRef(ctx, repoID, domain.RefBranch, ref)
	if berr == nil {
		return b.Target, nil
	}
	if !errors.Is(berr, domain.ErrNotFound) {
		return "", berr // actual storage error — do not replace with "not found"
	}
	t, terr := store.GetRef(ctx, repoID, domain.RefTag, ref)
	if terr == nil {
		return t.Target, nil
	}
	if !errors.Is(terr, domain.ErrNotFound) {
		return "", terr
	}
	if event, ok, err := branchLifecycleByName(ctx, store, repoID, ref); err != nil {
		return "", err
	} else if ok {
		return event.Target, nil
	}
	return "", fmt.Errorf("%w: ref %q — not found in any branch or tag (list: cxt list)", domain.ErrNotFound, ref)
}

func branchLifecycleByName(ctx context.Context, store outbound.SessionStore, repoID, branch string) (domain.BranchLifecycleEvent, bool, error) {
	if err := domain.ValidateBranchName(branch); err != nil {
		return domain.BranchLifecycleEvent{}, false, err
	}
	refs, err := store.ListRefs(ctx, repoID)
	if err != nil {
		return domain.BranchLifecycleEvent{}, false, err
	}
	return domain.LatestBranchLifecycle(refs, branch)
}
