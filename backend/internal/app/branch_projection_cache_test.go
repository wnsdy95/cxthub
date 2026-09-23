package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestRepositoryCommandsRequireExplicitAuthority(t *testing.T) {
	svc, _ := newFsckSvc(t)
	_, err := svc.Commit(context.Background(), inbound.CommitInput{RepoID: hh("unbound")})
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("missing actor accepted: %v", err)
	}
}

func TestBranchProjectionCacheOwnsInputsAndBoundsMemory(t *testing.T) {
	var c branchProjectionCache
	key := branchProjectionKey{repo: hh("repo"), graph: 1, evidence: 2}
	value := map[string]domain.BranchContext{"main": {Roots: []domain.ContentHash{hh("root")}, SnapshotIDs: []domain.ContentHash{hh("tip")}, Merges: []domain.BranchContextMerge{{State: "included"}}}}
	expected := cloneBranchContexts(value)
	c.put(key, value)
	value["main"].SnapshotIDs[0] = hh("changed")
	got, ok := c.get(key)
	if !ok || !reflect.DeepEqual(got, expected) {
		t.Fatal("cache aliases input")
	}
	got["main"].Merges[0].State = "changed"
	again, _ := c.get(key)
	if !reflect.DeepEqual(again, expected) {
		t.Fatal("cache aliases output")
	}
	for _, other := range []branchProjectionKey{{repo: hh("other"), graph: 1, evidence: 2}, {repo: key.repo, graph: 2, evidence: 2}, {repo: key.repo, graph: 1, evidence: 3}, {repo: key.repo, graph: 1, evidence: 2, origin: "new-origin"}} {
		if _, ok := c.get(other); ok {
			t.Fatal("cache crossed revision/repository boundary")
		}
	}
	c.put(branchProjectionKey{repo: key.repo, graph: 2}, expected)
	if _, ok := c.get(key); ok {
		t.Fatal("old generation retained")
	}
	for i := 0; i < 20; i++ {
		c.put(branchProjectionKey{repo: hh(fmt.Sprint(i))}, expected)
	}
	if len(c.entries) != 8 || c.weight > maxBranchProjectionWeight {
		t.Fatal("cache unbounded")
	}
	c.put(key, map[string]domain.BranchContext{"large": {SnapshotIDs: make([]domain.ContentHash, maxBranchProjectionWeight+1)}})
	if _, ok := c.get(key); ok {
		t.Fatal("oversized projection cached")
	}
}

func TestBranchProjectionCachePreservesPendingAndEvidenceBoundaries(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo, root, pending := hh("cache repo"), hh("root"), hh("pending")
	v := domain.RepositoryView{DefaultBranch: "main", Revision: domain.RepositoryRevision{Graph: 1}, Refs: []domain.Ref{{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: root}}, Snapshots: []domain.Snapshot{{RepoID: repo, ID: root, DocHash: root}}}
	// Legacy fixture has no bound Git origin; branch projection falls back safely.
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	first, err := svc.projectGraphState(ctx, v, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(svc.branchCache.entries) != 1 {
		t.Fatal("committed generation not cached")
	}
	v.Revision.Pending++
	v.Snapshots = append(v.Snapshots, domain.Snapshot{RepoID: repo, ID: pending, DocHash: pending, Parents: []domain.ContentHash{root}, Message: "hook: live"})
	v.Pending = []domain.Pending{{RepoID: repo, SessionID: "session", Target: pending}}
	next, err := svc.projectGraphState(ctx, v, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.BranchContexts, next.BranchContexts) || len(next.UncommittedIDs) != 1 || next.UncommittedIDs[0] != pending {
		t.Fatalf("pending response lost current capture: %+v", next)
	}
	v.Revision.Evidence++
	if _, err := svc.projectGraphState(ctx, v, ""); err != nil {
		t.Fatal(err)
	}
	if svc.branchCache.entries[0].key.evidence != v.Revision.Evidence {
		t.Fatal("evidence did not invalidate projection")
	}
	v.Revision.Graph++
	if _, err := svc.projectGraphState(context.WithValue(ctx, afterCommitKey{}, &afterCommitActions{}), v, ""); err != nil {
		t.Fatal(err)
	}
	if svc.branchCache.entries[0].key.graph == v.Revision.Graph {
		t.Fatal("uncommitted projection entered cache")
	}
}
