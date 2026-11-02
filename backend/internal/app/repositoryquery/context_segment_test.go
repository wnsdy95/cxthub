package repositoryquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

var _ ContextIndexReader = (*documentquery.Queries)(nil)

type segmentOwnerIndexes struct {
	test      *testing.T
	ctx       context.Context
	repo      domain.ContentHash
	values    map[domain.ContentHash]domain.DocReadIndex
	failures  map[domain.ContentHash]error
	calls     []domain.ContentHash
	afterRead func(domain.ContentHash)
}

func (reader *segmentOwnerIndexes) ReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	reader.test.Helper()
	if ctx != reader.ctx || repo != reader.repo {
		reader.test.Fatal("segment index read replaced the current pinned scope or repository")
	}
	reader.calls = append(reader.calls, hash)
	if reader.afterRead != nil {
		reader.afterRead(hash)
	}
	if err := reader.failures[hash]; err != nil {
		return domain.DocReadIndex{}, err
	}
	index, found := reader.values[hash]
	if !found {
		return domain.DocReadIndex{}, domain.ErrNotFound
	}
	return index, nil
}

func segmentOwnerSnapshot(test *testing.T, repo domain.ContentHash, words ...string) (domain.Snapshot, domain.DocReadIndex) {
	test.Helper()
	document := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "segment-owner"}}}
	for position, word := range words {
		document.CIR.Events = append(document.CIR.Events, domain.CIREvent{Seq: position, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: word}}})
	}
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	document.Hash = domain.HashContent(canonical)
	index, err := domain.BuildDocReadIndex(document)
	if err != nil {
		test.Fatal(err)
	}
	return domain.Snapshot{ID: document.Hash, RepoID: repo, DocHash: document.Hash, CreatedAt: time.Unix(int64(len(words)), 0).UTC()}, index
}

func segmentOwnerHistory(repo, snapshot domain.ContentHash, number int) []domain.HistoryEvent {
	observation := domain.HistoryEvent{ID: fmt.Sprintf("%032x", number), RepoID: string(repo), BranchID: "identity", Branch: "main", LocalBranch: "native", WorktreeID: strings.Repeat("a", 32), Kind: "position", Source: snapshot, Target: snapshot, GitAfter: fmt.Sprintf("%040x", number), CreatedAt: time.Unix(int64(number), 0).UTC()}
	publication := observation
	publication.ID, publication.Kind = fmt.Sprintf("%032x", number+100), "publish"
	return []domain.HistoryEvent{observation, publication}
}

type segmentOwnerFixture struct {
	ctx     context.Context
	repo    domain.ContentHash
	view    domain.ContextQueryView
	full    domain.RepositoryView
	key     contextSegmentCacheKey
	reader  *segmentOwnerIndexes
	queries *ContextQueries
}

func segmentOwnerNew(test *testing.T) segmentOwnerFixture {
	test.Helper()
	repo := legacyProjectHash(test.Name())
	base, old := segmentOwnerSnapshot(test, repo, "first")
	head, current := segmentOwnerSnapshot(test, repo, "first", "new contribution")
	head.Parents = []domain.ContentHash{base.ID}
	history := append(segmentOwnerHistory(repo, base.ID, 1), segmentOwnerHistory(repo, head.ID, 2)...)
	view := domain.ContextQueryView{Version: 1, Branch: "main", Position: head.ID, Revision: domain.RepositoryRevision{Graph: 1}, Snapshots: []domain.Snapshot{head, base}, History: history, Semantics: domain.ContextSemantics{Version: 1}}
	view.Inclusion = &domain.BranchContext{BranchID: "identity", SnapshotID: head.ID, SnapshotIDs: []domain.ContentHash{head.ID, base.ID}, Roots: []domain.ContentHash{head.ID}, Merges: []domain.BranchContextMerge{}}
	ctx := context.Background()
	reader := &segmentOwnerIndexes{test: test, ctx: ctx, repo: repo, values: map[domain.ContentHash]domain.DocReadIndex{head.DocHash: current, base.DocHash: old}, failures: map[domain.ContentHash]error{}}
	key := segmentOwnerCacheKey(repo)
	key.code, key.revision = "", view.Revision
	return segmentOwnerFixture{ctx: ctx, repo: repo, view: view, full: domain.RepositoryView{Snapshots: view.Snapshots, History: history, Semantics: view.Semantics}, key: key, reader: reader, queries: &ContextQueries{indexes: reader, cache: &SegmentCache{}}}
}

