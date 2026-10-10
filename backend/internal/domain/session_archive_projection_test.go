package domain

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

func archiveProjectionSnapshot(name, session string, provider ProviderKind, created int64, parents ...ContentHash) Snapshot {
	id := HashContent([]byte(name))
	return Snapshot{ID: id, DocHash: id, RepoID: HashContent([]byte("archive-repository")), Provider: provider, SessionID: session, Parents: parents, CreatedAt: time.Unix(created, 0).UTC(), Branch: "topic", Message: name, Author: TeamIdentity{Name: name}}
}

func archiveProjectionRecord(snapshot Snapshot, archived int64) SessionArchive {
	return SessionArchive{RepoID: snapshot.RepoID, Key: SessionArchiveKey(snapshot), SnapshotID: snapshot.ID, Provider: snapshot.Provider, SessionID: snapshot.SessionID, ArchivedAt: time.Unix(archived, 0).UTC(), ArchivedBy: "maintainer"}
}

func archiveProjectionObservation(snapshot Snapshot, branch, identity, commit string, operation int) HistoryEvent {
	return HistoryEvent{ID: fmt.Sprintf("%032x", operation), RepoID: string(snapshot.RepoID), Kind: "position", Branch: branch, BranchID: identity, Source: snapshot.ID, Target: snapshot.ID, GitAfter: commit, WorktreeID: strings.Repeat("b", 32), CreatedAt: time.Unix(int64(operation), 0).UTC()}
}

func archiveProjectionPublication(observation HistoryEvent, operation int) HistoryEvent {
	publication := observation
	publication.ID = fmt.Sprintf("%032x", operation)
	publication.Kind = "publish"
	publication.Source = publication.Target
	return publication
}

func archiveProjectionBirth(parent Snapshot, branch, identity, main, commit string, operation int) HistoryEvent {
	return HistoryEvent{ID: fmt.Sprintf("%032x", operation), RepoID: string(parent.RepoID), Kind: "birth", Branch: branch, BranchID: identity, Source: parent.ID, Target: parent.ID, GitAfter: commit, CreatedAt: time.Unix(int64(operation), 0).UTC(), Creation: &GitCreation{Evidence: "process-argv", Command: []string{"git", "switch", "-c", branch, main}, StartRef: main, StartCommit: commit, OriginBranch: main, OriginBranchID: LegacyContextBranchID(string(parent.RepoID), main)}}
}

func TestProjectSessionArchivesGroupsCapturesAndPreservesAnchor(t *testing.T) {
	base := archiveProjectionSnapshot("base", "parent", ProviderClaude, 1)
	committed := archiveProjectionSnapshot("committed", "worker", ProviderCodex, 2, base.ID)
	pending := archiveProjectionSnapshot("hook: pending", "worker", ProviderCodex, 3, committed.ID)
	record := archiveProjectionRecord(committed, 10)
	views := ProjectSessionArchives([]SessionArchive{record}, []Snapshot{pending, base, committed}, nil, "main")
	if len(views) != 1 {
		t.Fatal(views)
	}
	view := views[0]
	if view.SessionArchive != record || view.LatestSnapshotID != pending.ID || view.Message != pending.Message || view.Branch != pending.Branch || view.Author != pending.Author || !view.UpdatedAt.Equal(pending.CreatedAt) {
		t.Fatalf("archive or latest metadata changed: %+v", view)
	}
	wantIDs := []ContentHash{committed.ID, pending.ID}
	if wantIDs[0] > wantIDs[1] {
		wantIDs[0], wantIDs[1] = wantIDs[1], wantIDs[0]
	}
	if !reflect.DeepEqual(view.SnapshotIDs, wantIDs) || view.Origin.ParentSnapshotID != base.ID || view.Origin.ParentSessionID != base.SessionID || view.Origin.ParentProvider != base.Provider {
		t.Fatalf("capture grouping or natural predecessor: %+v", view)
	}
	newer := archiveProjectionSnapshot("later capture", "worker", ProviderCodex, 4, pending.ID)
	newer.Branch = "renamed"
	updated := ProjectSessionArchives([]SessionArchive{record}, []Snapshot{base, committed, pending, newer}, nil, "main")[0]
	if updated.SessionArchive != record || updated.LatestSnapshotID != newer.ID || updated.Branch != newer.Branch || updated.Origin != view.Origin {
		t.Fatalf("latest capture rewrote archive origin: %+v", updated)
	}
}

