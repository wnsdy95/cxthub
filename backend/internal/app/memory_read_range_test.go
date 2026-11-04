package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryReadRangeStoreFunc func(context.Context, domain.ContentHash, domain.ContentHash, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error

func (storage memoryReadRangeStoreFunc) WithMemoryReadRange(ctx context.Context, repo, identity domain.ContentHash, read func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	return storage(ctx, repo, identity, read)
}

func TestMemoryReadRangeValidatesBeforeOpening(test *testing.T) {
	captured, original := memoryReadFixture(test, 1, 32)
	bundle := buildMemoryReadFixture(test, captured)
	for _, scenario := range []string{"repo", "identity", "selection", "content", "negative-limit", "large-limit", "long-cursor", "canceled", "unsupported"} {
		test.Run(scenario, func(test *testing.T) {
			request, repo, identity := original, captured.reader.repo, bundle.ID
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := domain.ErrValidation
			calls := 0
			var storage outbound.MemoryReadRangeStore = memoryReadRangeStoreFunc(func(context.Context, domain.ContentHash, domain.ContentHash, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
				calls++
				return domain.ErrIntegrity
			})
			switch scenario {
			case "repo":
				repo = "invalid"
			case "identity":
				identity = "invalid"
			case "selection":
				request.Selection.CodeCommit = "invalid"
			case "content":
				request.Content = "invalid"
			case "negative-limit":
				request.Limit = -1
			case "large-limit":
				request.Limit = 51
			case "long-cursor":
				request.Cursor = strings.Repeat("x", 1025)
			case "canceled":
				cancel()
				want = context.Canceled
			case "unsupported":
				storage, want = nil, domain.ErrCatalogUnsupported
			}
			page, err := ReadMemoryReadPage(ctx, storage, repo, identity, request)
			if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) || calls != 0 {
				test.Fatalf("invalid input entered the range: calls=%d page=%+v err=%v", calls, page, err)
			}
		})
	}
}

func TestMemoryReadRangeScopeAndCursorBeforeChunks(test *testing.T) {
	captured, original := memoryReadFixture(test, 130, 32)
	bundle := buildMemoryReadFixture(test, captured)
	for _, scenario := range []string{"content", "branch", "snapshot", "code", "memory", "cursor", "stale", "past-end"} {
		test.Run(scenario, func(test *testing.T) {
			storage := &memoryReadFixtureStore{bundle: bundle}
			request := original
			want := domain.ErrValidation
			switch scenario {
			case "content":
				request.Content = "claims"
			case "branch":
				request.Selection.Branch = "main"
			case "snapshot":
				request.Selection.SnapshotID = hh("other-snapshot")
			case "code":
				request.Selection.CodeCommit = effectiveOID(99)
			case "memory":
				request.Selection.MemoryHash = hh("other-memory")
			case "cursor":
				request.Cursor = "!"
			case "stale":
				request.Cursor = memoryReadCursor(test, hh("other-state"), 1)
				want = domain.ErrEffectiveMemoryCursorStale
			case "past-end":
				request.Cursor = memoryReadCursor(test, bundle.Header.View.StateHash, bundle.Header.View.Total+1)
			}
			page, err := ReadMemoryReadPage(context.Background(), storage, captured.reader.repo, bundle.ID, request)
			if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) || storage.headers != 1 || len(storage.reads) != 0 {
				test.Fatalf("invalid scope read chunks: headers=%d chunks=%v err=%v", storage.headers, storage.reads, err)
			}
		})
	}
}

func TestMemoryReadRangeTerminalErrorDiscardsPage(test *testing.T) {
	captured, request := memoryReadFixture(test, 130, 32)
	bundle := buildMemoryReadFixture(test, captured)
	request.Limit = 20
	request.Cursor = memoryReadCursor(test, bundle.Header.View.StateHash, 60)
	for _, terminal := range []error{domain.ErrNotFound, domain.ErrIntegrity, context.Canceled, context.DeadlineExceeded, errors.New("snapshot completion failed")} {
		test.Run(terminal.Error(), func(test *testing.T) {
			fixture := &memoryReadFixtureStore{bundle: bundle}
			completed := false
			storage := memoryReadRangeStoreFunc(func(ctx context.Context, repo, identity domain.ContentHash, read func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
				if err := fixture.WithMemoryReadRange(ctx, repo, identity, read); err != nil {
					return err
				}
				completed = true
				return fmt.Errorf("finish range: %w", terminal)
			})
			page, err := ReadMemoryReadPage(context.Background(), storage, captured.reader.repo, bundle.ID, request)
			if !errors.Is(err, terminal) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) || !completed || !slices.Equal(fixture.reads, []int{0, 1}) {
				test.Fatalf("terminal failure leaked a completed page: completed=%t chunks=%v page=%+v err=%v", completed, fixture.reads, page, err)
			}
		})
	}
}