func segmentOwnerBasis(fixture segmentOwnerFixture) contextSegmentBasis {
	selected := map[domain.ContentHash]domain.Snapshot{}
	for _, snapshot := range fixture.view.Snapshots {
		selected[snapshot.ID] = snapshot
	}
	view := fixture.view
	view.StateHash = legacyProjectHash("pinned segment basis")
	return contextSegmentBasis{view: view, selected: selected, bindings: domain.ContextPublicationBindings(fixture.repo, fixture.full.History)}
}

func TestContextSegmentOwnerPagesPreserveHashesAndUnpagedCacheBasis(test *testing.T) {
	fixture := segmentOwnerNew(test)
	selection := domain.ContextSelection{Branch: "main", Position: "HEAD", Scope: "current"}
	plain, err := fixture.queries.finishContextQuery(fixture.ctx, fixture.repo, selection, fixture.view, fixture.full, fixture.key)
	if err != nil || plain.Segments != nil || len(fixture.reader.calls) != 0 || fixture.queries.cache.Stats() != (SegmentCacheStats{}) {
		test.Fatal("ordinary query read index metadata or warmed a segment basis", err)
	}
	raw, err := json.Marshal(struct {
		Version   int
		Selection domain.ContextSelection
		Position  domain.ContentHash
		Snapshots []domain.Snapshot
		History   []domain.HistoryEvent
	}{fixture.view.Version, selection, fixture.view.Position, fixture.view.Snapshots, fixture.view.History})
	if err != nil || plain.StateHash != domain.HashContent(raw) {
		test.Fatal("ordinary context state hash changed", err)
	}
	selection.SegmentLimit = 1
	first, err := fixture.queries.finishContextQuery(fixture.ctx, fixture.repo, selection, fixture.view, fixture.full, fixture.key)
	if err != nil {
		test.Fatal(err)
	}
	head, base := fixture.view.Snapshots[0], fixture.view.Snapshots[1]
	bindings := domain.ContextPublicationBindings(fixture.repo, fixture.full.History)
	type receipt struct {
		SnapshotID domain.ContentHash
		Bindings   []domain.CommitContextBinding
	}
	raw, err = json.Marshal(struct {
		Version      int
		Selection    domain.ContextSelection
		Position     domain.ContentHash
		Inclusion    *domain.BranchContext
		Snapshots    []domain.Snapshot
		Publications []receipt
		Delivery     domain.ContentHash
	}{domain.ContextSegmentVersion, domain.ContextSelection{Branch: "main", Scope: "current"}, fixture.view.Position, fixture.view.Inclusion, fixture.view.Snapshots, []receipt{{head.ID, bindings[head.ID]}, {base.ID, bindings[base.ID]}}, plain.DeliveryStateHash})
	if err != nil || first.StateHash != domain.HashContent(raw) || first.DeliveryStateHash != plain.DeliveryStateHash {
		test.Fatal("segment state or full-selection delivery hash changed", err)
	}
	current, previous := fixture.reader.values[head.DocHash], fixture.reader.values[base.DocHash]
	coverage, err := domain.ProjectConversationSegment(fixture.ctx, head, current, bindings[head.ID], &base, &previous)
	if err != nil {
		test.Fatal(err)
	}
	wantPage := domain.ContextSegmentPage{Version: 1, StateHash: first.StateHash, Offset: 0, Next: 1, Total: 2, Entries: []domain.ContextSegmentCoverage{coverage}}
	raw, err = json.Marshal(wantPage)
	if err != nil {
		test.Fatal(err)
	}
	wantPage.PageHash = domain.HashContent(raw)
	if !reflect.DeepEqual(first.Segments, &wantPage) || !reflect.DeepEqual(fixture.reader.calls, []domain.ContentHash{head.DocHash, base.DocHash}) || first.Segments.Entries[0].Segment.EventStart != 1 {
		test.Fatalf("first-page bytes, verified range or source reads changed: %+v %v", first.Segments, fixture.reader.calls)
	}
	if len(first.Snapshots) != 1 || first.Snapshots[0].ID != head.ID || first.Inclusion != nil || first.History == nil || len(first.History) != 0 || !reflect.DeepEqual(first.Semantics, domain.ContextSemantics{}) {
		test.Fatal("paged delivery leaked full selection metadata")
	}
	basis, hit := fixture.queries.cache.get(fixture.key)
	resolved := fixture.key
	resolved.position, resolved.branch = string(fixture.view.Position), fixture.view.Branch
	_, resolvedHit := fixture.queries.cache.get(resolved)
	if !hit || !resolvedHit || basis.view.Segments != nil || len(basis.view.Snapshots) != 2 || len(basis.view.History) != 4 || len(basis.selected) != 2 || fixture.queries.cache.Stats() != (SegmentCacheStats{Entries: 2, Weight: 12}) {
		test.Fatal("requested/resolved aliases lost the same complete bounded basis", fixture.queries.cache.Stats())
	}
	selection.SegmentOffset, selection.SegmentStateHash = 1, first.StateHash
	last, err := fixture.queries.contextSegmentPage(fixture.ctx, fixture.repo, selection, basis)
	if err != nil || last.Segments == nil || !last.Segments.Complete || last.Segments.Next != -1 || last.Segments.Total != 2 || last.Segments.Entries[0].Kind != "full_source" || last.Segments.Entries[0].Segment.EventStart != 0 || last.DeliveryStateHash != first.DeliveryStateHash || last.StateHash != first.StateHash || len(last.Snapshots) != 1 || last.Snapshots[0].ID != base.ID {
		test.Fatal("cached continuation changed baseline, delivery identity or exhaustion", err)
	}
	lastPage := *last.Segments
	lastPage.PageHash = ""
	raw, err = json.Marshal(lastPage)
	if err != nil || last.Segments.PageHash != domain.HashContent(raw) || last.Segments.PageHash == first.Segments.PageHash {
		test.Fatal("continuation page hash omitted its own coordinates or coverage", err)
	}
	if len(basis.view.Snapshots) != 2 || len(basis.view.History) != 4 || basis.view.Inclusion == nil || basis.view.Segments != nil || len(basis.bindings[head.ID][0].SegmentIDs) != 0 {
		test.Fatal("page projection mutated the shared complete basis or publication proof")
	}
	selection.SegmentOffset = 2
	reads := len(fixture.reader.calls)
	empty, err := fixture.queries.contextSegmentPage(fixture.ctx, fixture.repo, selection, basis)
	if err != nil || empty.Segments == nil || !empty.Segments.Complete || empty.Segments.Entries == nil || len(empty.Segments.Entries) != 0 || empty.Segments.Next != -1 || len(fixture.reader.calls) != reads {
		test.Fatal("terminal empty page read payload or changed exhaustion semantics", err)
	}
}

