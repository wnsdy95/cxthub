package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTrackingRejectsReleasedLegacyLifecycle(t *testing.T) {
	repo := string(HashContent([]byte("legacy tracking")))
	id := LegacyContextBranchID(repo, "task")
	a := HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo, BranchID: id, Branch: "temp", PreviousBranch: "task", Kind: "rename", CreatedAt: time.Unix(1, 0)}
	b := a
	b.ID, b.Branch, b.PreviousBranch, b.BindingParent, b.NameParent = strings.Repeat("2", 32), "task", "temp", a.ID, a.ID
	r := Ref{RepoID: repo, Kind: RefBranch, Name: "task", BranchID: id, Target: HashContent([]byte("target"))}
	for _, kind := range []string{"archive", "rename"} {
		t.Run(kind, func(t *testing.T) {
			c := HistoryEvent{ID: strings.Repeat("3", 32), RepoID: repo, BranchID: id, Branch: "task", Kind: kind, BindingParent: b.ID, CreatedAt: time.Unix(2, 0)}
			if kind == "rename" {
				c.Branch, c.PreviousBranch = "elsewhere", "task"
			}
			remote := []HistoryEvent{a, b}
			if _, err := ProjectContextBranches(append(append([]HistoryEvent{}, remote...), c)); err != nil {
				t.Fatal(err)
			}
			if _, err := TrackingHistory(r, remote, []HistoryEvent{c}); !errors.Is(err, ErrSyncConflict) {
				t.Fatalf("released legacy identity accepted: %v", err)
			}
		})
	}
}

func TestTrackingProofDoesNotAdoptPRSourceFutureLifecycle(t *testing.T) {
	repo := string(HashContent([]byte("scoped PR proof")))
	target := HashContent([]byte("R context"))
	source := HashContent([]byte("X context"))
	code, head := strings.Repeat("1", 40), strings.Repeat("2", 40)
	r := HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo, BranchID: "R", Branch: "task", Kind: "birth", Target: target, GitAfter: code, MemoryPinned: true, CreatedAt: time.Unix(1, 0)}
	x := r
	x.ID, x.BranchID, x.Branch, x.Target, x.GitAfter = strings.Repeat("2", 32), "X", "source", source, head
	pin := x
	pin.ID, pin.Kind, pin.Source = strings.Repeat("3", 32), "advance", source
	receipt := r
	receipt.ID, receipt.Kind, receipt.Source, receipt.SourceBranchID, receipt.PRCompleted = strings.Repeat("4", 32), "pr-merge", source, "X", true
	receipt.PR = &PullRequestMerge{Number: 1, BaseBranch: "task", HeadBranch: "source", HeadSHA: head, MergeSHA: code}
	future := x
	future.ID, future.Kind, future.BindingParent = strings.Repeat("5", 32), "archive", x.ID
	for _, selectedPR := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary selection", true: "PR selection"}[selectedPR], func(t *testing.T) {
			events := []HistoryEvent{r, x, pin, receipt, future}
			ref := Ref{RepoID: repo, Kind: RefBranch, Name: "task", BranchID: "R", Target: target}
			selectedCode, selectedTarget := strings.Repeat("9", 40), target
			if selectedPR {
				selectedCode, selectedTarget = code, source
			}
			proof, err := TrackingProof(ref, events, selectedCode, selectedTarget)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range proof {
				if e.ID == future.ID {
					t.Fatal("unrelated future source archive entered applied proof")
				}
				if !selectedPR && e.BranchID == "X" {
					t.Fatal("unselected PR source entered applied proof")
				}
			}
			if selectedPR {
				found := false
				for _, e := range proof {
					if e.ID == pin.ID {
						found = true
					}
				}
				if !found {
					t.Fatal("selected PR source proof missing")
				}
			}
		})
	}
}
