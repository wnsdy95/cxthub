package store

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type captureComparisonStore interface {
	outbound.BlobStore
	outbound.StoredCaptureComparator
}

func captureComparisonFixture(t *testing.T, marker string, tail ...string) domain.SessionDoc {
	t.Helper()
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, SessionOriginID: "prefix-native"}}
	for i, text := range append([]string{marker}, tail...) {
		cir.Events = append(cir.Events, domain.CIREvent{Kind: domain.EventMessage, Role: domain.RoleUser, Seq: i, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}})
	}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	return domain.SessionDoc{Hash: domain.HashContent(raw), CIR: cir}
}

func checkCaptureComparison(t *testing.T, s captureComparisonStore, repo domain.ContentHash) (domain.SessionDoc, domain.SessionDoc) {
	t.Helper()
	ctx := context.Background()
	old := captureComparisonFixture(t, string(repo), "saved")
	next := captureComparisonFixture(t, string(repo), "saved", "new")
	divergent := captureComparisonFixture(t, string(repo), "different", "new")
	for _, doc := range []domain.SessionDoc{old, next, divergent} {
		if _, err := s.PutDoc(ctx, repo, doc); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		left, right domain.ContentHash
		want        bool
	}{{old.Hash, next.Hash, true}, {next.Hash, old.Hash, false}, {old.Hash, divergent.Hash, false}} {
		for i := 0; i < 2; i++ {
			got, err := s.CaptureSupersedes(ctx, repo, tc.left, tc.right, domain.ProviderClaude, "prefix-native")
			if err != nil || got != tc.want {
				t.Fatal(got, err)
			}
		}
	}
	foreign := domain.HashContent([]byte("foreign " + string(repo)))
	if got, err := s.CaptureSupersedes(ctx, foreign, old.Hash, next.Hash, domain.ProviderClaude, "prefix-native"); got || !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("foreign ownership", got, err)
	}
	if got, err := s.CaptureSupersedes(ctx, repo, old.Hash, next.Hash, domain.ProviderClaude, "wrong-native"); got || err != nil {
		t.Fatal("wrong identity", got, err)
	}
	return old, next
}

func TestFSCaptureComparisonChecksCurrentBytes(t *testing.T) {
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	old, next := checkCaptureComparison(t, s, repo)
	// Warm proofs never hide a modified physical representation or removed grant.
	if err := writeAtomic(s.docPath(repo, next.Hash), []byte(`{"events":[]}`)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CaptureSupersedes(ctx, repo, old.Hash, next.Hash, domain.ProviderClaude, "prefix-native"); got || !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("corruption", got, err)
	}
	if err := os.Remove(s.docPath(repo, old.Hash)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CaptureSupersedes(ctx, repo, old.Hash, next.Hash, domain.ProviderClaude, "prefix-native"); got || !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("missing body", got, err)
	}
}