func TestContextSegmentOwnerUsesFullSemanticProofAndSelectedDependencies(test *testing.T) {
	fixture := segmentOwnerNew(test)
	fixture.key.source = domain.RepositoryViewSourceCheckpoint{}
	mergeID, birthID := strings.Repeat("8", 32), strings.Repeat("9", 32)
	inclusion := *fixture.view.Inclusion
	inclusion.Merges = []domain.BranchContextMerge{{EventID: mergeID, State: "included"}}
	fixture.view.Inclusion = &inclusion
	merge := domain.HistoryEvent{ID: mergeID, RepoID: string(fixture.repo), BranchID: "identity", Branch: "main", Kind: "pr-merge", PRCompleted: true, SourceBranchID: "upstream-identity", Source: fixture.view.Position, Target: fixture.view.Position, PR: &domain.PullRequestMerge{Number: 42, BaseBranch: "main", HeadBranch: "upstream", HeadSHA: legacyProjectOID(2), MergeSHA: legacyProjectOID(3)}, CreatedAt: time.Unix(10, 0).UTC()}
	birth := domain.HistoryEvent{ID: birthID, RepoID: string(fixture.repo), BranchID: "upstream-identity", Branch: "upstream", Kind: "birth", CreatedAt: time.Unix(9, 0).UTC()}
	for _, event := range []domain.HistoryEvent{merge, birth} {
		if err := domain.ValidateHistoryEvent(event); err != nil {
			test.Fatal("semantic proof fixture must be accepted history", err)
		}
	}
	fixture.full.History = append(append([]domain.HistoryEvent{}, fixture.full.History...), merge, birth)
	fixture.full.Semantics = domain.ContextSemantics{Version: 1, Merges: []domain.ContextMergeEvidence{{EventID: mergeID, BirthID: birthID, Completed: true, PlacementIntact: true}}}
	selection := domain.ContextSelection{Branch: "main", Scope: "current", SegmentLimit: 1}
	first, err := fixture.queries.finishContextQuery(fixture.ctx, fixture.repo, selection, fixture.view, fixture.full, fixture.key)
	if err != nil {
		test.Fatal(err)
	}
	semantic := fixture.view
	semantic.History, semantic.Semantics = fixture.full.History, fixture.full.Semantics
	want, err := domain.ContextDeliveryStateHash(fixture.repo, fixture.key.origin, selection, semantic, domain.ContextPublicationBindings(fixture.repo, fixture.full.History))
	if err != nil || first.DeliveryStateHash != want {
		test.Fatal("delivery fingerprint used filtered history instead of the pinned full semantic proof", err)
	}
	unrelated, _ := segmentOwnerSnapshot(test, fixture.repo, "unrelated pending capture")
	full := fixture.full
	full.History = append(append([]domain.HistoryEvent{}, full.History...), segmentOwnerHistory(fixture.repo, unrelated.ID, 3)...)
	full.Snapshots = append(append([]domain.Snapshot{}, full.Snapshots...), unrelated)
	selection.SegmentOffset, selection.SegmentStateHash = 1, first.StateHash
	if next, err := fixture.queries.finishContextQuery(fixture.ctx, fixture.repo, selection, fixture.view, full, fixture.key); err != nil || next.StateHash != first.StateHash || next.DeliveryStateHash != first.DeliveryStateHash {
		test.Fatal("unrelated capture/publication invalidated selected dependencies", err)
	}
	for _, change := range []string{"origin", "publication", "semantic proof", "selected display metadata"} {
		test.Run(change, func(test *testing.T) {
			key, view, changed := fixture.key, fixture.view, fixture.full
			switch change {
			case "origin":
				key.origin = "https://github.com/example/new-origin"
			case "publication":
				publication := segmentOwnerHistory(fixture.repo, view.Position, 2)[1]
				publication.ID = strings.Repeat("f", 32)
				changed.History = append(append([]domain.HistoryEvent{}, changed.History...), publication)
			case "semantic proof":
				changed.Semantics.Merges = append([]domain.ContextMergeEvidence{}, changed.Semantics.Merges...)
				changed.Semantics.Merges[0].PlacementIntact = false
			case "selected display metadata":
				view.Snapshots = append([]domain.Snapshot{}, view.Snapshots...)
				view.Snapshots[0].Message = "new display label"
			}
			if _, err := fixture.queries.finishContextQuery(fixture.ctx, fixture.repo, selection, view, changed, key); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("changed selected dependency accepted a stale continuation", err)
			}
		})
	}
}

