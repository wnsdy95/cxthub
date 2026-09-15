package store

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"strings"
	"testing"
	"time"
)

type protocolStore interface {
	PutRepo(context.Context, domain.Repo) (domain.Repo, error)
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
	GetManifest(context.Context, domain.ContentHash) (domain.Manifest, error)
	PutSnapshot(context.Context, domain.Snapshot) error
	PutDoc(context.Context, domain.ContentHash, domain.SessionDoc) (bool, error)
	GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
	CompareAndSwapRef(context.Context, domain.ContentHash, domain.Ref, domain.ContentHash) error
	GetRef(context.Context, domain.ContentHash, domain.RefKind, string) (domain.Ref, error)
	ApplyHistoryEvent(context.Context, domain.HistoryEvent) error
	ListHistoryEvents(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error)
	EnableContextProtocol(context.Context, domain.ContentHash) error
	ApplyBranchLifecycleRef(context.Context, domain.ContentHash, domain.Ref) error
	AppendRef(context.Context, domain.ContentHash, domain.Ref, domain.ContentHash) error
}

func TestFSContextProtocol(t *testing.T) {
	st := NewFSStore(t.TempDir())
	checkContextProtocol(t, st)
	if _, err := OpenFSStore(st.dataDir); err != nil {
		t.Fatal(err)
	}
}

