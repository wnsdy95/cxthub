package memoryquery

import (
	"context"
	"encoding/base64"
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

type rangePageStore struct {
	bundle  domain.MemoryReadBundle
	headers int
	reads   []int
	cancel  context.CancelFunc
}

type rangePageChunkReader func(context.Context, int) ([]domain.EffectiveMemoryItem, error)

func (reader rangePageChunkReader) ReadChunk(ctx context.Context, ordinal int) ([]domain.EffectiveMemoryItem, error) {
	return reader(ctx, ordinal)
}

func (storage *rangePageStore) WithMemoryReadRange(ctx context.Context, repo, identity domain.ContentHash, read func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	header, err := storage.ReadMemoryReadHeader(ctx, repo, identity)
	if err != nil {
		return err
	}
	verifier, err := domain.NewMemoryReadChunkVerifier(ctx, repo, identity, header)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(header)
	if err != nil {
		return err
	}
	var owned domain.MemoryReadHeader
	if err := domain.DecodeRepositorySourceJSON(ctx, raw, &owned); err != nil {
		return err
	}
	return read(ctx, owned, rangePageChunkReader(func(bound context.Context, ordinal int) ([]domain.EffectiveMemoryItem, error) {
		raw, err := storage.ReadMemoryReadChunk(bound, repo, identity, ordinal)
		if err != nil {
			return nil, err
		}
		return verifier.Decode(bound, ordinal, raw)
	}))
}

func (storage *rangePageStore) ReadMemoryReadHeader(ctx context.Context, repo, identity domain.ContentHash) (domain.MemoryReadHeader, error) {
	storage.headers++
	if err := ctx.Err(); err != nil {
		return domain.MemoryReadHeader{}, err
	}
	if repo != storage.bundle.Header.Source.Graph.RepoID || identity != storage.bundle.ID {
		return domain.MemoryReadHeader{}, domain.ErrNotFound
	}
	return storage.bundle.Header, nil
}

func (storage *rangePageStore) ReadMemoryReadChunk(ctx context.Context, repo, identity domain.ContentHash, ordinal int) ([]byte, error) {
	storage.reads = append(storage.reads, ordinal)
	if repo != storage.bundle.Header.Source.Graph.RepoID || identity != storage.bundle.ID || ordinal < 0 || ordinal >= len(storage.bundle.Chunks) {
		return nil, domain.ErrNotFound
	}
	if storage.cancel != nil {
		storage.cancel()
	}
	return storage.bundle.Chunks[ordinal].Bytes, ctx.Err()
}

type rangePageStoreFunc func(context.Context, domain.ContentHash, domain.ContentHash, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error

func (storage rangePageStoreFunc) WithMemoryReadRange(ctx context.Context, repo, identity domain.ContentHash, read func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	return storage(ctx, repo, identity, read)
}

func rangePageHash(value string) domain.ContentHash {
	return domain.HashContent([]byte(value))
}

func rangePageFixture(test *testing.T, count, textBytes int) (domain.MemoryReadBundle, domain.EffectiveMemoryRequest) {
	test.Helper()
	items := make([]domain.EffectiveMemoryItem, count)
	for ordinal := range items {
		items[ordinal] = domain.EffectiveMemoryItem{
			ID: rangePageHash(fmt.Sprint(ordinal)), SourceSnapshot: rangePageHash("selected snapshot"),
			Kind: "legacy_fact", Text: fmt.Sprintf("%06d %s", ordinal, strings.Repeat("x", textBytes)),
			MemoryClaimAssessment: domain.MemoryClaimAssessment{State: "review", Reason: "unverified"},
		}
	}
	header := domain.MemoryReadHeader{
		Version: domain.MemoryReadVersion, Rules: domain.MemoryReadRules, CaptureID: rangePageHash("capture"),
		Source: domain.RepositoryMemorySourceGeneration{
			Version: domain.RepositoryMemorySourceVersion, Epoch: "23456789-1234-4321-9876-123456789abc", Sequence: 9007199254740995,
			Graph: domain.RepositoryViewSourceCheckpoint{
				Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope,
				RepoID: rangePageHash("repository"), Epoch: "12345678-1234-4321-9876-123456789abc", Sequence: 9007199254740993,
			},
		},
		Origin: "https://example.invalid/owner/repo",
		View: domain.EffectiveMemoryPage{
			Selection: domain.EffectiveMemorySelection{SnapshotID: rangePageHash("selected snapshot"), CodeCommit: strings.Repeat("a", 40)},
			StateHash: rangePageHash("state"), LineageHash: rangePageHash("lineage"),
			Revision: domain.RepositoryRevision{Graph: 9007199254740997, Pending: 1, Evidence: 9007199254740999},
		},
	}
	bundle := rangePageBuild(test, header, items)
	return bundle, domain.EffectiveMemoryRequest{Selection: bundle.Header.View.Selection}
}

func rangePageBuild(test *testing.T, header domain.MemoryReadHeader, items []domain.EffectiveMemoryItem) domain.MemoryReadBundle {
	test.Helper()
	header.Chunks = nil
	header.View.Total = len(items)
	var err error
	header.View.DeliveryStateHash, err = domain.EffectiveMemoryDeliveryStateHash(context.Background(), header.Source.Graph.RepoID, header.Origin, header.View, items)
	if err != nil {
		test.Fatal(err)
	}
	bundle, err := domain.BuildMemoryReadBundle(context.Background(), header, items)
	if err != nil {
		test.Fatal(err)
	}
	if err := domain.ValidateMemoryReadBundle(context.Background(), bundle); err != nil {
		test.Fatal(err)
	}
	return bundle
}

func rangePageCursor(test *testing.T, state domain.ContentHash, offset int) string {
	test.Helper()
	raw, err := json.Marshal(Cursor{Version: 1, State: state, Index: offset})
	if err != nil {
		test.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestReadPageValidatesBeforeOpening(test *testing.T) {
	bundle, original := rangePageFixture(test, 1, 32)
	for _, scenario := range []string{"repo", "identity", "selection", "content", "negative-limit", "large-limit", "long-cursor", "canceled", "unsupported"} {
		test.Run(scenario, func(test *testing.T) {
			request, repo, identity := original, bundle.Header.Source.Graph.RepoID, bundle.ID
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := domain.ErrValidation
			calls := 0
			var storage outbound.MemoryReadRangeStore = rangePageStoreFunc(func(context.Context, domain.ContentHash, domain.ContentHash, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
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
			page, err := ReadPage(ctx, storage, repo, identity, request)
			if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) || calls != 0 {
				test.Fatalf("invalid input entered the range: calls=%d page=%+v err=%v", calls, page, err)
			}
		})
	}
}

func TestReadPageScopeAndCursorBeforeChunks(test *testing.T) {
	bundle, original := rangePageFixture(test, 130, 32)
	for _, scenario := range []string{"content", "branch", "snapshot", "code", "memory", "cursor", "stale", "past-end"} {
		test.Run(scenario, func(test *testing.T) {
			storage := &rangePageStore{bundle: bundle}
			request := original
			want := domain.ErrValidation
			switch scenario {
			case "content":
				request.Content = "claims"
			case "branch":
				request.Selection.Branch = "main"
			case "snapshot":
				request.Selection.SnapshotID = rangePageHash("other-snapshot")
			case "code":
				request.Selection.CodeCommit = strings.Repeat("f", 40)
			case "memory":
				request.Selection.MemoryHash = rangePageHash("other-memory")
			case "cursor":
				request.Cursor = "!"
			case "stale":
				request.Cursor = rangePageCursor(test, rangePageHash("other-state"), 1)
				want = domain.ErrEffectiveMemoryCursorStale
			case "past-end":
				request.Cursor = rangePageCursor(test, bundle.Header.View.StateHash, bundle.Header.View.Total+1)
			}
			page, err := ReadPage(context.Background(), storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request)
			if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) || storage.headers != 1 || len(storage.reads) != 0 {
				test.Fatalf("invalid scope read chunks: headers=%d chunks=%v err=%v", storage.headers, storage.reads, err)
			}
		})
	}
}

func TestReadPageTerminalErrorDiscardsPage(test *testing.T) {
	bundle, request := rangePageFixture(test, 130, 32)
	request.Limit = 20
	request.Cursor = rangePageCursor(test, bundle.Header.View.StateHash, 60)
	for _, terminal := range []error{domain.ErrNotFound, domain.ErrIntegrity, context.Canceled, context.DeadlineExceeded, errors.New("snapshot completion failed")} {
		test.Run(terminal.Error(), func(test *testing.T) {
			fixture := &rangePageStore{bundle: bundle}
			completed := false
			storage := rangePageStoreFunc(func(ctx context.Context, repo, identity domain.ContentHash, read func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
				if err := fixture.WithMemoryReadRange(ctx, repo, identity, read); err != nil {
					return err
				}
				completed = true
				return fmt.Errorf("finish range: %w", terminal)
			})
			page, err := ReadPage(context.Background(), storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request)
			if !errors.Is(err, terminal) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) || !completed || !slices.Equal(fixture.reads, []int{0, 1}) {
				test.Fatalf("terminal failure leaked a completed page: completed=%t chunks=%v page=%+v err=%v", completed, fixture.reads, page, err)
			}
		})
	}
}

func TestReadPageRejectsForeignCallbackHeader(test *testing.T) {
	bundle, request := rangePageFixture(test, 1, 32)
	fixture := &rangePageStore{bundle: bundle}
	storage := rangePageStoreFunc(func(ctx context.Context, repo, identity domain.ContentHash, read func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
		return fixture.WithMemoryReadRange(ctx, repo, identity, func(bound context.Context, header domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
			header.Source.Graph.RepoID = rangePageHash("foreign-repo")
			return read(bound, header, chunks)
		})
	})
	page, err := ReadPage(context.Background(), storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request)
	if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) || fixture.headers != 1 || len(fixture.reads) != 0 {
		test.Fatal("foreign callback header reached chunk reads", fixture.headers, fixture.reads, err)
	}
}

func TestReadPageUsesCallbackContext(test *testing.T) {
	bundle, request := rangePageFixture(test, 130, 32)
	request.Limit = 20
	request.Cursor = rangePageCursor(test, bundle.Header.View.StateHash, 60)
	for _, scenario := range []string{"active", "canceled-before-callback", "canceled-after-chunk", "parent-canceled-after-callback"} {
		test.Run(scenario, func(test *testing.T) {
			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			fixture := &rangePageStore{bundle: bundle}
			storage := rangePageStoreFunc(func(ctx context.Context, repo, identity domain.ContentHash, read func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
				return fixture.WithMemoryReadRange(ctx, repo, identity, func(ctx context.Context, header domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
					bound, cancel := context.WithCancel(ctx)
					defer cancel()
					if scenario == "canceled-before-callback" {
						cancel()
					}
					err := read(bound, header, rangePageChunkReader(func(actual context.Context, ordinal int) ([]domain.EffectiveMemoryItem, error) {
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
			page, err := ReadPage(parent, storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request)
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

func TestReadPageConsumesOwnedHeaderAndItems(test *testing.T) {
	ctx := context.Background()
	bundle, request := rangePageFixture(test, 1, 32)
	header := bundle.Header
	header.View.Inclusion = &domain.BranchContext{
		Roots: []domain.ContentHash{request.Selection.SnapshotID}, SnapshotIDs: []domain.ContentHash{request.Selection.SnapshotID},
	}
	items := []domain.EffectiveMemoryItem{{
		ID: rangePageHash("owned item"), SourceSnapshot: request.Selection.SnapshotID, Kind: "code", Text: "owned code scope",
		Code:                  &domain.MemoryCodeScope{Commit: strings.Repeat("a", 40), Paths: []string{"file"}},
		MemoryClaimAssessment: domain.MemoryClaimAssessment{State: "review", Reason: "scope_evidence_pending"},
	}}
	bundle = rangePageBuild(test, header, items)
	want := bundle.Header.View
	want.Items = items
	fixture := &rangePageStore{bundle: bundle}
	var callbackHeader domain.MemoryReadHeader
	var decoded []domain.EffectiveMemoryItem
	storage := rangePageStoreFunc(func(ctx context.Context, repo, identity domain.ContentHash, read func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
		return fixture.WithMemoryReadRange(ctx, repo, identity, func(bound context.Context, header domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
			callbackHeader = header
			return read(bound, header, rangePageChunkReader(func(ctx context.Context, ordinal int) ([]domain.EffectiveMemoryItem, error) {
				items, err := chunks.ReadChunk(ctx, ordinal)
				decoded = items
				return items, err
			}))
		})
	})
	page, err := ReadPage(ctx, storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request)
	if err != nil || len(page.Items) != 1 || len(decoded) != 1 || page.Items[0].Code == nil || page.Inclusion == nil || len(page.Inclusion.Roots) == 0 || len(page.Inclusion.SnapshotIDs) == 0 {
		test.Fatal("fixture did not produce owned nested header and item fields", page, err)
	}
	if page.Inclusion != callbackHeader.View.Inclusion || page.Items[0].Code != decoded[0].Code {
		test.Fatal("query re-decoded the owned callback header or items")
	}
	if page.Inclusion == bundle.Header.View.Inclusion {
		test.Fatal("range adapter did not own its header")
	}
	if !reflect.DeepEqual(page, want) {
		test.Fatal("scoped page changed the selected result")
	}
	page.Selection.SnapshotID = rangePageHash("changed-selector")
	page.Inclusion.Roots[0] = rangePageHash("changed-root")
	page.Inclusion.SnapshotIDs[0] = rangePageHash("changed-snapshot")
	page.Items[0].Code.Paths[0] = "changed-path"
	again, err := ReadPage(ctx, storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request)
	if err != nil || !reflect.DeepEqual(again, want) || fixture.headers != 2 || !slices.Equal(fixture.reads, []int{0, 0}) {
		test.Fatal("returned selection aliased persisted state", fixture.headers, fixture.reads, err)
	}
}

func TestReadPageNormalizedLimitsAndEmptyPages(test *testing.T) {
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
			bundle, request := rangePageFixture(test, scenario.count, 32)
			storage := &rangePageStore{bundle: bundle}
			request.Limit, request.Content = scenario.limit, "all"
			if scenario.end {
				request.Cursor = rangePageCursor(test, bundle.Header.View.StateHash, bundle.Header.View.Total)
			}
			page, err := ReadPage(context.Background(), storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request)
			if err != nil || page.Items == nil || len(page.Items) != scenario.want || page.Total != scenario.count || page.Content != "" || storage.headers != 1 {
				test.Fatal("normalization or page metadata changed", page, storage.headers, err)
			}
			if scenario.want == 0 {
				if len(storage.reads) != 0 || page.NextCursor != "" {
					test.Fatal("empty page read chunks or returned a continuation", storage.reads, page.NextCursor)
				}
			} else if !slices.Equal(storage.reads, []int{0}) || page.NextCursor != rangePageCursor(test, page.StateHash, scenario.want) {
				test.Fatal("page changed chunk cost or v1 cursor identity", storage.reads, page.NextCursor)
			}
		})
	}
}

func rangePageSizedItem(test *testing.T, snapshot domain.ContentHash, ordinal, size int) domain.EffectiveMemoryItem {
	test.Helper()
	item := domain.EffectiveMemoryItem{ID: rangePageHash(fmt.Sprint(ordinal)), SourceSnapshot: snapshot, Kind: "legacy_fact", MemoryClaimAssessment: domain.MemoryClaimAssessment{State: "review", Reason: "unverified"}}
	raw, err := json.Marshal(item)
	if err != nil || size < len(raw) {
		test.Fatal("invalid serialized item size", size, len(raw), err)
	}
	item.Text = strings.Repeat("x", size-len(raw))
	return item
}

func TestReadPageLargeItemBoundaries(test *testing.T) {
	for _, scenario := range []struct {
		name  string
		sizes []int
		want  int
		reads []int
	}{
		{name: "exact-item", sizes: []int{MaxPageBytes, 1024}, want: 1, reads: []int{0}},
		{name: "exact-page", sizes: []int{MaxPageBytes / 2, MaxPageBytes / 2, 1024}, want: 2, reads: []int{0, 1}},
		{name: "next-item-exceeds-page", sizes: []int{150 << 10, 150 << 10, 1024}, want: 1, reads: []int{0, 1}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			ctx := context.Background()
			original, request := rangePageFixture(test, 0, 0)
			header := original.Header
			items := make([]domain.EffectiveMemoryItem, len(scenario.sizes))
			for ordinal, size := range scenario.sizes {
				items[ordinal] = rangePageSizedItem(test, request.Selection.SnapshotID, ordinal, size)
			}
			header.View.Total = len(items)
			var err error
			header.View.DeliveryStateHash, err = domain.EffectiveMemoryDeliveryStateHash(ctx, header.Source.Graph.RepoID, header.Origin, header.View, items)
			if err != nil {
				test.Fatal(err)
			}
			bundle, err := domain.BuildMemoryReadBundle(ctx, header, items)
			if err != nil {
				test.Fatal(err)
			}
			storage := &rangePageStore{bundle: bundle}
			request.Limit = 50
			page, err := ReadPage(ctx, storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request)
			if err != nil || !reflect.DeepEqual(page.Items, items[:scenario.want]) || !slices.Equal(storage.reads, scenario.reads) || page.NextCursor != rangePageCursor(test, header.View.StateHash, scenario.want) {
				test.Fatalf("large item boundary changed: items=%d chunks=%v cursor=%s err=%v", len(page.Items), storage.reads, page.NextCursor, err)
			}
			request.Cursor, storage.reads = page.NextCursor, nil
			page, err = ReadPage(ctx, storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request)
			if err != nil || !reflect.DeepEqual(page.Items, items[scenario.want:]) || page.NextCursor != "" {
				test.Fatal("large item continuation skipped or duplicated items", err)
			}
		})
	}
}

func TestReadPageOversizedDecodedItemDiscardsPage(test *testing.T) {
	bundle, request := rangePageFixture(test, 2, 32)
	request.Limit = 50
	for _, ordinal := range []int{0, 1} {
		test.Run(fmt.Sprint(ordinal), func(test *testing.T) {
			fixture := &rangePageStore{bundle: bundle}
			storage := rangePageStoreFunc(func(ctx context.Context, repo, identity domain.ContentHash, read func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
				return fixture.WithMemoryReadRange(ctx, repo, identity, func(bound context.Context, header domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
					return read(bound, header, rangePageChunkReader(func(ctx context.Context, chunk int) ([]domain.EffectiveMemoryItem, error) {
						items, err := chunks.ReadChunk(ctx, chunk)
						if err != nil {
							return nil, err
						}
						items[ordinal].Text = strings.Repeat("<", MaxPageBytes/6)
						return items, nil
					}))
				})
			})
			page, err := ReadPage(context.Background(), storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request)
			if !errors.Is(err, domain.ErrMemoryProjectionLimit) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) || !slices.Equal(fixture.reads, []int{0}) {
				test.Fatal("oversized serialized item returned a partial page", fixture.reads, err)
			}
		})
	}
}

func TestReadPageRangeCostDoesNotScanWholeResult(test *testing.T) {
	ctx := context.Background()
	for _, count := range []int{256, 2560} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			bundle, request := rangePageFixture(test, count, 32)
			storage := &rangePageStore{bundle: bundle}
			request.Limit = 20
			request.Cursor = rangePageCursor(test, bundle.Header.View.StateHash, 60)
			got, err := ReadPage(ctx, storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request)
			if err != nil || len(got.Items) != 20 || got.Total != count || storage.headers != 1 || !slices.Equal(storage.reads, []int{0, 1}) {
				test.Fatalf("range scanned beyond two selected chunks: headers=%d chunks=%v count=%d err=%v", storage.headers, storage.reads, len(got.Items), err)
			}
			storage.bundle.Chunks[len(storage.bundle.Chunks)-1].Bytes[0] = '!'
			storage.reads = nil
			if _, err := ReadPage(ctx, storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request); err != nil || !slices.Equal(storage.reads, []int{0, 1}) {
				test.Fatal("page read unrelated tail bytes", storage.reads, err)
			}
			request.Cursor = rangePageCursor(test, bundle.Header.View.StateHash, count-1)
			if got, err := ReadPage(ctx, storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request); !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(got, domain.EffectiveMemoryPage{}) {
				test.Fatal("selected corrupted chunk was served", err)
			}
		})
	}
}

func TestReadPageValidationPrecedence(test *testing.T) {
	bundle, original := rangePageFixture(test, 1, 32)
	for _, scenario := range []struct {
		name           string
		invalidRequest bool
		canceled       bool
		unsupported    bool
		want           error
	}{
		{name: "request-before-cancellation", invalidRequest: true, canceled: true, unsupported: true, want: domain.ErrValidation},
		{name: "cancellation-before-storage", canceled: true, unsupported: true, want: context.Canceled},
		{name: "storage-before-identity", unsupported: true, want: domain.ErrCatalogUnsupported},
		{name: "identity-before-opening", want: domain.ErrValidation},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := original
			if scenario.invalidRequest {
				request.Limit = -1
			}
			if scenario.canceled {
				cancel()
			}
			calls := 0
			var storage outbound.MemoryReadRangeStore = rangePageStoreFunc(func(context.Context, domain.ContentHash, domain.ContentHash, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
				calls++
				return domain.ErrIntegrity
			})
			if scenario.unsupported {
				storage = nil
			}
			page, err := ReadPage(ctx, storage, bundle.Header.Source.Graph.RepoID, "invalid", request)
			if !errors.Is(err, scenario.want) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) || calls != 0 {
				test.Fatal("validation precedence changed or opened storage", page, calls, err)
			}
		})
	}
}

func TestReadPageOpeningFailurePrecedesCursorDecode(test *testing.T) {
	bundle, request := rangePageFixture(test, 1, 32)
	request.Cursor = "!"
	for _, failure := range []error{domain.ErrMemoryReadNotReady, domain.ErrNotFound, domain.ErrForbidden, context.DeadlineExceeded} {
		test.Run(failure.Error(), func(test *testing.T) {
			calls := 0
			storage := rangePageStoreFunc(func(context.Context, domain.ContentHash, domain.ContentHash, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
				calls++
				return fmt.Errorf("open range: %w", failure)
			})
			page, err := ReadPage(context.Background(), storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request)
			if !errors.Is(err, failure) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) || calls != 1 {
				test.Fatal("opening failure changed error or leaked a page", page, calls, err)
			}
		})
	}
}

func TestReadPageSelectedSecondChunkFailureDiscardsFirst(test *testing.T) {
	bundle, request := rangePageFixture(test, 130, 32)
	storage := &rangePageStore{bundle: bundle}
	storage.bundle.Chunks[1].Bytes[0] = '!'
	request.Limit = 20
	request.Cursor = rangePageCursor(test, bundle.Header.View.StateHash, 60)
	page, err := ReadPage(context.Background(), storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request)
	if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) || !slices.Equal(storage.reads, []int{0, 1}) {
		test.Fatal("selected chunk failure returned an earlier partial page", storage.reads, page, err)
	}
}

