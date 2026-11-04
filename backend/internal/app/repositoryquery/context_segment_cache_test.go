package repositoryquery

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func segmentOwnerCacheKey(repo domain.ContentHash) contextSegmentCacheKey {
	return contextSegmentCacheKey{repo: repo, source: legacyProjectCheckpoint(repo), revision: domain.RepositoryRevision{Graph: 1, Evidence: 2}, branch: "main", position: "HEAD", code: legacyProjectOID(1), scope: "current", defaultBranch: "main", origin: "https://github.com/example/repo"}
}

type segmentOwnerWriteContextKey struct{}

type segmentOwnerPinnedScope struct{}

func (segmentOwnerPinnedScope) IsPinnedRead(ctx context.Context) bool {
	return ctx.Value(segmentOwnerWriteContextKey{}) == nil
}

func TestContextSegmentOwnerCacheKeyPinsEveryDependency(test *testing.T) {
	repo := legacyProjectHash(test.Name())
	want := segmentOwnerCacheKey(repo)
	scope := &legacyProjectScope{pinned: true}
	generations := &legacyProjectGeneration{value: want.source}
	queries := &ContextQueries{cacheScope: scope, generations: generations}
	metadata := domain.Repo{ID: repo, DefaultBranch: want.defaultBranch, GitRemoteURL: want.origin}
	selection := domain.ContextSelection{Branch: want.branch, Position: want.position, CodeCommit: want.code, Scope: want.scope, SegmentLimit: 1}
	key, err := queries.segmentCacheKey(context.Background(), repo, metadata, want.revision, selection)
	if err != nil || key != want || !key.cacheable() || generations.calls != 1 || generations.repo != repo || scope.checks != 1 {
		test.Fatalf("explicit pinned dependencies changed: %+v %v", key, err)
	}
	selection.SegmentLimit, selection.SegmentOffset, selection.SegmentStateHash = 50, 1, legacyProjectHash("cursor")
	if paged, err := queries.segmentCacheKey(context.Background(), repo, metadata, want.revision, selection); err != nil || paged != key || generations.calls != 2 {
		test.Fatal("pagination altered the basis key or skipped the current source proof", paged, err)
	}
	cache := &SegmentCache{}
	cache.put(key, contextSegmentBasis{})
	for _, scenario := range []struct {
		name   string
		change func(*contextSegmentCacheKey)
	}{
		{"repository", func(changed *contextSegmentCacheKey) {
			changed.repo = legacyProjectHash("other repo")
			changed.source.RepoID = changed.repo
		}},
		{"graph revision", func(changed *contextSegmentCacheKey) { changed.revision.Graph++ }},
		{"evidence revision", func(changed *contextSegmentCacheKey) { changed.revision.Evidence++ }},
		{"pending revision", func(changed *contextSegmentCacheKey) { changed.revision.Pending++ }},
		{"source sequence", func(changed *contextSegmentCacheKey) { changed.source.Sequence++ }},
		{"source epoch", func(changed *contextSegmentCacheKey) { changed.source.Epoch = "22345678-abcd-4321-9876-123456789abc" }},
		{"branch", func(changed *contextSegmentCacheKey) { changed.branch = "feature" }},
		{"position", func(changed *contextSegmentCacheKey) { changed.position = "main" }},
		{"code", func(changed *contextSegmentCacheKey) { changed.code = legacyProjectOID(2) }},
		{"scope", func(changed *contextSegmentCacheKey) { changed.scope = "all" }},
		{"default branch", func(changed *contextSegmentCacheKey) { changed.defaultBranch = "trunk" }},
		{"origin", func(changed *contextSegmentCacheKey) { changed.origin = "https://github.com/example/other" }},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			changed := key
			scenario.change(&changed)
			if !changed.cacheable() {
				test.Fatal("dependency fixture lacks a valid independent source proof")
			}
			if _, hit := cache.get(changed); hit {
				test.Fatal("changed dependency reused the old projection")
			}
		})
	}
	selection.SegmentLimit, selection.SegmentOffset, selection.SegmentStateHash = 0, 0, ""
	queries.cacheScope, queries.generations = nil, nil
	if plain, err := queries.segmentCacheKey(context.Background(), repo, metadata, want.revision, selection); err != nil || plain.cacheable() || plain.source != (domain.RepositoryViewSourceCheckpoint{}) {
		test.Fatal("ordinary context query read or invented a generation", plain, err)
	}
}

