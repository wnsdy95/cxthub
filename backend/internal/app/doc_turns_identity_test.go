package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type agentPageReadKey struct{}
type agentPageReadStore struct {
	*store.FSStore
	transactions int
	proofs       map[domain.ContentHash]int
	after        func(domain.ContentHash)
	snapshot     func(domain.Snapshot) domain.Snapshot
}

func (s *agentPageReadStore) WithinReadSnapshot(ctx context.Context, fn func(context.Context) error) error {
	if ctx.Value(agentPageReadKey{}) != nil {
		return errors.New("nested repository read")
	}
	s.transactions++
	return fn(context.WithValue(ctx, agentPageReadKey{}, true))
}
func (s *agentPageReadStore) WithinRepository(context.Context, domain.ContentHash, func(context.Context) error) error {
	return errors.New("unexpected repository write")
}
func (s *agentPageReadStore) bound(ctx context.Context) error {
	if ctx.Value(agentPageReadKey{}) != true {
		return errors.New("read outside snapshot")
	}
	return ctx.Err()
}
func (s *agentPageReadStore) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	if err := s.bound(ctx); err != nil {
		return domain.Snapshot{}, err
	}
	snap, err := s.FSStore.GetSnapshot(ctx, repo, hash)
	if err == nil && s.snapshot != nil {
		snap = s.snapshot(snap)
	}
	return snap, err
}
func (s *agentPageReadStore) ReadVerifiedDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	if err := s.bound(ctx); err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	s.proofs[hash]++
	doc, err := s.FSStore.ReadVerifiedDoc(ctx, repo, hash)
	if err == nil && s.after != nil {
		s.after(hash)
	}
	return doc, err
}
func (s *agentPageReadStore) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	if err := s.bound(ctx); err != nil {
		return domain.DocReadIndex{}, err
	}
	return s.FSStore.DocReadIndex(ctx, repo, hash)
}
func (s *agentPageReadStore) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	if err := s.bound(ctx); err != nil {
		return domain.SessionDoc{}, err
	}
	return s.FSStore.GetDoc(ctx, repo, hash)
}
func (s *agentPageReadStore) GetDocManifest(ctx context.Context, repo, hash domain.ContentHash) (domain.DocChunkManifest, error) {
	if err := s.bound(ctx); err != nil {
		return domain.DocChunkManifest{}, err
	}
	return s.FSStore.GetDocManifest(ctx, repo, hash)
}
func (s *agentPageReadStore) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	if err := s.bound(ctx); err != nil {
		return nil, err
	}
	return s.FSStore.GetChunk(ctx, repo, hash)
}

func TestAgentPageIdentitySourcesAndCoverage(t *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), t.TempDir()
	st := store.NewFSStore(dir)
	old, _ := seedRootReadDoc(t, dir, st, repo, historyMessage(domain.RoleUser, "prompt"))
	next, _ := seedRootReadDoc(t, dir, st, repo, historyMessage(domain.RoleUser, "prompt"), historyMessage(domain.RoleAssistant, "answer"))
	empty, _ := seedRootReadDoc(t, dir, st, repo)
	legacy := putHistoryDoc(t, st, repo, historyEnvelope(), old.CIR.Events...)
	for _, tc := range []struct {
		name           string
		doc, cover     domain.SessionDoc
		before, proofs int
		covered        bool
	}{
		{"metadata", old, domain.SessionDoc{}, 0, 1, false},
		{"turn", old, domain.SessionDoc{}, -1, 1, false},
		{"root-root", old, next, 0, 2, true},
		{"legacy-root", legacy, next, 0, 1, true},
		{"root-legacy", old, legacy, 0, 1, true},
		{"same-root", old, old, 0, 1, true},
		{"empty", empty, domain.SessionDoc{}, 0, 1, false},
		{"empty-covered", empty, next, 0, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &agentPageReadStore{FSStore: st, proofs: map[domain.ContentHash]int{}}
			svc := NewService(spy, spy, nil, nil, nil)
			req := domain.AgentHistoryPageRequest{Before: tc.before, Limit: 1, MaxBytes: 4 << 20, DocIdentity: tc.doc.Identity, CoveredBy: tc.cover.Hash, CoveredByIdentity: tc.cover.Identity}
			for _, projection := range []string{"", "omit"} {
				req.IncompleteTail = projection
				before := spy.transactions
				got, err := svc.ReadAgentHistoryPage(ctx, repo, tc.doc.Hash, req)
				if err != nil || got.DocumentRef() != tc.doc.DocumentRef() || got.Covered != tc.covered || spy.transactions-before != 1 {
					t.Fatalf("page ref=%+v covered=%v err=%v transactions=%d", got.DocumentRef(), got.Covered, err, spy.transactions-before)
				}
				version := 1
				if projection != "" {
					version = 2
				}
				if got.Version != version {
					t.Fatal("version changed")
				}
				if tc.before == -1 && len(got.Turns) != 1 {
					t.Fatal("missing complete turn")
				}
				if tc.before == 0 && len(got.Turns) != 0 {
					t.Fatal("metadata returned body")
				}
			}
			total := 0
			for _, n := range spy.proofs {
				total += n
			}
			if total != 2*tc.proofs {
				t.Fatalf("proofs=%d want=%d", total, 2*tc.proofs)
			}
		})
	}
}

