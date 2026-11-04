package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryReadFixtureStore struct {
	outbound.MemoryReadStore
	bundle  domain.MemoryReadBundle
	headers int
	reads   []int
	cancel  context.CancelFunc
}

type memoryReadFixtureChunkReader func(context.Context, int) ([]domain.EffectiveMemoryItem, error)

func (reader memoryReadFixtureChunkReader) ReadChunk(ctx context.Context, ordinal int) ([]domain.EffectiveMemoryItem, error) {
	return reader(ctx, ordinal)
}

func (storage *memoryReadFixtureStore) WithMemoryReadRange(ctx context.Context, repo, identity domain.ContentHash, read func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
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
	return read(ctx, owned, memoryReadFixtureChunkReader(func(bound context.Context, ordinal int) ([]domain.EffectiveMemoryItem, error) {
		raw, err := storage.ReadMemoryReadChunk(bound, repo, identity, ordinal)
		if err != nil {
			return nil, err
		}
		return verifier.Decode(bound, ordinal, raw)
	}))
}

func (storage *memoryReadFixtureStore) ReadMemoryReadHeader(ctx context.Context, repo, identity domain.ContentHash) (domain.MemoryReadHeader, error) {
	storage.headers++
	if err := ctx.Err(); err != nil {
		return domain.MemoryReadHeader{}, err
	}
	if repo != storage.bundle.Header.Source.Graph.RepoID || identity != storage.bundle.ID {
		return domain.MemoryReadHeader{}, domain.ErrNotFound
	}
	return storage.bundle.Header, nil
}

func (storage *memoryReadFixtureStore) ReadMemoryReadChunk(ctx context.Context, repo, identity domain.ContentHash, ordinal int) ([]byte, error) {
	storage.reads = append(storage.reads, ordinal)
	if repo != storage.bundle.Header.Source.Graph.RepoID || identity != storage.bundle.ID || ordinal < 0 || ordinal >= len(storage.bundle.Chunks) {
		return nil, domain.ErrNotFound
	}
	if storage.cancel != nil {
		storage.cancel()
	}
	return storage.bundle.Chunks[ordinal].Bytes, ctx.Err()
}

func memoryReadFixture(test *testing.T, count int, textBytes int) (*CapturedEffectiveMemory, domain.EffectiveMemoryRequest) {
	test.Helper()
	captured, request := memoryBundleFixture(test, false)
	digest := domain.MemoryDigest{SnapshotID: request.Selection.SnapshotID}
	for ordinal := range count {
		digest.KeyFacts = append(digest.KeyFacts, fmt.Sprintf("%06d %s", ordinal, strings.Repeat("x", textBytes)))
	}
	replaceMemoryReadDigest(test, captured, digest)
	return captured, request
}

func replaceMemoryReadDigest(test *testing.T, captured *CapturedEffectiveMemory, digest domain.MemoryDigest) {
	test.Helper()
	raw, err := json.Marshal(digest)
	if err != nil {
		test.Fatal(err)
	}
	identity := domain.HashContent(raw)
	captured.reader.objects = map[domain.ContentHash]json.RawMessage{identity: raw}
	for ordinal := range captured.reader.view.Snapshots {
		if captured.reader.view.Snapshots[ordinal].ID == digest.SnapshotID {
			captured.reader.view.Snapshots[ordinal].MemoryHash = identity
		}
	}
	for ordinal, entry := range captured.graph.entries {
		if entry.Kind == "snapshot" && entry.Key == string(digest.SnapshotID) {
			var snapshot domain.Snapshot
			if err := json.Unmarshal(entry.Value, &snapshot); err != nil {
				test.Fatal(err)
			}
			snapshot.MemoryHash = identity
			captured.graph.entries[ordinal].Value, err = json.Marshal(snapshot)
			if err != nil {
				test.Fatal(err)
			}
		}
	}
}

