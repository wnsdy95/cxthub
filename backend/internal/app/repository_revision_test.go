package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"testing"
)

func TestPendingRevisionDoesNotInvalidateGraph(t *testing.T) {
	ctx := context.Background()
	svc, st := newFsckSvc(t)
	repo := hh(t.Name())
	snap := putPendingGCCapture(t, st, repo, pendingGCCIR(domain.ProviderCodex, "hello"))
	before, err := svc.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.PutPending(ctx, repo, snap.SessionID, domain.Pending{Provider: snap.Provider, Target: snap.ID}); err != nil {
		t.Fatal(err)
	}
	after, err := svc.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if after.Graph != before.Graph || after.Pending != before.Pending+1 {
		t.Fatalf("wrong scope: %+v -> %+v", before, after)
	}
	p, err := svc.GetPendingView(ctx, repo)
	if err != nil || len(p.Pending) != 1 || len(p.Snapshots) != 1 || p.Revision != after {
		t.Fatalf("%+v %v", p, err)
	}
	if err := svc.DismissPending(ctx, repo, snap.SessionID); err != nil {
		t.Fatal(err)
	}
	v, _ := svc.GetPendingView(ctx, repo)
	if !v.Pending[0].Dismissed || v.Revision.Pending <= p.Revision.Pending {
		t.Fatal("dismiss was not observed")
	}
	if err := svc.PutPending(ctx, repo, "bad", domain.Pending{Target: hh("missing")}); err == nil {
		t.Fatal("missing snapshot accepted")
	}
	final, _ := svc.RepositoryRevision(ctx, repo)
	if final != v.Revision {
		t.Fatal("failed write emitted revision")
	}
}

func TestObjectStagingDoesNotPublishGraphRevision(t *testing.T) {
	ctx := context.Background()
	svc, st := newFsckSvc(t)
	repo := hh(t.Name())
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, WorkspaceID: "synthetic-workspace"}); err != nil {
		t.Fatal(err)
	}
	cir := pendingGCCIR(domain.ProviderCodex, "only staged bytes")
	cb, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(cb)
	doc := domain.SessionDoc{Hash: hash, CIR: cir}
	snap := domain.Snapshot{ID: hash, DocHash: hash, RepoID: repo, Provider: domain.ProviderCodex, SessionID: cir.Envelope.SessionOriginID, Fidelity: domain.FidelityFull}
	if _, err := svc.Commit(ctx, inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{doc}, Snapshots: []domain.Snapshot{snap}}); err != nil {
		t.Fatal(err)
	}
	rev, err := svc.RepositoryRevision(ctx, repo)
	if err != nil || rev.Graph != 0 || rev.Pending != 0 {
		t.Fatalf("staged objects published graph: %+v %v", rev, err)
	}
	if err := svc.PutPending(ctx, repo, snap.SessionID, domain.Pending{Provider: snap.Provider, Target: hash}); err != nil {
		t.Fatal(err)
	}
	rev, _ = svc.RepositoryRevision(ctx, repo)
	if rev.Graph != 0 || rev.Pending != 1 {
		t.Fatalf("wrong publication scope: %+v", rev)
	}
}
