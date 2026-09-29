package app

import (
	"context"
	"errors"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Preserve the identity used to resolve a source. Snapshot.Branch is a capture
// label, not a selector; direct hashes and tags never inherit the current HEAD.
func loadAgentSourceBranch(ctx context.Context, store outbound.SessionStore, repo, ref, branch string, selected domain.ContentHash) (string, error) {
	canonical := func(name string) (string, error) {
		if err := domain.ValidateBranchName(name); err != nil {
			return "", err
		}
		if aliases, ok := store.(outbound.LocalBranchReader); ok {
			binding, err := aliases.ResolveLocalBranch(ctx, repo, name)
			if err != nil {
				return "", err
			}
			if binding.Inactive {
				return "", domain.ErrBranchArchived
			}
			return binding.Branch, nil
		}
		return name, nil
	}
	if branch != "" {
		return canonical(branch) // checkout already froze the source snapshot
	}
	if strings.HasPrefix(ref, "sha256:") {
		return "", domain.ValidateContentHash(domain.ContentHash(ref))
	}
	if ref == "" || ref == "HEAD" {
		head, err := store.GetRef(ctx, repo, domain.RefHEAD, "HEAD")
		if err != nil {
			return "", err
		}
		if head.Target != "" {
			if head.Target != selected {
				return "", domain.ErrSelectionChanged
			}
			if head.Symbolic != "" {
				return canonical(strings.TrimPrefix(head.Symbolic, "refs/heads/"))
			}
			if positions, ok := store.(outbound.WorkingPositionQueryReader); ok {
				p, err := positions.ReadWorkingPosition(ctx, repo)
				if err != nil && !errors.Is(err, domain.ErrNotFound) {
					return "", err
				}
				if err == nil {
					if p.Snapshot != selected {
						return "", domain.ErrSelectionChanged
					}
					return p.Branch, nil
				}
			}
			return "", nil
		}
		ref = strings.TrimPrefix(head.Symbolic, "refs/heads/")
	}
	name, err := canonical(ref)
	if err != nil {
		return "", err
	}
	b, err := store.GetRef(ctx, repo, domain.RefBranch, name)
	if err == nil {
		if b.Target != selected {
			return "", domain.ErrSelectionChanged
		}
		return name, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return "", err
	}
	tag, err := store.GetRef(ctx, repo, domain.RefTag, ref)
	if err == nil {
		if tag.Target != selected {
			return "", domain.ErrSelectionChanged
		}
		return "", nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return "", err
	}
	if event, ok, err := branchLifecycleByName(ctx, store, repo, ref); err != nil {
		return "", err
	} else if ok && event.Target == selected {
		return name, nil
	}
	return "", domain.ErrSelectionChanged
}