func buildMemoryReadFixture(test *testing.T, captured *CapturedEffectiveMemory) domain.MemoryReadBundle {
	test.Helper()
	input, err := captured.Export(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	bundle, err := BuildMemoryRead(context.Background(), input, memoryBundleFactory{})
	if err != nil {
		test.Fatal(err)
	}
	if err := domain.ValidateMemoryReadBundle(context.Background(), bundle); err != nil {
		test.Fatal(err)
	}
	return bundle
}

func TestMemoryReadCapturedPaginationParity(test *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"lineage", "branch", "stored"} {
		for _, content := range []string{"", "claims", "prompt"} {
			test.Run(mode+"/"+content, func(test *testing.T) {
				captured, request := memoryBundleFixture(test, mode == "branch")
				if mode == "stored" {
					request.Selection.MemoryHash = captured.Dependencies()[0].Hash
				}
				captured.content, request.Content, captured.selection = content, content, request.Selection
				bundle := buildMemoryReadFixture(test, captured)
				again := buildMemoryReadFixture(test, captured)
				if !reflect.DeepEqual(bundle, again) {
					test.Fatal("same input produced different persisted results")
				}
				storage := &memoryReadFixtureStore{bundle: bundle}
				limits := []int{1, 3, 0, 2, 50}
				for ordinal := 0; ; ordinal++ {
					request.Limit = limits[ordinal%len(limits)]
					want, err := captured.Project(ctx, request)
					if err != nil {
						test.Fatal(err)
					}
					got, err := ReadMemoryReadPage(ctx, storage, captured.reader.repo, bundle.ID, request)
					if err != nil || !reflect.DeepEqual(got, want) {
						test.Fatalf("page %d changed: %v\ngot %+v\nwant %+v", ordinal, err, got, want)
					}
					if got.NextCursor == "" {
						break
					}
					if ordinal > 100 || len(got.Items) == 0 {
						test.Fatal("page did not advance")
					}
					request.Cursor = got.NextCursor
				}
			})
		}
	}
}

func memoryReadCursor(test *testing.T, state domain.ContentHash, offset int) string {
	test.Helper()
	raw, err := json.Marshal(effectiveMemoryCursor{Version: 1, State: state, Index: offset})
	if err != nil {
		test.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestMemoryReadRangeCostDoesNotScanWholeResult(test *testing.T) {
	ctx := context.Background()
	for _, count := range []int{256, 2560} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			captured, request := memoryReadFixture(test, count, 32)
			bundle := buildMemoryReadFixture(test, captured)
			storage := &memoryReadFixtureStore{bundle: bundle}
			request.Limit = 20
			request.Cursor = memoryReadCursor(test, bundle.Header.View.StateHash, 60)
			got, err := ReadMemoryReadPage(ctx, storage, captured.reader.repo, bundle.ID, request)
			if err != nil || len(got.Items) != 20 || got.Total != count || storage.headers != 1 || !slices.Equal(storage.reads, []int{0, 1}) {
				test.Fatalf("range scanned beyond two selected chunks: headers=%d chunks=%v count=%d err=%v", storage.headers, storage.reads, len(got.Items), err)
			}
			storage.bundle.Chunks[len(storage.bundle.Chunks)-1].Bytes[0] = '!'
			storage.reads = nil
			if _, err := ReadMemoryReadPage(ctx, storage, captured.reader.repo, bundle.ID, request); err != nil || !slices.Equal(storage.reads, []int{0, 1}) {
				test.Fatal("page read unrelated tail bytes", storage.reads, err)
			}
			request.Cursor = memoryReadCursor(test, bundle.Header.View.StateHash, count-1)
			if got, err := ReadMemoryReadPage(ctx, storage, captured.reader.repo, bundle.ID, request); !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(got, domain.EffectiveMemoryPage{}) {
				test.Fatal("selected corrupted chunk was served", err)
			}
		})
	}
}

