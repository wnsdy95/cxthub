package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func uploadFixture(t testing.TB, s *FileStore, cir domain.CIRDocument) (domain.ContentHash, []byte, chunkcas.Plan) {
	t.Helper()
	canonical, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := chunkcas.PlanDoc(canonical)
	if !ok {
		t.Fatal("fixture must be chunkable")
	}
	id, err := s.PutDoc(context.Background(), domain.SessionDoc{CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	return id, canonical, plan
}

func writeUploadPlan(t testing.TB, s *FileStore, id domain.ContentHash, plan chunkcas.Plan) []byte {
	t.Helper()
	for hash, body := range plan.Bodies {
		if err := writeAtomic(s.objectPath("chunks", hash), docCompress(body)); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := json.Marshal(plan.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw := docCompress(manifest)
	if err := writeAtomic(s.objectPath("docs", id), raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func checkUploadDescriptor(t testing.TB, doc outbound.DocumentChunks, id domain.ContentHash, canonical []byte, plan chunkcas.Plan) {
	t.Helper()
	if doc.Hash != id || doc.Format != chunkcas.FormatV2 || !bytes.Equal(doc.Envelope, plan.Manifest.Envelope) || !reflect.DeepEqual(doc.Chunks, plan.Manifest.Chunks) {
		t.Fatal("descriptor differs from verified representation")
	}
	var bodies [][]byte
	for _, hash := range doc.Chunks {
		body, err := doc.ReadChunk(context.Background(), hash)
		if err != nil || !bytes.Equal(body, plan.Bodies[hash]) {
			t.Fatalf("chunk %s: %v", hash, err)
		}
		bodies = append(bodies, body)
	}
	got, err := chunkcas.AssembleChunks(plan.Manifest, bodies, id)
	if err != nil || !bytes.Equal(got, canonical) {
		t.Fatalf("descriptor identity: %v", err)
	}
}

func TestVerifiedDocChunksColdWarmAndDisabledCache(t *testing.T) {
	for _, mode := range []string{"cold", "warm-new-store", "disabled", "unavailable", "v1-receipt"} {
		t.Run(mode, func(t *testing.T) {
			s := verificationStore(t)
			id, canonical, plan := uploadFixture(t, s, bigDoc(30))
			old := time.Unix(100, 0)
			switch mode {
			case "warm-new-store", "v1-receipt":
				if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
					t.Fatal(err)
				}
				if mode == "v1-receipt" {
					writeV1DocReceipt(t, s, id)
				}
				if err := os.Chtimes(s.docReceiptPath(id), old, old); err != nil {
					t.Fatal(err)
				}
				fresh := NewFileStore(s.repoRoot)
				fresh.EnableDocVerificationCache(s.docProofKeyPath)
				s = fresh
			case "disabled":
				s.EnableDocVerificationCache("")
			case "unavailable":
				s.EnableDocVerificationCache(s.objectPath("keys", id))
			}
			called := false
			supported, err := s.WithVerifiedDocChunks(context.Background(), id, func(doc outbound.DocumentChunks) error {
				called = true
				checkUploadDescriptor(t, doc, id, canonical, plan)
				return nil
			})
			if err != nil || !supported || !called {
				t.Fatalf("supported=%v called=%v error=%v", supported, called, err)
			}
			if mode == "warm-new-store" || mode == "v1-receipt" {
				info, err := os.Stat(s.docReceiptPath(id))
				if err != nil || info.ModTime().Equal(old) != (mode == "warm-new-store") {
					t.Fatalf("receipt reuse/upgrade: %v", err)
				}
			}
		})
	}
}

func TestVerifiedDocChunksCurrentManifest(t *testing.T) {
	s := verificationStore(t)
	id, canonical, plan := uploadFixture(t, s, sampleCIR("synthetic manifest replacement"))
	if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	body := plan.Bodies[plan.Order[0]]
	left, right := body[:7], body[7:]
	plan.Manifest.Chunks = []domain.ContentHash{domain.HashContent(left), domain.HashContent(right)}
	plan.Bodies = map[domain.ContentHash][]byte{plan.Manifest.Chunks[0]: left, plan.Manifest.Chunks[1]: right}
	writeUploadPlan(t, s, id, plan)
	ok, err := s.WithVerifiedDocChunks(context.Background(), id, func(doc outbound.DocumentChunks) error {
		checkUploadDescriptor(t, doc, id, canonical, plan)
		return nil
	})
	if !ok || err != nil {
		t.Fatalf("replacement: %v %v", ok, err)
	}
	if len(receiptFor(t, s, id).Proof.Files) != 3 {
		t.Fatal("new partition was not verified")
	}
}

func TestVerifiedDocChunksUnsupportedAndCorrupt(t *testing.T) {
	for _, warm := range []bool{false, true} {
		for _, kind := range []string{"raw", "compressed-raw", "v1", "malformed", "manifest-tamper", "chunk-tamper", "missing-chunk", "mislabeled-chunk", "invalid-semantics"} {
			t.Run(kind+map[bool]string{false: "/cold", true: "/warm"}[warm], func(t *testing.T) {
				s := verificationStore(t)
				id, canonical, plan := uploadFixture(t, s, sampleCIR("synthetic"))
				if warm {
					if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
						t.Fatal(err)
					}
				}
				switch kind {
				case "raw", "compressed-raw":
					raw := canonical
					if kind == "compressed-raw" {
						raw = docCompress(raw)
					}
					if err := writeAtomic(s.objectPath("docs", id), raw); err != nil {
						t.Fatal(err)
					}
				case "v1":
					v1, _ := chunkcas.PlanDocV1(canonical)
					writeUploadPlan(t, s, id, v1)
				case "malformed":
					if err := writeAtomic(s.objectPath("docs", id), []byte(`{"format":"cxt-doc-chunks-v2",`)); err != nil {
						t.Fatal(err)
					}
				case "manifest-tamper":
					plan.Manifest.Envelope = json.RawMessage(`{"cir_version":"2"}`)
					writeUploadPlan(t, s, id, plan)
				case "chunk-tamper":
					path := s.objectPath("chunks", plan.Order[0])
					info, _ := os.Stat(path)
					raw, _ := os.ReadFile(path)
					raw[len(raw)/2] ^= 1
					if err := os.WriteFile(path, raw, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				case "missing-chunk":
					if err := os.Remove(s.objectPath("chunks", plan.Order[0])); err != nil {
						t.Fatal(err)
					}
				case "mislabeled-chunk":
					wrong := domain.HashContent([]byte("wrong label"))
					plan.Bodies[wrong] = plan.Bodies[plan.Order[0]]
					plan.Manifest.Chunks = []domain.ContentHash{wrong}
					writeUploadPlan(t, s, id, plan)
					// Preserve the public verifier's canonical validation behavior;
					// upload additionally checks each transport chunk's own identity.
					if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
						t.Fatal(err)
					}
				case "invalid-semantics":
					bad := []byte(`{"envelope":{"cir_version":"999"},"events":[{"kind":"message","seq":0}]}`)
					id = domain.HashContent(bad)
					invalid, _ := chunkcas.PlanDoc(bad)
					writeUploadPlan(t, s, id, invalid)
				}
				unsupported := kind == "raw" || kind == "compressed-raw" || kind == "v1"
				ok, err := s.WithVerifiedDocChunks(context.Background(), id, func(outbound.DocumentChunks) error {
					t.Fatal("unsupported/corrupt input reached callback")
					return nil
				})
				if ok || (err == nil) != unsupported {
					t.Fatalf("supported=%v error=%v", ok, err)
				}
			})
		}
	}
}

func TestVerifiedDocChunksPortablePartitions(t *testing.T) {
	for _, size := range []int{0, chunkcas.MaxPortableChunkBytes, chunkcas.MaxPortableChunkBytes + 1} {
		t.Run(stringSize(size), func(t *testing.T) {
			s := verificationStore(t)
			id, canonical, plan := uploadFixture(t, s, sampleCIR(strings.Repeat("x", chunkcas.MaxPortableChunkBytes+32)))
			var stream []byte
			for _, hash := range plan.Order {
				stream = append(stream, plan.Bodies[hash]...)
			}
			left, right := stream[:size], stream[size:]
			plan.Manifest.Chunks = []domain.ContentHash{domain.HashContent(left), domain.HashContent(right)}
			plan.Bodies = map[domain.ContentHash][]byte{plan.Manifest.Chunks[0]: left, plan.Manifest.Chunks[1]: right}
			writeUploadPlan(t, s, id, plan)
			for _, warm := range []bool{false, true} {
				called := false
				ok, err := s.WithVerifiedDocChunks(context.Background(), id, func(doc outbound.DocumentChunks) error {
					called = true
					checkUploadDescriptor(t, doc, id, canonical, plan)
					return nil
				})
				want := size == chunkcas.MaxPortableChunkBytes
				if err != nil || ok != want || called != want {
					t.Fatalf("warm=%v supported=%v called=%v error=%v", warm, ok, called, err)
				}
				if _, err := s.GetDoc(context.Background(), id); err != nil {
					t.Fatal("fallback cannot load valid partition", err)
				}
			}
		})
	}
}

func stringSize(size int) string {
	if size == 0 {
		return "empty"
	}
	if size == chunkcas.MaxPortableChunkBytes {
		return "at-bound"
	}
	return "over-bound"
}

func TestVerifiedDocChunksMembershipOwnershipAndCurrentBody(t *testing.T) {
	s := verificationStore(t)
	id, _, plan := uploadFixture(t, s, sampleCIR("synthetic"))
	other := []byte("unrelated private chunk")
	otherID := domain.HashContent(other)
	if err := s.PutChunk(context.Background(), otherID, other); err != nil {
		t.Fatal(err)
	}
	var retained outbound.DocumentChunks
	ok, err := s.WithVerifiedDocChunks(context.Background(), id, func(doc outbound.DocumentChunks) error {
		retained = doc
		original := doc.Chunks[0]
		doc.Chunks[0] = otherID
		doc.Envelope[0] = '!'
		for _, denied := range []domain.ContentHash{otherID, "../../escape"} {
			if _, err := doc.ReadChunk(context.Background(), denied); !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatal("mutable descriptor expanded membership", err)
			}
		}
		body, err := doc.ReadChunk(context.Background(), original)
		if err != nil {
			t.Fatal(err)
		}
		body[0] ^= 1
		again, err := doc.ReadChunk(context.Background(), original)
		if err != nil || !bytes.Equal(again, plan.Bodies[original]) {
			t.Fatal("chunk body was shared", err)
		}
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				if _, err := doc.ReadChunk(context.Background(), original); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		// A warm verification cannot authorize changed bytes at request time.
		if err := writeAtomic(s.objectPath("chunks", original), docCompress(other)); err != nil {
			t.Fatal(err)
		}
		if _, err := doc.ReadChunk(context.Background(), original); !errors.Is(err, domain.ErrHashMismatch) {
			t.Fatal("current chunk identity was not checked", err)
		}
		return nil
	})
	if !ok || err != nil {
		t.Fatalf("upload: %v %v", ok, err)
	}
	if _, err := retained.ReadChunk(context.Background(), plan.Order[0]); err == nil {
		t.Fatal("expired reader remained usable")
	}
	if err := writeAtomic(s.objectPath("chunks", plan.Order[0]), docCompress(plan.Bodies[plan.Order[0]])); err != nil {
		t.Fatal(err)
	}
	_, err = s.WithVerifiedDocChunks(context.Background(), id, func(doc outbound.DocumentChunks) error {
		if doc.Chunks[0] != plan.Order[0] || doc.Envelope[0] != '{' {
			t.Fatal("callback mutated future descriptor")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedDocChunksRetentionAndLifetime(t *testing.T) {
	for _, exit := range []string{"success", "error", "panic"} {
		t.Run(exit, func(t *testing.T) {
			s := verificationStore(t)
			id, _, plan := uploadFixture(t, s, sampleCIR("synthetic"))
			other := NewFileStore(s.repoRoot)
			var retained outbound.DocumentChunks
			failure := errors.New("callback failed")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := s.WithObjectsRetained(ctx, func() error {
				func() {
					defer func() {
						if got := recover(); got != nil && (exit != "panic" || got != failure) {
							t.Fatal(got)
						}
					}()
					ok, err := s.WithVerifiedDocChunks(ctx, id, func(doc outbound.DocumentChunks) error {
						retained = doc
						acquired, err := other.TryCollectObjects(ctx, func() error { t.Fatal("collected inside callback"); return nil })
						if err != nil || acquired {
							t.Fatal("callback not retained", err)
						}
						if exit == "panic" {
							panic(failure)
						}
						if exit == "error" {
							return failure
						}
						return nil
					})
					if !ok || (exit == "error" && !errors.Is(err, failure)) || (exit == "success" && err != nil) {
						t.Fatalf("callback result: %v %v", ok, err)
					}
				}()
				if _, err := retained.ReadChunk(ctx, plan.Order[0]); err == nil {
					t.Fatal("reader survived callback")
				}
				if acquired, err := other.TryCollectObjects(ctx, func() error { return nil }); err != nil || acquired {
					t.Fatal("nested lease released outer retention", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if acquired, err := other.TryCollectObjects(ctx, func() error { return nil }); err != nil || !acquired {
				t.Fatal("callback leaked retention", err)
			}
		})
	}
}

func TestVerifiedDocChunksCancellation(t *testing.T) {
	s := verificationStore(t)
	id, _, _ := uploadFixture(t, s, sampleCIR("synthetic"))
	for _, warm := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ok, err := s.WithVerifiedDocChunks(ctx, id, func(outbound.DocumentChunks) error { t.Fatal("canceled callback"); return nil })
		if ok || !errors.Is(err, context.Canceled) {
			t.Fatalf("warm=%v: %v %v", warm, ok, err)
		}
		if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ok, err := s.WithVerifiedDocChunks(ctx, id, func(doc outbound.DocumentChunks) error {
		readCtx, readCancel := context.WithCancel(context.Background())
		readCancel()
		if _, err := doc.ReadChunk(readCtx, doc.Chunks[0]); !errors.Is(err, context.Canceled) {
			t.Fatal("read cancellation lost", err)
		}
		cancel()
		if _, err := doc.ReadChunk(context.Background(), doc.Chunks[0]); !errors.Is(err, context.Canceled) {
			t.Fatal("parent cancellation lost", err)
		}
		return nil // even an ignored callback cancellation must propagate
	})
	if !ok || !errors.Is(err, context.Canceled) {
		t.Fatal("callback cancellation lost", ok, err)
	}
	_, err = s.TryCollectObjects(context.Background(), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		ok, err := s.WithVerifiedDocChunks(ctx, id, func(outbound.DocumentChunks) error { t.Fatal("callback entered during collection"); return nil })
		if ok || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("retention cancellation lost", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Switch the on-disk descriptor after its read without a timing-based race or
// production hook. The returned evidence must own the bytes that were checked.
type uploadSwapContext struct {
	context.Context
	polls int
	at    int
	swap  func()
}

func (c *uploadSwapContext) Err() error {
	c.polls++
	if c.polls == c.at {
		c.swap()
	}
	return c.Context.Err()
}

func TestVerifiedDocChunksEvidenceOwnsCheckedManifest(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "warm"}[warm], func(t *testing.T) {
			s := verificationStore(t)
			id, _, plan := uploadFixture(t, s, sampleCIR("synthetic"))
			before, _ := os.ReadFile(s.objectPath("docs", id))
			at := 3 // first cold chunk check, after observing the document
			if warm {
				if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
					t.Fatal(err)
				}
				at = 5 // warm doc EOF check, after hashing/capturing its bytes
			}
			ctx := &uploadSwapContext{Context: context.Background(), at: at, swap: func() {
				if err := writeAtomic(s.objectPath("docs", id), []byte("unverified replacement")); err != nil {
					t.Fatal(err)
				}
			}}
			evidence, err := s.verifyStoredDoc(ctx, id, true)
			if err != nil || ctx.polls < at {
				t.Fatalf("verification/swap failed: %v", err)
			}
			if !bytes.Equal(evidence.raw, before) || domain.HashContent(evidence.raw) != evidence.proof.Files[0].Stored {
				t.Fatal("descriptor was read separately from verified bytes")
			}
			manifest, _, ok, err := uploadDocManifest(context.Background(), evidence)
			if err != nil || !ok || !reflect.DeepEqual(manifest.Chunks, plan.Manifest.Chunks) {
				t.Fatal("descriptor extraction re-read unverified replacement", err)
			}
			// Restore the file: a before/after equality check would miss the
			// intervening representation. Evidence continues to bind consumed bytes.
			if err := writeAtomic(s.objectPath("docs", id), before); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func BenchmarkVerifiedDocChunks(b *testing.B) {
	s := verificationStore(b)
	id, _, _ := uploadFixture(b, s, bigDoc(250)) // 10 MiB synthetic transcript
	if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
		b.Fatal(err)
	}
	for _, warm := range []bool{false, true} {
		b.Run(map[bool]string{false: "cold-cache-disabled", true: "warm-new-store"}[warm], func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				fresh := NewFileStore(s.repoRoot)
				if warm {
					fresh.EnableDocVerificationCache(s.docProofKeyPath)
				}
				ok, err := fresh.WithVerifiedDocChunks(context.Background(), id, func(outbound.DocumentChunks) error { return nil })
				if !ok || err != nil {
					b.Fatalf("descriptor: %v %v", ok, err)
				}
			}
		})
	}
}

func TestVerifiedDocChunksNilCallbackAndMissingDocument(t *testing.T) {
	s := verificationStore(t)
	id, canonical, _ := uploadFixture(t, s, sampleCIR("synthetic"))
	for _, raw := range []bool{false, true} {
		if raw {
			if err := writeAtomic(s.objectPath("docs", id), canonical); err != nil {
				t.Fatal(err)
			}
		}
		if ok, err := s.WithVerifiedDocChunks(context.Background(), id, nil); ok || err == nil {
			t.Fatalf("nil callback accepted (raw=%v): %v %v", raw, ok, err)
		}
	}
	missing := domain.HashContent([]byte("not stored"))
	if ok, err := s.WithVerifiedDocChunks(context.Background(), missing, func(outbound.DocumentChunks) error {
		t.Fatal("missing document reached callback")
		return nil
	}); ok || !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("missing document became unsupported", ok, err)
	}
}

func TestVerifiedDocChunksSizeFallbackCannotHideCorruptChunkIdentity(t *testing.T) {
	s := verificationStore(t)
	id, _, plan := uploadFixture(t, s, sampleCIR(strings.Repeat("x", chunkcas.MaxPortableChunkBytes+32)))
	var stream []byte
	for _, hash := range plan.Order {
		stream = append(stream, plan.Bodies[hash]...)
	}
	large, suffix := stream[:chunkcas.MaxPortableChunkBytes+1], stream[chunkcas.MaxPortableChunkBytes+1:]
	largeID := domain.HashContent(large)
	wrong := domain.HashContent([]byte("mislabeled suffix"))
	plan.Manifest.Chunks = []domain.ContentHash{largeID, wrong}
	plan.Bodies = map[domain.ContentHash][]byte{largeID: large, wrong: suffix}
	writeUploadPlan(t, s, id, plan)
	for range 2 { // cold and warm must inspect the suffix despite the large prefix
		if ok, err := s.WithVerifiedDocChunks(context.Background(), id, func(outbound.DocumentChunks) error {
			t.Fatal("corrupt oversized representation reached callback")
			return nil
		}); ok || !errors.Is(err, domain.ErrHashMismatch) {
			t.Fatal("oversized prefix hid corruption", ok, err)
		}
	}
}

func TestVerifiedDocChunksCancellationDuringVerification(t *testing.T) {
	for _, warm := range []bool{false, true} {
		s := verificationStore(t)
		id, _, _ := uploadFixture(t, s, sampleCIR("synthetic"))
		if warm {
			if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
				t.Fatal(err)
			}
		}
		base, cancel := context.WithCancel(context.Background())
		ctx := &uploadSwapContext{Context: base, at: 8, swap: cancel}
		ok, err := s.WithVerifiedDocChunks(ctx, id, func(outbound.DocumentChunks) error {
			t.Fatal("verification cancellation reached callback")
			return nil
		})
		cancel()
		if ok || !errors.Is(err, context.Canceled) {
			t.Fatalf("warm=%v verification cancellation lost: %v %v", warm, ok, err)
		}
	}
}

func TestVerifiedDocChunksRepeatedIDsAndConflictingReceiptMetadata(t *testing.T) {
	for _, change := range []string{"matching", "body-hash", "body-size"} {
		t.Run(change, func(t *testing.T) {
			s := verificationStore(t)
			id, canonical, plan := uploadFixture(t, s, sampleCIR(strings.Repeat("x", 4*chunkcas.ChunkTarget)))
			if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			seen := make(map[domain.ContentHash]bool)
			repeated := false
			for _, hash := range plan.Manifest.Chunks {
				repeated = repeated || seen[hash]
				seen[hash] = true
			}
			if !repeated {
				t.Fatal("fixture does not contain repeated physical chunks")
			}
			r := receiptFor(t, s, id)
			duplicate := r.Proof.Files[1]
			switch change {
			case "body-hash":
				duplicate.Body = domain.HashContent([]byte("conflicting authenticated body metadata"))
			case "body-size":
				duplicate.BodyBytes++
			}
			r.Proof.Files = append(r.Proof.Files, duplicate)
			key, err := s.docVerificationKey()
			if err != nil {
				t.Fatal(err)
			}
			original := writeSignedReuseReceipt(t, s, r, key)
			if s.matchesDocReceipt(context.Background(), id, key) != (change == "matching") {
				t.Fatal("receipt accepted conflicting metadata or rejected a consistent duplicate")
			}
			ok, err := s.WithVerifiedDocChunks(context.Background(), id, func(doc outbound.DocumentChunks) error {
				checkUploadDescriptor(t, doc, id, canonical, plan)
				return nil
			})
			if !ok || err != nil {
				t.Fatal("cache fallback lost valid document", ok, err)
			}
			after, err := os.ReadFile(s.docReceiptPath(id))
			if err != nil || bytes.Equal(original, after) != (change == "matching") {
				t.Fatal("receipt was not reused/replaced as appropriate", err)
			}
		})
	}
}

func TestVerifiedDocChunksManifestEntryLimit(t *testing.T) {
	for _, count := range []int{chunkcas.MaxPortableManifestChunks, chunkcas.MaxPortableManifestChunks + 1} {
		for _, corruptSuffix := range []bool{false, true} {
			s := verificationStore(t)
			repeats := count - 2
			id, canonical, plan := uploadFixture(t, s, sampleCIR(strings.Repeat("x", repeats)))
			stream := plan.Bodies[plan.Order[0]]
			start := bytes.Index(stream, bytes.Repeat([]byte("x"), repeats))
			if start < 0 {
				t.Fatal("fixture text not found")
			}
			prefix, suffix := stream[:start], stream[start+repeats:]
			prefixID, repeatedID, suffixID := domain.HashContent(prefix), domain.HashContent([]byte("x")), domain.HashContent(suffix)
			if corruptSuffix {
				suffixID = domain.HashContent([]byte("wrong suffix label"))
			}
			plan.Manifest.Chunks = []domain.ContentHash{prefixID}
			for range repeats {
				plan.Manifest.Chunks = append(plan.Manifest.Chunks, repeatedID)
			}
			plan.Manifest.Chunks = append(plan.Manifest.Chunks, suffixID)
			plan.Bodies = map[domain.ContentHash][]byte{prefixID: prefix, repeatedID: []byte("x"), suffixID: suffix}
			writeUploadPlan(t, s, id, plan)
			for _, warm := range []bool{false, true} {
				called := false
				ok, err := s.WithVerifiedDocChunks(context.Background(), id, func(doc outbound.DocumentChunks) error {
					called = true
					checkUploadDescriptor(t, doc, id, canonical, plan)
					return nil
				})
				want := count == chunkcas.MaxPortableManifestChunks && !corruptSuffix
				if ok != want || called != want || (err != nil) != corruptSuffix {
					t.Fatalf("entries=%d corrupt=%v warm=%v: supported=%v called=%v error=%v", count, corruptSuffix, warm, ok, called, err)
				}
				if corruptSuffix && !errors.Is(err, domain.ErrHashMismatch) {
					t.Fatal("entry fallback hid corrupt suffix", err)
				}
			}
			if !corruptSuffix {
				// The ordinary planner can transport the same canonical document
				// with one chunk after source eligibility declines the tiny pieces.
				doc, err := s.GetDoc(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				body, err := domain.CanonicalBytes(doc.CIR)
				if err != nil || !bytes.Equal(body, canonical) {
					t.Fatal("fallback lost canonical identity", err)
				}
				fallback, ok := chunkcas.PlanDoc(body)
				if !ok || len(fallback.Order) != 1 || !chunkcas.PortableManifest(fallback.Manifest) {
					t.Fatal("fallback did not produce portable manifest")
				}
			}
		}
	}
}

func TestVerifiedDocChunksEncodedManifestLimit(t *testing.T) {
	for _, extra := range []int{0, 1} {
		for _, corruptSuffix := range []bool{false, true} {
			s := verificationStore(t)
			cir := sampleCIR("synthetic")
			canonical, _ := domain.CanonicalBytes(cir)
			base, _ := chunkcas.PlanDoc(canonical)
			encoded, _ := json.Marshal(base.Manifest)
			cir.Envelope.Cwd = strings.Repeat("x", chunkcas.MaxPortableManifestBytes-len(encoded)+extra)
			id, _, plan := uploadFixture(t, s, cir)
			encoded, _ = json.Marshal(plan.Manifest)
			if len(encoded) != chunkcas.MaxPortableManifestBytes+extra {
				t.Fatal("fixture missed encoded boundary", len(encoded))
			}
			if corruptSuffix {
				wrong := domain.HashContent([]byte("wrong label"))
				plan.Bodies[wrong] = plan.Bodies[plan.Order[0]]
				plan.Manifest.Chunks[0] = wrong
				writeUploadPlan(t, s, id, plan)
			}
			for _, warm := range []bool{false, true} {
				called := false
				ok, err := s.WithVerifiedDocChunks(context.Background(), id, func(outbound.DocumentChunks) error {
					called = true
					return nil
				})
				want := extra == 0 && !corruptSuffix
				if ok != want || called != want || (err != nil) != corruptSuffix {
					t.Fatalf("encoded+%d corrupt=%v warm=%v: supported=%v called=%v error=%v", extra, corruptSuffix, warm, ok, called, err)
				}
			}
		}
	}
}

func TestVerifiedDocChunksAssembledLimitCountsOccurrencesAndFraming(t *testing.T) {
	// Test the private eligibility boundary using proof metadata so exercising
	// a 512 MiB bound does not allocate/decode a 512 MiB test document. Real
	// proofs obtain these lengths only from the already verified decoded bytes.
	repeated := domain.HashContent([]byte("repeated"))
	tail := domain.HashContent([]byte("tail"))
	manifest := chunkcas.Manifest{Format: chunkcas.FormatV2, Envelope: json.RawMessage(`{}`)}
	for range 255 {
		manifest.Chunks = append(manifest.Chunks, repeated)
	}
	manifest.Chunks = append(manifest.Chunks, tail)
	raw, _ := json.Marshal(manifest)
	frame := len(chunkcas.Assemble(manifest.Envelope, nil))
	for _, extra := range []int{0, 1} {
		evidence := storedDocEvidence{raw: raw, proof: docVerificationProof{Files: []docVerifiedFile{
			{Kind: "chunks", ID: repeated, Body: repeated, BodyBytes: chunkcas.MaxPortableChunkBytes},
			{Kind: "chunks", ID: tail, Body: tail, BodyBytes: chunkcas.MaxPortableChunkBytes - frame + extra},
		}}}
		_, _, ok, err := uploadDocManifest(context.Background(), evidence)
		if err != nil || ok != (extra == 0) {
			t.Fatalf("assembled limit+%d: supported=%v error=%v", extra, ok, err)
		}
		// No subtraction overflow, and an earlier size miss cannot suppress a
		// later identity error. This is shape-level private evidence, not a MAC.
		evidence.proof.Files[0].BodyBytes = int(^uint(0) >> 1)
		_, _, ok, err = uploadDocManifest(context.Background(), evidence)
		if err != nil || ok {
			t.Fatal("size accounting overflowed", ok, err)
		}
		evidence.proof.Files[1].Body = repeated
		_, _, ok, err = uploadDocManifest(context.Background(), evidence)
		if ok || !errors.Is(err, domain.ErrHashMismatch) {
			t.Fatal("assembled limit hid a corrupt suffix", ok, err)
		}
	}
}
