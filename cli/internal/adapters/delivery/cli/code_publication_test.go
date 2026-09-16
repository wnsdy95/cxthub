package cli

import (
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"testing"
	"time"
)

func TestPublicationCannotEraseRecordedMemorySelection(t *testing.T) {
	cwd, _, _, _, target := historyFixture(t)
	oid := gitOut(cwd, "rev-parse", "HEAD")
	memory := domain.HashContent([]byte("remembered project"))
	proof := domain.HistoryEvent{Branch: "main", Kind: "position", Target: target, GitAfter: oid, MemoryHash: memory, MemoryPinned: true, CreatedAt: time.Unix(1, 0).UTC()}
	published := proof
	published.Kind = "publish"
	published.MemoryHash = ""
	published.CreatedAt = time.Unix(2, 0).UTC()
	p := contextSelectionAtCode(cwd, oid, "main", nil, []domain.HistoryEvent{proof, published})
	if p.Snapshot != target || p.MemoryHash != memory || !p.MemoryPinned {
		t.Fatalf("publication overwrote selection: %+v", p)
	}
}