func TestMemoryReadRejectsMismatchedScopeCursorAndCancellation(test *testing.T) {
	ctx := context.Background()
	captured, original := memoryReadFixture(test, 130, 32)
	bundle := buildMemoryReadFixture(test, captured)
	for _, scenario := range []string{"repo", "identity", "content", "snapshot", "cursor", "stale", "past-end", "header", "cancel", "cancel-read"} {
		test.Run(scenario, func(test *testing.T) {
			storage := &memoryReadFixtureStore{bundle: bundle}
			request, repo, identity := original, captured.reader.repo, bundle.ID
			bound, cancel := context.WithCancel(ctx)
			defer cancel()
			want := domain.ErrValidation
			switch scenario {
			case "repo":
				repo, want = hh("foreign-repo"), domain.ErrNotFound
			case "identity":
				identity, want = hh("foreign-result"), domain.ErrNotFound
			case "content":
				request.Content = "claims"
			case "snapshot":
				request.Selection.SnapshotID = hh("foreign-snapshot")
			case "cursor":
				request.Cursor = "!"
			case "stale":
				request.Cursor = memoryReadCursor(test, hh("old-state"), 1)
				want = domain.ErrEffectiveMemoryCursorStale
			case "past-end":
				request.Cursor = memoryReadCursor(test, bundle.Header.View.StateHash, bundle.Header.View.Total+1)
			case "header":
				storage.bundle.Header.Origin += "/changed"
				want = domain.ErrIntegrity
			case "cancel":
				cancel()
				want = context.Canceled
			case "cancel-read":
				storage.cancel, want = cancel, context.Canceled
			}
			got, err := ReadMemoryReadPage(bound, storage, repo, identity, request)
			if !errors.Is(err, want) || !reflect.DeepEqual(got, domain.EffectiveMemoryPage{}) {
				test.Fatalf("partial or mismatched page: %v, wanted %v", err, want)
			}
			if scenario != "cancel-read" && len(storage.reads) != 0 {
				test.Fatal("invalid request read result bytes")
			}
		})
	}
}

func TestMemoryReadPageByteLimitAndOwnership(test *testing.T) {
	ctx := context.Background()
	captured, request := memoryReadFixture(test, 80, 12000)
	bundle := buildMemoryReadFixture(test, captured)
	storage := &memoryReadFixtureStore{bundle: bundle}
	request.Limit = 50
	count := 0
	for {
		got, err := ReadMemoryReadPage(ctx, storage, captured.reader.repo, bundle.ID, request)
		if err != nil || len(got.Items) == 0 {
			test.Fatal("byte-bounded page made no progress", err)
		}
		used := 0
		for _, item := range got.Items {
			raw, err := json.Marshal(item)
			if err != nil {
				test.Fatal(err)
			}
			used += len(raw)
		}
		if used > effectiveMemoryPageBytes || len(got.Items) >= 50 {
			test.Fatal("page ignored serialized byte limit")
		}
		count += len(got.Items)
		got.Items[0].Text = "changed by caller"
		again, err := ReadMemoryReadPage(ctx, storage, captured.reader.repo, bundle.ID, request)
		if err != nil || again.Items[0].Text == got.Items[0].Text {
			test.Fatal("caller mutated persisted result", err)
		}
		if got.NextCursor == "" {
			break
		}
		request.Cursor = got.NextCursor
	}
	if count != bundle.Header.View.Total || count != 160 {
		test.Fatal("byte pagination lost or duplicated items", count)
	}
	request.Cursor = memoryReadCursor(test, bundle.Header.View.StateHash, count)
	storage.reads = nil
	page, err := ReadMemoryReadPage(ctx, storage, captured.reader.repo, bundle.ID, request)
	if err != nil || len(page.Items) != 0 || page.NextCursor != "" || len(storage.reads) != 0 {
		test.Fatal("end cursor reread payload", err)
	}
}

type memoryReadLongEvidence struct {
	outbound.GraphEvidenceReader
	missing bool
}

func (reader memoryReadLongEvidence) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, commit string) (domain.GitCommitTree, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitCommitTree{}, err
	}
	if reader.missing {
		return domain.GitCommitTree{}, domain.ErrNotFound
	}
	ordinal, err := strconv.ParseInt(commit, 16, 64)
	if err != nil || ordinal < 1 {
		return domain.GitCommitTree{}, domain.ErrIntegrity
	}
	parents := []string{}
	if ordinal > 1 {
		parents = append(parents, effectiveOID(int(ordinal-1)))
	}
	return domain.GitCommitTree{Commit: commit, Tree: effectiveOID(99999), Parents: parents}, nil
}