func TestMemoryReadRangeRejectsForeignCallbackHeader(test *testing.T) {
	captured, request := memoryReadFixture(test, 1, 32)
	bundle := buildMemoryReadFixture(test, captured)
	fixture := &memoryReadFixtureStore{bundle: bundle}
	storage := memoryReadRangeStoreFunc(func(ctx context.Context, repo, identity domain.ContentHash, read func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
		return fixture.WithMemoryReadRange(ctx, repo, identity, func(bound context.Context, header domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
			header.Source.Graph.RepoID = hh("foreign-repo")
			return read(bound, header, chunks)
		})
	})
	page, err := ReadMemoryReadPage(context.Background(), storage, captured.reader.repo, bundle.ID, request)
	if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) || fixture.headers != 1 || len(fixture.reads) != 0 {
		test.Fatal("foreign callback header reached chunk reads", fixture.headers, fixture.reads, err)
	}
}

func TestMemoryReadRangeUsesCallbackContext(test *testing.T) {
	captured, request := memoryReadFixture(test, 130, 32)
	bundle := buildMemoryReadFixture(test, captured)
	request.Limit = 20
	request.Cursor = memoryReadCursor(test, bundle.Header.View.StateHash, 60)
	for _, scenario := range []string{"active", "canceled-before-callback", "canceled-after-chunk", "parent-canceled-after-callback"} {
		test.Run(scenario, func(test *testing.T) {
			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			fixture := &memoryReadFixtureStore{bundle: bundle}
			storage := memoryReadRangeStoreFunc(func(ctx context.Context, repo, identity domain.ContentHash, read func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
				return fixture.WithMemoryReadRange(ctx, repo, identity, func(ctx context.Context, header domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
					bound, cancel := context.WithCancel(ctx)
					defer cancel()
					if scenario == "canceled-before-callback" {
						cancel()
					}
					err := read(bound, header, memoryReadFixtureChunkReader(func(actual context.Context, ordinal int) ([]domain.EffectiveMemoryItem, error) {
						if actual != bound {
							test.Fatal("chunk read escaped the callback context")
						}
						items, err := chunks.ReadChunk(actual, ordinal)
						if scenario == "canceled-after-chunk" {
							cancel()
						}
						return items, err
					}))
					if scenario == "parent-canceled-after-callback" {
						cancelParent()
					}
					return err
				})
			})
			page, err := ReadMemoryReadPage(parent, storage, captured.reader.repo, bundle.ID, request)
			if scenario == "active" {
				if err != nil || len(page.Items) != 20 || !slices.Equal(fixture.reads, []int{0, 1}) {
					test.Fatal("scoped range did not read each selected chunk once", fixture.reads, err)
				}
			} else if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) {
				test.Fatal("canceled range returned a page", page, err)
			}
			if scenario == "canceled-before-callback" && len(fixture.reads) != 0 || scenario == "canceled-after-chunk" && !slices.Equal(fixture.reads, []int{0}) {
				test.Fatal("canceled callback continued reading chunks", fixture.reads)
			}
		})
	}
}

