package domain

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestGitPublicationIndexPreservesOriginalLateCapture(t *testing.T) {
	checkpoint, _, publication, history := participationFixture()
	index, err := NewGitPublicationIndex(context.Background(), checkpoint.RepoID, history)
	if err != nil {
		t.Fatal(err)
	}
	got, err := index.Publication(publication.ID)
	if err != nil || got != publication || got.GitAfter == checkpoint.PR.HeadSHA {
		t.Fatalf("original source relabeled: %+v %v", got, err)
	}
	if _, err := index.Publication(history[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ordinary observation returned as publication: %v", err)
	}
	for position := range history {
		history[position].LocalBranch = "same-git-branch"
		history[position].WorktreeID = strings.Repeat("f", 32)
	}
	index, err = NewGitPublicationIndex(context.Background(), checkpoint.RepoID, history)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := index.Publication(publication.ID); err != nil || got.LocalBranch != "same-git-branch" {
		t.Fatalf("exact worktree observation was not retained: %+v %v", got, err)
	}
}

func TestGitPublicationIndexRequiresExactOrdinaryEvidence(t *testing.T) {
	for _, scenario := range []string{"missing", "branch-identity", "branch-name", "native-branch", "worktree", "git-head", "capture", "archive", "renamed"} {
		t.Run(scenario, func(t *testing.T) {
			checkpoint, _, publication, history := participationFixture()
			switch scenario {
			case "missing":
				history = history[1:]
			case "branch-identity":
				history[0].BranchID = "reused-branch"
			case "branch-name":
				history[0].Branch = "different"
			case "native-branch":
				history[0].LocalBranch = "different"
			case "worktree":
				history[0].WorktreeID = strings.Repeat("f", 32)
			case "git-head":
				history[0].GitAfter = strings.Repeat("d", 40)
			case "capture":
				history[0].Target = HashContent([]byte("another capture"))
			case "archive":
				history[0].Kind = "archive"
			case "renamed":
				history[0].Kind, history[0].PreviousBranch = "rename", "previous-name"
			}
			index, err := NewGitPublicationIndex(context.Background(), checkpoint.RepoID, history)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := index.Publication(publication.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("unproven publication admitted: %v", err)
			}
			if got := index.ObservationIDs(publication.ID); len(got) != 0 {
				t.Fatalf("inexact observation IDs retained: %v", got)
			}
		})
	}
}

func TestGitPublicationIndexObservationIDsPreserveSortedOrdinaryEvidence(t *testing.T) {
	checkpoint, _, publication, history := participationFixture()
	ordinary := history[0]
	history = []HistoryEvent{publication}
	for _, observation := range []struct {
		kind string
		id   string
	}{
		{kind: "birth", id: strings.Repeat("f", 32)},
		{kind: "attach", id: strings.Repeat("3", 32)},
		{kind: "position", id: strings.Repeat("c", 32)},
		{kind: "advance", id: strings.Repeat("9", 32)},
	} {
		event := ordinary
		event.Kind, event.ID = observation.kind, observation.id
		history = append(history, event)
	}
	unrelated := ordinary
	unrelated.ID, unrelated.GitAfter = strings.Repeat("a", 32), strings.Repeat("e", 40)
	history = append(history, unrelated)
	want := []string{strings.Repeat("3", 32), strings.Repeat("9", 32), strings.Repeat("c", 32), strings.Repeat("f", 32)}
	for _, ordering := range []string{"original", "reversed"} {
		t.Run(ordering, func(t *testing.T) {
			if ordering == "reversed" {
				slices.Reverse(history)
			}
			index, err := NewGitPublicationIndex(context.Background(), checkpoint.RepoID, history)
			if err != nil {
				t.Fatal(err)
			}
			if got := index.ObservationIDs(publication.ID); !slices.Equal(got, want) {
				t.Fatalf("ordinary observation IDs = %v, want exact sorted IDs %v", got, want)
			}
		})
	}
}

func TestGitPublicationIndexObservationIDsExcludeNonOrdinaryEvidence(t *testing.T) {
	for _, kind := range []string{"rename", "archive", "publish"} {
		t.Run(kind, func(t *testing.T) {
			checkpoint, _, publication, history := participationFixture()
			ordinary := history[0]
			excluded := ordinary
			excluded.ID, excluded.Kind = strings.Repeat("3", 32), kind
			if kind == "rename" {
				excluded.PreviousBranch = "previous-name"
			}
			index, err := NewGitPublicationIndex(context.Background(), checkpoint.RepoID, append(history, excluded))
			if err != nil {
				t.Fatal(err)
			}
			if got := index.ObservationIDs(publication.ID); !slices.Equal(got, []string{ordinary.ID}) {
				t.Fatalf("publication self or %s admitted as ordinary evidence: %v", kind, got)
			}
			index, err = NewGitPublicationIndex(context.Background(), checkpoint.RepoID, []HistoryEvent{publication, excluded})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{publication.ID, excluded.ID} {
				if got := index.ObservationIDs(id); len(got) != 0 {
					t.Fatalf("nonordinary history certified %s: %v", id, got)
				}
			}
		})
	}
}

func TestGitPublicationIndexObservationIDsReturnIndependentCopy(t *testing.T) {
	checkpoint, _, publication, history := participationFixture()
	additional := history[0]
	additional.ID, additional.Kind = strings.Repeat("3", 32), "position"
	index, err := NewGitPublicationIndex(context.Background(), checkpoint.RepoID, append(history, additional))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{history[0].ID, additional.ID}
	first, second := index.ObservationIDs(publication.ID), index.ObservationIDs(publication.ID)
	if !slices.Equal(first, want) || !slices.Equal(second, want) {
		t.Fatalf("incorrect original IDs: %v / %v", first, second)
	}
	first[0] = publication.ID
	if !slices.Equal(second, want) || !slices.Equal(index.ObservationIDs(publication.ID), want) {
		t.Fatal("returned slice mutation changed another copy or index")
	}
	second[1] = publication.ID
	if got := index.ObservationIDs(publication.ID); !slices.Equal(got, want) {
		t.Fatalf("second returned slice mutation changed index: %v", got)
	}
}

func TestGitPublicationIndexRejectsMixedOrCorruptHistory(t *testing.T) {
	for _, scenario := range []string{"foreign", "corrupt", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			checkpoint, _, _, history := participationFixture()
			switch scenario {
			case "foreign":
				history[0].RepoID = string(HashContent([]byte("another repository")))
			case "corrupt":
				history[0].ID = "invalid"
			case "duplicate":
				history = append(history, history[0])
			}
			if _, err := NewGitPublicationIndex(context.Background(), checkpoint.RepoID, history); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("invalid input accepted: %v", err)
			}
		})
	}
	checkpoint, _, _, history := participationFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewGitPublicationIndex(ctx, checkpoint.RepoID, history); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestGitPublicationIndexCarryPreservesOriginalAndSortedCopies(t *testing.T) {
	checkpoint, _, _, history := participationFixture()
	for position := range history {
		history[position].LocalBranch = "native-feature"
		history[position].WorktreeID = strings.Repeat("e", 32)
	}
	ordinary, publication := history[0], history[1]
	sourceHead := strings.Repeat("f", 40)
	for _, observation := range []struct {
		kind string
		id   string
	}{
		{kind: "birth", id: strings.Repeat("f", 32)},
		{kind: "attach", id: strings.Repeat("3", 32)},
		{kind: "position", id: strings.Repeat("c", 32)},
		{kind: "advance", id: strings.Repeat("9", 32)},
	} {
		carry := ordinary
		carry.Kind, carry.ID, carry.GitAfter = observation.kind, observation.id, sourceHead
		history = append(history, carry)
	}
	want := []string{strings.Repeat("3", 32), strings.Repeat("9", 32), strings.Repeat("c", 32), strings.Repeat("f", 32)}
	for _, ordering := range []string{"original", "reversed"} {
		t.Run(ordering, func(t *testing.T) {
			if ordering == "reversed" {
				slices.Reverse(history)
			}
			index, err := NewGitPublicationIndex(context.Background(), checkpoint.RepoID, history)
			if err != nil {
				t.Fatal(err)
			}
			first, err := index.CarryObservationIDs(publication.ID, sourceHead)
			if err != nil || !slices.Equal(first, want) {
				t.Fatalf("carry IDs = %v, want %v: %v", first, want, err)
			}
			second, err := index.CarryObservationIDs(publication.ID, sourceHead)
			if err != nil || !slices.Equal(second, want) {
				t.Fatalf("second carry lookup changed: %v %v", second, err)
			}
			first[0] = publication.ID
			if !slices.Equal(second, want) {
				t.Fatal("carry results share a mutable slice")
			}
			second[1] = publication.ID
			if got, err := index.CarryObservationIDs(publication.ID, sourceHead); err != nil || !slices.Equal(got, want) {
				t.Fatalf("returned carry mutation changed index: %v %v", got, err)
			}
			if got, err := index.Publication(publication.ID); err != nil || got != publication || got.GitAfter == sourceHead {
				t.Fatalf("carry lookup relabeled original publication: %+v %v", got, err)
			}
			if got := index.ObservationIDs(publication.ID); !slices.Equal(got, []string{ordinary.ID}) {
				t.Fatalf("carry replaced original capture evidence: %v", got)
			}
			original, err := index.CarryObservationIDs(publication.ID, publication.GitAfter)
			if err != nil || !slices.Equal(original, []string{ordinary.ID}) {
				t.Fatalf("exact original head did not preserve ordinary proof: %v %v", original, err)
			}
			original[0] = publication.ID
			if got := index.ObservationIDs(publication.ID); !slices.Equal(got, []string{ordinary.ID}) {
				t.Fatalf("carry return mutation changed original proof: %v", got)
			}
		})
	}
}

