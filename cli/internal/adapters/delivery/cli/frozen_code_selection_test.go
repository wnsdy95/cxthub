package cli

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestFrozenCompletionSelectsFinalContextAndMemory(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "historical"}[historical], func(t *testing.T) {
			ctx := context.Background()
			f := newFrozenDriver(t)
			f.save.WithFrozenMemory(nil, memory.NewRuleDistiller())
			p := f.attempt(t, "final-code-selection", domain.DocumentIdentityRootV1, false)
			next := *p
			next.Outcomes = append([]domain.CaptureOutcome(nil), p.Outcomes...)
			input := *p.Outcomes[0].Input
			var err error
			input.SessionID, err = f.capture.FrozenSessionID(ctx, f.cwd, input)
			if err != nil {
				t.Fatal(err)
			}
			next.Outcomes[0].Input = &input
			next.Outcomes[0].SessionID = input.SessionID
			next.InputsReady = true
			if err := p.replace(ctx, f.cwd, next); err != nil {
				t.Fatal(err)
			}
			baseline := p.Proof
			baseline.ID = domain.CaptureBaselineObservationID(p.Proof.ID)
			baseline.Source, baseline.Target = p.Initial, p.Initial
			if err := f.c.History.RecordHistory(ctx, baseline); err != nil {
				t.Fatal(err)
			}
			if historical {
				runLifecycleGit(t, f.cwd, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "later code")
			}
			before, err := f.store.GetWorkingPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := processFrozenCapture(ctx, f.c, f.cwd, f.cwd, p); err != nil {
				t.Fatal(err)
			}
			completed := f.readAttempt(t, p)
			p = &completed
			if p.Observation == nil || p.FinalMemory == nil {
				t.Fatal("missing finalized memory evidence")
			}
			events, err := f.c.History.ListHistory(ctx, f.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			var final domain.HistoryEvent
			for _, e := range events {
				if e.ID == domain.CaptureCompletionObservationID(p.Proof.ID) {
					final = e
				}
			}
			if final.Target != p.Proof.Target || final.MemoryHash != p.FinalMemory.Memory || !final.CreatedAt.Equal(p.Proof.CreatedAt) || final.GitBefore != final.GitAfter {
				t.Fatal("completion did not retain exact frozen code/memory", final)
			}
			// A timestamp/ID sort or reverse iteration must not select the
			// baseline or the first provider's pre-final memory instead.
			for i := 0; i < 2; i++ {
				got := contextSelectionAtCode(f.cwd, p.Proof.GitAfter, "main", nil, events)
				if got.Snapshot != p.Proof.Target || got.MemoryHash != p.FinalMemory.Memory || !got.MemoryPinned {
					t.Fatalf("final code selection = %+v", got)
				}
				for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
					events[left], events[right] = events[right], events[left]
				}
			}
			if historical {
				after, err := f.store.GetWorkingPosition(ctx)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("historical completion changed current selection", err)
				}
			}
			// A later explicit repin wins even if completion was delivered last.
			later := baseline
			later.ID = "ffffffffffffffffffffffffffffffff"
			later.GitBefore = later.GitAfter
			later.CreatedAt = p.Proof.CreatedAt.Add(time.Second)
			events = append([]domain.HistoryEvent{later}, events...)
			got := contextSelectionAtCode(f.cwd, p.Proof.GitAfter, "main", nil, events)
			if got.Snapshot != later.Target || got.MemoryHash != later.MemoryHash {
				t.Fatal("delayed completion overwrote later selection", got)
			}
		})
	}
}

func TestIncompleteFrozenCaptureHasNoFinalCodeSelection(t *testing.T) {
	f := newFrozenDriver(t)
	p := f.attempt(t, "incomplete-selection", domain.DocumentIdentityRootV1, true)
	if err := publishCommitCapture(context.Background(), f.c, f.cwd, p, nil); err == nil {
		t.Fatal("incomplete receipt published")
	}
	events, err := f.c.History.ListHistory(context.Background(), f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.ID == domain.CaptureCompletionObservationID(p.Proof.ID) {
			t.Fatal("incomplete capture became explicit code selection")
		}
	}
}