func TestMemoryReadRangeConsumesOwnedHeaderAndItems(test *testing.T) {
	ctx := context.Background()
	captured, request := memoryBundleFixture(test, true)
	replaceMemoryReadDigest(test, captured, domain.MemoryDigest{SnapshotID: request.Selection.SnapshotID, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{
		SourceSnapshot: request.Selection.SnapshotID,
		Claims:         []domain.MemoryClaim{{Kind: "code", Text: "owned code scope", Code: &domain.MemoryCodeScope{Commit: effectiveOID(1), Paths: []string{"file"}}}},
	}}})
	bundle := buildMemoryReadFixture(test, captured)
	fixture := &memoryReadFixtureStore{bundle: bundle}
	var callbackHeader domain.MemoryReadHeader
	var decoded []domain.EffectiveMemoryItem
	storage := memoryReadRangeStoreFunc(func(ctx context.Context, repo, identity domain.ContentHash, read func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
		return fixture.WithMemoryReadRange(ctx, repo, identity, func(bound context.Context, header domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
			callbackHeader = header
			return read(bound, header, memoryReadFixtureChunkReader(func(ctx context.Context, ordinal int) ([]domain.EffectiveMemoryItem, error) {
				items, err := chunks.ReadChunk(ctx, ordinal)
				decoded = items
				return items, err
			}))
		})
	})
	page, err := ReadMemoryReadPage(ctx, storage, captured.reader.repo, bundle.ID, request)
	if err != nil || len(page.Items) != 1 || len(decoded) != 1 || page.Items[0].Code == nil || page.Inclusion == nil || len(page.Inclusion.Roots) == 0 || len(page.Inclusion.SnapshotIDs) == 0 {
		test.Fatal("fixture did not produce owned nested header and item fields", page, err)
	}
	if page.Inclusion != callbackHeader.View.Inclusion || page.Items[0].Code != decoded[0].Code {
		test.Fatal("application re-decoded the owned callback header or items")
	}
	if page.Inclusion == bundle.Header.View.Inclusion {
		test.Fatal("range adapter did not own its header")
	}
	want, err := captured.Project(ctx, request)
	if err != nil || !reflect.DeepEqual(page, want) {
		test.Fatal("scoped page changed the selected result", err)
	}
	page.Selection.SnapshotID = hh("changed-selector")
	page.Inclusion.Roots[0] = hh("changed-root")
	page.Inclusion.SnapshotIDs[0] = hh("changed-snapshot")
	page.Items[0].Code.Paths[0] = "changed-path"
	again, err := ReadMemoryReadPage(ctx, storage, captured.reader.repo, bundle.ID, request)
	if err != nil || !reflect.DeepEqual(again, want) || fixture.headers != 2 || !slices.Equal(fixture.reads, []int{0, 0}) {
		test.Fatal("returned selection aliased persisted state", fixture.headers, fixture.reads, err)
	}
}

func TestMemoryReadRangeNormalizedLimitsAndEmptyPages(test *testing.T) {
	for _, scenario := range []struct {
		name  string
		count int
		limit int
		end   bool
		want  int
	}{
		{name: "default", count: 130, want: 20},
		{name: "maximum", count: 130, limit: 50, want: 50},
		{name: "empty"},
		{name: "end", count: 130, end: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			captured, request := memoryReadFixture(test, scenario.count, 32)
			bundle := buildMemoryReadFixture(test, captured)
			storage := &memoryReadFixtureStore{bundle: bundle}
			request.Limit, request.Content = scenario.limit, "all"
			if scenario.end {
				request.Cursor = memoryReadCursor(test, bundle.Header.View.StateHash, bundle.Header.View.Total)
			}
			page, err := ReadMemoryReadPage(context.Background(), storage, captured.reader.repo, bundle.ID, request)
			if err != nil || page.Items == nil || len(page.Items) != scenario.want || page.Total != scenario.count || page.Content != "" || storage.headers != 1 {
				test.Fatal("normalization or page metadata changed", page, storage.headers, err)
			}
			if scenario.want == 0 {
				if len(storage.reads) != 0 || page.NextCursor != "" {
					test.Fatal("empty page read chunks or returned a continuation", storage.reads, page.NextCursor)
				}
			} else if !slices.Equal(storage.reads, []int{0}) || page.NextCursor != memoryReadCursor(test, page.StateHash, scenario.want) {
				test.Fatal("page changed chunk cost or v1 cursor identity", storage.reads, page.NextCursor)
			}
		})
	}
}

