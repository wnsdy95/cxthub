package store

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestHistoryReadOnlyDoesNotBuildIndexOrRepackLegacyDoc(t *testing.T) {
	st := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte("history-read-only"))
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "legacy"}}}}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if err = writeAtomic(st.docPath(repo, doc.Hash), raw); err != nil {
		t.Fatal(err)
	}
	ctx := outbound.WithDocReadOnly(context.Background())
	if _, err = st.DocReadIndex(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrAgentHistoryUnavailable) {
		t.Fatalf("legacy index: %v", err)
	}
	if _, err = st.GetDocManifest(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrAgentHistoryUnavailable) {
		t.Fatalf("legacy manifest: %v", err)
	}
	after, err := os.ReadFile(st.docPath(repo, doc.Hash))
	if err != nil || string(after) != string(raw) {
		t.Fatal("legacy archive mutated", err)
	}
	if _, err = os.Stat(st.readIndexPath(repo, doc.Hash)); !os.IsNotExist(err) {
		t.Fatal("index was created", err)
	}
	plan, ok := domain.PlanDocChunks(raw)
	if !ok {
		t.Fatal("fixture has no chunks")
	}
	for _, hash := range plan.Order {
		if _, err = os.Stat(st.chunkPath(repo, hash)); !os.IsNotExist(err) {
			t.Fatal("chunk was created", err)
		}
	}
	// Explicit legacy maintenance can still prepare the projection and chunks.
	if _, err = st.DocReadIndex(context.Background(), repo, doc.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DocReadIndex(ctx, repo, doc.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err = st.GetDocManifest(ctx, repo, doc.Hash); err != nil {
		t.Fatal(err)
	}
}