func TestContextSegmentOwnerNeverInfersBaselineFromGraftsOrMissingProof(test *testing.T) {
	for _, scenario := range []struct {
		name string
		kind string
	}{
		{"finalized natural parent", "verified_prefix"},
		{"no source publication", "full_source"},
		{"no baseline publication", "full_source"},
		{"baseline outside selection", "full_source"},
		{"graft only", "full_source"},
		{"ambiguous natural parents", "full_source"},
		{"self parent", "full_source"},
		{"baseline unavailable", "full_source"},
		{"different native stream", "full_source"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := segmentOwnerNew(test)
			basis := segmentOwnerBasis(fixture)
			head, base := basis.view.Snapshots[0], basis.view.Snapshots[1]
			wantReads := []domain.ContentHash{head.DocHash}
			switch scenario.name {
			case "no source publication":
				delete(basis.bindings, head.ID)
			case "no baseline publication":
				delete(basis.bindings, base.ID)
			case "baseline outside selection":
				delete(basis.selected, base.ID)
				basis.view.Snapshots = basis.view.Snapshots[:1]
			case "graft only":
				head.Parents, head.GraftParents = nil, []domain.ContentHash{base.ID}
			case "ambiguous natural parents":
				head.Parents = []domain.ContentHash{base.ID, legacyProjectHash("another parent")}
			case "self parent":
				head.Parents = []domain.ContentHash{head.ID}
			case "baseline unavailable":
				fixture.reader.failures[base.DocHash] = fmt.Errorf("baseline: %w", domain.ErrNotFound)
				wantReads = append(wantReads, base.DocHash)
			case "different native stream":
				previous := fixture.reader.values[base.DocHash]
				previous.Envelope.SessionOriginID = "other native stream"
				fixture.reader.values[base.DocHash] = previous
				wantReads = append(wantReads, base.DocHash)
			default:
				wantReads = append(wantReads, base.DocHash)
			}
			basis.view.Snapshots[0], basis.selected[head.ID] = head, head
			got, err := fixture.queries.contextSegmentPage(fixture.ctx, fixture.repo, domain.ContextSelection{SegmentLimit: 1}, basis)
			if err != nil || got.Segments == nil || len(got.Segments.Entries) != 1 {
				test.Fatal("baseline projection failed", err)
			}
			entry := got.Segments.Entries[0]
			if entry.Kind != scenario.kind || entry.Segment == nil || !reflect.DeepEqual(fixture.reader.calls, wantReads) {
				test.Fatalf("capture inferred a baseline or read outside its proof: %+v %v", entry, fixture.reader.calls)
			}
			if scenario.kind == "verified_prefix" {
				if entry.Segment.EventStart != 1 || entry.BaselineSnapshotID != base.ID || entry.BaselineDocHash != base.DocHash {
					test.Fatal("finalized natural prefix lost its exact baseline", entry)
				}
			} else if entry.Segment.EventStart != 0 || entry.BaselineSnapshotID != "" || entry.BaselineDocHash != "" {
				test.Fatal("unproven capture omitted source events", entry)
			}
		})
	}
}

