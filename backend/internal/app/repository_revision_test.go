package app

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"testing"
)

func TestFullAndPendingViewMembershipOwnership(t *testing.T) {
	ctx := context.Background()
	svc, st := newFsckSvc(t)
	repo, id := hh(t.Name()), hh("shared pending capture")
	if err := st.PutSnapshot(ctx, domain.Snapshot{RepoID: repo, ID: id, DocHash: id, Branch: "main", Branches: []string{"legacy-stale"}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"main", "topic"} {
		if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: name, Target: id}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: "session", Target: id}); err != nil {
		t.Fatal(err)
	}
	full, err := svc.GetRepositoryView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := svc.GetPendingView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	fullGraph, _ := json.Marshal(full.Graph)
	patchGraph, _ := json.Marshal(patch.Graph)
	if string(fullGraph) != string(patchGraph) {
		t.Fatalf("different graph contracts: %s / %s", fullGraph, patchGraph)
	}
	// Assert actual serialization, not a mock that enriches both endpoints.
	decode := func(v any) map[string]any {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		return wire["snapshots"].([]any)[0].(map[string]any)
	}
	if got := fmt.Sprint(decode(full)["branches"]); got != "[main topic]" {
		t.Fatalf("full memberships: %s", got)
	}
	if _, exists := decode(patch)["branches"]; exists {
		t.Fatal("capture patch claimed graph membership")
	}
}

func TestPendingViewDoesNotRetransmitOldAncestorMetadata(t *testing.T) {
	ctx := context.Background()
	svc, st := newFsckSvc(t)
	repo := hh(t.Name())
	var parent domain.ContentHash
	for i := 0; i < 300; i++ {
		id := hh(fmt.Sprintf("historical-%d", i))
		snap := domain.Snapshot{RepoID: repo, ID: id, DocHash: id}
		if parent != "" {
			snap.Parents = []domain.ContentHash{parent}
		}
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		parent = id
	}
	for _, session := range []string{"capture-a", "capture-b"} {
		if err := st.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: session, Target: parent}); err != nil {
			t.Fatal(err)
		}
	}
	v, err := svc.GetPendingView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Pending) != 2 || len(v.Snapshots) != 1 || v.Snapshots[0].ID != parent {
		t.Fatalf("pending=%d snapshots=%d: historical ancestors must not be retransmitted", len(v.Pending), len(v.Snapshots))
	}
}

func TestEmptyRefBatchReconcilesPendingWithoutGraphRevision(t *testing.T) {
	ctx := context.Background()
	svc, st := newFsckSvc(t)
	repo := hh(t.Name())
	snap := putPendingGCCapture(t, st, repo, pendingGCCIR(domain.ProviderCodex, "shared capture"))
	if err := svc.PutPending(ctx, repo, snap.SessionID, domain.Pending{Target: snap.ID, Provider: snap.Provider}); err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: snap.ID}, ""); err != nil {
		t.Fatal(err)
	}
	before, _ := svc.RepositoryRevision(ctx, repo)
	if _, err := svc.UpdateRefs(ctx, inbound.UpdateRefsInput{RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	after, _ := svc.RepositoryRevision(ctx, repo)
	if after.Graph != before.Graph || after.Pending <= before.Pending {
		t.Fatalf("empty batch scope: %+v -> %+v", before, after)
	}
	pendings, err := st.ListPendings(ctx, repo)
	if err != nil || len(pendings) != 0 {
		t.Fatalf("shared pending reconciliation lost: %+v %v", pendings, err)
	}
	assertPendingGCCapture(t, st, snap)
}

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
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: domain.NewID("ws_")}); err != nil {
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

func TestGraphQuerySeesStagingAcrossIndependentReads(t *testing.T) {
	ctx := context.Background()
	svc, st := newFsckSvc(t)
	repo := hh(t.Name())
	first, err := svc.GetRepositoryView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	id := hh("new staged metadata")
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, RepoID: repo, DocHash: id, Message: "staged commit"}); err != nil {
		t.Fatal(err)
	}
	full, err := svc.GetRepositoryView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := svc.GetPendingView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != full.Revision {
		t.Fatal("fixture unexpectedly advanced publication revision")
	}
	a, _ := json.Marshal(full.Graph)
	b, _ := json.Marshal(patch.Graph)
	if string(a) != string(b) || len(patch.Graph.SnapshotIDs) != 1 || patch.Graph.SnapshotIDs[0] != id {
		t.Fatalf("stale classification: %s / %s", a, b)
	}
	full.Graph.SnapshotIDs[0] = "mutated caller copy"
	next, err := svc.GetPendingView(ctx, repo)
	if err != nil || next.Graph.SnapshotIDs[0] != id {
		t.Fatalf("caller mutation escaped: %+v %v", next, err)
	}
}
