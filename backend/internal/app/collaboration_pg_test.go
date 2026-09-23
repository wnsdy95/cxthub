//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func collaborationDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	// Saturate a small pool on every machine. A store helper escaping the
	// transaction would otherwise appear correct with many spare connections.
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("pool_max_conns", "2")
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " pool_max_conns=2"
}

func collaborationPG(t *testing.T) (*Service, *store.PostgresStore, domain.ContentHash) {
	t.Helper()
	dsn := collaborationDSN(t)
	ctx := systemTestContext()
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	repo := hh(fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/proj.git"}); err != nil {
		t.Fatal(err)
	}
	return NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st), st, repo
}

func collaborationPR(t *testing.T, svc *Service, st *store.PostgresStore, repo domain.ContentHash, base domain.ContentHash, n int) (domain.PullRequestMerge, domain.ContentHash) {
	t.Helper()
	tip := collaborationSnapshot(t, st, repo, fmt.Sprintf("feature-%d", n), base)
	pr := domain.PullRequestMerge{Number: n, BaseBranch: "main", HeadBranch: fmt.Sprintf("feature/%d", n), HeadSHA: fmt.Sprintf("%040x", n), MergeSHA: fmt.Sprintf("%040x", n+100)}
	birth := domain.HistoryEvent{ID: fmt.Sprintf("%032x", n), RepoID: string(repo), BranchID: pr.HeadBranch, Branch: pr.HeadBranch, Kind: "birth", Source: base, Target: tip, GitAfter: pr.HeadSHA, CreatedAt: time.Now().UTC()}
	if err := svc.RecordHistory(systemTestContext(), birth); err != nil {
		t.Fatal(err)
	}
	publishPRSource(t, svc, birth)
	return pr, tip
}

func TestPGCollaborationConcurrentPRsAndPushes(t *testing.T) {
	svc, st, repo := collaborationPG(t)
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	other := NewService(peer, peer, auth.NewTeamTokenAuth(), gitengine.NewEngine(peer), peer)
	base := collaborationSnapshot(t, st, repo, "base")
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
		t.Fatal(err)
	}
	const count = 6
	var prs []domain.PullRequestMerge
	var tips []domain.ContentHash
	for i := 1; i <= count; i++ {
		pr, tip := collaborationPR(t, svc, st, repo, base, i)
		prs = append(prs, pr)
		tips = append(tips, tip)
	}
	push := collaborationSnapshot(t, st, repo, "concurrent push", base)
	start := make(chan struct{})
	errs := make(chan error, count*2+1)
	var wg sync.WaitGroup
	for _, pr := range prs {
		for _, writer := range []*Service{svc, other} {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; _, err := writer.PromoteRepositoryPR(ctx, repo, pr); errs <- err }()
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, err := other.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repo, Ref: domain.Ref{Kind: domain.RefBranch, Name: "main", Target: push}, Append: true})
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	head, err := st.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, tip := range append(tips, push) {
		yes, err := svc.engine.IsAncestor(ctx, repo, tip, head.Target)
		if err != nil || !yes {
			t.Fatalf("lost collaborator %s: %v", tip, err)
		}
	}
	events, err := svc.ListHistory(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	completions := map[int]int{}
	for _, e := range events {
		if e.PRCompleted {
			completions[e.PR.Number]++
		}
	}
	for _, pr := range prs {
		if completions[pr.Number] != 1 {
			t.Fatalf("PR %d completion count %d", pr.Number, completions[pr.Number])
		}
	}
	for _, tip := range tips {
		snap, _ := st.GetSnapshot(ctx, repo, tip)
		if len(snap.Parents) != 1 || snap.Parents[0] != base {
			t.Fatal("natural ancestry changed")
		}
	}
}

type rejectJobFinishPG struct{ *store.PostgresStore }

func (s *rejectJobFinishPG) FinishPRJob(ctx context.Context, j domain.PRPromotionJob) error {
	if j.State == "completed" {
		return errors.New("injected job completion failure")
	}
	return s.PostgresStore.FinishPRJob(ctx, j)
}

