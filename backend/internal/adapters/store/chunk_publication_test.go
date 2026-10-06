package store

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Exercise the worker's chunk-backed value, including an event crossing physical
// chunk boundaries. Compare both archive bytes and derived search locations
// against the established whole-document path.
func verifiedChunkFixture(t testing.TB) (domain.VerifiedSessionDoc, domain.VerifiedSessionDoc) {
	t.Helper()
	cir := chunkBigDoc(40)
	cir.Events[0].Blocks[0].Text = t.Name() + time.Now().UTC().Format(time.RFC3339Nano) + cir.Events[0].Blocks[0].Text
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := domain.PlanDocChunks(raw)
	if !ok || len(plan.Order) < 2 {
		t.Fatal("fixture must span chunks")
	}
	var verifier domain.CanonicalDocVerifier
	segmented, err := verifier.VerifyChunks(context.Background(), domain.HashContent(raw), plan.Manifest, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		return plan.Bodies[hash], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: domain.HashContent(raw), CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	if segmented.Hash() != legacy.Hash() || !bytes.Equal(segmented.Bytes(), legacy.Bytes()) {
		t.Fatal("chunk-backed value changed legacy document identity")
	}
	return segmented, legacy
}

func TestFSChunkBackedDocumentPublication(t *testing.T) {
	st := NewFSStore(t.TempDir())
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name()))
	doc, legacy := verifiedChunkFixture(t)
	if created, err := st.PutVerifiedDoc(ctx, repo, doc); err != nil || !created {
		t.Fatalf("publish: created=%v err=%v", created, err)
	}
	if created, err := st.PutVerifiedDoc(ctx, repo, doc); err != nil || created {
		t.Fatalf("replay: created=%v err=%v", created, err)
	}
	got, err := st.GetDoc(ctx, repo, doc.Hash())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.ValidatedSessionDocBytes(got)
	if err != nil || !bytes.Equal(raw, legacy.Bytes()) {
		t.Fatal("archived bytes changed", err)
	}
	want, err := legacy.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := st.DocReadIndex(ctx, repo, doc.Hash())
	if err != nil || len(idx.Events) != len(want.Events) {
		t.Fatal("read index changed", err)
	}
	for i, e := range idx.Events {
		w := want.Events[i]
		if e.Hash != w.Hash || e.Offset != w.Offset || e.Length != w.Length || e.Seq != w.Seq || e.Role != w.Role {
			t.Fatalf("event %d metadata changed", i)
		}
	}
}
