package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// This recording transaction adapter tests application policy with real owned
// documents. It does NOT claim FS production transaction/rollback support.
type initializationTestTxKey struct{}
type initializationReadKey struct{}
type initializationTestStore struct {
	*store.FSStore
	mu                    sync.Mutex
	receipt               *domain.RepositoryInitializationReceipt
	locks, commits, reads int
	snapshots             map[domain.ContentHash]domain.Snapshot
	memories              map[domain.ContentHash]domain.MemoryDigest
}

func (s *initializationTestStore) WithinRepository(ctx context.Context, _ domain.ContentHash, fn func(context.Context) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(context.WithValue(ctx, initializationTestTxKey{}, s))
}
func (s *initializationTestStore) WithinReadSnapshot(ctx context.Context, fn func(context.Context) error) error {
	return fn(context.WithValue(context.WithValue(ctx, initializationTestTxKey{}, s), initializationReadKey{}, true))
}
func (s *initializationTestStore) InReadOnlyTransaction(ctx context.Context) bool {
	return ctx.Value(initializationReadKey{}) == true
}
func (s *initializationTestStore) CaptureRepositoryInitialization(context.Context, domain.ContentHash, domain.RepositoryInitializationAnchor, outbound.RepositoryInitializationEvidence) (outbound.RepositoryInitializationProof, error) {
	return nil, nil
}
func (s *initializationTestStore) LockRepositoryAccess(ctx context.Context, _, _ string) error {
	if ctx.Value(initializationTestTxKey{}) != s {
		return domain.ErrConflict
	}
	s.locks++
	return nil
}
func (s *initializationTestStore) GetRepositoryInitialization(ctx context.Context, id domain.ContentHash) (domain.RepositoryInitializationReceipt, error) {
	if ctx.Value(initializationTestTxKey{}) != s {
		return domain.RepositoryInitializationReceipt{}, domain.ErrConflict
	}
	if s.receipt == nil {
		return domain.RepositoryInitializationReceipt{}, domain.ErrNotFound
	}
	r := *s.receipt
	r.Anchor = nil
	return r, nil
}
func (s *initializationTestStore) GetRepositoryInitializationAnchor(ctx context.Context, id domain.ContentHash, branch string) (domain.RepositoryInitializationAnchor, error) {
	if s.receipt == nil || s.receipt.Anchor == nil || s.receipt.Anchor.Ref.Name != branch {
		return domain.RepositoryInitializationAnchor{}, domain.ErrNotFound
	}
	return *s.receipt.Anchor, nil
}
func (s *initializationTestStore) BeginRepositoryInitialization(ctx context.Context, r domain.Repo) (domain.RepositoryInitializationReceipt, error) {
	r.ContextProtocol = 1
	if s.receipt != nil {
		if !reflect.DeepEqual(s.receipt.Repo, r) {
			return domain.RepositoryInitializationReceipt{}, domain.ErrRepositoryInitializationConflict
		}
		return *s.receipt, nil
	}
	if _, err := s.GetRepo(ctx, r.ID); !errors.Is(err, domain.ErrNotFound) {
		return domain.RepositoryInitializationReceipt{}, domain.ErrRepositoryInitializationConflict
	}
	got, err := s.PutRepo(ctx, r)
	if err != nil {
		return domain.RepositoryInitializationReceipt{}, err
	}
	if err := s.FSStore.EnableContextProtocol(ctx, r.ID); err != nil {
		return domain.RepositoryInitializationReceipt{}, err
	}
	got.ContextProtocol = 1
	s.receipt = &domain.RepositoryInitializationReceipt{Version: 1, CreationID: "init_" + strings.Repeat("a", 32), Repo: got}
	return *s.receipt, nil
}
func (s *initializationTestStore) FinalizeRepositoryInitialization(_ context.Context, _ domain.ContentHash, in domain.RepositoryInitializationFinalize, _ outbound.RepositoryInitializationProof) (domain.RepositoryInitializationReceipt, error) {
	s.commits++
	s.receipt.Anchor = &in.Anchor
	return *s.receipt, nil
}
func (s *initializationTestStore) GetSnapshot(ctx context.Context, repo, id domain.ContentHash) (domain.Snapshot, error) {
	if ctx.Value(initializationTestTxKey{}) != s {
		return domain.Snapshot{}, domain.ErrConflict
	}
	s.reads++
	if snap, ok := s.snapshots[id]; ok {
		return snap, nil
	}
	return s.FSStore.GetSnapshot(ctx, repo, id)
}
func (s *initializationTestStore) GetMemory(ctx context.Context, repo, id domain.ContentHash) (domain.MemoryDigest, error) {
	if ctx.Value(initializationTestTxKey{}) != s {
		return domain.MemoryDigest{}, domain.ErrConflict
	}
	if m, ok := s.memories[id]; ok {
		return m, nil
	}
	return s.FSStore.GetMemory(ctx, repo, id)
}
func initializationFixture(t *testing.T) (*Service, *initializationTestStore, context.Context, domain.ContentHash, domain.RepositoryInitializationRequest) {
	t.Helper()
	st := &initializationTestStore{FSStore: store.NewFSStore(t.TempDir()), snapshots: map[domain.ContentHash]domain.Snapshot{}, memories: map[domain.ContentHash]domain.MemoryDigest{}}
	r := domain.Repository{ID: "ws_" + strings.Repeat("b", 32), Name: "code", Slug: "code", OwnerID: "owner", OwnerUsername: "alice"}
	if err := st.CreateRepository(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	in := domain.RepositoryInitializationRequest{RemoteURL: "https://host.test/alice/code", GitRemoteURL: "https://git.test/code", DefaultBranch: "main"}
	id := domain.HashContent([]byte(normalizeGitURL(in.RemoteURL)))
	return NewService(st, st, nil, nil, st), st, inbound.WithRepositoryActor(context.Background(), "owner"), id, in
}
func initializationSnapshot(t *testing.T, st *initializationTestStore, repo domain.ContentHash, label string, parents ...domain.ContentHash) domain.Snapshot {
	t.Helper()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: label}}}}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := st.PutDoc(context.Background(), repo, doc); err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull, Parents: parents}
	if err := st.PutSnapshot(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	return snap
}
func initializationFinalize(receipt domain.RepositoryInitializationReceipt, snaps ...domain.Snapshot) domain.RepositoryInitializationFinalize {
	target := snaps[len(snaps)-1].ID
	a := domain.RepositoryInitializationAnchor{Ref: domain.Ref{RepoID: receipt.Repo.ID, Kind: domain.RefBranch, Name: "main", BranchID: domain.LegacyContextBranchID(string(receipt.Repo.ID), "main"), Target: target}, SnapshotStates: map[domain.ContentHash]domain.ContentHash{}}
	for _, snap := range snaps {
		a.SnapshotStates[snap.ID], _ = domain.SnapshotStateHash(snap)
	}
	return domain.RepositoryInitializationFinalize{CreationID: receipt.CreationID, Anchor: a}
}
func TestRepositoryInitializationApplicationRecoveryAndAuthority(t *testing.T) {
	svc, st, ctx, id, in := initializationFixture(t)
	receipt, err := svc.BeginRepositoryInitialization(ctx, "owner", id, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.BeginRepositoryInitialization(ctx, "owner", id, in)
	if err != nil || !reflect.DeepEqual(receipt, again) {
		t.Fatal("server receipt not recoverable", err)
	}
	if _, err := svc.EnsureRepo(ctx, "owner", receipt.Repo); err != nil {
		t.Fatal(err)
	}
	if st.receipt.CreationID != receipt.CreationID {
		t.Fatal("registration consumed receipt")
	}
	changed := in
	changed.DefaultBranch = "other"
	if _, err := svc.BeginRepositoryInitialization(ctx, "owner", id, changed); !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
		t.Fatal("changed intent", err)
	}
	snap := initializationSnapshot(t, st, id, "real first context")
	final := initializationFinalize(receipt, snap)
	if _, err := svc.FinalizeRepositoryInitialization(ctx, id, final); err != nil {
		t.Fatal(err)
	}
	reads, commits := st.reads, st.commits
	// Accepted replay does not re-read object state or reapply a moved tip.
	if _, err := svc.FinalizeRepositoryInitialization(ctx, id, final); err != nil {
		t.Fatal(err)
	}
	if st.commits != commits || st.reads != reads {
		t.Fatal("completion replay reapplied/revalidated")
	}
	final.Anchor.Ref.Target = domain.HashContent([]byte("changed"))
	final.Anchor.SnapshotStates[final.Anchor.Ref.Target] = domain.HashContent([]byte("state"))
	if _, err := svc.FinalizeRepositoryInitialization(ctx, id, final); !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
		t.Fatal("changed completion accepted", err)
	}
	r, err := st.GetRepository(ctx, receipt.Repo.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	r.Archived = true
	if err := st.CreateRepository(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FinalizeRepositoryInitialization(ctx, id, initializationFinalize(receipt, snap)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("completed replay skipped current authorization", err)
	}
	if st.locks == 0 {
		t.Fatal("no access lock")
	}
}
func TestRepositoryInitializationApplicationDependencyFailures(t *testing.T) {
	for _, name := range []string{"missing_parent", "unrelated_extra", "stale_state", "foreign_snapshot", "missing_doc", "missing_settings", "wrong_settings_kind", "missing_memory", "foreign_memory_owner", "missing_memory_ancestor", "foreign_memory_ancestor", "corrupt_memory_hash", "cycle"} {
		t.Run(name, func(t *testing.T) {
			svc, st, ctx, id, in := initializationFixture(t)
			receipt, err := svc.BeginRepositoryInitialization(ctx, "owner", id, in)
			if err != nil {
				t.Fatal(err)
			}
			a := initializationSnapshot(t, st, id, "ancestor")
			b := initializationSnapshot(t, st, id, "initial tip", a.ID)
			f := initializationFinalize(receipt, a, b)
			switch name {
			case "missing_parent":
				delete(f.Anchor.SnapshotStates, a.ID)
			case "unrelated_extra":
				x := initializationSnapshot(t, st, id, "unrelated")
				f.Anchor.SnapshotStates[x.ID], _ = domain.SnapshotStateHash(x)
			case "stale_state":
				f.Anchor.SnapshotStates[b.ID] = domain.HashContent([]byte("stale"))
			case "foreign_snapshot":
				b.RepoID = domain.HashContent([]byte("foreign"))
				st.snapshots[b.ID] = b
			case "missing_doc":
				b.ID = domain.HashContent([]byte("no owned doc"))
				b.DocHash = b.ID
				st.snapshots[b.ID] = b
				f = initializationFinalize(receipt, a, b)
			case "cycle":
				b.GraftParents = []domain.ContentHash{b.ID}
				st.snapshots[b.ID] = b
				f = initializationFinalize(receipt, a, b)
			case "missing_settings":
				b.ClaudeSettings = domain.HashContent([]byte("missing settings"))
				st.snapshots[b.ID] = b
				f = initializationFinalize(receipt, a, b)
			case "wrong_settings_kind":
				bundle := domain.SettingsBundle{Kind: "codex"}
				hash, _ := domain.SettingsObjectHash(bundle)
				if err := st.PutSettingsObject(ctx, id, hash, bundle); err != nil {
					t.Fatal(err)
				}
				b.ClaudeSettings = hash
				st.snapshots[b.ID] = b
				f = initializationFinalize(receipt, a, b)
			default:
				m := domain.MemoryDigest{SnapshotID: b.ID, Provider: domain.ProviderClaude}
				if name == "foreign_memory_owner" {
					m.SnapshotID = a.ID
				}
				if name == "missing_memory_ancestor" {
					m.PreviousMemoryHash = domain.HashContent([]byte("missing ancestor"))
				}
				if name == "foreign_memory_ancestor" {
					old := domain.MemoryDigest{SnapshotID: a.ID, Provider: domain.ProviderClaude}
					hash, _ := domain.MemoryDigestHash(old)
					st.memories[hash] = old
					m.PreviousMemoryHash = hash
				}
				hash, _ := domain.MemoryDigestHash(m)
				st.memories[hash] = m
				if name == "missing_memory" {
					delete(st.memories, hash)
				}
				if name == "corrupt_memory_hash" {
					m.Summary = "changed"
					st.memories[hash] = m
				}
				b.MemoryHash = hash
				st.snapshots[b.ID] = b
				f = initializationFinalize(receipt, a, b)
			}
			if _, err := svc.FinalizeRepositoryInitialization(ctx, id, f); err == nil {
				t.Fatal("invalid dependency accepted")
			}
			if st.commits != 0 || st.receipt.Anchor != nil {
				t.Fatal("invalid evidence reached mutation")
			}
		})
	}
}
func TestRepositoryInitializationApplicationExistingAndUnsupported(t *testing.T) {
	svc, _, ctx, id, in := initializationFixture(t)
	if _, err := svc.EnsureRepo(ctx, "owner", domain.Repo{ID: id, RemoteURL: in.RemoteURL, DefaultBranch: "main", GitRemoteURL: in.GitRemoteURL}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BeginRepositoryInitialization(ctx, "owner", id, in); !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
		t.Fatal("existing empty accepted", err)
	}
	fs := store.NewFSStore(t.TempDir())
	unsupported := NewService(fs, fs, nil, nil, fs)
	if _, err := unsupported.BeginRepositoryInitialization(ctx, "owner", id, in); !errors.Is(err, domain.ErrRepositoryInitializationUnsupported) {
		t.Fatal(err)
	}
	if _, err := fs.GetRepo(ctx, id); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("unsupported wrote registration", err)
	}
}
func TestRepositoryInitializationApplicationMemoryDepth(t *testing.T) {
	for _, depth := range []int{1024, 1025} {
		t.Run(string(rune('A'+depth-1024)), func(t *testing.T) {
			svc, st, ctx, id, in := initializationFixture(t)
			receipt, err := svc.BeginRepositoryInitialization(ctx, "owner", id, in)
			if err != nil {
				t.Fatal(err)
			}
			snap := initializationSnapshot(t, st, id, "depth")
			var previous domain.ContentHash
			for i := 0; i < depth; i++ {
				m := domain.MemoryDigest{SnapshotID: snap.ID, Provider: domain.ProviderClaude, PreviousMemoryHash: previous}
				hash, _ := domain.MemoryDigestHash(m)
				st.memories[hash] = m
				previous = hash
			}
			snap.MemoryHash = previous
			st.snapshots[snap.ID] = snap
			_, err = svc.FinalizeRepositoryInitialization(ctx, id, initializationFinalize(receipt, snap))
			if (err == nil) != (depth == 1024) {
				t.Fatalf("depth=%d err=%v", depth, err)
			}
		})
	}
}