func TestProjectSessionArchivesLatestPrefersTerminalCaptureOverAncestor(t *testing.T) {
	for _, ancestorTime := range []int64{100, 101} {
		t.Run(fmt.Sprintf("ancestor_time_%d", ancestorTime), func(t *testing.T) {
			first := archiveProjectionSnapshot("anchor", "worker", ProviderCodex, ancestorTime)
			first.Message = "First"
			latest := archiveProjectionSnapshot("Latest", "worker", ProviderCodex, 100, first.ID)
			latest.Branch = "renamed-topic"
			if first.ID >= latest.ID {
				t.Fatal("regression requires the ancestor hash to sort before its child")
			}
			record := archiveProjectionRecord(first, 200)
			for _, snapshots := range [][]Snapshot{{first, latest}, {latest, first}} {
				view := ProjectSessionArchives([]SessionArchive{record}, snapshots, nil, "main")[0]
				if view.LatestSnapshotID != latest.ID || view.Message != "Latest" || view.Branch != latest.Branch || view.Author != latest.Author || !view.UpdatedAt.Equal(latest.CreatedAt) {
					t.Fatalf("archive opens ancestor instead of terminal capture: %+v", view)
				}
				if view.SessionArchive != record || !reflect.DeepEqual(view.SnapshotIDs, []ContentHash{first.ID, latest.ID}) {
					t.Fatal("latest selection changed archive anchor or members", view)
				}
			}
		})
	}
}

func TestProjectSessionArchivesLatestUsesDeterministicTerminalOrdering(t *testing.T) {
	for _, scenario := range []string{"equal terminal times", "different terminal times", "cycle", "graft only"} {
		t.Run(scenario, func(t *testing.T) {
			base := archiveProjectionSnapshot("base", "worker", ProviderCodex, 300)
			first := archiveProjectionSnapshot("first terminal", "worker", ProviderCodex, 100, base.ID)
			second := archiveProjectionSnapshot("second terminal", "worker", ProviderCodex, 100, base.ID)
			want := first
			if second.ID < first.ID {
				want = second
			}
			snapshots := []Snapshot{base, first, second}
			switch scenario {
			case "different terminal times":
				second.CreatedAt = time.Unix(101, 0).UTC()
				snapshots[2], want = second, second
			case "cycle":
				first.Parents, second.Parents = []ContentHash{second.ID}, []ContentHash{first.ID}
				snapshots = []Snapshot{first, second}
			case "graft only":
				first.Parents, second.Parents = nil, nil
				if first.ID < second.ID {
					second.Grafted, second.GraftSeq, second.GraftParents = true, 1, []ContentHash{first.ID}
				} else {
					first.Grafted, first.GraftSeq, first.GraftParents = true, 1, []ContentHash{second.ID}
				}
				snapshots = []Snapshot{first, second}
			}
			record := archiveProjectionRecord(first, 400)
			random := rand.New(rand.NewSource(3))
			for iteration := 0; iteration < 10; iteration++ {
				random.Shuffle(len(snapshots), func(left, right int) { snapshots[left], snapshots[right] = snapshots[right], snapshots[left] })
				view := ProjectSessionArchives([]SessionArchive{record}, snapshots, nil, "main")[0]
				if view.LatestSnapshotID != want.ID || view.Message != want.Message || !view.UpdatedAt.Equal(want.CreatedAt) || view.SessionArchive != record {
					t.Fatal("unstable terminal ordering or cycle fallback", view)
				}
			}
		})
	}
}

func TestProjectSessionArchivesIsolatesProviderRepositoryAndLegacyIDs(t *testing.T) {
	codex := archiveProjectionSnapshot("codex", "same-id", ProviderCodex, 1)
	claude := archiveProjectionSnapshot("claude", "same-id", ProviderClaude, 2)
	legacy := archiveProjectionSnapshot("legacy", "", ProviderCodex, 3)
	otherLegacy := archiveProjectionSnapshot("other legacy", "", ProviderCodex, 4)
	foreign := codex
	foreign.RepoID, foreign.Message, foreign.CreatedAt = HashContent([]byte("other repo")), "foreign", time.Unix(100, 0)
	snapshots := []Snapshot{foreign, claude, codex, otherLegacy, legacy}
	records := []SessionArchive{archiveProjectionRecord(codex, 10), archiveProjectionRecord(claude, 10), archiveProjectionRecord(legacy, 10), archiveProjectionRecord(otherLegacy, 10), archiveProjectionRecord(foreign, 10)}
	views := ProjectSessionArchives(records, snapshots, nil, "main")
	if len(views) != len(records) {
		t.Fatal(views)
	}
	for _, view := range views {
		if len(view.SnapshotIDs) != 1 || view.LatestSnapshotID != view.SnapshotID {
			t.Fatalf("colliding identities merged: %+v", view)
		}
		if view.RepoID == codex.RepoID && view.Provider == ProviderCodex && view.SessionID == "same-id" && view.Message != "codex" {
			t.Fatal("foreign metadata used", view)
		}
	}
}

