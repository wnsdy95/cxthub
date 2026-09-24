package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"log"
	"reflect"
	"slices"
	"time"
)

// GitScans turns durable observations into immutable evidence. It owns no
// context ref writes, so late or local observations cannot move shared state.
type GitScans struct {
	sourceAccess GitSourceAuthorizer
	core         *Service
	store        outbound.GitScanStore
	reader       outbound.GitCommitReader
}

func NewGitScans(core *Service, reader outbound.GitCommitReader) (*GitScans, error) {
	st, ok := core.meta.(outbound.GitScanStore)
	_, headStore := core.meta.(outbound.GitHeadScanStore)
	_, treeStore := core.meta.(outbound.GitTreeStore)
	_, treeReader := reader.(outbound.GitTreeReader)
	if !ok || !headStore || !treeStore || !treeReader || reader == nil {
		return nil, domain.ErrValidation
	}
	return &GitScans{core: core, store: st, reader: reader}, nil
}
func (s *Service) queueHistoryGitScan(ctx context.Context, e domain.HistoryEvent) error {
	st, ok := s.meta.(outbound.GitScanStore)
	if !ok {
		return nil
	}
	repo, err := s.meta.GetRepo(ctx, domain.ContentHash(e.RepoID))
	if err != nil {
		return err
	}
	if repo.GitRemoteURL == "" {
		return nil
	}
	// Existing accepted history is also the CLI's durable offline outbox. A
	// duplicate delivery repairs enqueue after a development-FS process crash.
	oids := []string{e.GitBefore, e.GitAfter}
	if e.PR != nil {
		oids = append(oids, e.PR.MergeSHA, e.PR.HeadSHA)
	}
	for _, sha := range oids {
		if sha == "" {
			continue
		}
		if domain.ValidateGitOID(sha) != nil {
			continue
		}
		if err = st.EnqueueGitScan(ctx, domain.NewGitScan(repo.ID, repo.GitRemoteURL, sha, time.Now().UTC())); err != nil {
			return err
		}
	}
	return nil
}
func (g *GitScans) ObservePush(ctx context.Context, origin, ref, before, after string, forced bool, delivery string) (int, error) {
	repos, err := g.core.meta.ListRepos(ctx, "default")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range repos {
		if origin == "" || normalizeGitURL(origin) != normalizeGitURL(r.GitRemoteURL) {
			continue
		}
		o := domain.GitRefObservation{RepoID: r.ID, GitOrigin: r.GitRemoteURL, Source: "github-push", Ref: ref, Before: before, After: after, Forced: forced, Delivery: delivery}.WithID()
		err = repositoryWriteError(evidenceWriteContext(ctx), g.core, r.ID, func(tx context.Context) error {
			current, err := g.core.meta.GetRepo(tx, r.ID)
			if err != nil {
				return err
			}
			if current.GitRemoteURL != r.GitRemoteURL {
				return domain.ErrConflict
			}
			return g.store.RecordGitRefObservation(tx, o, time.Now().UTC())
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
func (g *GitScans) run(ctx context.Context, j domain.GitScanJob) error {
	work, cancel := context.WithTimeout(outbound.WithGitRepository(ctx, j.RepoID), 90*time.Second)
	defer cancel()
	p := domain.GitScanFinish{Job: j}
	p.Job.LeaseUntil = time.Time{}
	p.Job.UpdatedAt = time.Now().UTC()
	p.Job.NextAttempt = p.Job.UpdatedAt
	p.Job.State = "waiting"
	repo, err := g.core.meta.GetRepo(work, j.RepoID)
	if err == nil && repo.GitRemoteURL != j.GitOrigin {
		err = domain.ErrConflict
	}
	var fence GitReadFence
	if err == nil {
		fence, err = authorizeGitRead(work, g.sourceAccess, j.RepoID)
	}
	if err == nil && !j.TreeIndexed {
		var evidence domain.GitTreeEvidence
		evidence, err = g.reader.(outbound.GitTreeReader).ReadCommitTree(work, j.GitOrigin, j.Commit)
		if err == nil {
			p.Tree = &evidence
			p.Job.TreeIndexed = true
			err = p.Validate()
		}
	} else if err == nil && !j.Indexed {
		var deltas []domain.GitCommitDelta
		deltas, err = g.reader.ReadCommitDeltas(work, j.GitOrigin, j.Commit)
		if err == nil {
			err = planGitIndex(&p, deltas)
		}
		if err == nil {
			err = g.validateIndexTree(work, j, deltas)
		}
	} else if err == nil {
		var candidates []domain.GitInverseCandidate
		candidates, err = g.store.FindGitInverses(work, j.RepoID, j.GitOrigin, j.Commit, j.Cursor, 51)
		if err == nil {
			if len(candidates) <= 50 {
				p.Job.State = "completed"
			} else {
				candidates = candidates[:50]
			}
			for _, c := range candidates {
				p.Job.Cursor = c.Cursor
				// Arrival order is unrelated to Git ancestry. Both orientations are
				// verified; at most the ancestor -> descendant orientation can prove true.
				reverse := domain.GitChangeRequest{Target: c.Request.Commit, TargetParent: c.Request.Parent, Commit: c.Request.Target, Parent: c.Request.TargetParent}
				for _, r := range []domain.GitChangeRequest{c.Request, reverse} {
					now := p.Job.UpdatedAt
					p.Changes = append(p.Changes, domain.GitChangeJob{ID: domain.GitChangeID(j.RepoID, j.GitOrigin, r), RepoID: j.RepoID, GitOrigin: j.GitOrigin, Request: r, State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now})
				}
			}
		}
	}
	if err == nil {
		err = repositoryWriteError(evidenceWriteContext(work), g.core, j.RepoID, func(tx context.Context) error {
			r, e := g.core.meta.GetRepo(tx, j.RepoID)
			if e != nil {
				return e
			}
			if r.GitRemoteURL != j.GitOrigin {
				return domain.ErrConflict
			}
			if e := fence(tx); e != nil {
				return e
			}
			return g.store.FinishGitScan(tx, p)
		})
		if err == nil {
			return nil
		}
	}
	// No candidate/cursor publication on failure. Immutable job identity and
	// version fence prevent a canceled worker from replacing a newer result.
	fail := domain.GitScanFinish{Job: j}
	fail.Job.State = "retrying"
	fail.Job.Reason = "temporary_provider_or_storage_failure"
	fail.Job.UpdatedAt = time.Now().UTC()
	fail.Job.LeaseUntil = time.Time{}
	fail.Job.NextAttempt = fail.Job.UpdatedAt.Add(time.Second << min(j.Attempts, 9))
	switch {
	case errors.Is(err, domain.ErrIntegrity):
		fail.Job.State = "attention"
		fail.Job.Reason = "integrity_check_failed"
	case errors.Is(err, domain.ErrValidation):
		fail.Job.State = "attention"
		fail.Job.Reason = "invalid_or_ambiguous_git_evidence"
	case errors.Is(err, domain.ErrConflict):
		fail.Job.State = "attention"
		fail.Job.Reason = "origin_or_lease_changed"
	}
	finish, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	e := repositoryWriteError(evidenceWriteContext(finish), g.core, j.RepoID, func(tx context.Context) error { return g.store.FinishGitScan(tx, fail) })
	return errors.Join(err, e)
}
func planGitIndex(p *domain.GitScanFinish, deltas []domain.GitCommitDelta) error {
	if len(deltas) == 0 || len(deltas) > 16 {
		return domain.ErrIntegrity
	}
	parents := deltas[0].Parents
	want := len(parents)
	if want == 0 {
		want = 1
	}
	if len(deltas) != want {
		return domain.ErrIntegrity
	}
	seen := map[string]bool{}
	for _, d := range deltas {
		if d.Commit != p.Job.Commit || !d.Complete || !reflect.DeepEqual(d.Parents, parents) || seen[d.Parent] {
			return domain.ErrIntegrity
		}
		if err := d.Validate(); err != nil {
			return err
		}
		seen[d.Parent] = true
		p.Deltas = append(p.Deltas, domain.NewGitDelta(p.Job.RepoID, p.Job.GitOrigin, d))
	}
	for _, sha := range parents {
		p.Parents = append(p.Parents, domain.NewGitScan(p.Job.RepoID, p.Job.GitOrigin, sha, p.Job.UpdatedAt))
	}
	p.Job.Indexed = true
	return p.Validate()
}
func (g *GitScans) Process(ctx context.Context, limit int) error {
	ctx = inbound.WithSystemActor(ctx)
	for i := 0; i < limit; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		j, err := g.store.ClaimGitScan(ctx, "", time.Now().UTC(), 2*time.Minute)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = g.run(ctx, j); err != nil {
			log.Printf("Git observation %s attempt %d deferred", j.ID, j.Attempts)
		}
	}
	return nil
}
func (g *GitScans) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if err := g.Process(ctx, 2); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Git observation worker: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Cached evidence is repository-scoped and immutable. It avoids re-reading
// large provider trees for every candidate. Unknown objects use the provider.
type cachedGitEvidence struct {
	store  outbound.GitScanStore
	repo   domain.ContentHash
	reader outbound.GitEvidenceReader
}

func (c cachedGitEvidence) ReadCommitDelta(ctx context.Context, origin, commit, parent string) (domain.GitCommitDelta, error) {
	d, err := c.store.GetGitDelta(ctx, c.repo, origin, commit, parent)
	if err == nil {
		return d, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return d, err
	}
	return c.reader.ReadCommitDelta(outbound.WithGitRepository(ctx, c.repo), origin, commit, parent)
}
func (c cachedGitEvidence) IsGitAncestor(ctx context.Context, origin, ancestor, descendant string) (bool, error) {
	// Cached complete parent edges are authoritative immutable evidence. A
	// missing/ambiguous object is unknown, never proof of non-ancestry.
	todo := []string{descendant}
	seen := map[string]bool{}
	unknown := false
	for len(todo) > 0 && len(seen) < 256 {
		sha := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if sha == ancestor {
			return true, nil
		}
		if seen[sha] {
			continue
		}
		seen[sha] = true
		d, err := c.store.GetGitDelta(ctx, c.repo, origin, sha, "")
		if errors.Is(err, domain.ErrNotFound) {
			unknown = true
			continue
		}
		if err != nil {
			return false, err
		}
		todo = append(todo, d.Parents...)
	}
	if len(todo) == 0 && !unknown {
		return false, nil
	}
	return c.reader.IsGitAncestor(outbound.WithGitRepository(ctx, c.repo), origin, ancestor, descendant)
}

func (g *GitScans) ListScans(ctx context.Context, repo domain.ContentHash, cursor string, limit int) (domain.GitScanPage, error) {
	return repositoryRead(ctx, g.core, func(ctx context.Context) (domain.GitScanPage, error) {
		out := domain.GitScanPage{Items: []domain.GitScanJob{}}
		if domain.ValidateContentHash(repo) != nil || limit < 1 || limit > 100 {
			return out, domain.ErrValidation
		}
		if cursor != "" && domain.ValidateGitChangeID(cursor) != nil {
			return out, domain.ErrValidation
		}
		items, err := g.store.ListGitScans(ctx, repo, cursor, limit+1)
		if err != nil {
			return out, err
		}
		if len(items) > limit {
			out.NextCursor = items[limit-1].ID
			items = items[:limit]
		}
		out.Items = items
		if st, ok := g.core.meta.(outbound.GitHeadScanStore); ok {
			r, e := g.core.meta.GetRepo(ctx, repo)
			if e != nil {
				return out, e
			}
			head, e := st.GetGitHeadScan(ctx, repo, r.GitRemoteURL)
			if e == nil {
				out.Reconciliation = &head
			} else if !errors.Is(e, domain.ErrNotFound) {
				return out, e
			}
		}
		return out, nil
	})
}
func (g *GitScans) RetryScan(ctx context.Context, repo domain.ContentHash, id string) error {
	if domain.ValidateContentHash(repo) != nil || domain.ValidateGitChangeID(id) != nil {
		return domain.ErrValidation
	}
	return repositoryWriteError(evidenceWriteContext(ctx), g.core, repo, func(tx context.Context) error { return g.store.RetryGitScan(tx, repo, id, time.Now().UTC()) })
}

// Reconcile observes one durable page per repository on each pass. Failed or
// canceled reads retain their page; other repositories continue independently.
func (g *GitScans) Reconcile(ctx context.Context) error {
	ctx = inbound.WithSystemActor(ctx)
	st, ok := g.core.meta.(outbound.GitHeadScanStore)
	if !ok {
		return domain.ErrValidation
	}
	repos, err := g.core.meta.ListRepos(ctx, "default")
	if err != nil {
		return err
	}
	for _, repo := range repos {
		if repo.GitRemoteURL == "" {
			continue
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		j, e := st.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, time.Now().UTC(), time.Minute)
		if errors.Is(e, domain.ErrNotFound) {
			continue
		}
		if e != nil {
			return e
		}
		work, cancel := context.WithTimeout(outbound.WithGitRepository(ctx, j.RepoID), 30*time.Second)
		fence, e := authorizeGitRead(work, g.sourceAccess, j.RepoID)
		var heads []outbound.GitHead
		var more bool
		if e == nil {
			heads, more, e = g.reader.ListGitHeads(work, j.GitOrigin, j.Page)
		}
		if e == nil {
			observations := []domain.GitRefObservation{}
			for _, h := range heads {
				observations = append(observations, domain.GitRefObservation{RepoID: j.RepoID, GitOrigin: j.GitOrigin, Source: "reconciliation", Ref: h.Ref, After: h.Commit}.WithID())
			}
			e = repositoryWriteError(evidenceWriteContext(work), g.core, j.RepoID, func(tx context.Context) error {
				r, e := g.core.meta.GetRepo(tx, j.RepoID)
				if e != nil {
					return e
				}
				if r.GitRemoteURL != j.GitOrigin {
					return domain.ErrConflict
				}
				if e := fence(tx); e != nil {
					return e
				}
				return st.FinishGitHeadScan(tx, j, observations, more, time.Now().UTC())
			})
		}
		cancel()
		if e != nil {
			finish, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = repositoryWriteError(evidenceWriteContext(finish), g.core, j.RepoID, func(tx context.Context) error { return st.FailGitHeadScan(tx, j, time.Now().UTC()) })
			done()
			log.Printf("Git head reconciliation %s page %d deferred", j.RepoID, j.Page)
		}
	}
	return nil
}
func (g *GitScans) RunReconciler(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if err := g.Reconcile(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Git reconciliation: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) queuePRGitScans(ctx context.Context, repo domain.ContentHash, origin string, pr domain.PullRequestMerge) error {
	st, ok := s.meta.(outbound.GitScanStore)
	if !ok {
		return nil
	}
	for _, sha := range []string{pr.HeadSHA, pr.MergeSHA} {
		if domain.ValidateGitOID(sha) != nil {
			continue
		}
		if err := st.EnqueueGitScan(ctx, domain.NewGitScan(repo, origin, sha, time.Now().UTC())); err != nil {
			return err
		}
	}
	return nil
}

// Index and tree come from separate bounded reads. Verify their common
// immutable identity before marking the index complete.
func (g *GitScans) validateIndexTree(ctx context.Context, j domain.GitScanJob, deltas []domain.GitCommitDelta) error {
	st := g.core.meta.(outbound.GitTreeStore)
	c, err := st.GetGitCommitTree(ctx, j.RepoID, j.GitOrigin, j.Commit)
	if err != nil {
		return err
	}
	cache := map[string]domain.GitTreeNode{}
	for _, d := range deltas {
		if !slices.Equal(c.Parents, d.Parents) {
			return domain.ErrIntegrity
		}
		for _, change := range d.Changes {
			entry, err := cachedGitEntry(ctx, st, j.RepoID, j.GitOrigin, c.Tree, change.Path, cache)
			if err != nil {
				return err
			}
			if entry != change.After {
				return domain.ErrIntegrity
			}
		}
	}
	return nil
}

// SetSourceAuthorizer configures source policy before workers start.
func (g *GitScans) SetSourceAuthorizer(a GitSourceAuthorizer) { g.sourceAccess = a }
