package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func makeCommitDoc(t *testing.T, text string) domain.SessionDoc {
	t.Helper()
	cir := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull, GitBranch: "main"},
		Events:   []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}},
	}
	cb, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	return domain.SessionDoc{Hash: domain.HashContent(cb), CIR: cir}
}

func TestCommitRejectsDocHashMismatchBeforeStore(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("commit-integrity-repo")
	doc := makeCommitDoc(t, "actual body")
	doc.Hash = hh("claimed-doc-hash")
	snap := domain.Snapshot{ID: doc.Hash, RepoID: repo, Branch: "main", DocHash: doc.Hash, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull}

	_, err := svc.Commit(ctx, inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{doc}, Snapshots: []domain.Snapshot{snap}})
	if !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity, got %v", err)
	}
	if _, gerr := st.GetDoc(ctx, repo, doc.Hash); !errors.Is(gerr, domain.ErrNotFound) {
		t.Fatalf("mismatched doc was stored: %v", gerr)
	}
	if _, gerr := st.GetRepo(ctx, repo); !errors.Is(gerr, domain.ErrNotFound) {
		t.Fatalf("failed commit created repo record: %v", gerr)
	}
}

func TestCommitDoesNotPartiallyStoreDocsWhenSnapshotValidationFails(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("partial-store-repo")
	doc := makeCommitDoc(t, "valid but unrelated")
	missing := hh("missing-doc")
	snap := domain.Snapshot{ID: missing, RepoID: repo, Branch: "main", DocHash: missing, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull}

	_, err := svc.Commit(ctx, inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{doc}, Snapshots: []domain.Snapshot{snap}})
	if !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity, got %v", err)
	}
	if _, gerr := st.GetDoc(ctx, repo, doc.Hash); !errors.Is(gerr, domain.ErrNotFound) {
		t.Fatalf("valid doc was partially stored before snapshot failure: %v", gerr)
	}
}

func TestPutSettingsObjectRejectsHashMismatch(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("settings-object-repo")
	bundle := domain.SettingsBundle{
		Kind:  "claude",
		Files: []domain.SettingsFile{{Path: "CLAUDE.md", ContentB64: "IyB0ZXN0Cg=="}},
	}
	correct, err := settingsObjectHash(bundle)
	if err != nil {
		t.Fatal(err)
	}
	wrong := hh("wrong-settings-hash")

	if err := svc.PutSettingsObject(ctx, repo, wrong, bundle); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity, got %v", err)
	}
	if _, gerr := st.GetSettingsObject(ctx, repo, wrong); !errors.Is(gerr, domain.ErrNotFound) {
		t.Fatalf("mismatched settings object was stored: %v", gerr)
	}
	if err := svc.PutSettingsObject(ctx, repo, correct, bundle); err != nil {
		t.Fatalf("valid settings object rejected: %v", err)
	}
	if got, err := svc.GetSettingsObjectByHash(ctx, repo, correct); err != nil || got.Kind != bundle.Kind || len(got.Files) != 1 {
		t.Fatalf("valid settings object roundtrip failed: got=%+v err=%v", got, err)
	}
}

func TestCommitRejectsMissingSettingsObjectBeforeStore(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("missing-settings-repo")
	bindCommitTestRepo(t, st, repo)
	doc := makeCommitDoc(t, "snapshot with absent settings")
	snap := domain.Snapshot{
		ID: doc.Hash, RepoID: repo, Branch: "main", DocHash: doc.Hash,
		ClaudeSettings: hh("absent-settings"), Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull,
	}

	_, err := svc.Commit(ctx, inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{doc}, Snapshots: []domain.Snapshot{snap}})
	if !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("missing settings error = %v", err)
	}
	if _, err := st.GetDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("doc stored before settings preflight failed: %v", err)
	}
}

func bindCommitTestRepo(t *testing.T, st interface {
	PutRepo(context.Context, domain.Repo) (domain.Repo, error)
}, repo domain.ContentHash) {
	t.Helper()
	if _, err := st.PutRepo(context.Background(), domain.Repo{
		ID: repo, DefaultBranch: "main", WorkspaceID: domain.NewID("ws_"),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCommitRejectsMissingParentBeforeStore(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("missing-parent-repo")
	bindCommitTestRepo(t, st, repo)
	doc := makeCommitDoc(t, "child")
	snap := domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, Parents: []domain.ContentHash{hh("absent-parent")}}

	_, err := svc.Commit(ctx, inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{doc}, Snapshots: []domain.Snapshot{snap}})
	if !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("missing parent error = %v", err)
	}
	if _, err := st.GetDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("doc stored before graph preflight failed: %v", err)
	}
}

func TestCommitRejectsCyclicBatchBeforeStore(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("cycle-repo")
	bindCommitTestRepo(t, st, repo)
	docA := makeCommitDoc(t, "cycle-a")
	docB := makeCommitDoc(t, "cycle-b")
	snapA := domain.Snapshot{ID: docA.Hash, RepoID: repo, DocHash: docA.Hash, Parents: []domain.ContentHash{docB.Hash}}
	snapB := domain.Snapshot{ID: docB.Hash, RepoID: repo, DocHash: docB.Hash, Parents: []domain.ContentHash{docA.Hash}}

	_, err := svc.Commit(ctx, inbound.CommitInput{
		RepoID: repo, Docs: []domain.SessionDoc{docA, docB}, Snapshots: []domain.Snapshot{snapA, snapB},
	})
	if !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("cycle error = %v", err)
	}
	if _, err := st.GetDoc(ctx, repo, docA.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("doc stored before cycle preflight failed: %v", err)
	}
}

func TestCommitRejectsDuplicateSnapshotBeforeStore(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("duplicate-snapshot-repo")
	bindCommitTestRepo(t, st, repo)
	doc := makeCommitDoc(t, "duplicate snapshot")
	first := domain.Snapshot{
		ID: doc.Hash, RepoID: repo, DocHash: doc.Hash,
		Parents: []domain.ContentHash{hh("missing-parent")},
	}
	last := domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash}

	_, err := svc.Commit(ctx, inbound.CommitInput{
		RepoID: repo, Docs: []domain.SessionDoc{doc}, Snapshots: []domain.Snapshot{first, last},
	})
	if !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("duplicate snapshot error = %v", err)
	}
	if _, err := st.GetDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("doc stored before duplicate preflight failed: %v", err)
	}
}

func TestCommitRejectsClientOwnedGraftMetadata(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("client-graft-repo")
	bindCommitTestRepo(t, st, repo)
	parentDoc := makeCommitDoc(t, "parent")
	if _, err := st.PutDoc(ctx, repo, parentDoc); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: parentDoc.Hash, RepoID: repo, DocHash: parentDoc.Hash}); err != nil {
		t.Fatal(err)
	}
	doc := makeCommitDoc(t, "malicious graft")
	snap := domain.Snapshot{
		ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, Grafted: true,
		GraftParents: []domain.ContentHash{parentDoc.Hash},
	}

	_, err := svc.Commit(ctx, inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{doc}, Snapshots: []domain.Snapshot{snap}})
	if !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("client graft error = %v", err)
	}
}

func TestCommitRejectsClientOwnedGraftSequence(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("client-graft-seq-repo")
	bindCommitTestRepo(t, st, repo)
	doc := makeCommitDoc(t, "malicious graft sequence")
	snap := domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, GraftSeq: 99}

	_, err := svc.Commit(ctx, inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{doc}, Snapshots: []domain.Snapshot{snap}})
	if !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("client graft sequence error = %v", err)
	}
}
