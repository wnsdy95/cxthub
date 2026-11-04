//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type graphCachePGRootPolicy struct{ *store.PostgresStore }

func (storage graphCachePGRootPolicy) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	metadata, err := storage.PostgresStore.GetRepo(ctx, repo)
	metadata.RequiredDocIdentity = domain.DocumentIdentityRootV1
	return metadata, err
}

func TestPGGraphProjectionSurvivesPRQueueReplay(t *testing.T) {
	svc, storage, repo := collaborationPG(t)
	ctx := systemTestContext()
	metadata, err := storage.GetRepo(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	metadata.GitRemoteURL = "https://github.com/cache/repository.git"
	if _, err := storage.PutRepo(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	first := collaborationSnapshot(t, storage, repo, "first")
	if err := svc.PutPending(ctx, repo, "pending", domain.Pending{Target: first}); err != nil {
		t.Fatal(err)
	}
	full, err := svc.GetRepositoryView(ctx, repo)
	if err != nil || svc.graphCache.Stats().Entries != 1 {
		t.Fatalf("initial cache: %v", err)
	}
	keys := svc.graphCache.Keys()
	encoded, found := svc.graphCache.Get(keys[0])
	if !found || len(encoded) == 0 {
		t.Fatal("initial projection encoding is unavailable")
	}
	pr := domain.PullRequestMerge{Number: 42, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	for range 3 {
		if _, err := svc.SubmitPRPromotion(ctx, repo, pr); err != nil {
			t.Fatal(err)
		}
	}
	admission, gate := admissionTestContext(10 * time.Millisecond)
	_, releaseBulk, err := AcquireResources(admission, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBulk()
	after, err := svc.GetRepositoryView(admission, repo)
	keys = svc.graphCache.Keys()
	retained, found := svc.graphCache.Get(keys[0])
	if err != nil || after.Revision != full.Revision || !found || len(retained) == 0 || &encoded[0] != &retained[0] {
		t.Fatalf("queue replay discarded warm graph: before=%+v after=%+v err=%v", full.Revision, after.Revision, err)
	}
	releaseBulk()
	assertResourcesReleased(t, gate)
}

func TestPGGraphProjectionCacheSharedEndpointsAndUnversionedUpload(t *testing.T) {
	svc, storage, repo := collaborationPG(t)
	ctx := systemTestContext()
	first := collaborationSnapshot(t, storage, repo, "first")
	if err := svc.PutPending(ctx, repo, "pending", domain.Pending{Target: first}); err != nil {
		t.Fatal(err)
	}
	full, err := svc.GetRepositoryView(ctx, repo)
	if err != nil || svc.graphCache.Stats().Entries != 1 {
		t.Fatalf("first projection: entries=%d err=%v", svc.graphCache.Stats().Entries, err)
	}
	keys := svc.graphCache.Keys()
	encoded, found := svc.graphCache.Get(keys[0])
	if !found || len(encoded) == 0 {
		t.Fatal("initial projection encoding is unavailable")
	}
	admission, gate := admissionTestContext(10 * time.Millisecond)
	_, releaseBulk, err := AcquireResources(admission, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBulk()
	for _, read := range []func() error{
		func() error { _, err := svc.GetRepositoryView(admission, repo); return err },
		func() error { _, err := svc.QueryGraphState(admission, repo, ""); return err },
		func() error { _, err := svc.GetPendingView(admission, repo); return err },
		func() error { _, err := svc.GetPendingState(admission, repo); return err },
	} {
		if err := read(); err != nil {
			t.Fatalf("warm PG read blocked by worker: %v", err)
		}
	}
	releaseBulk()
	assertResourcesReleased(t, gate)
	graph, err := svc.QueryGraphState(ctx, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := svc.GetPendingView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := json.Marshal(full.Graph)
	for _, value := range []any{graph, pending.Graph} {
		actual, _ := json.Marshal(value)
		if string(actual) != string(expected) {
			t.Fatal("endpoint graph contracts diverged")
		}
	}
	keys = svc.graphCache.Keys()
	retained, found := svc.graphCache.Get(keys[0])
	if !found || len(retained) == 0 || &encoded[0] != &retained[0] {
		t.Fatal("same pinned inputs recomputed graph")
	}
	state, err := svc.GetPendingState(ctx, repo)
	keys = svc.graphCache.Keys()
	retained, found = svc.graphCache.Get(keys[0])
	if err != nil || state.Projection != "on-hold-v1" || state.Revision != full.Revision || len(state.Snapshots) != 1 || !found || len(retained) == 0 || &encoded[0] != &retained[0] {
		t.Fatalf("independent PG pending projection: %+v %v", state, err)
	}
	svc.meta = graphCachePGRootPolicy{storage}
	if _, err := svc.GetPendingView(ctx, repo); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatalf("cache bypassed current peer compatibility: %v", err)
	}
	if _, err := svc.GetPendingState(ctx, repo); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatalf("lightweight projection bypassed compatibility: %v", err)
	}
	svc.meta = storage
	second := collaborationSnapshot(t, storage, repo, "uploaded without public revision")
	after, err := svc.GetRepositoryView(ctx, repo)
	if err != nil || after.Revision != full.Revision || len(after.Graph.SnapshotIDs) != 2 {
		t.Fatalf("unversioned snapshot reused old projection: revision=%+v ids=%v err=%v", after.Revision, after.Graph.SnapshotIDs, err)
	}
	if err := svc.PutPending(ctx, repo, "pending", domain.Pending{Target: second}); err != nil {
		t.Fatal(err)
	}
	changed, err := svc.GetPendingView(ctx, repo)
	if err != nil || changed.Revision.Pending <= full.Revision.Pending || len(changed.Snapshots) != 1 || changed.Snapshots[0].ID != second {
		t.Fatalf("pending generation not refreshed: %+v %v", changed, err)
	}
}
