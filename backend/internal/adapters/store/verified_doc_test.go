package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func verifiedDocFixture(t *testing.T) (domain.SessionDoc, domain.VerifiedSessionDoc) {
	t.Helper()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{SessionOriginID: t.Name() + time.Now().String()}, Events: []domain.CIREvent{
		{Kind: domain.EventMessage, Seq: 9, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: "later"}}},
		{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "earlier"}}},
	}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	verified, err := domain.VerifySessionDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	return doc, verified
}

func TestVerifiedDocStoreRejectsZeroCorruptionAndForeignRead(t *testing.T) {
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	doc, v := verifiedDocFixture(t)
	if _, err := s.PutVerifiedDoc(ctx, repo, domain.VerifiedSessionDoc{}); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("zero proof accepted", err)
	}
	if fresh, err := s.PutVerifiedDoc(ctx, repo, v); err != nil || !fresh {
		t.Fatal("first put", fresh, err)
	}
	if fresh, err := s.PutVerifiedDoc(ctx, repo, v); err != nil || fresh {
		t.Fatal("replay", fresh, err)
	}
	hits, err := s.SearchDocEvents(ctx, repo, doc.Hash, "earlier", -1, 10)
	if err != nil || len(hits) != 1 || hits[0].Seq != 1 || hits[0].Index != 0 || hits[0].Role != "user" {
		t.Fatalf("search metadata: %+v %v", hits, err)
	}
	if _, err := s.GetDoc(ctx, domain.HashContent([]byte("foreign")), doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("foreign read", err)
	}
	// The incoming proof must not hide a corrupt pre-existing object.
	if err := writeAtomic(s.docPath(repo, doc.Hash), []byte(`{"events":[]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutVerifiedDoc(ctx, repo, v); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("corrupt stored doc accepted", err)
	}
}