func TestPGCollaborationWorkerFencingAndCompletion(t *testing.T) {
	svc, st, repo := collaborationPG(t)
	ctx := systemTestContext()
	base := collaborationSnapshot(t, st, repo, "base")
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
		t.Fatal(err)
	}
	pr, tip := collaborationPR(t, svc, st, repo, base, 1)
	job, err := svc.SubmitPRPromotion(ctx, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	old, err := st.ClaimPRJob(ctx, repo, job.ID, time.Now(), -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.runPRJob(ctx, old); err == nil {
		t.Fatal("expired worker accepted")
	}
	if err := st.RetryPRJob(ctx, repo, job.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	current, err := st.ClaimPRJob(ctx, repo, job.ID, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.runPRJob(ctx, old); err == nil {
		t.Fatal("superseded worker accepted")
	}
	ref, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
	if ref.Target != base {
		t.Fatal("stale worker moved main")
	}
	failing := &rejectJobFinishPG{st}
	writer := NewService(failing, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	if _, err := writer.runPRJob(ctx, current); err == nil {
		t.Fatal("job failure ignored")
	}
	ref, _ = st.GetRef(ctx, repo, domain.RefBranch, "main")
	events, _ := svc.ListHistory(ctx, repo)
	if ref.Target != base {
		t.Fatal("graph committed without job completion")
	}
	for _, e := range events {
		if e.PRCompleted {
			t.Fatal("completion committed without job")
		}
	}
	if err := svc.RetryPRPromotion(ctx, repo, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeliverPRPromotion(ctx, repo, pr); err != nil {
		t.Fatal(err)
	}
	job, err = st.GetPRJob(ctx, repo, job.ID)
	if err != nil || job.State != "completed" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	ref, _ = st.GetRef(ctx, repo, domain.RefBranch, "main")
	if ref.Target != tip {
		t.Fatal("retry did not complete")
	}
	// A completed replay must not undo a later explicit rewind.
	if _, err := svc.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repo, Ref: domain.Ref{Kind: domain.RefBranch, Name: "main", Target: base}, ExpectedTarget: tip, Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeliverPRPromotion(ctx, repo, pr); err != nil {
		t.Fatal(err)
	}
	ref, _ = st.GetRef(ctx, repo, domain.RefBranch, "main")
	if ref.Target != base {
		t.Fatal("duplicate delivery undid rewind")
	}
}

func TestPGCollaborationStaleForceAndMemoryCAS(t *testing.T) {
	svc, st, repo := collaborationPG(t)
	ctx := systemTestContext()
	a := collaborationSnapshot(t, st, repo, "a")
	b := collaborationSnapshot(t, st, repo, "b", a)
	c := collaborationSnapshot(t, st, repo, "c", a)
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: b}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repo, Ref: domain.Ref{Kind: domain.RefBranch, Name: "main", Target: c}, ExpectedTarget: a, Force: true}); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatalf("stale force: %v", err)
	}
	first, err := svc.PutMemoryDigestCAS(ctx, repo, domain.MemoryDigest{SnapshotID: b, Summary: "BASE"})
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, text := range []string{"ALICE", "BOB"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.PutMemoryDigestCAS(ctx, repo, domain.MemoryDigest{SnapshotID: b, Summary: text, PreviousMemoryHash: first})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	ok, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			ok++
		} else if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrRefConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if ok != 1 || conflicts != 1 {
		t.Fatalf("memory CAS winners=%d conflicts=%d", ok, conflicts)
	}
	for _, text := range []string{"ALICE", "BOB"} {
		d := domain.MemoryDigest{SnapshotID: b, Summary: text, PreviousMemoryHash: first}
		hash, _ := domain.MemoryDigestHash(d)
		if _, err := st.GetMemory(ctx, repo, hash); err != nil {
			t.Fatalf("losing immutable memory lost: %v", err)
		}
	}
}

func collaborationSnapshot(t *testing.T, st *store.PostgresStore, repo domain.ContentHash, name string, parents ...domain.ContentHash) domain.ContentHash {
	t.Helper()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: string(repo) + name}}}}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err = st.PutDoc(systemTestContext(), repo, doc); err != nil {
		t.Fatal(err)
	}
	if err = st.PutSnapshot(systemTestContext(), domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, Parents: parents, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	return doc.Hash
}

func TestPGCollaborationRefBatchRollback(t *testing.T) {
	svc, st, repo := collaborationPG(t)
	ctx := systemTestContext()
	a := collaborationSnapshot(t, st, repo, "a")
	b := collaborationSnapshot(t, st, repo, "b", a)
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: a}
	if err := st.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	before, _ := st.ReadReflog(ctx, repo)
	ref.Target = b
	_, err := svc.UpdateRefs(ctx, inbound.UpdateRefsInput{RepoID: repo, Updates: []inbound.UpdateRefInput{
		{Ref: ref, ExpectedTarget: a},
		{Ref: domain.Ref{Kind: domain.RefBranch, Name: "other", Target: hh("missing")}},
	}})
	if err == nil {
		t.Fatal("invalid batch accepted")
	}
	after, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
	logs, _ := st.ReadReflog(ctx, repo)
	if after.Target != a || len(logs) != len(before) {
		t.Fatalf("partial batch became visible: ref=%s reflog=%d/%d", after.Target, len(logs), len(before))
	}
}