// The same scenarios run against real PostgreSQL in TestPGSmoke.
func checkContextProtocol(t *testing.T, st protocolStore) {
	t.Helper()
	ctx := context.Background()
	repo := domain.HashContent([]byte("context-protocol-contract"))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	makeDoc := func(text string) domain.ContentHash {
		doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull}}}
		doc.CIR.Events = []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}}
		raw, err := domain.CanonicalBytes(doc.CIR)
		if err != nil {
			t.Fatal(err)
		}
		doc.Hash = domain.HashContent(raw)
		if _, err := st.PutDoc(ctx, repo, doc); err != nil {
			t.Fatal(err)
		}
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull}); err != nil {
			t.Fatal(err)
		}
		return doc.Hash
	}
	a, b := makeDoc("protocol-a"), makeDoc("protocol-b")
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: a}
	if err := st.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableContextProtocol(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableContextProtocol(ctx, repo); err != nil {
		t.Fatal("upgrade retry", err)
	}
	r, err := st.GetRepo(ctx, repo)
	if err != nil || r.ContextProtocol != 1 {
		t.Fatalf("protocol: %+v %v", r, err)
	}
	man, err := st.GetManifest(ctx, repo)
	if err != nil || man.ContextProtocol != 1 {
		t.Fatalf("manifest protocol %+v %v", man, err)
	}
	cur, err := st.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || cur.BranchID != domain.LegacyContextBranchID(string(repo), "main") {
		t.Fatalf("legacy migration: %+v %v", cur, err)
	}
	if err := st.CompareAndSwapRef(ctx, repo, ref, a); err == nil {
		t.Fatal("old name-only writer accepted")
	}
	ref.BranchID = cur.BranchID
	if err := st.CompareAndSwapRef(ctx, repo, ref, a); err != nil {
		t.Fatal("new writer rejected", err)
	}
	now := time.Now().UTC()
	birth := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: string(repo), BranchID: "first", Branch: "work", Kind: "birth", Source: a, Target: a, CreatedAt: now}
	if err := st.ApplyHistoryEvent(ctx, birth); err != nil {
		t.Fatal("birth", err)
	}
	current, err := st.GetRef(ctx, repo, domain.RefBranch, "work")
	if err != nil || current.BranchID != "first" || current.Target != a {
		t.Fatalf("birth pointer %+v %v", current, err)
	}
	competing := birth
	competing.ID = strings.Repeat("b", 32)
	competing.BranchID = "competing"
	if err := st.ApplyHistoryEvent(ctx, competing); err == nil {
		t.Fatal("same-name birth accepted")
	}
	rename := domain.HistoryEvent{ID: strings.Repeat("c", 32), RepoID: string(repo), BranchID: "first", Branch: "renamed", PreviousBranch: "work", Kind: "rename", BindingParent: birth.ID, Source: a, Target: a, CreatedAt: now.Add(time.Second)}
	if err := st.ApplyHistoryEvent(ctx, rename); err != nil {
		t.Fatal("rename", err)
	}
	if _, err := st.GetRef(ctx, repo, domain.RefBranch, "work"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("old name remains", err)
	}
	reused := birth
	reused.ID = strings.Repeat("d", 32)
	reused.BranchID = "second"
	reused.BindingParent = rename.ID
	reused.CreatedAt = now.Add(2 * time.Second)
	if err := st.ApplyHistoryEvent(ctx, reused); err != nil {
		t.Fatal("reuse", err)
	}
	// Identical names AND hashes do not authorize the old identity.
	if err := st.CompareAndSwapRef(ctx, repo, current, a); err == nil {
		t.Fatal("stale identity accepted at same hash")
	}
	current.Target = b
	if err := st.AppendRef(ctx, repo, current, a); err == nil {
		t.Fatal("stale append accepted")
	}
	snap, err := st.GetSnapshot(ctx, repo, b)
	if err != nil || len(snap.GraftParents) != 0 {
		t.Fatal("rejected append changed overlay", err)
	}
	oldTag, _ := domain.NewBranchLifecycleRef(repo, "work", a, 99, domain.BranchArchived)
	if err := st.ApplyBranchLifecycleRef(ctx, repo, oldTag); err == nil {
		t.Fatal("old archive bypassed protection")
	}
	archived := domain.HistoryEvent{ID: strings.Repeat("e", 32), RepoID: string(repo), BranchID: "first", Branch: "renamed", Kind: "archive", BindingParent: rename.ID, Source: a, Target: a, CreatedAt: now.Add(3 * time.Second)}
	if err := st.ApplyHistoryEvent(ctx, archived); err != nil {
		t.Fatal("archive", err)
	}
	if err := st.ApplyHistoryEvent(ctx, birth); err != nil {
		t.Fatal("acknowledged retry", err)
	}
	if _, err := st.GetRef(ctx, repo, domain.RefBranch, "renamed"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("archive/retry revived branch", err)
	}
	current, err = st.GetRef(ctx, repo, domain.RefBranch, "work")
	if err != nil || current.BranchID != "second" {
		t.Fatalf("reuse lost: %+v %v", current, err)
	}
	detached := domain.HistoryEvent{ID: strings.Repeat("2", 32), RepoID: string(repo), BranchID: "detached", Kind: "position", Source: a, Target: b, GitAfter: strings.Repeat("a", 40), CreatedAt: now.Add(4 * time.Second)}
	if err := st.ApplyHistoryEvent(ctx, detached); err != nil {
		t.Fatal("detached position retention", err)
	}
	orphan := domain.HistoryEvent{ID: strings.Repeat("f", 32), RepoID: string(repo), BranchID: "orphan", Branch: "empty", Kind: "orphan", CreatedAt: now.Add(4 * time.Second)}
	if err := st.ApplyHistoryEvent(ctx, orphan); err != nil {
		t.Fatal("empty orphan", err)
	}
	emptyRename := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "orphan", Branch: "empty-renamed", PreviousBranch: "empty", Kind: "rename", BindingParent: orphan.ID, CreatedAt: now.Add(5 * time.Second)}
	if err := st.ApplyHistoryEvent(ctx, emptyRename); err != nil {
		t.Fatal("empty orphan rename", err)
	}
	firstCapture := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "empty-renamed", BranchID: "orphan", Target: b}
	if err := st.CompareAndSwapRef(ctx, repo, firstCapture, ""); err != nil {
		t.Fatal("orphan first capture", err)
	}
}