func TestProjectSessionArchivesRejectsAmbiguousParentEvidence(t *testing.T) {
	for _, scenario := range []string{"distinct roots", "root and inherited", "multiple parents", "missing parent", "graft only", "legacy destructive graft", "cycle"} {
		t.Run(scenario, func(t *testing.T) {
			base := archiveProjectionSnapshot("base", "parent", ProviderClaude, 1)
			other := archiveProjectionSnapshot("other", "other-parent", ProviderClaude, 1)
			first := archiveProjectionSnapshot("first", "worker", ProviderCodex, 2, base.ID)
			second := archiveProjectionSnapshot("second", "worker", ProviderCodex, 3, first.ID)
			switch scenario {
			case "distinct roots":
				second.Parents = []ContentHash{other.ID}
			case "root and inherited":
				second.Parents = nil
			case "multiple parents":
				first.Parents = append(first.Parents, other.ID)
			case "missing parent":
				first.Parents = []ContentHash{HashContent([]byte("missing"))}
			case "graft only":
				first.Parents, first.GraftParents, first.Grafted = nil, []ContentHash{base.ID}, true
			case "legacy destructive graft":
				first.Grafted = true
			case "cycle":
				first.Parents = []ContentHash{second.ID, base.ID}
			}
			view := ProjectSessionArchives([]SessionArchive{archiveProjectionRecord(second, 10)}, []Snapshot{base, other, first, second}, nil, "main")[0]
			if view.Origin != (SessionOrigin{MainBranch: "main"}) {
				t.Fatalf("invented origin: %+v", view.Origin)
			}
		})
	}
}

func TestProjectSessionArchivesRefusesUnprovenParentWithOverlay(t *testing.T) {
	base := archiveProjectionSnapshot("base", "parent", ProviderClaude, 1)
	other := archiveProjectionSnapshot("overlay", "other", ProviderCodex, 2)
	worker := archiveProjectionSnapshot("worker", "worker", ProviderCodex, 3, base.ID)
	worker.Grafted, worker.GraftParents = true, []ContentHash{other.ID}
	view := ProjectSessionArchives([]SessionArchive{archiveProjectionRecord(worker, 10)}, []Snapshot{base, other, worker}, nil, "main")[0]
	if view.Origin != (SessionOrigin{MainBranch: "main"}) {
		t.Fatal(view.Origin)
	}
}

func TestProjectSessionArchivesUsesRecordedMainBirthNotCurrentMain(t *testing.T) {
	base := archiveProjectionSnapshot("old main", "parent", ProviderClaude, 1)
	base.Branch = "main"
	worker := archiveProjectionSnapshot("worker", "worker", ProviderCodex, 2, base.ID)
	commit := strings.Repeat("a", 40)
	birth := archiveProjectionBirth(base, "topic", "topic-id", "main", commit, 1)
	observation := archiveProjectionObservation(worker, "topic", birth.BranchID, strings.Repeat("c", 40), 2)
	record := archiveProjectionRecord(worker, 10)
	history := []HistoryEvent{birth, observation}
	want := ProjectSessionArchives([]SessionArchive{record}, []Snapshot{base, worker}, history, "main")[0]
	if want.Origin.MainSnapshotID != base.ID || want.Origin.MainGitCommit != commit || want.Origin.MainBranch != "main" {
		t.Fatalf("explicit main origin lost: %+v", want.Origin)
	}
	advanced := archiveProjectionSnapshot("advanced main", "new-main-session", ProviderCodex, 100, base.ID)
	advanced.Branch = "main"
	later := archiveProjectionObservation(advanced, "main", "new-main-id", strings.Repeat("d", 40), 3)
	history = append(history, later, archiveProjectionPublication(later, 4))
	got := ProjectSessionArchives([]SessionArchive{record}, []Snapshot{advanced, worker, base}, history, "main")[0]
	if got.Origin != want.Origin {
		t.Fatalf("current main reassigned historical origin: got %+v want %+v", got.Origin, want.Origin)
	}
}