func TestProjectionCacheRequiresCompletePinnedGraphSource(test *testing.T) {
	repo := legacyProjectHash(test.Name())
	selection := domain.ContextSelection{SegmentLimit: 1}
	for _, scenario := range []struct {
		name      string
		pinned    bool
		captured  bool
		missing   bool
		change    func(*domain.RepositoryViewSourceCheckpoint)
		readErr   error
		wantErr   error
		wantReads int
	}{
		{name: "write transaction is not pinned"},
		{name: "captured", pinned: true, captured: true},
		{name: "no generation port", pinned: true, missing: true},
		{name: "not found", pinned: true, readErr: fmt.Errorf("source: %w", domain.ErrNotFound), wantReads: 1},
		{name: "unsupported", pinned: true, readErr: domain.ErrCatalogUnsupported, wantReads: 1},
		{name: "reset", pinned: true, readErr: domain.ErrCatalogResetRequired, wantReads: 1},
		{name: "integrity", pinned: true, readErr: domain.ErrIntegrity, wantErr: domain.ErrIntegrity, wantReads: 1},
		{name: "overloaded", pinned: true, readErr: domain.ErrResourceOverloaded, wantErr: domain.ErrResourceOverloaded, wantReads: 1},
		{name: "canceled", pinned: true, readErr: context.Canceled, wantErr: context.Canceled, wantReads: 1},
		{name: "deadline", pinned: true, readErr: context.DeadlineExceeded, wantErr: context.DeadlineExceeded, wantReads: 1},
		{name: "version", pinned: true, change: func(source *domain.RepositoryViewSourceCheckpoint) { source.Version++ }, wantErr: domain.ErrIntegrity, wantReads: 1},
		{name: "view scope", pinned: true, change: func(source *domain.RepositoryViewSourceCheckpoint) { source.Scope = domain.RepositoryViewSourceScope }, wantErr: domain.ErrIntegrity, wantReads: 1},
		{name: "catalog scope", pinned: true, change: func(source *domain.RepositoryViewSourceCheckpoint) { source.Scope = domain.CatalogScope }, wantErr: domain.ErrIntegrity, wantReads: 1},
		{name: "foreign repo", pinned: true, change: func(source *domain.RepositoryViewSourceCheckpoint) { source.RepoID = legacyProjectHash("foreign") }, wantErr: domain.ErrIntegrity, wantReads: 1},
		{name: "missing epoch", pinned: true, change: func(source *domain.RepositoryViewSourceCheckpoint) { source.Epoch = "" }, wantErr: domain.ErrIntegrity, wantReads: 1},
		{name: "zero epoch", pinned: true, change: func(source *domain.RepositoryViewSourceCheckpoint) {
			source.Epoch = "00000000-0000-0000-0000-000000000000"
		}, wantErr: domain.ErrIntegrity, wantReads: 1},
		{name: "negative sequence", pinned: true, change: func(source *domain.RepositoryViewSourceCheckpoint) { source.Sequence = -1 }, wantErr: domain.ErrIntegrity, wantReads: 1},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			generations := &legacyProjectGeneration{value: legacyProjectCheckpoint(repo), err: scenario.readErr}
			if scenario.change != nil {
				scenario.change(&generations.value)
			}
			queries := &ContextQueries{cacheScope: &legacyProjectScope{pinned: scenario.pinned}, generations: generations, captured: scenario.captured}
			if scenario.missing {
				queries.generations = nil
			}
			key, err := queries.segmentCacheKey(context.Background(), repo, domain.Repo{ID: repo}, domain.RepositoryRevision{}, selection)
			if !errors.Is(err, scenario.wantErr) || key.cacheable() || key.source != (domain.RepositoryViewSourceCheckpoint{}) || generations.calls != scenario.wantReads {
				test.Fatalf("unproven read escaped its source fence: %+v %v reads=%d", key, err, generations.calls)
			}
			cache := &SegmentCache{}
			warm := segmentOwnerCacheKey(repo)
			cache.put(warm, contextSegmentBasis{})
			cache.put(key, contextSegmentBasis{})
			_, warmHit := cache.get(warm)
			if _, hit := cache.get(key); hit || !warmHit || cache.Stats() != (SegmentCacheStats{Entries: 1}) {
				test.Fatal("unproven key entered or displaced the shared warm segment cache")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	generations := &legacyProjectGeneration{value: legacyProjectCheckpoint(repo), cancel: cancel}
	queries := &ContextQueries{cacheScope: &legacyProjectScope{pinned: true}, generations: generations}
	if key, err := queries.segmentCacheKey(ctx, repo, domain.Repo{ID: repo}, domain.RepositoryRevision{}, selection); !errors.Is(err, context.Canceled) || key.cacheable() || generations.calls != 1 {
		test.Fatal("point-read cancellation certified a cache hit", key, err)
	}
	if key, err := queries.segmentCacheKey(ctx, repo, domain.Repo{ID: repo}, domain.RepositoryRevision{}, selection); !errors.Is(err, context.Canceled) || key.cacheable() || generations.calls != 1 {
		test.Fatal("already canceled read reached the source port", key, err)
	}
	generations = &legacyProjectGeneration{value: legacyProjectCheckpoint(repo)}
	queries.cacheScope, queries.generations = segmentOwnerPinnedScope{}, generations
	write := context.WithValue(context.Background(), segmentOwnerWriteContextKey{}, true)
	if key, err := queries.segmentCacheKey(write, repo, domain.Repo{ID: repo}, domain.RepositoryRevision{}, selection); err != nil || key.cacheable() || generations.calls != 0 {
		test.Fatal("application write context reused a committed graph proof", key, err)
	}
}

func TestContextSegmentOwnerCacheKeepsEightFIFOEntries(test *testing.T) {
	cache := &SegmentCache{}
	if cache.Stats() != (SegmentCacheStats{}) {
		test.Fatal("zero-value shared cache is not empty")
	}
	key := segmentOwnerCacheKey(legacyProjectHash(test.Name()))
	original := key
	for sequence := 0; sequence < 8; sequence++ {
		key.source.Sequence = int64(sequence)
		cache.put(key, contextSegmentBasis{view: domain.ContextQueryView{StateHash: legacyProjectHash(fmt.Sprint(sequence)), Snapshots: []domain.Snapshot{{}}, History: []domain.HistoryEvent{{}}}})
	}
	original.source.Sequence = 0
	before, hit := cache.get(original)
	if !hit || cache.Stats() != (SegmentCacheStats{Entries: 8, Weight: 16}) {
		test.Fatal("entry or combined snapshot/history weight bound changed", cache.Stats())
	}
	cache.put(original, contextSegmentBasis{})
	after, _ := cache.get(original)
	if after.view.StateHash != before.view.StateHash || cache.Stats() != (SegmentCacheStats{Entries: 8, Weight: 16}) {
		test.Fatal("duplicate key replaced immutable basis or inflated accounting")
	}
	key.source.Sequence = 8
	cache.put(key, contextSegmentBasis{view: domain.ContextQueryView{History: []domain.HistoryEvent{{}}}})
	if _, hit := cache.get(original); hit || cache.Stats() != (SegmentCacheStats{Entries: 8, Weight: 15}) {
		test.Fatal("a cache read changed FIFO eviction or the eight-entry bound", cache.Stats())
	}
}

func TestContextSegmentOwnerCacheKeepsExact250000WeightBound(test *testing.T) {
	key := segmentOwnerCacheKey(legacyProjectHash(test.Name()))
	snapshots := make([]domain.Snapshot, 250001)
	cache := &SegmentCache{}
	cache.put(key, contextSegmentBasis{view: domain.ContextQueryView{Snapshots: snapshots}})
	if cache.Stats() != (SegmentCacheStats{}) {
		test.Fatal("oversized projection entered the cache")
	}
	cache.put(key, contextSegmentBasis{view: domain.ContextQueryView{Snapshots: snapshots[:249999], History: []domain.HistoryEvent{{}}}})
	if _, hit := cache.get(key); hit || cache.Stats() != (SegmentCacheStats{}) || cache.RetainedBytes() != 0 {
		test.Fatal("the count boundary bypassed the stronger retained-byte bound", cache.Stats())
	}
	newer := key
	newer.source.Sequence++
	cache.put(newer, contextSegmentBasis{view: domain.ContextQueryView{History: []domain.HistoryEvent{{}}}})
	if _, hit := cache.get(key); hit || cache.Stats() != (SegmentCacheStats{Entries: 1, Weight: 1}) {
		test.Fatal("combined weight overflow did not evict the oldest projection", cache.Stats())
	}
	for sequence := 0; sequence < 5; sequence++ {
		key.source.Sequence = int64(sequence + 100)
		cache.put(key, contextSegmentBasis{view: domain.ContextQueryView{Snapshots: snapshots[:62500]}})
	}
	if cache.Stats() != (SegmentCacheStats{Entries: 1, Weight: 1}) || cache.RetainedBytes() > ProjectionRetentionBytes {
		test.Fatal("oversized entries bypassed cumulative byte admission", cache.Stats())
	}
}
