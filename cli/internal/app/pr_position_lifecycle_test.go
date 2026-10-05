package app

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"strings"
	"testing"
)

func TestResolvePRSourcePositionIgnoresLaterLifecycleMemory(t *testing.T) {
	f := newPRPositionMemoryFixture(t)
	old := f.memory(f.receipt.Source, "", "pinned PR source")
	later := f.memory(f.receipt.Source, old, "later archive memory")
	pin := f.observation(old)
	archive := pin
	archive.ID, archive.Kind, archive.MemoryHash = strings.Repeat("7", 32), "archive", later
	got, err := f.svc.ResolvePRSourcePositionFromHistory(context.Background(), f.receipt, []domain.HistoryEvent{pin, archive})
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryHash != old {
		t.Fatal("lifecycle-only event replaced PR context pin")
	}
}