func TestProjectSessionArchivesRequiresQualifiedMainEvidence(t *testing.T) {
	for _, scenario := range []string{"no history", "label only", "shared git sha", "missing work observation", "wrong branch identity", "wrong source", "orphan", "unavailable creation", "short oid", "zero oid", "uppercase oid", "missing origin identity", "foreign repo", "conflicting history id", "competing main origins"} {
		t.Run(scenario, func(t *testing.T) {
			base := archiveProjectionSnapshot("base", "parent", ProviderClaude, 1)
			base.Branch = "main"
			worker := archiveProjectionSnapshot("worker [git abcdef0]", "worker", ProviderCodex, 2, base.ID)
			commit := strings.Repeat("a", 40)
			birth := archiveProjectionBirth(base, "topic", "topic-id", "main", commit, 1)
			observation := archiveProjectionObservation(worker, "topic", "topic-id", commit, 2)
			history := []HistoryEvent{birth, observation}
			switch scenario {
			case "no history":
				history = nil
			case "label only":
				worker.Branch = "main"
				history = nil
			case "shared git sha":
				unrelated := archiveProjectionSnapshot("unrelated main", "unrelated", ProviderClaude, 1)
				mainObservation := archiveProjectionObservation(unrelated, "main", "main-id", commit, 3)
				history = []HistoryEvent{observation, mainObservation, archiveProjectionPublication(mainObservation, 4)}
			case "missing work observation":
				history = history[:1]
			case "wrong branch identity":
				history[1].BranchID = "reused-topic"
			case "wrong source":
				history[0].Source, history[0].Target = worker.ID, worker.ID
			case "orphan":
				history[0].Kind, history[0].Source, history[0].Target, history[0].Creation = "orphan", "", "", nil
			case "unavailable creation":
				history[0].Creation = &GitCreation{Evidence: "unavailable", StartCommit: commit}
			case "short oid":
				history[0].GitAfter, history[0].Creation.StartCommit = "abcdef0", "abcdef0"
			case "zero oid":
				history[0].GitAfter, history[0].Creation.StartCommit = strings.Repeat("0", 40), strings.Repeat("0", 40)
			case "uppercase oid":
				history[0].GitAfter, history[0].Creation.StartCommit = strings.Repeat("A", 40), strings.Repeat("A", 40)
			case "missing origin identity":
				history[0].Creation.OriginBranchID = "unrecorded-main"
			case "foreign repo":
				history[0].RepoID = string(HashContent([]byte("foreign")))
			case "conflicting history id":
				conflict := observation
				conflict.GitAfter = strings.Repeat("e", 40)
				history = append(history, conflict)
			case "competing main origins":
				otherBirth := archiveProjectionBirth(base, "other-topic", "other-topic-id", "main", strings.Repeat("f", 40), 3)
				history = append(history, otherBirth, archiveProjectionObservation(worker, otherBirth.Branch, otherBirth.BranchID, commit, 4))
			}
			view := ProjectSessionArchives([]SessionArchive{archiveProjectionRecord(worker, 10)}, []Snapshot{base, worker}, history, "main")[0]
			if view.Origin.MainSnapshotID != "" || view.Origin.MainGitCommit != "" || view.Origin.MainBranch != "main" {
				t.Fatalf("unproven main context: %+v", view.Origin)
			}
		})
	}
}

func TestProjectSessionArchivesRequiresExactMainPublication(t *testing.T) {
	for _, scenario := range []string{"valid", "sha256 oid", "unfinalized", "missing proof", "different worktree", "different branch", "different target", "ambiguous commits", "unknown default"} {
		t.Run(scenario, func(t *testing.T) {
			worker := archiveProjectionSnapshot("worker", "worker", ProviderCodex, 1)
			commit, main := strings.Repeat("a", 40), "trunk"
			if scenario == "sha256 oid" {
				commit = strings.Repeat("a", 64)
			}
			observation := archiveProjectionObservation(worker, main, "trunk-id", commit, 1)
			publication := archiveProjectionPublication(observation, 2)
			history := []HistoryEvent{observation, publication}
			switch scenario {
			case "unfinalized":
				history = history[:1]
			case "missing proof":
				history = history[1:]
			case "different worktree":
				history[0].WorktreeID = strings.Repeat("c", 32)
			case "different branch":
				history[0].BranchID = "other-trunk-id"
			case "different target":
				history[0].Target = HashContent([]byte("other capture"))
			case "ambiguous commits":
				other := archiveProjectionObservation(worker, main, "trunk-id", strings.Repeat("e", 40), 3)
				history = append(history, other, archiveProjectionPublication(other, 4))
			case "unknown default":
				main = ""
			}
			view := ProjectSessionArchives([]SessionArchive{archiveProjectionRecord(worker, 10)}, []Snapshot{worker}, history, main)[0]
			if scenario == "valid" || scenario == "sha256 oid" {
				if view.Origin.MainSnapshotID != worker.ID || view.Origin.MainGitCommit != commit || view.Origin.MainBranch != main {
					t.Fatal("exact main publication lost", view.Origin)
				}
			} else if view.Origin != (SessionOrigin{MainBranch: main}) {
				t.Fatal("unproven main publication", view.Origin)
			}
		})
	}
}

