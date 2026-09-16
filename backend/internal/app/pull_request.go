package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// PromoteMergedPR shares the same durable source binding as authenticated local
// hook promotion. Branch names never select the current source ref.
func (s *Service) PromoteMergedPR(ctx context.Context, gitURL string, pr domain.PullRequestMerge) (int, error) {
	if err := pr.Validate(); err != nil {
		return 0, fmt.Errorf("%w: %v", domain.ErrValidation, err)
	}
	repos, err := s.meta.ListRepos(ctx, "default")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, repo := range repos {
		if gitURL == "" || repo.GitRemoteURL == "" || normalizeGitURL(gitURL) != normalizeGitURL(repo.GitRemoteURL) {
			continue
		}
		out, err := s.PromoteRepositoryPR(ctx, repo.ID, pr)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return n, err
		}
		if out.Result != inbound.RefUpToDate {
			n++
		}
	}
	return n, nil
}

// The receipt is published before append, so a crash or another delivery always
// resumes the same snapshot. Appending is idempotent and preserves the base DAG.
func (s *Service) PromoteRepositoryPR(ctx context.Context, repoID domain.ContentHash, pr domain.PullRequestMerge) (inbound.UpdateRefOutput, error) {
	var zero inbound.UpdateRefOutput
	if err := pr.Validate(); err != nil {
		return zero, fmt.Errorf("%w: %v", domain.ErrValidation, err)
	}
	repo, err := s.meta.GetRepo(ctx, repoID)
	if err != nil {
		return zero, err
	}
	if repo.GitRemoteURL == "" {
		return zero, fmt.Errorf("%w: repository has no Git origin", domain.ErrValidation)
	}
	key := sha256.Sum256([]byte(string(repoID) + "\x00" + normalizeGitURL(repo.GitRemoteURL) + fmt.Sprintf("\x00pr:%d", pr.Number)))
	id := fmt.Sprintf("%x", key[:16])
	events, err := s.ListHistory(ctx, repoID)
	if err != nil {
		return zero, err
	}
	var receipt domain.HistoryEvent
	for _, e := range events {
		if e.ID == id {
			receipt = e
			break
		}
	}
	if receipt.ID == "" {
		source, identity, err := s.resolvePRSource(ctx, repoID, pr, events)
		if err != nil {
			return zero, err
		}
		bindings, err := domain.ProjectContextBranches(events)
		if err != nil {
			return zero, err
		}
		baseID := domain.LegacyContextBranchID(string(repoID), pr.BaseBranch)
		if b, ok := bindings.Active[pr.BaseBranch]; ok {
			baseID = b.ID
		} else if bindings.Released[pr.BaseBranch] != "" {
			return zero, fmt.Errorf("%w: PR base no longer exists", domain.ErrConflict)
		}
		base, err := s.meta.GetRef(ctx, repoID, domain.RefBranch, pr.BaseBranch)
		if err != nil {
			return zero, err
		}
		receipt = domain.HistoryEvent{ID: id, RepoID: string(repoID), BranchID: baseID, Branch: pr.BaseBranch, Kind: "pr-merge", Source: source, Target: source, SharedTarget: base.Target, SourceBranchID: identity, PR: &pr, CreatedAt: time.Now().UTC()}
		if err := s.recordHistory(ctx, receipt, true); err != nil {
			// Concurrent delivery may have published the immutable receipt first.
			if !errors.Is(err, domain.ErrRefConflict) {
				return zero, err
			}
			latest, readErr := s.ListHistory(ctx, repoID)
			if readErr != nil {
				return zero, readErr
			}
			receipt = domain.HistoryEvent{}
			for _, e := range latest {
				if e.ID == id {
					receipt = e
					break
				}
			}
		}
	}
	if receipt.Kind != "pr-merge" || receipt.PR == nil || *receipt.PR != pr {
		return zero, fmt.Errorf("%w: PR identity differs from its stored binding", domain.ErrConflict)
	}
	for attempt := 0; attempt < 4; attempt++ {
		// Recheck the current base identity. A newly reused base name must not receive
		// an old PR. A verified rename of the original base follows that identity.
		latest, err := s.ListHistory(ctx, repoID)
		if err != nil {
			return zero, err
		}
		bindings, err := domain.ProjectContextBranches(latest)
		if err != nil {
			return zero, err
		}
		baseName := receipt.Branch
		if b, ok := bindings.ByID[receipt.BranchID]; ok {
			if b.Archived {
				return zero, fmt.Errorf("%w: PR base is archived", domain.ErrConflict)
			}
			baseName = b.Name
		} else if b, ok := bindings.Active[baseName]; (ok && b.ID != receipt.BranchID) || bindings.Released[baseName] != "" {
			return zero, fmt.Errorf("%w: PR base identity changed", domain.ErrConflict)
		}
		current, err := s.meta.GetRef(ctx, repoID, domain.RefBranch, baseName)
		if err != nil {
			return zero, err
		}
		if yes, err := s.engine.IsAncestor(ctx, repoID, receipt.Source, current.Target); err != nil {
			return zero, err
		} else if yes {
			return s.completePRPromotion(ctx, receipt, current.Target, inbound.UpdateRefOutput{Ref: current, ServerTarget: current.Target, RequestedTarget: receipt.Source, Result: inbound.RefUpToDate})
		}
		out, err := s.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repoID, Ref: domain.Ref{RepoID: repoID, Kind: domain.RefBranch, Name: baseName, BranchID: receipt.BranchID, Target: receipt.Source}, ExpectedTarget: current.Target, Append: true})
		if err == nil {
			return s.completePRPromotion(ctx, receipt, current.Target, out)
		}
		if !errors.Is(err, domain.ErrRefConflict) {
			return out, err
		}
	}
	return zero, domain.ErrRefConflict
}