func TestAgentPageIdentityRejectsBeforeShortcuts(t *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), t.TempDir()
	st := store.NewFSStore(dir)
	root, _ := seedRootReadDoc(t, dir, st, repo)
	legacy := putHistoryDoc(t, st, repo, historyEnvelope())
	for _, tc := range []struct {
		name    string
		hash    domain.ContentHash
		request domain.AgentHistoryPageRequest
	}{
		{"missing-root", root.Hash, domain.AgentHistoryPageRequest{}},
		{"root-on-legacy", legacy.Hash, domain.AgentHistoryPageRequest{DocIdentity: domain.DocumentIdentityRootV1}},
		{"unknown-request", root.Hash, domain.AgentHistoryPageRequest{DocIdentity: "future"}},
		{"missing-coverage", legacy.Hash, domain.AgentHistoryPageRequest{CoveredBy: root.Hash}},
		{"unknown-coverage", legacy.Hash, domain.AgentHistoryPageRequest{CoveredBy: root.Hash, CoveredByIdentity: "future"}},
		{"coverage-without-hash", legacy.Hash, domain.AgentHistoryPageRequest{CoveredByIdentity: domain.DocumentIdentityRootV1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &agentPageReadStore{FSStore: st, proofs: map[domain.ContentHash]int{}}
			svc := NewService(spy, spy, nil, nil, nil)
			req := tc.request
			req.Before = 0
			req.Limit = 1
			req.MaxBytes = 1
			if _, err := svc.ReadAgentHistoryPage(ctx, repo, tc.hash, req); err == nil {
				t.Fatal("identity shortcut accepted")
			}
			if len(spy.proofs) != 0 {
				t.Fatal("mismatch read root bytes before ref check")
			}
		})
	}
	spy := &agentPageReadStore{FSStore: st, proofs: map[domain.ContentHash]int{}}
	svc := NewService(spy, spy, nil, nil, nil)
	req := domain.AgentHistoryPageRequest{Before: 0, Limit: 1, MaxBytes: 1, DocIdentity: domain.DocumentIdentityRootV1}
	for _, mutation := range []func(domain.Snapshot) domain.Snapshot{
		func(s domain.Snapshot) domain.Snapshot { s.DocHash = legacy.Hash; return s },
		func(s domain.Snapshot) domain.Snapshot { s.ID = legacy.Hash; return s },
		func(s domain.Snapshot) domain.Snapshot { s.DocIdentity = "future"; return s },
	} {
		spy.snapshot = mutation
		if _, err := svc.ReadAgentHistoryPage(ctx, repo, root.Hash, req); err == nil {
			t.Fatal("snapshot ref mismatch")
		}
	}
	if len(spy.proofs) != 0 {
		t.Fatal("snapshot mismatch loaded bodies")
	}
	spy.snapshot = nil
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := svc.ReadAgentHistoryPage(cancelled, repo, root.Hash, req); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation", err)
	}
	if _, err := svc.ReadAgentHistoryPage(ctx, h('2'), root.Hash, req); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("foreign root", err)
	}
}