func TestProjectSessionArchivesDeterministicOrderingAndMissingAnchor(t *testing.T) {
	first := archiveProjectionSnapshot("first", "same", ProviderCodex, 1)
	second := archiveProjectionSnapshot("second", "same", ProviderCodex, 1)
	third := archiveProjectionSnapshot("third", "other", ProviderClaude, 2)
	missing := archiveProjectionSnapshot("missing", "missing", ProviderCodex, 3)
	records := []SessionArchive{archiveProjectionRecord(first, 10), archiveProjectionRecord(third, 10), archiveProjectionRecord(missing, 20)}
	snapshots := []Snapshot{first, second, third}
	want := ProjectSessionArchives(records, snapshots, nil, "main")
	if want[0].SessionArchive != records[2] || want[0].LatestSnapshotID != "" || len(want[0].SnapshotIDs) != 0 || want[0].Origin != (SessionOrigin{MainBranch: "main"}) || want[1].Key > want[2].Key {
		t.Fatalf("missing archive or sorting: %+v", want)
	}
	latest := first.ID
	if second.ID < latest {
		latest = second.ID
	}
	for _, view := range want {
		if view.Key == records[0].Key && view.LatestSnapshotID != latest {
			t.Fatal("unstable equal-time latest", view)
		}
	}
	random := rand.New(rand.NewSource(1))
	for iteration := 0; iteration < 20; iteration++ {
		random.Shuffle(len(records), func(left, right int) { records[left], records[right] = records[right], records[left] })
		random.Shuffle(len(snapshots), func(left, right int) { snapshots[left], snapshots[right] = snapshots[right], snapshots[left] })
		if got := ProjectSessionArchives(records, snapshots, nil, "main"); !reflect.DeepEqual(got, want) {
			t.Fatal("input order changed projection", got)
		}
	}
	if views := ProjectSessionArchives(nil, snapshots, nil, "main"); views == nil || len(views) != 0 {
		t.Fatal("empty result must be an array", views)
	}
}

func TestProjectSessionArchivesMainEvidenceStaysAtArchivedCapture(t *testing.T) {
	anchor := archiveProjectionSnapshot("anchor", "worker", ProviderCodex, 1)
	latest := archiveProjectionSnapshot("latest", "worker", ProviderCodex, 2, anchor.ID)
	record := archiveProjectionRecord(anchor, 10)
	oldObservation := archiveProjectionObservation(anchor, "main", "main-id", strings.Repeat("a", 40), 1)
	newObservation := archiveProjectionObservation(latest, "main", "main-id", strings.Repeat("c", 40), 3)
	history := []HistoryEvent{oldObservation, archiveProjectionPublication(oldObservation, 2), newObservation, archiveProjectionPublication(newObservation, 4)}
	snapshots := []Snapshot{anchor, latest}
	before, err := json.Marshal([]any{record, snapshots, history})
	if err != nil {
		t.Fatal(err)
	}
	want := ProjectSessionArchives([]SessionArchive{record}, snapshots, history, "main")
	if want[0].Origin.MainSnapshotID != anchor.ID || want[0].Origin.MainGitCommit != oldObservation.GitAfter || want[0].LatestSnapshotID != latest.ID || want[0].SessionArchive != record {
		t.Fatal("newer capture replaced archival main evidence", want)
	}
	after, err := json.Marshal([]any{record, snapshots, history})
	if err != nil || string(before) != string(after) {
		t.Fatal("projection mutated inputs", err)
	}
	random := rand.New(rand.NewSource(2))
	for iteration := 0; iteration < 20; iteration++ {
		random.Shuffle(len(history), func(left, right int) { history[left], history[right] = history[right], history[left] })
		random.Shuffle(len(snapshots), func(left, right int) { snapshots[left], snapshots[right] = snapshots[right], snapshots[left] })
		if got := ProjectSessionArchives([]SessionArchive{record}, snapshots, history, "main"); !reflect.DeepEqual(got, want) {
			t.Fatal("history arrival order changed evidence", got)
		}
	}
	missing := ProjectSessionArchives([]SessionArchive{record}, []Snapshot{latest}, history, "main")[0]
	if missing.LatestSnapshotID != latest.ID || missing.Origin != (SessionOrigin{MainBranch: "main"}) || missing.SessionArchive != record {
		t.Fatal("missing archived anchor inferred from later capture", missing)
	}
}

func TestProjectSessionArchivesDuplicateSnapshotsFailClosed(t *testing.T) {
	anchor := archiveProjectionSnapshot("anchor", "worker", ProviderCodex, 1)
	record := archiveProjectionRecord(anchor, 10)
	duplicate := ProjectSessionArchives([]SessionArchive{record}, []Snapshot{anchor, anchor}, nil, "main")[0]
	if len(duplicate.SnapshotIDs) != 1 || duplicate.LatestSnapshotID != anchor.ID {
		t.Fatal("duplicate capture listed twice", duplicate)
	}
	conflict := anchor
	conflict.Parents = []ContentHash{HashContent([]byte("unproven parent"))}
	for _, snapshots := range [][]Snapshot{{anchor, conflict}, {conflict, anchor}} {
		view := ProjectSessionArchives([]SessionArchive{record}, snapshots, nil, "main")[0]
		if view.SessionArchive != record || len(view.SnapshotIDs) != 0 || view.LatestSnapshotID != "" || view.Origin != (SessionOrigin{MainBranch: "main"}) {
			t.Fatal("conflicting immutable capture chosen by arrival order", view)
		}
	}
}

