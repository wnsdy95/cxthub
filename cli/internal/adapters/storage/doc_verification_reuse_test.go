package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func receiptReuseFixture(t *testing.T) (*FileStore, domain.ContentHash, docVerificationReceipt, []byte) {
	t.Helper()
	s := verificationStore(t)
	doc := bigDoc(1)
	doc.Events[0].Blocks[0].Text = strings.Repeat("x", 4*chunkcas.ChunkTarget)
	id, err := s.PutDoc(context.Background(), domain.SessionDoc{CIR: doc})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	r := receiptFor(t, s, id)
	seen := map[docVerifiedFile]bool{}
	repeated := false
	for _, file := range r.Proof.Files {
		repeated = repeated || seen[file]
		seen[file] = true
	}
	if !repeated {
		t.Fatal("fixture requires repeated physical chunks")
	}
	key, err := s.docVerificationKey()
	if err != nil {
		t.Fatal(err)
	}
	return s, id, r, key
}

func writeSignedReuseReceipt(t *testing.T, s *FileStore, r docVerificationReceipt, key []byte) []byte {
	t.Helper()
	r.MAC = signDocProof(r.Proof, key)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(s.docReceiptPath(r.Proof.Doc), raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDocVerificationSignedDuplicateEntries(t *testing.T) {
	for _, kind := range []string{"matching", "conflicting-stored", "malformed-stored", "duplicate-doc", "wrong-kind", "malformed-id"} {
		t.Run(kind, func(t *testing.T) {
			s, id, r, key := receiptReuseFixture(t)
			duplicate := r.Proof.Files[1]
			switch kind {
			case "conflicting-stored":
				duplicate.Stored = domain.HashContent([]byte("different stored representation"))
			case "malformed-stored":
				duplicate.Stored = "invalid"
			case "duplicate-doc":
				// This exact key was already checked at index zero. Shape validation
				// must still reject a document entry after the first position.
				duplicate = r.Proof.Files[0]
			case "wrong-kind":
				duplicate.Kind = "settings"
			case "malformed-id":
				duplicate.ID = "sha256:invalid"
			}
			r.Proof.Files = append(r.Proof.Files, duplicate)
			original := writeSignedReuseReceipt(t, s, r, key)
			if got := s.matchesDocReceipt(context.Background(), id, key); got != (kind == "matching") {
				t.Fatalf("receipt reuse = %t for %s", got, kind)
			}
			// Invalid optimization evidence must fall back, not reject the valid
			// underlying document or accidentally certify the conflicting receipt.
			if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(s.docReceiptPath(id))
			if err != nil {
				t.Fatal(err)
			}
			if unchanged := bytes.Equal(original, after); unchanged != (kind == "matching") {
				t.Fatalf("receipt unchanged = %t for %s", unchanged, kind)
			}
			if !s.matchesDocReceipt(context.Background(), id, key) {
				t.Fatal("full verification did not leave a usable receipt")
			}
		})
	}
}

func TestDocVerificationReceiptIdentityIncludesKind(t *testing.T) {
	s, id, r, key := receiptReuseFixture(t)
	// A signed shape-level control isolates the two storage namespaces. It does
	// not claim these fabricated proof entries came from document reconstruction.
	other := []byte("different bytes under the same ID in the chunks namespace")
	if err := writeAtomic(s.objectPath("chunks", id), other); err != nil {
		t.Fatal(err)
	}
	r.Proof.Files = []docVerifiedFile{r.Proof.Files[0], {Kind: "chunks", ID: id, Stored: domain.HashContent(other)}}
	writeSignedReuseReceipt(t, s, r, key)
	if !s.matchesDocReceipt(context.Background(), id, key) {
		t.Fatal("document and chunk identities were conflated")
	}
	other[0] ^= 1
	if err := writeAtomic(s.objectPath("chunks", id), other); err != nil {
		t.Fatal(err)
	}
	if s.matchesDocReceipt(context.Background(), id, key) {
		t.Fatal("document digest hid changed bytes in the chunk namespace")
	}
}

func TestDocVerificationDuplicateReceiptRechecksEachInvocation(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-store", true: "new-store"}[reopen], func(t *testing.T) {
			s, id, r, key := receiptReuseFixture(t)
			if !s.matchesDocReceipt(context.Background(), id, key) {
				t.Fatal("initial authenticated receipt missed")
			}
			path := s.objectPath(r.Proof.Files[1].Kind, r.Proof.Files[1].ID)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			raw[len(raw)/2] ^= 0x40
			if err := os.WriteFile(path, raw, info.Mode().Perm()); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if reopen {
				fresh := NewFileStore(s.repoRoot)
				fresh.EnableDocVerificationCache(s.docProofKeyPath)
				s = fresh
			}
			if s.matchesDocReceipt(context.Background(), id, key) {
				t.Fatal("digest survived its invocation despite changed bytes")
			}
			if err := s.VerifyStoredDoc(context.Background(), id); err == nil {
				t.Fatal("cold fallback accepted damaged chunk bytes")
			}
		})
	}
}

// Cancel deterministically during a long duplicate-only suffix, without timing
// a filesystem race or adding a production hook. Cancellation remains sticky.
type receiptCancelContext struct {
	context.Context
	cancel context.CancelFunc
	polls  int
}

func (c *receiptCancelContext) Err() error {
	c.polls++
	if c.polls == 32 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestDocVerificationDuplicateWalkChecksCancellation(t *testing.T) {
	s, id, r, key := receiptReuseFixture(t)
	for range 100 {
		r.Proof.Files = append(r.Proof.Files, r.Proof.Files[1])
	}
	before := writeSignedReuseReceipt(t, s, r, key)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &receiptCancelContext{Context: base, cancel: cancel}
	if s.matchesDocReceipt(ctx, id, key) || !errors.Is(base.Err(), context.Canceled) {
		t.Fatal("duplicate walk stopped polling cancellation")
	}
	if err := s.VerifyStoredDoc(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled verification entered fallback", err)
	}
	after, err := os.ReadFile(s.docReceiptPath(id))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("canceled verification replaced its receipt", err)
	}
}