type rejectCompletionPG struct{ *store.PostgresStore }

type rejectPendingCleanupPG struct{ *store.PostgresStore }

func (s *rejectPendingCleanupPG) CompareAndDeletePending(context.Context, domain.ContentHash, string, domain.ContentHash) (domain.PendingDeleteResult, error) {
	return domain.PendingDeleteKept, errors.New("injected pending cleanup failure")
}

func TestPGCollaborationPendingResolutionRollback(t *testing.T) {
	_, st, repo := collaborationPG(t)
	ctx := systemTestContext()
	base := collaborationSnapshot(t, st, repo, "base")
	tip := collaborationSnapshot(t, st, repo, "tip", base)
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: "capture", Target: tip}); err != nil {
		t.Fatal(err)
	}
	failing := &rejectPendingCleanupPG{st}
	svc := NewService(failing, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(failing), st)
	_, err := svc.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repo, Ref: domain.Ref{Kind: domain.RefBranch, Name: "main", Target: tip}})
	if err == nil || err.Error() != "injected pending cleanup failure" {
		t.Fatalf("cleanup error hidden: %v", err)
	}
	ref, err := st.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || ref.Target != base {
		t.Fatalf("partial ref publication: %+v %v", ref, err)
	}
	pending, err := st.ListPendings(ctx, repo)
	if err != nil || len(pending) != 1 || pending[0].Target != tip {
		t.Fatalf("capture lost: %+v %v", pending, err)
	}
}

func (s *rejectCompletionPG) ApplyHistoryEvent(ctx context.Context, e domain.HistoryEvent) error {
	if e.PRCompleted {
		return errors.New("injected completion failure")
	}
	return s.PostgresStore.ApplyHistoryEvent(ctx, e)
}

func TestPGCollaborationPromotionRollback(t *testing.T) {
	svc, st, repo := collaborationPG(t)
	ctx := systemTestContext()
	base := collaborationSnapshot(t, st, repo, "base")
	main := collaborationSnapshot(t, st, repo, "main", base)
	feature := collaborationSnapshot(t, st, repo, "feature", base)
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: main}, ""); err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	birth := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "feature-id", Branch: "feature", Kind: "birth", Source: base, Target: feature, GitAfter: pr.HeadSHA, CreatedAt: time.Now().UTC()}
	if err := svc.RecordHistory(ctx, birth); err != nil {
		t.Fatal(err)
	}
	publishPRSource(t, svc, birth)
	failing := &rejectCompletionPG{st}
	svc = NewService(failing, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(failing), st)
	if _, err := svc.PromoteRepositoryPR(ctx, repo, pr); err == nil {
		t.Fatal("injected failure ignored")
	}
	ref, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
	snap, _ := st.GetSnapshot(ctx, repo, feature)
	if ref.Target != main || len(snap.GraftParents) != 0 {
		t.Fatalf("incomplete promotion committed: ref=%s graft=%v", ref.Target, snap.GraftParents)
	}
}

type pausedViewPG struct {
	*store.PostgresStore
	read   chan struct{}
	resume chan struct{}
}