func TestProjectSessionArchivesParentPublicationCannotEstablishChildMain(t *testing.T) {
	for _, observedAt := range []int{1, 20} {
		t.Run(fmt.Sprintf("publication_at_%d", observedAt), func(t *testing.T) {
			base := archiveProjectionSnapshot("parent on topic", "parent", ProviderClaude, 0)
			pending := archiveProjectionSnapshot("hook: pending", "worker", ProviderCodex, 2, base.ID)
			record := archiveProjectionRecord(pending, 10)
			snapshots := []Snapshot{base, pending}
			before := ProjectSessionArchives([]SessionArchive{record}, snapshots, nil, "main")[0]
			if before.Origin.ParentSnapshotID != base.ID || before.Origin.MainSnapshotID != "" || before.Origin.MainGitCommit != "" {
				t.Fatal("invalid initial child provenance", before.Origin)
			}
			observation := archiveProjectionObservation(base, "main", "main-id", strings.Repeat("a", 40), observedAt)
			publication := archiveProjectionPublication(observation, observedAt+1)
			publication.CreatedAt = time.Unix(int64(observedAt+1), 0).UTC()
			if ValidateHistoryEvent(observation) != nil || ValidateHistoryEvent(publication) != nil || !IsPublicationProof(publication, observation) {
				t.Fatal("fixture must have a valid parent publication")
			}
			history := []HistoryEvent{observation, publication}
			after := ProjectSessionArchives([]SessionArchive{record}, snapshots, history, "main")[0]
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("parent publication retroactively assigned child main: before %+v after %+v", before.Origin, after.Origin)
			}
			advanced := archiveProjectionSnapshot("advanced main", "new-main", ProviderClaude, 100, base.ID)
			advanced.Branch = "main"
			newObservation := archiveProjectionObservation(advanced, "main", "main-id", strings.Repeat("c", 40), 30)
			history = append(history, newObservation, archiveProjectionPublication(newObservation, 31))
			updated := ProjectSessionArchives([]SessionArchive{record}, append(snapshots, advanced), history, "main")[0]
			if !reflect.DeepEqual(updated, before) {
				t.Fatal("advanced main changed historical child evidence", updated.Origin)
			}
			newCapture := archiveProjectionSnapshot("future session capture", "worker", ProviderCodex, 200, advanced.ID)
			uncertain := ProjectSessionArchives([]SessionArchive{record}, []Snapshot{pending, newCapture, advanced, base}, history, "main")[0]
			if uncertain.LatestSnapshotID != newCapture.ID || uncertain.SessionArchive != record || uncertain.Origin != (SessionOrigin{MainBranch: "main"}) {
				t.Fatal("future session root silently reassigned archived origin", uncertain)
			}
		})
	}
}

func TestProjectSessionArchivesParentMainPublicationFailsClosed(t *testing.T) {
	for _, scenario := range []string{"missing observation", "missing publication", "other branch", "other identity", "other worktree", "other repository", "other target", "short oid", "multiple code positions"} {
		t.Run(scenario, func(t *testing.T) {
			base := archiveProjectionSnapshot("main [git abcdef0]", "parent", ProviderClaude, 1)
			base.Branch = "main"
			pending := archiveProjectionSnapshot("hook: pending", "worker", ProviderCodex, 2, base.ID)
			observation := archiveProjectionObservation(base, "main", "main-id", strings.Repeat("a", 40), 1)
			history := []HistoryEvent{observation, archiveProjectionPublication(observation, 2)}
			switch scenario {
			case "missing observation":
				history = history[1:]
			case "missing publication":
				history = history[:1]
			case "other branch":
				history[0].Branch, history[1].Branch = "topic", "topic"
			case "other identity":
				history[0].BranchID = "different-main"
			case "other worktree":
				history[0].WorktreeID = strings.Repeat("e", 32)
			case "other repository":
				history[0].RepoID, history[1].RepoID = string(HashContent([]byte("foreign"))), string(HashContent([]byte("foreign")))
			case "other target":
				unrelated := HashContent([]byte("unrelated capture"))
				history[0].Source, history[0].Target = unrelated, unrelated
				history[1] = archiveProjectionPublication(history[0], 2)
			case "short oid":
				history[0].GitAfter, history[1].GitAfter = "abcdef0", "abcdef0"
			case "multiple code positions":
				other := archiveProjectionObservation(base, "main", "main-id", strings.Repeat("c", 40), 3)
				history = append(history, other, archiveProjectionPublication(other, 4))
			}
			view := ProjectSessionArchives([]SessionArchive{archiveProjectionRecord(pending, 10)}, []Snapshot{base, pending}, history, "main")[0]
			if view.Origin.ParentSnapshotID != base.ID || view.Origin.MainSnapshotID != "" || view.Origin.MainGitCommit != "" || view.Origin.MainBranch != "main" {
				t.Fatal("unproven parent-main position", view.Origin)
			}
		})
	}
}

