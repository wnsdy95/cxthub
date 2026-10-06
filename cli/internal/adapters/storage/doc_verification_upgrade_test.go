package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Encode and sign the historical schema, independently of today's omitempty
// fields. This models a receipt written before upload body metadata existed.
func writeV1DocReceipt(t testing.TB, s *FileStore, id domain.ContentHash) []byte {
	t.Helper()
	type oldFile struct {
		Kind   string             `json:"kind"`
		ID     domain.ContentHash `json:"id"`
		Stored domain.ContentHash `json:"stored"`
	}
	type oldProof struct {
		Version int                `json:"version"`
		Doc     domain.ContentHash `json:"doc"`
		Files   []oldFile          `json:"files"`
	}
	current := receiptFor(t, s, id)
	proof := oldProof{Version: 1, Doc: id}
	for _, file := range current.Proof.Files {
		proof.Files = append(proof.Files, oldFile{file.Kind, file.ID, file.Stored})
	}
	key, err := s.docVerificationKey()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(encoded)
	raw, err := json.Marshal(struct {
		Proof oldProof `json:"proof"`
		MAC   string   `json:"mac"`
	}{proof, hex.EncodeToString(mac.Sum(nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(s.docReceiptPath(id), raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDocVerificationV1ReceiptReusedUntilUpload(t *testing.T) {
	s := verificationStore(t)
	id, canonical, plan := uploadFixture(t, s, bigDoc(30))
	if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	original := writeV1DocReceipt(t, s, id)
	old := time.Unix(100, 0)
	if err := os.Chtimes(s.docReceiptPath(id), old, old); err != nil {
		t.Fatal(err)
	}
	fresh := NewFileStore(s.repoRoot)
	fresh.EnableDocVerificationCache(s.docProofKeyPath)
	if err := fresh.VerifyStoredDoc(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.docReceiptPath(id))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.docReceiptPath(id))
	if err != nil || !bytes.Equal(raw, original) || !info.ModTime().Equal(old) {
		t.Fatalf("fetch replaced an already valid v1 receipt: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := fresh.VerifyStoredDoc(ctx, id); err != context.Canceled {
		t.Fatalf("cached verification ignored cancellation: %v", err)
	}
	called := false
	ok, err := fresh.WithVerifiedDocChunks(context.Background(), id, func(doc outbound.DocumentChunks) error {
		called = true
		checkUploadDescriptor(t, doc, id, canonical, plan)
		return nil
	})
	if err != nil || !ok || !called {
		t.Fatalf("upload did not upgrade v1 proof: supported=%v called=%v err=%v", ok, called, err)
	}
	upgraded := receiptFor(t, s, id)
	if upgraded.Proof.Version != docVerificationVersion {
		t.Fatal("upload did not save the current proof schema")
	}
	for _, file := range upgraded.Proof.Files {
		if file.Kind == "chunks" && (file.Body != file.ID || file.BodyBytes != len(plan.Bodies[file.ID])) {
			t.Fatal("upload did not certify decoded chunk identity and size")
		}
	}
}
