package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func (state *graphCacheReadState) ReadRepositoryGraphSourceGeneration(_ context.Context, repo domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, error) {
	return testProjectionGeneration(repo), nil
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
	keys := svc.graphCache.Keys()
	if len(keys) != 1 {
		t.Fatal("initial projection was not retained")
	}
	key := keys[0]
	encoded, found := svc.graphCache.Get(keys[0])
	if !found || len(encoded) == 0 {
		t.Fatal("initial projection encoding is unavailable")
	}
	first.SnapshotIDs[0] = hh("caller mutation")
	first.ScopeLabels["caller"] = "mutation"
	second, err := svc.cachedGraphState(ctx, repo, view, "")
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := json.Marshal(second)
	keys = svc.graphCache.Keys()
	retained, found := svc.graphCache.Get(keys[0])
	if !bytes.Equal(expected, actual) || !found || len(retained) == 0 || &retained[0] != &encoded[0] {
		t.Fatal("same projection was rebuilt or caller mutated cached data")
	}
	view.Snapshots = append(view.Snapshots, domain.Snapshot{ID: hh("upload without public revision"), RepoID: repo, DocHash: hh("doc2")})
	changed, err := svc.cachedGraphState(ctx, repo, view, "")
	if err != nil || len(changed.SnapshotIDs) != 2 || svc.graphCache.Keys()[0].Inputs == key.Inputs {
		t.Fatalf("same-revision upload reused stale graph: %v %+v", err, changed.SnapshotIDs)
	}
	for _, revision := range []domain.RepositoryRevision{{Graph: 1, Pending: 1}, {Graph: 1, Pending: 1, Evidence: 1}, {Graph: 2, Pending: 1, Evidence: 1}} {
		view.Revision = revision
		graph, err := svc.cachedGraphState(ctx, repo, view, "")
		if err != nil || graph.Revision != revision || svc.graphCache.Stats().Entries != 1 || svc.graphCache.Keys()[0].Revision != revision {
			t.Fatalf("generation invalidation: %v %+v", err, graph.Revision)
		}
	}
	prior := svc.graphCache.Keys()[0]
	view.DefaultBranch = "changed-default"
	if _, err := svc.cachedGraphState(ctx, repo, view, ""); err != nil || svc.graphCache.Keys()[0].Inputs == prior.Inputs {
		t.Fatalf("default branch not in projection identity: %v", err)
	}
	origin := "https://github.com/cache/changed.git"
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.cachedGraphState(ctx, repo, view, ""); err != nil || svc.graphCache.Keys()[0].Origin != origin {
		t.Fatalf("origin not in projection identity: %v", err)
	}
	if _, err := svc.cachedGraphState(ctx, repo, view, "unknown-position"); !errors.Is(err, domain.ErrNotFound) || svc.graphCache.Stats().Building != 0 || svc.graphCache.Stats().Entries != 1 {
		t.Fatalf("failed position cached or flight leaked: %v", err)
	}
	other := hh("other repository")
	if _, err := svc.cachedGraphState(ctx, other, view, ""); err != nil || svc.graphCache.Stats().Entries != 2 || svc.graphCache.Keys()[1].Repo != other {
		t.Fatalf("repository cache scope: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := svc.cachedGraphState(canceled, repo, view, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled hit returned data: %v", err)
	}
}

func TestGraphProjectionCacheRequiresCommittedGeneration(t *testing.T) {
	for _, name := range []string{"filesystem", "write", "zero revision", "write marker", "catalog only"} {
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
			if name == "catalog only" {
				svc.meta = &metadataOnlyProjectionStore{MetadataStore: storage}
			}
			if _, err := svc.cachedGraphState(ctx, hh(t.Name()), view, ""); err != nil {
				t.Fatal(err)
			}
			if svc.graphCache.Stats().Entries != 0 {
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
	if _, err := svc.cachedGraphState(interactive, repo, view, ""); !errors.Is(err, domain.ErrResourceOverloaded) || svc.graphCache.Stats().Building != 0 {
		t.Fatalf("cold miss bypassed bulk or leaked reservation: %v", err)
	}
	releaseBulk()
	if _, err := svc.cachedGraphState(interactive, repo, view, ""); err != nil {
		t.Fatalf("retry could not upgrade: %v", err)
	}
	releaseRead()
	assertResourcesReleased(t, gate)
}

func TestGraphProjectionCacheFencesSourceOutsideFingerprint(t *testing.T) {
	service, storage := newFsckSvc(t)
	repo := hh(t.Name())
	metadata := &projectionCacheMetadataStore{MetadataStore: storage, generation: testProjectionGeneration(repo), readOnly: true}
	service.meta = metadata
	ctx := systemTestContext()
	view := domain.RepositoryView{Revision: domain.RepositoryRevision{Graph: 1}}
	if _, err := service.cachedGraphState(ctx, repo, view, ""); err != nil {
		t.Fatal(err)
	}
	for _, reset := range []bool{false, true} {
		keys := service.graphCache.Keys()
		oldKey := keys[0]
		oldEncoded, found := service.graphCache.Get(keys[0])
		if !found || len(oldEncoded) == 0 {
			t.Fatal("prior projection encoding is unavailable")
		}
		if reset {
			metadata.generation.Epoch = "22345678-abcd-4321-9876-123456789abc"
		} else {
			metadata.generation.Sequence++
		}
		if _, err := service.cachedGraphState(ctx, repo, view, ""); err != nil {
			t.Fatal(err)
		}
		keys = service.graphCache.Keys()
		nextKey := keys[0]
		nextEncoded, found := service.graphCache.Get(keys[0])
		if !found || len(nextEncoded) == 0 || oldKey.Inputs != nextKey.Inputs || oldKey.Revision != nextKey.Revision || oldKey.Source == nextKey.Source || &oldEncoded[0] == &nextEncoded[0] {
			t.Fatalf("source reset=%v reused outer graph: before=%+v after=%+v", reset, oldKey, nextKey)
		}
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