// Binding and completion are separate immutable events. If recording completion
// fails after append, the next delivery verifies reachability and retries it.
// This also proves joins that leave the base ref unchanged.
func (s *Service) completePRPromotion(ctx context.Context, receipt domain.HistoryEvent, before domain.ContentHash, out inbound.UpdateRefOutput) (inbound.UpdateRefOutput, error) {
	key := sha256.Sum256([]byte(receipt.ID + ":completed"))
	id := fmt.Sprintf("%x", key[:16])
	accepted := func() (bool, error) {
		rows, err := s.ListHistory(ctx, domain.ContentHash(receipt.RepoID))
		if err != nil {
			return false, err
		}
		for _, e := range rows {
			if e.ID != id {
				continue
			}
			if e.Kind != "pr-merge" || !e.PRCompleted || e.PR == nil || *e.PR != *receipt.PR || e.Source != receipt.Source || e.SourceBranchID != receipt.SourceBranchID || e.BranchID != receipt.BranchID {
				return false, domain.ErrRefConflict
			}
			return true, nil
		}
		return false, nil
	}
	if exists, err := accepted(); exists || err != nil {
		return out, err
	}
	completed := receipt
	completed.ID = id
	completed.PRCompleted = true
	completed.SharedTarget = before
	completed.Target = out.Ref.Target
	completed.Branch = out.Ref.Name
	completed.CreatedAt = time.Now().UTC()
	if err := s.recordHistory(ctx, completed, true); err != nil {
		if !errors.Is(err, domain.ErrRefConflict) {
			return out, err
		}
		if exists, readErr := accepted(); exists || readErr != nil {
			return out, readErr
		}
		return out, err
	}
	return out, nil
}

var legacyPRGitLink = regexp.MustCompile(`\[git ([0-9a-f]{40}|[0-9a-f]{64})\]`)

func (s *Service) resolvePRSource(ctx context.Context, repoID domain.ContentHash, pr domain.PullRequestMerge, events []domain.HistoryEvent) (domain.ContentHash, string, error) {
	candidates := map[domain.ContentHash]string{}
	for _, e := range events {
		if e.Kind == "pr-merge" || e.GitAfter != pr.HeadSHA || e.Target == "" || !matchesPRSourceBranch(e, pr.HeadBranch, events) {
			continue
		}
		if old, ok := candidates[e.Target]; ok && old != e.BranchID {
			return "", "", fmt.Errorf("%w: multiple identities at the PR source revision", domain.ErrConflict)
		}
		candidates[e.Target] = e.BranchID
	}
	if len(candidates) == 0 {
		projection, err := domain.ProjectContextBranches(events)
		if err != nil {
			return "", "", err
		}
		// Legacy labels are usable only before an explicit identity/release exists.
		// Only full Git links prove the revision. Parallel candidates must form
		// one proven ancestry chain below.
		if _, known := projection.Active[pr.HeadBranch]; !known && projection.Released[pr.HeadBranch] == "" {
			snaps, err := s.meta.ListSnapshots(ctx, repoID, "")
			if err != nil {
				return "", "", err
			}
			for _, snap := range snaps {
				if snap.Branch != pr.HeadBranch {
					continue
				}
				match := legacyPRGitLink.FindStringSubmatch(snap.Message)
				if len(match) == 2 && pr.HeadSHA == match[1] {
					candidates[snap.ID] = domain.LegacyContextBranchID(string(repoID), pr.HeadBranch)
				}
			}
		}
	}
	if len(candidates) == 0 {
		p, err := domain.ProjectContextBranches(events)
		if err != nil {
			return "", "", err
		}
		_, known := p.Active[pr.HeadBranch]
		if _, err := s.meta.GetRef(ctx, repoID, domain.RefBranch, pr.HeadBranch); err == nil {
			known = true
		} else if !errors.Is(err, domain.ErrNotFound) {
			return "", "", err
		}
		if known || p.Released[pr.HeadBranch] != "" {
			return "", "", fmt.Errorf("%w: PR #%d source revision has no exact context association; sync its history and retry", domain.ErrConflict, pr.Number)
		}
		return "", "", fmt.Errorf("%w: no recorded context at PR #%d head %s; sync the source history and retry", domain.ErrNotFound, pr.Number, pr.HeadSHA)
	}
	identity := ""
	for _, branchID := range candidates {
		if identity != "" && identity != branchID {
			return "", "", fmt.Errorf("%w: PR source revision belongs to multiple context identities", domain.ErrConflict)
		}
		identity = branchID
	}
	for tip := range candidates {
		containsAll := true
		for other := range candidates {
			yes, err := s.engine.IsAncestor(ctx, repoID, other, tip)
			if err != nil {
				return "", "", err
			}
			if !yes {
				containsAll = false
				break
			}
		}
		if containsAll {
			return tip, identity, nil
		}
	}
	return "", "", fmt.Errorf("%w: PR revision has divergent context tips; reconcile them before promotion", domain.ErrConflict)
}

// A tracking alias publishes under the shared context branch while LocalBranch
// records the native Git name. Use that exact observation only with an existing
// attachment of the same worktree and context identity. Names may have changed
// since attachment; neither today's ref nor a same-named branch proves the source.
func matchesPRSourceBranch(e domain.HistoryEvent, head string, events []domain.HistoryEvent) bool {
	if e.Branch == head {
		return true
	}
	if e.LocalBranch != head || e.WorktreeID == "" {
		return false
	}
	for _, attached := range events {
		if attached.Kind == "attach" && attached.LocalBranch != "" &&
			attached.WorktreeID == e.WorktreeID && attached.BranchID == e.BranchID &&
			!attached.CreatedAt.After(e.CreatedAt) {
			return true
		}
	}
	return false
}
