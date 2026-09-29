package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestAgentPreparationStaleCursorRejectsChangedObservedMemory(t *testing.T) {
	for _, change := range []string{"item state", "item body", "lineage"} {
		t.Run(change, func(t *testing.T) {
			svc, in, history, memory, documents := agentServiceFixture(t)
			historyHash := history.view.StateHash
			originalRevision := memory.rev
			calls := 0
			svc.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				calls++
				if calls > 3 {
					t.Fatalf("continued reading after observing changed memory: call %d", calls)
				}
				if calls == 2 {
					if req.Cursor != fmt.Sprint("cursor-", originalRevision.Evidence) {
						t.Fatalf("did not request the original continuation: %q", req.Cursor)
					}
					// Keep the history hash and selected code fixed. Evidence can
					// change a memory assessment without changing context inclusion.
					revision := *history.view.Revision
					revision.Evidence++
					history.view.Revision, memory.rev = &revision, revision
					return domain.EffectiveMemoryPage{}, domain.ErrEffectiveMemoryCursorStale
				}
				if req.Cursor != "" {
					t.Fatalf("restart reused a cursor: %q", req.Cursor)
				}
				page, err := memory.QueryEffectiveMemory(ctx, repo, req)
				page.Items = []domain.EffectiveMemoryItem{{
					ID: agentHash("observed code claim"), SourceSnapshot: in.SnapshotID,
					Kind: "code", Text: "Preserve the selected implementation",
					Code:  &domain.MemoryCodeScope{Commit: history.view.Selection.CodeCommit, Paths: []string{"selected.go"}},
					State: "review", Reason: "selected_tree_pending",
				}}
				page.Total = 2
				page.StateHash = agentHash(fmt.Sprint("memory revision ", page.Revision.Evidence))
				page.NextCursor = fmt.Sprint("cursor-", page.Revision.Evidence)
				if calls == 3 {
					// A legitimate revision-derived StateHash/cursor refresh must
					// not hide a change in an already observed body or lineage.
					if page.StateHash == agentHash(fmt.Sprint("memory revision ", originalRevision.Evidence)) {
						t.Fatal("fixture did not refresh the memory state hash")
					}
					switch change {
					case "item state":
						page.Items[0].State, page.Items[0].Reason = "applied", "changes_present"
					case "item body":
						page.Items[0].Text = "Replace the selected implementation"
					case "lineage":
						page.LineageHash = agentHash("changed lineage")
					}
				}
				if !validEffectivePromptPage(page, req) {
					t.Fatal("fixture must return a valid page, not malformed input")
				}
				return page, err
			})
			got, err := svc.PrepareAgentContext(context.Background(), in)
			var contention *agentRevisionContention
			if !errors.Is(err, domain.ErrSelectionChanged) || errors.As(err, &contention) {
				t.Fatalf("changed memory did not fail terminally: %v", err)
			}
			if !reflect.DeepEqual(got, domain.AgentContextPackage{}) || documents.calls != 0 {
				t.Fatalf("rejected memory produced materializable content: package=%+v documents=%d", got, documents.calls)
			}
			if calls != 3 || history.calls != 2 || history.view.StateHash != historyHash {
				t.Fatalf("unexpected retry or changed history: memory=%d history=%d hash=%s", calls, history.calls, history.view.StateHash)
			}
		})
	}
}

func TestAgentPreparationRetryRejectsSameRevisionMemoryCursorChange(t *testing.T) {
	svc, in, history, memory, documents := agentServiceFixture(t)
	historyHash := history.view.StateHash
	calls := 0
	svc.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
		calls++
		if calls > 2 || req.Cursor != "" {
			t.Fatalf("read after observing renamed cursor: call=%d cursor=%q", calls, req.Cursor)
		}
		if calls == 1 {
			// The first memory page is newer than its context read. The retry
			// observes that same memory revision with different cursor bytes.
			revision := *history.view.Revision
			revision.Evidence++
			history.view.Revision, memory.rev = &revision, revision
		}
		page, err := memory.QueryEffectiveMemory(ctx, repo, req)
		page.Total = 2
		page.StateHash = agentHash(fmt.Sprint("memory revision ", page.Revision.Evidence))
		page.NextCursor = fmt.Sprint("cursor-", calls)
		if !validEffectivePromptPage(page, req) {
			t.Fatal("fixture must return a valid page")
		}
		return page, err
	})
	got, err := svc.PrepareAgentContext(context.Background(), in)
	var contention *agentRevisionContention
	if !errors.Is(err, domain.ErrSelectionChanged) || errors.As(err, &contention) {
		t.Fatalf("same-revision cursor change did not fail terminally: %v", err)
	}
	if !reflect.DeepEqual(got, domain.AgentContextPackage{}) || documents.calls != 0 || calls != 2 || history.calls != 2 || history.view.StateHash != historyHash {
		t.Fatalf("cursor change leaked output or triggered another retry: package=%+v documents=%d memory=%d history=%d", got, documents.calls, calls, history.calls)
	}
}
