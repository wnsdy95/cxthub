package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type graphCacheReadState struct {
	*store.FSStore
	readOnly bool
}

func (state *graphCacheReadState) InReadOnlyTransaction(context.Context) bool {
	return state.readOnly
}

func TestGraphProjectionCacheReuseOwnsResultsAndInputs(t *testing.T) {
	svc, storage := newFsckSvc(t)
	state := &graphCacheReadState{FSStore: storage, readOnly: true}
	svc.meta = state
	ctx := systemTestContext()
	repo := hh(t.Name())
	view := domain.RepositoryView{Revision: domain.RepositoryRevision{Graph: 1}, DefaultBranch: "main", Snapshots: []domain.Snapshot{{ID: hh("one"), RepoID: repo, DocHash: hh("doc")}}}
	first, err := svc.cachedGraphState(ctx, repo, view, "")
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := json.Marshal(first)
	entry := svc.graphCache.entries[0]
	first.SnapshotIDs[0] = hh("caller mutation")
	first.ScopeLabels["caller"] = "mutation"
	second, err := svc.cachedGraphState(ctx, repo, view, "")
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := json.Marshal(second)
	if !bytes.Equal(expected, actual) || &svc.graphCache.entries[0].value[0] != &entry.value[0] {
		t.Fatal("same projection was rebuilt or caller mutated cached data")
	}
	view.Snapshots = append(view.Snapshots, domain.Snapshot{ID: hh("upload without public revision"), RepoID: repo, DocHash: hh("doc2")})
	changed, err := svc.cachedGraphState(ctx, repo, view, "")
	if err != nil || len(changed.SnapshotIDs) != 2 || svc.graphCache.entries[0].key.inputs == entry.key.inputs {
		t.Fatalf("same-revision upload reused stale graph: %v %+v", err, changed.SnapshotIDs)
	}
	for _, revision := range []domain.RepositoryRevision{{Graph: 1, Pending: 1}, {Graph: 1, Pending: 1, Evidence: 1}, {Graph: 2, Pending: 1, Evidence: 1}} {
		view.Revision = revision
		graph, err := svc.cachedGraphState(ctx, repo, view, "")
		if err != nil || graph.Revision != revision || len(svc.graphCache.entries) != 1 || svc.graphCache.entries[0].key.revision != revision {
			t.Fatalf("generation invalidation: %v %+v", err, graph.Revision)
		}
	}
	prior := svc.graphCache.entries[0].key
	view.DefaultBranch = "changed-default"
	if _, err := svc.cachedGraphState(ctx, repo, view, ""); err != nil || svc.graphCache.entries[0].key.inputs == prior.inputs {
		t.Fatalf("default branch not in projection identity: %v", err)
	}
	origin := "https://github.com/cache/changed.git"
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.cachedGraphState(ctx, repo, view, ""); err != nil || svc.graphCache.entries[0].key.origin != origin {
		t.Fatalf("origin not in projection identity: %v", err)
	}
	if _, err := svc.cachedGraphState(ctx, repo, view, "unknown-position"); !errors.Is(err, domain.ErrNotFound) || len(svc.graphCache.building) != 0 || len(svc.graphCache.entries) != 1 {
		t.Fatalf("failed position cached or flight leaked: %v", err)
	}
	other := hh("other repository")
	if _, err := svc.cachedGraphState(ctx, other, view, ""); err != nil || len(svc.graphCache.entries) != 2 || svc.graphCache.entries[1].key.repo != other {
		t.Fatalf("repository cache scope: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := svc.cachedGraphState(canceled, repo, view, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled hit returned data: %v", err)
	}
}

func TestGraphProjectionCacheRequiresCommittedGeneration(t *testing.T) {
	for _, name := range []string{"filesystem", "write", "zero revision", "write marker"} {
		t.Run(name, func(t *testing.T) {
			svc, storage := newFsckSvc(t)
			ctx := systemTestContext()
			view := domain.RepositoryView{Revision: domain.RepositoryRevision{Graph: 1}}
			if name != "filesystem" {
				svc.meta = &graphCacheReadState{FSStore: storage, readOnly: name != "write"}
			}
			if name == "zero revision" {
				view.Revision = domain.RepositoryRevision{}
			}
			if name == "write marker" {
				ctx = context.WithValue(ctx, afterCommitKey{}, &afterCommitActions{})
			}
			if _, err := svc.cachedGraphState(ctx, hh(t.Name()), view, ""); err != nil {
				t.Fatal(err)
			}
			if len(svc.graphCache.entries) != 0 {
				t.Fatal("uncommitted/unknown generation entered shared cache")
			}
		})
	}
}