func TestProjectSessionArchivesExactChildMainPublicationIgnoresParentPublication(t *testing.T) {
	base := archiveProjectionSnapshot("base", "parent", ProviderClaude, 1)
	worker := archiveProjectionSnapshot("worker", "worker", ProviderCodex, 2, base.ID)
	oldObservation := archiveProjectionObservation(base, "main", "main-id", strings.Repeat("a", 40), 1)
	anchorObservation := archiveProjectionObservation(worker, "main", "main-id", strings.Repeat("c", 40), 3)
	history := []HistoryEvent{oldObservation, archiveProjectionPublication(oldObservation, 2), anchorObservation, archiveProjectionPublication(anchorObservation, 4)}
	view := ProjectSessionArchives([]SessionArchive{archiveProjectionRecord(worker, 10)}, []Snapshot{base, worker}, history, "main")[0]
	if view.Origin.MainSnapshotID != worker.ID || view.Origin.MainGitCommit != anchorObservation.GitAfter {
		t.Fatal("parent publication overrode exact capture proof", view.Origin)
	}
	worker.Grafted, worker.GraftSeq, worker.GraftParents = true, 1, []ContentHash{base.ID}
	touched := ProjectSessionArchives([]SessionArchive{archiveProjectionRecord(worker, 10)}, []Snapshot{base, worker}, history, "main")[0]
	if touched.Origin.ParentSnapshotID != "" || touched.Origin.MainSnapshotID != worker.ID || touched.Origin.MainGitCommit != anchorObservation.GitAfter {
		t.Fatal("exact child receipt must remain independent of unproven parents", touched.Origin)
	}
}

func TestProjectSessionArchivesTouchedGraftRegisterNeverProvesNaturalParent(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, location := range []string{"boundary", "anchor"} {
			t.Run(fmt.Sprintf("legacy_%t_%s", legacy, location), func(t *testing.T) {
				base := archiveProjectionSnapshot("base", "parent", ProviderClaude, 1)
				boundary := archiveProjectionSnapshot("boundary", "worker", ProviderCodex, 2, base.ID)
				anchor := archiveProjectionSnapshot("anchor", "worker", ProviderCodex, 3, boundary.ID)
				overlay := archiveProjectionSnapshot("overlay", "other", ProviderClaude, 4)
				record := archiveProjectionRecord(anchor, 10)
				birth := archiveProjectionBirth(base, "topic", "topic-id", "main", strings.Repeat("a", 40), 1)
				observation := archiveProjectionObservation(anchor, "topic", birth.BranchID, strings.Repeat("c", 40), 2)
				history := []HistoryEvent{birth, observation}
				stages := []struct {
					name    string
					grafted bool
					seq     uint64
					parents []ContentHash
				}{
					{"initial", legacy, 0, nil},
					{"overlay added", true, 1, []ContentHash{overlay.ID}},
					{"overlay replaced", true, 2, []ContentHash{base.ID}},
					{"overlay removed flag retained", true, 3, nil},
					{"overlay removed sequence only", false, 4, nil},
					{"overlay without flag or sequence", false, 0, []ContentHash{overlay.ID}},
					{"flag only", true, 0, nil},
				}
				for _, stage := range stages {
					t.Run(stage.name, func(t *testing.T) {
						first, last := boundary, anchor
						changed := &first
						if location == "anchor" {
							changed = &last
						}
						changed.Grafted, changed.GraftSeq, changed.GraftParents = stage.grafted, stage.seq, stage.parents
						view := ProjectSessionArchives([]SessionArchive{record}, []Snapshot{base, first, last, overlay}, history, "main")[0]
						if !legacy && stage.name == "initial" {
							if view.Origin.ParentSnapshotID != base.ID || view.Origin.MainSnapshotID != base.ID || view.Origin.MainGitCommit != birth.Creation.StartCommit {
								t.Fatal("untouched natural provenance lost", view.Origin)
							}
						} else if view.Origin != (SessionOrigin{MainBranch: "main"}) {
							t.Fatal("mutable graft register manufactured provenance", view.Origin)
						}
						if view.SessionArchive != record || view.LatestSnapshotID != anchor.ID || len(view.SnapshotIDs) != 2 {
							t.Fatal("graft state changed archive membership", view)
						}
					})
				}
			})
		}
	}
}

