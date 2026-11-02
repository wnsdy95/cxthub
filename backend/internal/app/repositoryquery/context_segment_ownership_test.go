package repositoryquery

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestContextSegmentRetainsOnlySelectedPublicationBindings(test *testing.T) {
	fixture := segmentOwnerNew(test)
	selection := domain.ContextSelection{Branch: "main", Scope: "current", SegmentLimit: 1}
	uncachedKey := fixture.key
	uncachedKey.source = domain.RepositoryViewSourceCheckpoint{}
	want, err := fixture.queries.finishContextQuery(fixture.ctx, fixture.repo, selection, fixture.view, fixture.full, uncachedKey)
	if err != nil {
		test.Fatal(err)
	}
	unrelated, _ := segmentOwnerSnapshot(test, fixture.repo, "unselected publication")
	extraHistory := segmentOwnerHistory(fixture.repo, unrelated.ID, 3)
	fixture.full.History = append(fixture.full.History, extraHistory...)
	fixture.view.History = fixture.full.History
	fixture.full.Snapshots = append(fixture.full.Snapshots, unrelated)
	if len(domain.ContextPublicationBindings(fixture.repo, fixture.full.History)[unrelated.ID]) == 0 {
		test.Fatal("fixture lacks an unrelated accepted publication")
	}
	got, err := fixture.queries.finishContextQuery(fixture.ctx, fixture.repo, selection, fixture.view, fixture.full, fixture.key)
	if err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		test.Fatal("unselected publication changed page, delivery hash or segment hash")
	}
	resolved := fixture.key
	resolved.position, resolved.branch = string(fixture.view.Position), fixture.view.Branch
	allBindings := domain.ContextPublicationBindings(fixture.repo, fixture.full.History)
	for _, key := range []contextSegmentCacheKey{fixture.key, resolved} {
		basis, hit := fixture.queries.cache.get(key)
		if !hit {
			test.Fatal("selected basis was not retained for both request aliases")
		}
		if _, retained := basis.bindings[unrelated.ID]; retained || len(basis.bindings) != len(fixture.view.Snapshots) {
			test.Fatal("segment basis retained unselected publication records")
		}
		for _, snapshot := range fixture.view.Snapshots {
			if !reflect.DeepEqual(basis.bindings[snapshot.ID], allBindings[snapshot.ID]) {
				test.Fatal("selected publication receipts changed")
			}
		}
		if !reflect.DeepEqual(basis.view.History, fixture.view.History) || !reflect.DeepEqual(basis.view.Semantics, fixture.view.Semantics) || !reflect.DeepEqual(basis.view.Inclusion, fixture.view.Inclusion) || len(basis.selected) != len(fixture.view.Snapshots) {
			test.Fatal("binding restriction pruned the complete private basis")
		}
	}
}

func TestContextSegmentPageOwnsSnapshotsAndCoverageBindings(test *testing.T) {
	for _, unavailable := range []bool{false, true} {
		name := "projected"
		if unavailable {
			name = "unavailable"
		}
		test.Run(name, func(test *testing.T) {
			fixture := segmentOwnerNew(test)
			basis := segmentOwnerBasis(fixture)
			head := basis.view.Snapshots[0]
			head.Branches = []string{"main", "feature"}
			head.GraftParents = []domain.ContentHash{basis.view.Snapshots[1].ID}
			head.Models = []string{"model"}
			basis.view.Snapshots[0], basis.selected[head.ID] = head, head
			originalSegment := legacyProjectHash("original receipt segment")
			basis.bindings[head.ID][0].SegmentIDs = []domain.ContentHash{originalSegment}
			wantBinding := basis.bindings[head.ID][0]
			if unavailable {
				fixture.reader.failures[head.DocHash] = domain.ErrNotFound
			}
			selection := domain.ContextSelection{SegmentLimit: 1}
			got, err := fixture.queries.contextSegmentPage(fixture.ctx, fixture.repo, selection, basis)
			if err != nil {
				test.Fatal(err)
			}
			if len(got.Snapshots) != 1 || cap(got.Snapshots) != 1 || len(got.Segments.Entries) != 1 || len(got.Segments.Entries[0].Bindings) != 1 {
				test.Fatal("page retained a full-selection container or lost coverage")
			}
			before, err := json.Marshal(got)
			if err != nil {
				test.Fatal(err)
			}
			got.Snapshots[0].Branches[0] = "changed branch"
			got.Snapshots[0].Parents[0] = legacyProjectHash("changed parent")
			got.Snapshots[0].GraftParents[0] = legacyProjectHash("changed graft")
			got.Snapshots[0].Models[0] = "changed model"
			got.Snapshots[0] = domain.Snapshot{}
			got.Segments.Entries[0].Bindings[0].EventID = "changed receipt"
			got.Segments.Entries[0].Bindings[0].SegmentIDs[0] = legacyProjectHash("changed segment")
			if basis.bindings[head.ID][0].EventID != wantBinding.EventID || basis.bindings[head.ID][0].SegmentIDs[0] != originalSegment {
				test.Fatal("returned coverage mutated the basis publication receipt")
			}
			for _, retained := range []domain.Snapshot{basis.view.Snapshots[0], basis.selected[head.ID]} {
				if retained.ID != head.ID || retained.Branches[0] != "main" || retained.Parents[0] != basis.view.Snapshots[1].ID || retained.GraftParents[0] != basis.view.Snapshots[1].ID || retained.Models[0] != "model" {
					test.Fatal("returned snapshot mutated the retained containers or nested slices")
				}
			}
			next, err := fixture.queries.contextSegmentPage(fixture.ctx, fixture.repo, selection, basis)
			if err != nil {
				test.Fatal(err)
			}
			after, err := json.Marshal(next)
			if err != nil || string(before) != string(after) {
				test.Fatal("returned page mutation changed a later page or its hashes", err)
			}
		})
	}
}