func TestContextSegmentOwnerUnavailableErrorsAndCursorFences(test *testing.T) {
	for _, scenario := range []string{"missing source", "source integrity", "baseline integrity", "source overloaded", "baseline deadline", "foreign snapshot", "invalid index hash", "stale cursor", "offset exceeds selection", "canceled", "canceled during index"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := segmentOwnerNew(test)
			basis := segmentOwnerBasis(fixture)
			head, base := basis.view.Snapshots[0], basis.view.Snapshots[1]
			selection := domain.ContextSelection{SegmentLimit: 1}
			wantErr := error(domain.ErrIntegrity)
			switch scenario {
			case "missing source":
				fixture.reader.failures[head.DocHash] = fmt.Errorf("source: %w", domain.ErrNotFound)
			case "source integrity":
				fixture.reader.failures[head.DocHash] = wantErr
			case "baseline integrity":
				fixture.reader.failures[base.DocHash] = wantErr
			case "source overloaded":
				wantErr = domain.ErrResourceOverloaded
				fixture.reader.failures[head.DocHash] = wantErr
			case "baseline deadline":
				wantErr = context.DeadlineExceeded
				fixture.reader.failures[base.DocHash] = wantErr
			case "foreign snapshot":
				basis.view.Snapshots[0].RepoID = legacyProjectHash("foreign")
			case "invalid index hash":
				index := fixture.reader.values[head.DocHash]
				index.Hash = legacyProjectHash("wrong canonical source")
				fixture.reader.values[head.DocHash] = index
			case "stale cursor":
				wantErr, selection.SegmentStateHash = domain.ErrConflict, legacyProjectHash("stale")
			case "offset exceeds selection":
				wantErr, selection.SegmentOffset = domain.ErrValidation, 3
			case "canceled":
				ctx, cancel := context.WithCancel(fixture.ctx)
				cancel()
				fixture.ctx, fixture.reader.ctx, wantErr = ctx, ctx, context.Canceled
			case "canceled during index":
				ctx, cancel := context.WithCancel(fixture.ctx)
				defer cancel()
				fixture.ctx, fixture.reader.ctx, wantErr = ctx, ctx, context.Canceled
				fixture.reader.afterRead = func(domain.ContentHash) { cancel() }
			}
			got, err := fixture.queries.contextSegmentPage(fixture.ctx, fixture.repo, selection, basis)
			if scenario == "missing source" {
				want := domain.ContextSegmentCoverage{SnapshotID: head.ID, Kind: "unavailable", Reason: "source_unavailable", TotalEvents: -1, Bindings: basis.bindings[head.ID]}
				if err != nil || got.Segments == nil || !reflect.DeepEqual(got.Segments.Entries, []domain.ContextSegmentCoverage{want}) || !reflect.DeepEqual(fixture.reader.calls, []domain.ContentHash{head.DocHash}) {
					test.Fatal("unavailable source lost its publication receipts or invented ranges", err)
				}
				return
			}
			if !errors.Is(err, wantErr) || got.Segments != nil {
				test.Fatal("page hid a source/cursor error or published partial coverage", err)
			}
			if (scenario == "stale cursor" || scenario == "offset exceeds selection" || scenario == "foreign snapshot" || scenario == "canceled") && len(fixture.reader.calls) != 0 {
				test.Fatal("rejected page reached document indexes", fixture.reader.calls)
			}
		})
	}
}