func memoryReadSizedItem(test *testing.T, snapshot domain.ContentHash, ordinal, size int) domain.EffectiveMemoryItem {
	test.Helper()
	item := domain.EffectiveMemoryItem{ID: hh(fmt.Sprint(ordinal)), SourceSnapshot: snapshot, Kind: "legacy_fact", MemoryClaimAssessment: domain.MemoryClaimAssessment{State: "review", Reason: "unverified"}}
	raw, err := json.Marshal(item)
	if err != nil || size < len(raw) {
		test.Fatal("invalid serialized item size", size, len(raw), err)
	}
	item.Text = strings.Repeat("x", size-len(raw))
	return item
}

func TestMemoryReadRangeLargeItemBoundaries(test *testing.T) {
	for _, scenario := range []struct {
		name  string
		sizes []int
		want  int
		reads []int
	}{
		{name: "exact-item", sizes: []int{effectiveMemoryPageBytes, 1024}, want: 1, reads: []int{0}},
		{name: "exact-page", sizes: []int{effectiveMemoryPageBytes / 2, effectiveMemoryPageBytes / 2, 1024}, want: 2, reads: []int{0, 1}},
		{name: "next-item-exceeds-page", sizes: []int{150 << 10, 150 << 10, 1024}, want: 1, reads: []int{0, 1}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			ctx := context.Background()
			captured, request := memoryReadFixture(test, 0, 0)
			header := buildMemoryReadFixture(test, captured).Header
			items := make([]domain.EffectiveMemoryItem, len(scenario.sizes))
			for ordinal, size := range scenario.sizes {
				items[ordinal] = memoryReadSizedItem(test, request.Selection.SnapshotID, ordinal, size)
			}
			header.View.Total = len(items)
			var err error
			header.View.DeliveryStateHash, err = domain.EffectiveMemoryDeliveryStateHash(ctx, captured.reader.repo, header.Origin, header.View, items)
			if err != nil {
				test.Fatal(err)
			}
			bundle, err := domain.BuildMemoryReadBundle(ctx, header, items)
			if err != nil {
				test.Fatal(err)
			}
			storage := &memoryReadFixtureStore{bundle: bundle}
			request.Limit = 50
			page, err := ReadMemoryReadPage(ctx, storage, captured.reader.repo, bundle.ID, request)
			if err != nil || !reflect.DeepEqual(page.Items, items[:scenario.want]) || !slices.Equal(storage.reads, scenario.reads) || page.NextCursor != memoryReadCursor(test, header.View.StateHash, scenario.want) {
				test.Fatalf("large item boundary changed: items=%d chunks=%v cursor=%s err=%v", len(page.Items), storage.reads, page.NextCursor, err)
			}
			request.Cursor, storage.reads = page.NextCursor, nil
			page, err = ReadMemoryReadPage(ctx, storage, captured.reader.repo, bundle.ID, request)
			if err != nil || !reflect.DeepEqual(page.Items, items[scenario.want:]) || page.NextCursor != "" {
				test.Fatal("large item continuation skipped or duplicated items", err)
			}
		})
	}
}

func TestMemoryReadRangeOversizedDecodedItemDiscardsPage(test *testing.T) {
	captured, request := memoryReadFixture(test, 2, 32)
	bundle := buildMemoryReadFixture(test, captured)
	request.Limit = 50
	for _, ordinal := range []int{0, 1} {
		test.Run(fmt.Sprint(ordinal), func(test *testing.T) {
			fixture := &memoryReadFixtureStore{bundle: bundle}
			storage := memoryReadRangeStoreFunc(func(ctx context.Context, repo, identity domain.ContentHash, read func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
				return fixture.WithMemoryReadRange(ctx, repo, identity, func(bound context.Context, header domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
					return read(bound, header, memoryReadFixtureChunkReader(func(ctx context.Context, chunk int) ([]domain.EffectiveMemoryItem, error) {
						items, err := chunks.ReadChunk(ctx, chunk)
						if err != nil {
							return nil, err
						}
						items[ordinal].Text = strings.Repeat("<", effectiveMemoryPageBytes/6)
						return items, nil
					}))
				})
			})
			page, err := ReadMemoryReadPage(context.Background(), storage, captured.reader.repo, bundle.ID, request)
			if !errors.Is(err, domain.ErrMemoryProjectionLimit) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) || !slices.Equal(fixture.reads, []int{0}) {
				test.Fatal("oversized serialized item returned a partial page", fixture.reads, err)
			}
		})
	}
}
