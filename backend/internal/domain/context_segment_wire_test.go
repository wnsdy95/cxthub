package domain

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// This fixture is also consumed by the independently compiled CLI module. It
// catches field/tag/hash drift that mocks built from the CLI's own DTOs conceal.
func TestContextSegmentWireFixture(t *testing.T) {
	repo := HashContent([]byte("segment-wire-repository"))
	source := func(words ...string) (Snapshot, DocReadIndex) {
		doc := SessionDoc{CIR: CIRDocument{Envelope: CIREnvelope{CIRVersion: "1", SourceProvider: ProviderCodex, SessionOriginID: "wire-session"}}}
		for i, word := range words {
			doc.CIR.Events = append(doc.CIR.Events, CIREvent{Seq: i, Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: word}}})
		}
		raw, err := CanonicalBytes(doc.CIR)
		if err != nil {
			t.Fatal(err)
		}
		doc.Hash = HashContent(raw)
		idx, err := BuildDocReadIndex(doc)
		if err != nil {
			t.Fatal(err)
		}
		return Snapshot{RepoID: repo, ID: doc.Hash, DocHash: doc.Hash}, idx
	}
	parent, pi := source("first turn")
	child, ci := source("first turn", "second turn")
	child.Parents = []ContentHash{parent.ID}
	baseBinding := CommitContextBinding{EventID: strings.Repeat("a", 32), GitCommit: strings.Repeat("b", 40), BranchID: "main-identity", WorktreeID: strings.Repeat("c", 32)}
	base, err := ProjectConversationSegment(context.Background(), parent, pi, []CommitContextBinding{baseBinding}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	nextBinding := baseBinding
	nextBinding.EventID = strings.Repeat("d", 32)
	nextBinding.GitCommit = strings.Repeat("e", 40)
	next, err := ProjectConversationSegment(context.Background(), child, ci, []CommitContextBinding{nextBinding}, &parent, &pi)
	if err != nil {
		t.Fatal(err)
	}
	missing := Snapshot{RepoID: repo, ID: HashContent([]byte("wire-unavailable")), DocHash: HashContent([]byte("wire-unavailable"))}
	page := ContextSegmentPage{Version: 1, StateHash: HashContent([]byte("wire-generation")), Offset: 0, Next: -1, Total: 3, Complete: true, Entries: []ContextSegmentCoverage{next, base, {SnapshotID: missing.ID, Kind: "unavailable", Reason: "source_unavailable", TotalEvents: -1, Bindings: []CommitContextBinding{}}}}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	page.PageHash = HashContent(raw)
	view := ContextQueryView{Version: 1, Branch: "main", Position: child.ID, StateHash: page.StateHash, Revision: RepositoryRevision{Evidence: 9007199254740993, Graph: 42, Pending: 7}, Snapshots: []Snapshot{child, parent, missing}, History: []HistoryEvent{}, Semantics: ContextSemantics{Version: 1, Merges: []ContextMergeEvidence{}}, Inclusion: &BranchContext{BranchID: "main-identity", SnapshotID: child.ID, CodeCommit: strings.Repeat("e", 40), Reason: "selected_code", Roots: []ContentHash{child.ID, missing.ID}, SnapshotIDs: []ContentHash{child.ID, parent.ID, missing.ID}, Merges: []BranchContextMerge{}}, Segments: &page}
	encoded, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	const path = "../../../schemas/testdata/context-segment-v1.json"
	if os.Getenv("CXT_UPDATE_CONTEXT_SEGMENT_FIXTURE") == "1" {
		if err := os.WriteFile(path, append(encoded, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
	fixture, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got ContextQueryView
	if err = json.Unmarshal(fixture, &got); err != nil || !reflect.DeepEqual(got, view) {
		t.Fatalf("server segment wire contract drifted: %v", err)
	}
}
