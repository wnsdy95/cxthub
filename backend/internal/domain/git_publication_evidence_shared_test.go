package domain

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func gitPublicationSharedHistory(witnesses, publications int) (ContentHash, []HistoryEvent, []HistoryEvent) {
	repo := HashContent([]byte("shared publication evidence"))
	capture := HashContent([]byte("original shared capture"))
	ordinary := HistoryEvent{RepoID: string(repo), BranchID: "source-incarnation", Branch: "source-context", LocalBranch: "source-native", WorktreeID: strings.Repeat("1", 32), Source: capture, Target: capture, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(1, 0).UTC()}
	history := make([]HistoryEvent, 0, 2*witnesses+publications)
	for _, head := range []string{ordinary.GitAfter, strings.Repeat("b", 40)} {
		for position := 0; position < witnesses; position++ {
			event := ordinary
			event.ID, event.Kind, event.GitAfter = fmt.Sprintf("%032x", len(history)+1), []string{"birth", "attach", "position", "advance"}[position%4], head
			history = append(history, event)
		}
	}
	accepted := make([]HistoryEvent, 0, publications)
	for position := 0; position < publications; position++ {
		publication := ordinary
		publication.ID, publication.Kind = fmt.Sprintf("%032x", len(history)+1), "publish"
		publication.CreatedAt = publication.CreatedAt.Add(time.Duration(position) * time.Second)
		publication.MemoryPinned = true
		if position%2 == 0 {
			publication.MemoryHash = HashContent([]byte(fmt.Sprintf("original memory %d", position)))
			publication.MemorySource = capture
		}
		accepted = append(accepted, publication)
		history = append(history, publication)
	}
	return repo, history, accepted
}

func gitPublicationSharedExpected(history []HistoryEvent, publication HistoryEvent, head string) []string {
	ids := []string{}
	for _, event := range history {
		switch event.Kind {
		case "birth", "attach", "position", "advance":
			if event.RepoID == publication.RepoID && event.BranchID == publication.BranchID && event.Branch == publication.Branch && event.LocalBranch == publication.LocalBranch && event.WorktreeID == publication.WorktreeID && event.Target == publication.Target && event.GitAfter == head {
				ids = append(ids, event.ID)
			}
		}
	}
	slices.Sort(ids)
	return ids
}

func TestGitPublicationIndexSharedGroupsParity(t *testing.T) {
	repo, history, publications := gitPublicationSharedHistory(257, 32)
	carryHead := strings.Repeat("b", 40)
	for position, publication := range publications {
		if position%2 == 1 {
			publication.GitAfter = carryHead
			publications[position], history[2*257+position] = publication, publication
		}
	}
	foreignKey := history[0]
	foreignKey.ID, foreignKey.WorktreeID = strings.Repeat("f", 32), strings.Repeat("2", 32)
	history = append(history, foreignKey)
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			if reverse {
				slices.Reverse(history)
			}
			index, err := NewGitPublicationIndex(context.Background(), repo, history)
			if err != nil {
				t.Fatal(err)
			}
			for _, publication := range publications {
				if got, err := index.Publication(publication.ID); err != nil || got != publication {
					t.Fatal("shared evidence relabeled original publication or pin", got, publication, err)
				}
				want := gitPublicationSharedExpected(history, publication, publication.GitAfter)
				if got := index.ObservationIDs(publication.ID); len(got) != 257 || !slices.Equal(got, want) {
					t.Fatal("shared original witnesses truncated or changed", publication.ID, len(got))
				}
				for _, head := range []string{strings.Repeat("a", 40), carryHead} {
					want := gitPublicationSharedExpected(history, publication, head)
					if got, err := index.CarryObservationIDs(publication.ID, head); err != nil || len(got) != 257 || !slices.Equal(got, want) {
						t.Fatal("shared carry key changed or witnesses truncated", publication.ID, head, len(got), err)
					}
				}
			}
			if got := index.ObservationIDs(history[0].ID + "missing"); got == nil || len(got) != 0 {
				t.Fatal("missing publication no longer returns an empty slice", got)
			}
		})
	}
}

