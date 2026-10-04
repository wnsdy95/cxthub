package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func deliveryHash(s string) ContentHash { return HashContent([]byte(s)) }

func TestContextDeliveryStateScopeAndProvenance(t *testing.T) {
	repo, a, b := deliveryHash("repo"), deliveryHash("a"), deliveryHash("b")
	base := ContextQueryView{Version: 1, Branch: "main", Position: b,
		Snapshots: []Snapshot{{ID: b, RepoID: repo, Parents: []ContentHash{a}, DocHash: b, MemoryHash: deliveryHash("memory"), Branch: "main", Branches: []string{"main"}}, {ID: a, RepoID: repo, DocHash: a}},
		Inclusion: &BranchContext{BranchID: "main-id", SnapshotID: b, CodeCommit: strings.Repeat("1", 40), Reason: "selected_code", Roots: []ContentHash{a, b}, SnapshotIDs: []ContentHash{b, a}, Merges: []BranchContextMerge{{EventID: "merge", Source: a, State: "included", Reason: "verified_git_order"}}},
		Semantics: ContextSemantics{Version: 1, Merges: []ContextMergeEvidence{{EventID: "merge", BirthID: "birth", Completed: true, PlacementIntact: true, SourceAvailable: true, Lineage: "natural"}}},
		History:   []HistoryEvent{{ID: "merge", Kind: "pr-merge", Source: a, Target: b, PRCompleted: true}, {ID: "birth", Kind: "birth", BranchID: "source-id", Source: a}},
	}
	bindings := map[ContentHash][]CommitContextBinding{b: {{EventID: "publication", GitCommit: strings.Repeat("1", 40), BranchID: "main-id"}}}
	in := ContextSelection{Scope: "current", Branch: "main", Position: "HEAD"}
	want, err := ContextDeliveryStateHash(repo, "origin", in, base, bindings)
	if err != nil || ValidateContentHash(want) != nil {
		t.Fatal(want, err)
	}
	clone := func() ContextQueryView {
		raw, _ := json.Marshal(base)
		var v ContextQueryView
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tt := range []struct {
		name   string
		mutate func(*ContextQueryView)
		same   bool
	}{
		{"global revisions", func(v *ContextQueryView) {
			v.Revision = RepositoryRevision{Graph: 9, Evidence: 8}
			v.StateHash = deliveryHash("cursor")
			v.DeliveryStateHash = deliveryHash("old")
		}, true},
		{"display membership", func(v *ContextQueryView) { v.Snapshots[0].Branches = []string{"other", "main"} }, true},
		{"unrelated journal and semantics", func(v *ContextQueryView) {
			v.History = append(v.History, HistoryEvent{ID: "unrelated", Kind: "pr-merge", PRCompleted: true, Source: deliveryHash("elsewhere")})
			v.Semantics.Merges = append(v.Semantics.Merges, ContextMergeEvidence{EventID: "unrelated"})
		}, true},
		{"document", func(v *ContextQueryView) { v.Snapshots[1].DocHash = deliveryHash("changed") }, false},
		{"memory", func(v *ContextQueryView) { v.Snapshots[0].MemoryHash = deliveryHash("changed") }, false},
		{"ancestry", func(v *ContextQueryView) { v.Snapshots[0].Parents = nil }, false},
		{"overlay", func(v *ContextQueryView) { v.Snapshots[0].GraftParents = []ContentHash{a}; v.Snapshots[0].GraftSeq++ }, false},
		{"settings", func(v *ContextQueryView) { v.Snapshots[0].CodexSettings = deliveryHash("settings") }, false},
		{"original provenance", func(v *ContextQueryView) { v.Snapshots[0].Branch = "other" }, false},
		{"ordered snapshots", func(v *ContextQueryView) { v.Snapshots[0], v.Snapshots[1] = v.Snapshots[1], v.Snapshots[0] }, false},
		{"revert truth", func(v *ContextQueryView) { v.Inclusion.Merges[0].State = "not_selected" }, false},
		{"merge placement", func(v *ContextQueryView) { v.Semantics.Merges[0].PlacementIntact = false }, false},
		{"birth provenance", func(v *ContextQueryView) { v.History[1].BranchID = "different" }, false},
		{"code selection", func(v *ContextQueryView) { v.Inclusion.CodeCommit = strings.Repeat("2", 40) }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v := clone()
			tt.mutate(&v)
			got, err := ContextDeliveryStateHash(repo, "origin", in, v, bindings)
			if err != nil || (got == want) != tt.same {
				t.Fatalf("same=%v want %v: %v", got == want, tt.same, err)
			}
		})
	}
	alias := in
	alias.Position = string(b)
	alias.SegmentOffset = 5
	alias.SegmentLimit = 1
	alias.SegmentStateHash = deliveryHash("cursor")
	got, err := ContextDeliveryStateHash(repo, "origin", alias, base, bindings)
	if err != nil || got != want {
		t.Fatal("pagination or resolved alias changed proof", err)
	}
	bindings[deliveryHash("unselected")] = []CommitContextBinding{{EventID: "elsewhere"}}
	got, _ = ContextDeliveryStateHash(repo, "origin", in, base, bindings)
	if got != want {
		t.Fatal("unselected publication entered proof")
	}
	bindings[b] = append(bindings[b], CommitContextBinding{EventID: "second-publication"})
	got, _ = ContextDeliveryStateHash(repo, "origin", in, base, bindings)
	if got == want {
		t.Fatal("selected publication not bound")
	}
	if !reflect.DeepEqual(base.Snapshots[0].Branches, []string{"main"}) {
		t.Fatal("hash mutated response")
	}
	got, _ = ContextDeliveryStateHash(deliveryHash("different repo"), "origin", in, base, bindings)
	other, _ := ContextDeliveryStateHash(repo, "other origin", in, base, bindings)
	if got == want || got == other {
		t.Fatal("scope missing")
	}
}