func (reader memoryReadLongEvidence) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, commit, parent string) (domain.GitCommitDelta, error) {
	return domain.GitCommitDelta{Commit: commit, Parents: []string{}, Changes: []domain.GitPathChange{}, Complete: true}, ctx.Err()
}

type memoryReadLongFactory struct{ missing bool }

func (factory memoryReadLongFactory) BuildGraphSourceReader(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, metadata domain.Repo, entries []domain.RepositoryViewSourceEntry) (outbound.GraphEvidenceReader, error) {
	reader, err := (memoryBundleFactory{}).BuildGraphSourceReader(ctx, checkpoint, metadata, entries)
	return memoryReadLongEvidence{GraphEvidenceReader: reader, missing: factory.missing}, err
}

func TestMemoryReadRejectsTruncatedAssessmentButKeepsObservedAbsence(test *testing.T) {
	ctx := context.Background()
	captured, request := memoryReadFixture(test, 0, 0)
	digest := domain.MemoryDigest{SnapshotID: request.Selection.SnapshotID, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{
		SourceSnapshot: request.Selection.SnapshotID,
		Claims:         []domain.MemoryClaim{{Kind: "code", Text: "bounded evidence", Code: &domain.MemoryCodeScope{Commit: effectiveOID(1), Paths: []string{"file"}}}},
	}}}
	replaceMemoryReadDigest(test, captured, digest)
	captured.selection.CodeCommit = effectiveOID(5000)
	observation := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(captured.reader.repo), BranchID: "main", Branch: "main", Kind: "position", Target: request.Selection.SnapshotID, GitAfter: effectiveOID(1), CreatedAt: time.Unix(1, 0).UTC()}
	for _, event := range []domain.HistoryEvent{observation, prPublication(observation)} {
		raw, err := json.Marshal(event)
		if err != nil {
			test.Fatal(err)
		}
		captured.graph.entries = append(captured.graph.entries, domain.RepositoryViewSourceEntry{Kind: "history", Key: event.ID, Sequence: 1, Value: raw})
	}
	input, err := captured.Export(ctx)
	if err != nil {
		test.Fatal(err)
	}
	partial, err := BuildMemoryRead(ctx, input, memoryReadLongFactory{})
	if !errors.Is(err, domain.ErrMemoryProjectionLimit) || !reflect.DeepEqual(partial, domain.MemoryReadBundle{}) {
		test.Fatal("truncated ancestry was sealed as a complete assessment", err)
	}
	complete, err := BuildMemoryRead(ctx, input, memoryReadLongFactory{missing: true})
	if err != nil || complete.Header.View.Total != 1 {
		test.Fatal("observed missing evidence was treated as budget exhaustion", err)
	}
	items, err := domain.DecodeMemoryReadChunk(ctx, complete.Header, 0, complete.Chunks[0].Bytes)
	if err != nil || len(items) != 1 || items[0].State != "review" || items[0].Reason == "evidence_budget_exhausted" {
		test.Fatal("missing evidence did not remain an explicit review state", items, err)
	}
}

func TestMemoryReadStrictEvidenceReadBudget(test *testing.T) {
	ctx := context.Background()
	captured, _ := memoryBundleFixture(test, false)
	for _, strict := range []bool{false, true} {
		service := &Service{projectionEvidence: captured.evidence, requireCompleteMemory: strict}
		evidence, err := service.newCodeEvidence(ctx, captured.reader.repo)
		if err != nil {
			test.Fatal(err)
		}
		exhaustCodeEvidenceReads(test, evidence)
		_, err = evidence.GetGitCommitTree(ctx, evidence.repo, evidence.origin, effectiveOID(1))
		want := domain.ErrNotFound
		if strict {
			want = domain.ErrMemoryProjectionLimit
		}
		if !errors.Is(err, want) || !evidence.Limited() || evidence.TraversalLimited() {
			test.Fatal("read exhaustion did not reach the strict builder", strict, err)
		}
	}
}