func TestGitPublicationIndexSharedGroupsAliasSafety(t *testing.T) {
	repo, history, publications := gitPublicationSharedHistory(257, 8)
	carryHead := strings.Repeat("b", 40)
	wantOriginal := gitPublicationSharedExpected(history, publications[0], publications[0].GitAfter)
	wantCarry := gitPublicationSharedExpected(history, publications[0], carryHead)
	index, err := NewGitPublicationIndex(context.Background(), repo, history)
	if err != nil {
		t.Fatal(err)
	}
	for position := range history {
		history[position] = HistoryEvent{}
	}
	var readers sync.WaitGroup
	for reader := 0; reader < 32; reader++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for _, publication := range publications {
				original := index.ObservationIDs(publication.ID)
				if !slices.Equal(original, wantOriginal) {
					t.Error("original witnesses aliased caller history or another returned slice")
					return
				}
				original[0] = publication.ID
				for _, selection := range []struct {
					head string
					want []string
				}{{publication.GitAfter, wantOriginal}, {carryHead, wantCarry}} {
					carry, err := index.CarryObservationIDs(publication.ID, selection.head)
					if err != nil || !slices.Equal(carry, selection.want) {
						t.Error("carry witnesses aliased original or another publication", err)
						return
					}
					carry[0] = publication.ID
				}
			}
		}()
	}
	readers.Wait()
	for _, publication := range publications {
		if got := index.ObservationIDs(publication.ID); !slices.Equal(got, wantOriginal) {
			t.Fatal("returned original mutation changed a shared group")
		}
		if got, err := index.CarryObservationIDs(publication.ID, carryHead); err != nil || !slices.Equal(got, wantCarry) {
			t.Fatal("returned carry mutation changed a shared group", err)
		}
		if got, err := index.Publication(publication.ID); err != nil || got != publication {
			t.Fatal("caller history mutation changed accepted publication", got, err)
		}
	}
}

type gitPublicationIndexCancelContext struct {
	context.Context
	cancel context.CancelFunc
	after  int
	calls  int
}

func (ctx *gitPublicationIndexCancelContext) Err() error {
	ctx.calls++
	if ctx.calls == ctx.after {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestGitPublicationIndexSharedGroupsCancellation(t *testing.T) {
	repo, history, _ := gitPublicationSharedHistory(257, 32)
	for _, phase := range []struct {
		name  string
		after int
	}{{"validation", 7}, {"sorting", len(history) + 2}, {"publication", len(history) + 2*257 + 5}} {
		t.Run(phase.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &gitPublicationIndexCancelContext{Context: parent, cancel: cancel, after: phase.after}
			if index, err := NewGitPublicationIndex(ctx, repo, history); !errors.Is(err, context.Canceled) || index != nil {
				t.Fatal("canceled construction returned a partial shared index", index, err)
			}
		})
	}
}

func TestGitPublicationIndexSharedGroupsRejectInvalidTail(t *testing.T) {
	for _, scenario := range []string{"foreign-ordinary", "duplicate-ordinary", "duplicate-publication", "corrupt-publication", "invalid-repo"} {
		t.Run(scenario, func(t *testing.T) {
			repo, history, publications := gitPublicationSharedHistory(257, 32)
			want := ErrIntegrity
			switch scenario {
			case "foreign-ordinary":
				event := history[0]
				event.ID, event.RepoID = strings.Repeat("f", 32), string(HashContent([]byte("foreign repository")))
				history = append(history, event)
			case "duplicate-ordinary":
				history = append(history, history[0])
			case "duplicate-publication":
				history = append(history, publications[len(publications)-1])
			case "corrupt-publication":
				history[len(history)-1].MemoryHash = "invalid"
			case "invalid-repo":
				repo, want = "invalid", ErrValidation
			}
			if index, err := NewGitPublicationIndex(context.Background(), repo, history); !errors.Is(err, want) || index != nil {
				t.Fatal("shared group bypassed full history validation", index, err)
			}
		})
	}
	var missing *GitPublicationIndex
	if missing.ObservationIDs("missing") != nil {
		t.Fatal("nil index changed observation contract")
	}
}

var benchmarkGitPublicationSharedIndex *GitPublicationIndex

func BenchmarkGitPublicationIndexRepeatedKey(benchmark *testing.B) {
	for _, size := range []struct {
		witnesses    int
		publications int
	}{{256, 1}, {256, 128}, {256, 1024}, {4096, 1024}} {
		benchmark.Run(fmt.Sprintf("witnesses=%d/publications=%d", size.witnesses, size.publications), func(benchmark *testing.B) {
			repo, history, _ := gitPublicationSharedHistory(size.witnesses, size.publications)
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for iteration := 0; iteration < benchmark.N; iteration++ {
				index, err := NewGitPublicationIndex(context.Background(), repo, history)
				if err != nil {
					benchmark.Fatal(err)
				}
				benchmarkGitPublicationSharedIndex = index
			}
		})
	}
	benchmarkGitPublicationSharedIndex = nil
}