func TestContextSegmentUnavailableBindingsPreserveNestedSliceShape(test *testing.T) {
	fixture := segmentOwnerNew(test)
	basis := segmentOwnerBasis(fixture)
	head := basis.view.Snapshots[0]
	receipt := basis.bindings[head.ID][0]
	basis.bindings[head.ID] = []domain.CommitContextBinding{receipt, receipt, receipt}
	basis.bindings[head.ID][0].SegmentIDs = nil
	basis.bindings[head.ID][1].SegmentIDs = []domain.ContentHash{}
	basis.bindings[head.ID][2].SegmentIDs = []domain.ContentHash{legacyProjectHash("retained segment")}
	fixture.reader.failures[head.DocHash] = domain.ErrNotFound
	got, err := fixture.queries.contextSegmentPage(fixture.ctx, fixture.repo, domain.ContextSelection{SegmentLimit: 1}, basis)
	if err != nil {
		test.Fatal(err)
	}
	bindings := got.Segments.Entries[0].Bindings
	if !reflect.DeepEqual(bindings, basis.bindings[head.ID]) || bindings[0].SegmentIDs != nil || bindings[1].SegmentIDs == nil {
		test.Fatal("unavailable coverage changed nil versus empty segment ID slices")
	}
	bindings[2].SegmentIDs[0] = legacyProjectHash("caller mutation")
	if bindings[2].SegmentIDs[0] == basis.bindings[head.ID][2].SegmentIDs[0] {
		test.Fatal("unavailable coverage retained a nested basis slice")
	}
	for _, empty := range [][]domain.CommitContextBinding{nil, {}} {
		basis.bindings[head.ID] = empty
		got, err = fixture.queries.contextSegmentPage(fixture.ctx, fixture.repo, domain.ContextSelection{SegmentLimit: 1}, basis)
		if err != nil {
			test.Fatal(err)
		}
		if bindings := got.Segments.Entries[0].Bindings; bindings == nil || len(bindings) != 0 {
			test.Fatal("unavailable coverage changed its existing empty-array wire shape")
		}
	}
}

func TestContextSegmentEmptyPagePreservesSnapshotSliceShape(test *testing.T) {
	for _, snapshots := range [][]domain.Snapshot{nil, {}} {
		fixture := segmentOwnerNew(test)
		basis := segmentOwnerBasis(fixture)
		basis.view.Snapshots = snapshots
		got, err := fixture.queries.contextSegmentPage(fixture.ctx, fixture.repo, domain.ContextSelection{SegmentLimit: 1}, basis)
		if err != nil {
			test.Fatal(err)
		}
		if !reflect.DeepEqual(got.Snapshots, snapshots) || cap(got.Snapshots) != 0 || len(fixture.reader.calls) != 0 {
			test.Fatal("empty page changed snapshot shape, retained capacity or read indexes")
		}
	}
}