func TestGraphProjectionCacheWarmReadDoesNotNeedBulk(t *testing.T) {
	svc, storage := newFsckSvc(t)
	svc.meta = &graphCacheReadState{FSStore: storage, readOnly: true}
	ctx, gate := admissionTestContext(10 * time.Millisecond)
	repo := hh(t.Name())
	view := domain.RepositoryView{Revision: domain.RepositoryRevision{Graph: 1}}
	if _, err := svc.cachedGraphState(ctx, repo, view, ""); err != nil {
		t.Fatal(err)
	}
	_, releaseBulk, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBulk()
	interactive, releaseRead, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRead()
	if _, err := svc.cachedGraphState(interactive, repo, view, ""); err != nil {
		t.Fatalf("warm read blocked by worker: %v", err)
	}
	view.Revision.Pending++
	if _, err := svc.cachedGraphState(interactive, repo, view, ""); !errors.Is(err, domain.ErrResourceOverloaded) || len(svc.graphCache.building) != 0 {
		t.Fatalf("cold miss bypassed bulk or leaked reservation: %v", err)
	}
	releaseBulk()
	if _, err := svc.cachedGraphState(interactive, repo, view, ""); err != nil {
		t.Fatalf("retry could not upgrade: %v", err)
	}
	releaseRead()
	assertResourcesReleased(t, gate)
}

func TestGraphProjectionCacheBoundsAndBusyRetry(t *testing.T) {
	var cache graphProjectionCache
	first := graphProjectionKey{repo: hh("repo")}
	_, finish, err := cache.lookup(first)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cache.lookup(first); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatal("concurrent identical miss must retry instead of holding a read slot")
	}
	finish()
	_, finish, err = cache.lookup(first)
	if err != nil {
		t.Fatal(err)
	}
	finish()
	cache.put(first, make([]byte, graphProjectionCacheBytes+1))
	if len(cache.entries) != 0 || cache.bytes != 0 {
		t.Fatal("oversized projection cached")
	}
	for index := range 10 {
		cache.put(graphProjectionKey{repo: hh(fmt.Sprint(index))}, make([]byte, 3<<20))
		if cache.bytes > graphProjectionCacheBytes || len(cache.entries) > graphProjectionCacheEntries {
			t.Fatal("cache exceeded retention bound")
		}
	}
	for index := range 10 {
		cache.put(graphProjectionKey{repo: hh(fmt.Sprint(index))}, []byte("{}"))
	}
	if len(cache.entries) != graphProjectionCacheEntries {
		t.Fatal("entry count bound not exercised")
	}
}

func TestResourceAdmissionSeparatesGraphUploadFromPendingAndRevision(t *testing.T) {
	svc, storage := newFsckSvc(t)
	ctx, gate := admissionTestContext(10 * time.Millisecond)
	repo := hh(t.Name())
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	_, releaseBulk, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBulk()
	if _, err := svc.QueryGraphState(ctx, repo, ""); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatalf("graph escaped bulk gate: %v", err)
	}
	if _, err := svc.GetRepositoryView(ctx, repo); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatalf("view escaped bulk gate: %v", err)
	}
	if _, err := svc.Commit(ctx, inbound.CommitInput{RepoID: repo}); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatalf("upload escaped shared gate: %v", err)
	}
	if _, err := svc.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo}); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatalf("chunks escaped shared gate: %v", err)
	}
	if err := svc.ProcessDocFinalizations(ctx, 1); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatalf("doc worker escaped shared gate: %v", err)
	}
	if _, err := svc.GetPendingView(ctx, repo); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatalf("cold legacy complete graph escaped bulk: %v", err)
	}
	if _, err := svc.GetPendingState(ctx, repo); err != nil {
		t.Fatalf("lightweight On Hold blocked behind bulk: %v", err)
	}
	if _, err := svc.QueryContext(ctx, repo, domain.ContextSelection{}); err != nil {
		t.Fatalf("context blocked behind bulk: %v", err)
	}
	_, releaseRead, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RepositoryRevision(ctx, repo); err != nil {
		t.Fatalf("lightweight revision waited for heavy capacity: %v", err)
	}
	releaseRead()
	releaseBulk()
	assertResourcesReleased(t, gate)
}