func TestProjectSessionArchivesLegacyAnchorSurvivesIdentityEnrichment(t *testing.T) {
	legacy := archiveProjectionSnapshot("legacy anchor", "", ProviderCodex, 1)
	record := archiveProjectionRecord(legacy, 10)
	base := archiveProjectionSnapshot("base", "parent", ProviderClaude, 0)
	enriched := legacy
	enriched.SessionID, enriched.Parents = "discovered-session", []ContentHash{base.ID}
	later := archiveProjectionSnapshot("new session capture", enriched.SessionID, enriched.Provider, 100, enriched.ID)
	observation := archiveProjectionObservation(base, "main", "main-id", strings.Repeat("a", 40), 1)
	history := []HistoryEvent{observation, archiveProjectionPublication(observation, 2)}
	view := ProjectSessionArchives([]SessionArchive{record}, []Snapshot{later, base, enriched}, history, "main")[0]
	if view.SessionArchive != record || view.LatestSnapshotID != enriched.ID || !reflect.DeepEqual(view.SnapshotIDs, []ContentHash{enriched.ID}) || view.Message != enriched.Message || view.Origin != (SessionOrigin{MainBranch: "main"}) {
		t.Fatalf("legacy archive disappeared or adopted a new session identity: %+v", view)
	}
}

func TestProjectSessionArchivesMainEvidenceRespectsArchiveTime(t *testing.T) {
	for _, scenario := range []string{"late publication", "late proof", "late conflicting publication", "late repeated proof", "archive boundary"} {
		t.Run(scenario, func(t *testing.T) {
			anchor := archiveProjectionSnapshot("anchor", "worker", ProviderCodex, 1)
			record := archiveProjectionRecord(anchor, 10)
			observation := archiveProjectionObservation(anchor, "main", "main-id", strings.Repeat("a", 40), 1)
			publication := archiveProjectionPublication(observation, 2)
			publication.CreatedAt = time.Unix(2, 0).UTC()
			history := []HistoryEvent{observation, publication}
			known := true
			switch scenario {
			case "late publication":
				history[1].CreatedAt = time.Unix(11, 0).UTC()
				known = false
			case "late proof":
				history[0].CreatedAt = time.Unix(11, 0).UTC()
				known = false
			case "late conflicting publication":
				later := archiveProjectionObservation(anchor, "main", "main-id", strings.Repeat("c", 40), 11)
				history = append(history, later, archiveProjectionPublication(later, 12))
			case "late repeated proof":
				later := archiveProjectionObservation(anchor, "main", "main-id", observation.GitAfter, 11)
				history = append(history, later)
			case "archive boundary":
				history[0].CreatedAt, history[1].CreatedAt = record.ArchivedAt, record.ArchivedAt
			}
			for _, event := range history {
				if err := ValidateHistoryEvent(event); err != nil {
					t.Fatal("invalid fixture", err)
				}
			}
			view := ProjectSessionArchives([]SessionArchive{record}, []Snapshot{anchor}, history, "main")[0]
			if known {
				if view.Origin.MainSnapshotID != anchor.ID || view.Origin.MainGitCommit != observation.GitAfter {
					t.Fatal("later events erased eligible historical proof", view.Origin)
				}
			} else if view.Origin != (SessionOrigin{MainBranch: "main"}) {
				t.Fatal("post-archive event invented historical main", view.Origin)
			}
		})
	}
}

func TestProjectSessionArchivesBirthEvidenceRespectsArchiveTime(t *testing.T) {
	for _, scenario := range []string{"birth", "work observation", "origin identity"} {
		t.Run(scenario, func(t *testing.T) {
			base := archiveProjectionSnapshot("base", "parent", ProviderClaude, 1)
			anchor := archiveProjectionSnapshot("anchor", "worker", ProviderCodex, 2, base.ID)
			record := archiveProjectionRecord(anchor, 10)
			birth := archiveProjectionBirth(base, "topic", "topic-id", "main", strings.Repeat("a", 40), 1)
			birth.Creation.OriginBranchID = "main-origin"
			observation := archiveProjectionObservation(anchor, "topic", birth.BranchID, strings.Repeat("c", 40), 2)
			origin := archiveProjectionObservation(base, "main", "main-origin", birth.Creation.StartCommit, 3)
			history := []HistoryEvent{birth, observation, origin}
			known := ProjectSessionArchives([]SessionArchive{record}, []Snapshot{base, anchor}, history, "main")[0]
			if known.Origin.MainSnapshotID != base.ID || known.Origin.MainGitCommit != birth.Creation.StartCommit {
				t.Fatal("fixture must prove the original birth", known.Origin)
			}
			switch scenario {
			case "birth":
				history[0].CreatedAt = time.Unix(20, 0).UTC()
			case "work observation":
				history[1].CreatedAt = time.Unix(20, 0).UTC()
			case "origin identity":
				history[2].CreatedAt = time.Unix(20, 0).UTC()
			}
			view := ProjectSessionArchives([]SessionArchive{record}, []Snapshot{base, anchor}, history, "main")[0]
			if view.Origin.ParentSnapshotID != base.ID || view.Origin.MainSnapshotID != "" || view.Origin.MainGitCommit != "" || view.Origin.MainBranch != "main" {
				t.Fatal("post-archive birth evidence invented main", view.Origin)
			}
		})
	}
}