func (s *pausedViewPG) ListRefs(ctx context.Context, repo domain.ContentHash) ([]domain.Ref, error) {
	refs, err := s.PostgresStore.ListRefs(ctx, repo)
	close(s.read)
	select {
	case <-s.resume:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return refs, err
}

func TestPGCollaborationGraphViewIsOneGeneration(t *testing.T) {
	svc, st, repo := collaborationPG(t)
	ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
	defer cancel()
	base := collaborationSnapshot(t, st, repo, "base")
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
		t.Fatal(err)
	}
	paused := &pausedViewPG{PostgresStore: st, read: make(chan struct{}), resume: make(chan struct{})}
	reader := NewService(paused, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(paused), st)
	type result struct {
		view domain.RepositoryView
		err  error
	}
	resultCh := make(chan result, 1)
	go func() { view, err := reader.GetRepositoryView(ctx, repo); resultCh <- result{view, err} }()
	select {
	case <-paused.read:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Commit after the reader fetched refs but before its snapshots/history reads.
	pr, tip := collaborationPR(t, svc, st, repo, base, 75)
	if _, err := svc.PromoteRepositoryPR(ctx, repo, pr); err != nil {
		t.Fatal(err)
	}
	close(paused.resume)
	old := <-resultCh
	if old.err != nil {
		t.Fatal(old.err)
	}
	if len(old.view.Snapshots) != 1 || len(old.view.History) != 0 || old.view.Refs[0].Target != base {
		t.Fatalf("mixed graph generations: %+v", old.view)
	}
	current, err := svc.GetRepositoryView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ref := range current.Refs {
		if ref.Name == "main" {
			found = ref.Target == tip
		}
	}
	if !found || len(current.Snapshots) != 2 || len(current.History) == 0 {
		t.Fatalf("missing committed view: %+v", current)
	}
	if current.Pending == nil || current.Unsync == nil {
		t.Fatal("empty collections must be JSON arrays")
	}
}

type rejectSnapshotPG struct {
	*store.PostgresStore
	reject domain.ContentHash
}

func (s *rejectSnapshotPG) PutSnapshot(ctx context.Context, snap domain.Snapshot) error {
	if snap.ID == s.reject {
		return errors.New("injected second snapshot failure")
	}
	return s.PostgresStore.PutSnapshot(ctx, snap)
}

func TestPGCollaborationObjectBatchRollbackAndAfterCommit(t *testing.T) {
	svc, st, repo := collaborationPG(t)
	ctx := systemTestContext()
	username := fmt.Sprintf("acid%d", time.Now().UnixNano())
	user := domain.User{ID: "dev:" + username, Name: "ACID", Email: username + "@example.com", Username: username}
	if err := st.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	repositoryRecord := domain.Repository{ID: domain.NewID("ws_"), Name: "ACID", Slug: "acid", OwnerID: user.ID, OwnerUsername: username, CreatedAt: time.Now().UTC()}
	if err := st.CreateRepository(ctx, repositoryRecord); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: repositoryRecord.ID}); err != nil {
		t.Fatal(err)
	}

	var docs []domain.SessionDoc
	var snaps []domain.Snapshot
	for i := 0; i < 2; i++ {
		doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("%s/%d", repo, i)}}}}}}
		raw, err := domain.CanonicalBytes(doc.CIR)
		if err != nil {
			t.Fatal(err)
		}
		doc.Hash = domain.HashContent(raw)
		docs = append(docs, doc)
		snaps = append(snaps, domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull})
	}
	failing := &rejectSnapshotPG{PostgresStore: st, reject: snaps[1].ID}
	broken := NewService(failing, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(failing), st)
	if _, err := broken.Commit(ctx, inbound.CommitInput{RepoID: repo, Docs: docs, Snapshots: snaps}); err == nil || err.Error() != "injected second snapshot failure" {
		t.Fatalf("did not reach injected second snapshot failure: %v", err)
	}
	for _, doc := range docs {
		if _, err := st.GetDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("document partially published: %v", err)
		}
		if _, err := st.GetSnapshot(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("snapshot partially published: %v", err)
		}
	}
	notifications := 0
	if err := repositoryWriteError(ctx, svc, repo, func(ctx context.Context) error {
		afterRepositoryCommit(ctx, func() { notifications++ })
		return errors.New("abort")
	}); err == nil {
		t.Fatal("rollback failed")
	}
	if notifications != 0 {
		t.Fatal("notified an aborted operation")
	}
	if err := repositoryWriteError(ctx, svc, repo, func(ctx context.Context) error {
		return repositoryWriteError(ctx, svc, repo, func(ctx context.Context) error {
			afterRepositoryCommit(ctx, func() { notifications++ })
			if notifications != 0 {
				t.Fatal("nested operation notified before outer commit")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if notifications != 1 {
		t.Fatalf("notifications=%d", notifications)
	}
	attempts := 0
	if err := repositoryWriteError(ctx, svc, repo, func(ctx context.Context) error {
		attempts++
		afterRepositoryCommit(ctx, func() { notifications++ })
		if attempts == 1 {
			return &pgconn.PgError{Code: "40001", Message: "injected serialization abort"}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || notifications != 2 {
		t.Fatalf("retry leaked callbacks: attempts=%d notifications=%d", attempts, notifications)
	}
	attempts = 0
	if err := repositoryWriteError(ctx, svc, repo, func(ctx context.Context) error {
		attempts++
		afterRepositoryCommit(ctx, func() { notifications++ })
		return errors.New("unknown commit outcome")
	}); err == nil {
		t.Fatal("unknown outcome acknowledged")
	}
	if attempts != 1 || notifications != 2 {
		t.Fatal("unknown commit outcome was retried or notified")
	}
}