func TestContextSegmentOwnerRetainsOnlyTwoPageIndexes(test *testing.T) {
	fixture := segmentOwnerNew(test)
	basis := segmentOwnerBasis(fixture)
	first := basis.view.Snapshots[0]
	second, secondIndex := segmentOwnerSnapshot(test, fixture.repo, "second independent source")
	third, thirdIndex := segmentOwnerSnapshot(test, fixture.repo, "third independent source")
	child, childIndex := segmentOwnerSnapshot(test, fixture.repo, "first", "new contribution", "later capture")
	child.Parents = []domain.ContentHash{first.ID}
	first.Parents = nil
	child.CreatedAt = time.Unix(-1, 0).UTC()
	basis.view.Snapshots = []domain.Snapshot{first, second, third, child}
	basis.selected = map[domain.ContentHash]domain.Snapshot{first.ID: first, second.ID: second, third.ID: third, child.ID: child}
	basis.bindings[child.ID] = domain.ContextPublicationBindings(fixture.repo, segmentOwnerHistory(fixture.repo, child.ID, 4))[child.ID]
	fixture.reader.values[second.DocHash], fixture.reader.values[third.DocHash] = secondIndex, thirdIndex
	fixture.reader.values[child.DocHash] = childIndex
	got, err := fixture.queries.contextSegmentPage(fixture.ctx, fixture.repo, domain.ContextSelection{SegmentLimit: 4}, basis)
	if err != nil || got.Segments == nil || len(got.Segments.Entries) != 4 || !reflect.DeepEqual(fixture.reader.calls, []domain.ContentHash{first.DocHash, second.DocHash, third.DocHash, child.DocHash, first.DocHash}) {
		test.Fatal("page retained an unbounded set of complete indexes or changed their order", err, fixture.reader.calls)
	}
	if got.Segments.Entries[3].Kind != "verified_prefix" || got.Segments.Entries[3].BaselineSnapshotID != first.ID || got.Segments.Entries[3].Segment.EventStart != 2 {
		test.Fatal("bounded eviction lost the selected natural baseline proof")
	}
}