func TestReadPageHeaderAndStoreBindingBeforeChunks(test *testing.T) {
	for _, scenario := range []string{"foreign-repo", "foreign-identity", "header-hash", "item-limit"} {
		test.Run(scenario, func(test *testing.T) {
			bundle, request := rangePageFixture(test, 1, 32)
			storage := &rangePageStore{bundle: bundle}
			repo, identity := bundle.Header.Source.Graph.RepoID, bundle.ID
			want := domain.ErrNotFound
			switch scenario {
			case "foreign-repo":
				repo = rangePageHash("foreign repository")
			case "foreign-identity":
				identity = rangePageHash("foreign identity")
			case "header-hash":
				storage.bundle.Header.Origin += "/changed"
				want = domain.ErrIntegrity
			case "item-limit":
				storage.bundle.Header.View.Total = MaxItems + 1
				want = domain.ErrMemoryProjectionLimit
			}
			page, err := ReadPage(context.Background(), storage, repo, identity, request)
			if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) || storage.headers != 1 || len(storage.reads) != 0 {
				test.Fatal("unbound or oversized header reached payload reads", storage.headers, storage.reads, page, err)
			}
		})
	}
}

func TestReadPagePendingRevisionDoesNotStaleCursor(test *testing.T) {
	ctx := context.Background()
	bundle, request := rangePageFixture(test, 3, 32)
	storage := &rangePageStore{bundle: bundle}
	request.Limit = 1
	first, err := ReadPage(ctx, storage, bundle.Header.Source.Graph.RepoID, bundle.ID, request)
	if err != nil || len(first.Items) != 1 || first.NextCursor == "" {
		test.Fatal("fixture did not produce a continuation", first, err)
	}
	storage.bundle.Header.View.Revision.Pending++
	raw, err := domain.MemoryReadHeaderBytes(ctx, storage.bundle.Header)
	if err != nil {
		test.Fatal(err)
	}
	storage.bundle.ID = domain.HashContent(raw)
	request.Cursor, storage.reads = first.NextCursor, nil
	next, err := ReadPage(ctx, storage, bundle.Header.Source.Graph.RepoID, storage.bundle.ID, request)
	if err != nil || len(next.Items) != 1 || next.StateHash != first.StateHash || next.DeliveryStateHash != first.DeliveryStateHash ||
		next.Items[0].ID == first.Items[0].ID || next.Revision.Pending != first.Revision.Pending+1 || !slices.Equal(storage.reads, []int{0}) {
		test.Fatal("pending revision changed cursor or delivery binding", next, storage.reads, err)
	}
}