func TestGitPublicationIndexCarryRequiresExactOrdinaryEvidence(t *testing.T) {
	for _, scenario := range []string{"missing", "missing-capture", "foreign-capture", "identity", "name", "native", "worktree", "head", "rename", "archive", "publish", "foreign-repo"} {
		t.Run(scenario, func(t *testing.T) {
			checkpoint, _, publication, history := participationFixture()
			sourceHead := strings.Repeat("f", 40)
			carry := history[0]
			carry.ID, carry.Kind, carry.GitAfter = strings.Repeat("3", 32), "position", sourceHead
			switch scenario {
			case "missing-capture":
				carry.Target = ""
			case "foreign-capture":
				carry.Target = HashContent([]byte("unrelated capture"))
			case "identity":
				carry.BranchID = "another-incarnation"
			case "name":
				carry.Branch = "other"
			case "native":
				carry.LocalBranch = "other"
			case "worktree":
				carry.WorktreeID = strings.Repeat("e", 32)
			case "head":
				carry.GitAfter = strings.Repeat("e", 40)
			case "rename":
				carry.Kind, carry.PreviousBranch = "rename", "previous"
			case "archive", "publish":
				carry.Kind = scenario
			case "foreign-repo":
				carry.RepoID = string(HashContent([]byte("other repo")))
			}
			if scenario != "missing" {
				history = append(history, carry)
			}
			index, err := NewGitPublicationIndex(context.Background(), checkpoint.RepoID, history)
			if scenario == "foreign-repo" {
				if !errors.Is(err, ErrIntegrity) || index != nil {
					t.Fatalf("foreign repository carry admitted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, err := index.CarryObservationIDs(publication.ID, sourceHead); !errors.Is(err, ErrNotFound) || len(got) != 0 {
				t.Fatalf("inexact or nonordinary carry admitted: %v %v", got, err)
			}
			if got, err := index.Publication(publication.ID); err != nil || got != publication {
				t.Fatalf("missing carry changed original admission: %+v %v", got, err)
			}
		})
	}
}

func TestGitPublicationIndexCarryCannotAdmitUnprovenPublication(t *testing.T) {
	checkpoint, _, publication, history := participationFixture()
	carry := history[0]
	carry.ID, carry.GitAfter = strings.Repeat("3", 32), strings.Repeat("f", 40)
	for _, events := range [][]HistoryEvent{{publication}, {publication, carry}} {
		index, err := NewGitPublicationIndex(context.Background(), checkpoint.RepoID, events)
		if err != nil {
			t.Fatal(err)
		}
		for _, sourceHead := range []string{publication.GitAfter, carry.GitAfter} {
			if got, err := index.CarryObservationIDs(publication.ID, sourceHead); !errors.Is(err, ErrNotFound) || len(got) != 0 {
				t.Fatalf("carry or publication self admitted missing original proof: %v %v", got, err)
			}
		}
		if _, err := index.Publication(publication.ID); !errors.Is(err, ErrNotFound) || len(index.ObservationIDs(publication.ID)) != 0 {
			t.Fatalf("carry fabricated original publication evidence: %v", err)
		}
	}
}

func TestGitPublicationIndexCarryValidatesLookup(t *testing.T) {
	checkpoint, _, publication, history := participationFixture()
	index, err := NewGitPublicationIndex(context.Background(), checkpoint.RepoID, history)
	if err != nil {
		t.Fatal(err)
	}
	for _, sourceHead := range []string{"", "abc123", strings.Repeat("0", 40), strings.Repeat("0", 64), strings.Repeat("A", 40), strings.Repeat("g", 64)} {
		for _, candidate := range []*GitPublicationIndex{index, nil, {}} {
			if got, err := candidate.CarryObservationIDs(publication.ID, sourceHead); !errors.Is(err, ErrValidation) || len(got) != 0 {
				t.Fatalf("invalid source head accepted: %q %v %v", sourceHead, got, err)
			}
		}
	}
	for _, candidate := range []*GitPublicationIndex{nil, {}} {
		if got, err := candidate.CarryObservationIDs(publication.ID, publication.GitAfter); !errors.Is(err, ErrNotFound) || len(got) != 0 {
			t.Fatalf("empty index produced carry: %v %v", got, err)
		}
	}
	for _, id := range []string{history[0].ID, strings.Repeat("f", 32)} {
		if got, err := index.CarryObservationIDs(id, publication.GitAfter); !errors.Is(err, ErrNotFound) || len(got) != 0 {
			t.Fatalf("nonpublication lookup produced carry: %v %v", got, err)
		}
	}
	if got, err := index.CarryObservationIDs(publication.ID, strings.Repeat("f", 64)); !errors.Is(err, ErrNotFound) || len(got) != 0 {
		t.Fatalf("full SHA256 lookup did not report missing evidence: %v %v", got, err)
	}
}