func TestEffectiveMemoryDeliveryStateBindsCompleteAssessment(t *testing.T) {
	ctx := context.Background()
	repo := deliveryHash("repo")
	page := EffectiveMemoryPage{Content: "prompt", Selection: EffectiveMemorySelection{SnapshotID: deliveryHash("tip"), CodeCommit: strings.Repeat("1", 40)}, LineageHash: deliveryHash("lineage"), Total: 2}
	items := []EffectiveMemoryItem{{ID: deliveryHash("first"), Kind: "rationale", Text: "first"}, {ID: deliveryHash("last"), Kind: "code", Text: "outside page", MemoryClaimAssessment: MemoryClaimAssessment{State: "applied", Reason: "declared_scope_matches"}, PublicationIDs: []string{"p"}, IntegrationReceipt: "receipt", Paths: []CodePathState{{Path: "file", State: "applied"}}}}
	want, err := EffectiveMemoryDeliveryStateHash(ctx, repo, "origin", page, items)
	if err != nil || ValidateContentHash(want) != nil {
		t.Fatal(want, err)
	}
	page.Items = items[:1]
	page.NextCursor = "other"
	page.StateHash = deliveryHash("page-state")
	page.Revision = RepositoryRevision{Graph: 20, Evidence: 10}
	got, err := EffectiveMemoryDeliveryStateHash(ctx, repo, "origin", page, items)
	if err != nil || got != want {
		t.Fatal("page/revision affected proof", err)
	}
	for _, mutate := range []func(*EffectiveMemoryItem){
		func(i *EffectiveMemoryItem) { i.Text += " changed" },
		func(i *EffectiveMemoryItem) { i.State = "inactive" },
		func(i *EffectiveMemoryItem) { i.PublicationIDs = []string{"different"} },
		func(i *EffectiveMemoryItem) { i.IntegrationReceipt = "different" },
		func(i *EffectiveMemoryItem) { i.Paths = []CodePathState{{Path: "file", State: "before"}} },
	} {
		copyItems := append([]EffectiveMemoryItem{}, items...)
		mutate(&copyItems[1])
		got, err := EffectiveMemoryDeliveryStateHash(ctx, repo, "origin", page, copyItems)
		if err != nil || got == want {
			t.Fatal("outside-page change not bound", err)
		}
	}
	page.LineageHash = deliveryHash("omitted historical text changed")
	got, _ = EffectiveMemoryDeliveryStateHash(ctx, repo, "origin", page, items)
	if got == want {
		t.Fatal("complete lineage not bound")
	}
	if _, err := EffectiveMemoryDeliveryStateHash(ctx, repo, "origin", page, items[:1]); !errors.Is(err, ErrIntegrity) {
		t.Fatal("partial assessment accepted", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := EffectiveMemoryDeliveryStateHash(canceled, repo, "origin", page, items); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
