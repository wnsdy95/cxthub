package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestPendingStateRefOrderDoesNotDependOnDatabaseCollation(t *testing.T) {
	root := graphFixture("ref order")
	refs := []Ref{{Kind: RefBranch, Name: "topic/a", Target: root.ID}, {Kind: RefBranch, Name: "topic/한글", Target: root.ID}, {Kind: RefTag, Name: "tag", Target: root.ID}, {Kind: RefBranch, Name: "topic/B", Target: root.ID}}
	want := []Ref{refs[3], refs[0], refs[1], refs[2]}
	view, err := ProjectPendingState(context.Background(), RepositoryView{Snapshots: []Snapshot{root}, Refs: refs})
	if err != nil || !reflect.DeepEqual(view.Refs, want) {
		t.Fatal("noncanonical ref order", view.Refs, err)
	}
	if refs[0].Name != "topic/a" {
		t.Fatal("input ref order mutated")
	}
}

func TestPendingStateUsesFullGraphPublicationClassification(t *testing.T) {
	root := graphFixture("root")
	local := graphFixture("local", root.ID)
	grafted := graphFixture("grafted")
	grafted.GraftParents = []ContentHash{local.ID}
	live := graphFixture("live", grafted.ID)
	live.Message = "hook: live"
	ignored := graphFixture("ignored")
	ignored.Branch = "(stash)"
	for _, kind := range []string{"branch", "session", "tag", "reflog", "publish", "advance", "uncompleted-pr", "completed-pr"} {
		t.Run(kind, func(t *testing.T) {
			view := RepositoryView{Snapshots: []Snapshot{root, local, grafted, live, ignored}, Pending: []Pending{{SessionID: "live", Target: live.ID, Branch: "topic"}, {SessionID: "published", Target: root.ID, Branch: "main"}, {SessionID: "dismissed", Target: local.ID, Branch: "topic", Dismissed: true}}, Unsync: []Unsync{{User: "one", Branch: "topic", Target: local.ID}, {User: "two", Branch: "topic", Target: grafted.ID}}}
			switch kind {
			case "branch", "session", "tag":
				view.Refs = []Ref{{Kind: RefKind(kind), Name: "main", Target: root.ID}}
			case "reflog":
				view.Reflog = []RefLogEntry{{Kind: RefBranch, Old: root.ID}}
			default:
				event := HistoryEvent{ID: "history", Kind: kind, Branch: "main", BranchID: "main-id", Source: root.ID, Target: root.ID}
				if kind == "uncompleted-pr" || kind == "completed-pr" {
					event.Kind, event.PRCompleted = "pr-merge", kind == "completed-pr"
				}
				view.History = []HistoryEvent{event}
			}
			full, err := ProjectGraphState(view, "main", "")
			if err != nil {
				t.Fatal(err)
			}
			pending, err := ProjectPendingState(context.Background(), view)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(pending.Hold, full.Hold) || !reflect.DeepEqual(pending.OrphanSessions, full.OrphanSessions) || !reflect.DeepEqual(pending.HoldCounts, full.HoldCounts) {
				t.Fatalf("classification forked: pending=%+v full=%+v", pending, full)
			}
			published := kind != "tag" && kind != "uncompleted-pr"
			for _, session := range pending.OrphanSessions {
				if published && session == "published" || session == "dismissed" {
					t.Fatalf("publication/dismissal lost: %v", pending.OrphanSessions)
				}
			}
			if !published && len(pending.Hold) > 0 && len(pending.Hold[0].IDs) != 3 {
				t.Fatal("unpublished root must remain in held ancestry")
			}
			encoded, _ := json.Marshal(pending)
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(encoded, &fields)
			if _, exists := fields["graph"]; exists || pending.Projection != "on-hold-v1" {
				t.Fatal("partial classification masquerades as complete graph")
			}
		})
	}
}

func TestPendingStateMetadataOwnershipAndInvalidAncestry(t *testing.T) {
	parent := graphFixture("archive parent")
	capture := graphFixture("archive capture", parent.ID)
	capture.Branches = []string{"not-owned-by-patch"}
	unrelated := graphFixture("unrelated")
	view := RepositoryView{Snapshots: []Snapshot{parent, capture, unrelated}, ArchivedSessions: []SessionArchiveView{{SessionArchive: SessionArchive{SnapshotID: capture.ID}, LatestSnapshotID: capture.ID, SnapshotIDs: []ContentHash{capture.ID}, Origin: SessionOrigin{ParentSnapshotID: parent.ID}}}}
	result, err := ProjectPendingState(context.Background(), view)
	if err != nil || len(result.Snapshots) != 2 || len(result.Snapshots[1].Branches) != 0 || len(view.Snapshots[1].Branches) != 1 {
		t.Fatalf("archive metadata ownership: %+v %v", result, err)
	}
	view.Pending = []Pending{{Target: HashContent([]byte("missing"))}}
	if _, err := ProjectPendingState(context.Background(), view); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing pending target accepted: %v", err)
	}
	view.Pending = nil
	view.Snapshots[0].Parents = []ContentHash{capture.ID}
	if _, err := ProjectPendingState(context.Background(), view); err == nil {
		t.Fatal("cycle accepted by lightweight projection")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ProjectPendingState(canceled, RepositoryView{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
