package app

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestRewriteHistorySourcesValidateBeforeProjection(t *testing.T) {
	for _, mode := range []string{"valid-reversed", "unlinked", "unrelated-empty", "invalid", "missing", "collision", "nonroot", "foreign", "other-scope", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := newPRPositionMemoryFixture(t)
			before := f.observation("")
			f.history.events = append(f.history.events, before)
			memory := f.memory(before.Target, "", "root")
			after := f.observation(memory)
			after.MemorySelectionParent = before.ID
			after.CreatedAt = before.CreatedAt.Add(-time.Hour)
			events := []domain.HistoryEvent{after, before}
			wantErr := false
			wantLen := 1
			ctx := context.Background()
			switch mode {
			case "unlinked":
				events[0].MemorySelectionParent = ""
				wantLen = 2
			case "unrelated-empty":
				other := before
				other.ID = strings.Repeat("9", 32)
				events = append(events, other)
				wantLen = 2
			case "invalid":
				events[0].MemoryPinned = false
				wantErr = true
			case "missing":
				events = events[:1]
				wantErr = true
			case "collision":
				other := before
				other.GitAfter = strings.Repeat("9", 40)
				events = append(events, other)
				wantErr = true
			case "nonroot":
				events[0].MemoryHash = f.memory(before.Target, memory, "later")
				wantErr = true
			case "foreign":
				events[0].MemoryHash = f.memory(domain.HashContent([]byte("foreign-owner")), "", "foreign")
				wantErr = true
			case "other-scope":
				foreignBefore := before
				foreignBefore.ID, foreignBefore.BranchID, foreignBefore.WorktreeID = strings.Repeat("8", 32), "unselected", strings.Repeat("8", 32)
				other := after
				other.ID, other.BranchID, other.WorktreeID = strings.Repeat("9", 32), foreignBefore.BranchID, foreignBefore.WorktreeID
				other.MemorySelectionParent = foreignBefore.ID
				other.MemoryHash = domain.HashContent([]byte("not-local"))
				events = append(events, other, foreignBefore)
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				wantErr = true
			}
			original := append([]domain.HistoryEvent{}, events...)
			got, err := f.svc.RewriteHistorySources(ctx, events, before.BranchID, before.WorktreeID)
			if wantErr {
				if err == nil || got != nil {
					t.Fatalf("unproven projection accepted: %v %v", got, err)
				}
			} else if err != nil || len(got) != wantLen {
				t.Fatalf("projection: %d %v", len(got), err)
			}
			if !reflect.DeepEqual(events, original) {
				t.Fatal("raw history changed")
			}
			if mode == "unlinked" && (got[0].ID != after.ID || got[1].ID != before.ID) {
				t.Fatal("ordinary clock order changed")
			}
		})
	}
}