func TestAgentPageIdentityCurrentBytesAndCoverage(t *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), t.TempDir()
	st := store.NewFSStore(dir)
	old, _ := seedRootReadDoc(t, dir, st, repo, historyMessage(domain.RoleUser, "prompt"))
	next, manifest := seedRootReadDoc(t, dir, st, repo, historyMessage(domain.RoleUser, "prompt"), historyMessage(domain.RoleAssistant, strings.Repeat("tail", 180000)))
	spy := &agentPageReadStore{FSStore: st, proofs: map[domain.ContentHash]int{}}
	svc := NewService(spy, spy, nil, nil, nil)
	req := domain.AgentHistoryPageRequest{Before: 0, Limit: 1, MaxBytes: 1, DocIdentity: old.Identity, CoveredBy: next.Hash, CoveredByIdentity: next.Identity}
	for i := 0; i < 2; i++ {
		if page, err := svc.ReadAgentHistoryPage(ctx, repo, old.Hash, req); err != nil || !page.Covered {
			t.Fatal("warm coverage", err)
		}
	}
	path := rootReadObjectPath(dir, repo, "chunks", manifest.Chunks[len(manifest.Chunks)-1].Hash)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := bytes.Clone(original)
	bad[len(bad)-1] ^= 1
	if err = os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetDoc(ctx, repo, next.Hash); err == nil {
		t.Fatal("full root read hid corrupt tail")
	}
	if _, err := svc.Diff(ctx, inbound.DiffInput{RepoID: repo, Left: old.Hash, Right: next.Hash}); err == nil {
		t.Fatal("diff hid corrupt right source")
	}
	if _, err = svc.ReadAgentHistoryPage(ctx, repo, old.Hash, req); err == nil {
		t.Fatal("warm covered shortcut hid corrupt tail")
	}
	req.CoveredBy = ""
	req.CoveredByIdentity = ""
	if _, err = svc.ReadAgentHistoryPage(ctx, repo, next.Hash, req); err == nil {
		t.Fatal("metadata shortcut hid corrupt tail")
	}
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	// A proof retains bytes owned at its read instant, even if storage changes
	// before the requested range is materialized. The next call must fail.
	spy.after = func(hash domain.ContentHash) {
		if hash == next.Hash {
			if err := os.WriteFile(path, bad, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	req.Before = -1
	req.MaxBytes = 4 << 20
	page, err := svc.ReadAgentHistoryPage(ctx, repo, next.Hash, req)
	if err != nil || len(page.Turns) != 1 {
		t.Fatal("owned range reread storage", err)
	}
	spy.after = nil
	if _, err := svc.ReadAgentHistoryPage(ctx, repo, next.Hash, req); err == nil {
		t.Fatal("next call reused stale proof")
	}
}

func TestAgentPageRootFullGetDocAndDiff(t *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), t.TempDir()
	st := store.NewFSStore(dir)
	old, _ := seedRootReadDoc(t, dir, st, repo, historyMessage(domain.RoleUser, "prompt"))
	next, _ := seedRootReadDoc(t, dir, st, repo, historyMessage(domain.RoleUser, "prompt"), historyMessage(domain.RoleAssistant, "answer"))
	legacy := putHistoryDoc(t, st, repo, historyEnvelope(), old.CIR.Events...)
	spy := &agentPageReadStore{FSStore: st, proofs: map[domain.ContentHash]int{}}
	svc := NewService(spy, spy, nil, nil, nil)
	got, err := svc.GetDoc(ctx, repo, old.Hash)
	if err != nil || got.DocumentRef() != old.DocumentRef() {
		t.Fatal("root full doc", err)
	}
	a, _ := domain.CanonicalBytes(got.CIR)
	b, _ := domain.CanonicalBytes(old.CIR)
	if !bytes.Equal(a, b) {
		t.Fatal("materialization changed canonical bytes")
	}
	for _, left := range []domain.ContentHash{old.Hash, legacy.Hash, next.Hash} {
		before := spy.transactions
		out, err := svc.Diff(ctx, inbound.DiffInput{RepoID: repo, Left: left, Right: next.Hash})
		if err != nil || spy.transactions-before != 1 {
			t.Fatal("coherent diff", err)
		}
		if left == next.Hash && len(out.Changes) != 0 {
			t.Fatal("self diff")
		}
	}
}
