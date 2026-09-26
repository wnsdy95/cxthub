package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func verificationStore(t testing.TB) *FileStore {
	t.Helper()
	s := NewFileStore(t.TempDir())
	s.EnableDocVerificationCache(filepath.Join(t.TempDir(), "private", "key"))
	return s
}

func receiptFor(t testing.TB, s *FileStore, id domain.ContentHash) docVerificationReceipt {
	t.Helper()
	raw, err := os.ReadFile(s.docReceiptPath(id))
	if err != nil {
		t.Fatal(err)
	}
	var r docVerificationReceipt
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDocVerificationReceiptSurvivesNewStoreAndRepacking(t *testing.T) {
	for _, format := range []string{"legacy", chunkcas.FormatV1, chunkcas.FormatV2} {
		t.Run(format, func(t *testing.T) {
			s := verificationStore(t)
			cb, err := domain.CanonicalBytes(bigDoc(30))
			if err != nil {
				t.Fatal(err)
			}
			id := domain.HashContent(cb)
			if format == "legacy" {
				err = writeAtomic(s.objectPath("docs", id), cb)
			} else {
				plan, ok := chunkcas.PlanDoc(cb)
				if format == chunkcas.FormatV1 {
					plan, ok = chunkcas.PlanDocV1(cb)
				}
				if !ok {
					t.Fatal("fixture plan")
				}
				for hash, body := range plan.Bodies {
					if err := writeAtomic(s.objectPath("chunks", hash), docCompress(body)); err != nil {
						t.Fatal(err)
					}
				}
				manifest, _ := json.Marshal(plan.Manifest)
				err = writeAtomic(s.objectPath("docs", id), docCompress(manifest))
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			original := receiptFor(t, s, id)
			old := time.Unix(100, 0)
			if err := os.Chtimes(s.docReceiptPath(id), old, old); err != nil {
				t.Fatal(err)
			}
			fresh := NewFileStore(s.repoRoot)
			fresh.EnableDocVerificationCache(s.docProofKeyPath)
			if err := fresh.VerifyStoredDoc(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			info, _ := os.Stat(s.docReceiptPath(id))
			if !info.ModTime().Equal(old) {
				t.Fatal("warm process repeated canonical verification and replaced its receipt")
			}
			// The same canonical content in another representation must revalidate.
			if err := writeAtomic(s.objectPath("docs", id), docCompress(cb)); err != nil {
				t.Fatal(err)
			}
			if err := fresh.VerifyStoredDoc(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			updated := receiptFor(t, s, id)
			if updated.MAC == original.MAC || len(updated.Proof.Files) != 1 {
				t.Fatal("repacked representation was not revalidated")
			}
		})
	}
}

func TestDocVerificationRejectsChangedBytesAndForgedReceipts(t *testing.T) {
	for _, damage := range []string{"doc", "chunk", "forged-mac", "missing-chunk", "symlink", "receipt-other-doc"} {
		t.Run(damage, func(t *testing.T) {
			s := verificationStore(t)
			id, err := s.PutDoc(context.Background(), domain.SessionDoc{CIR: bigDoc(30)})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			r := receiptFor(t, s, id)
			fileIndex := 1
			if damage == "doc" {
				fileIndex = 0
			}
			file := r.Proof.Files[fileIndex]
			path := s.objectPath(file.Kind, file.ID)
			raw, _ := os.ReadFile(path)
			info, _ := os.Stat(path)
			switch damage {
			case "missing-chunk":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				outside := filepath.Join(t.TempDir(), "object")
				if err := os.WriteFile(outside, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			default:
				// Same length and restored timestamp: metadata cannot certify bytes.
				raw[len(raw)/2] ^= 0x40
				if err := os.WriteFile(path, raw, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				if damage == "forged-mac" || damage == "receipt-other-doc" {
					r.Proof.Files[fileIndex].Stored = domain.HashContent(raw)
					if damage == "receipt-other-doc" {
						r.Proof.Doc = domain.HashContent([]byte("other"))
					}
					r.MAC = signDocProof(r.Proof, bytes.Repeat([]byte{1}, 32))
					forged, _ := json.Marshal(r)
					if err := writeAtomic(s.docReceiptPath(id), forged); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := s.VerifyStoredDoc(context.Background(), id); err == nil {
				t.Fatalf("accepted %s", damage)
			}
		})
	}
}

func TestDocVerificationCacheFailureFallsBackAndColdSemanticsRemainStrict(t *testing.T) {
	for _, failure := range []string{"no-key", "key-lost", "key-corrupt", "receipt-corrupt", "old-version", "cache-unwritable"} {
		t.Run(failure, func(t *testing.T) {
			s := verificationStore(t)
			id, err := s.PutDoc(context.Background(), domain.SessionDoc{CIR: bigDoc(1)})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "no-key":
				s.EnableDocVerificationCache("")
			case "key-lost":
				if err := os.Remove(s.docProofKeyPath); err != nil {
					t.Fatal(err)
				}
			case "key-corrupt":
				if err := os.WriteFile(s.docProofKeyPath, []byte("bad"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "receipt-corrupt":
				if err := writeAtomic(s.docReceiptPath(id), []byte("{")); err != nil {
					t.Fatal(err)
				}
			case "old-version":
				r := receiptFor(t, s, id)
				r.Proof.Version--
				key, _ := s.docVerificationKey()
				r.MAC = signDocProof(r.Proof, key)
				raw, _ := json.Marshal(r)
				if err := writeAtomic(s.docReceiptPath(id), raw); err != nil {
					t.Fatal(err)
				}
			case "cache-unwritable":
				if err := os.Remove(s.docReceiptPath(id)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(s.docReceiptPath(id), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Matching raw SHA alone is insufficient: reject invalid typed CIR, union
	// fields, sequence ordering and unsupported versions exactly as before.
	for _, raw := range []string{
		`{"envelope":{"cir_version":"999"},"events":[]}`,
		`{"envelope":{"cir_version":"1"},"events":[{"kind":"message","seq":2},{"kind":"message","seq":1}]}`,
		`{"envelope":{"cir_version":"1"},"events":[],"unrecognized":true}`,
		`{"envelope":{"cir_version":"1"},"events":[{"kind":"compaction","seq":1}]}`,
	} {
		s := verificationStore(t)
		id := domain.HashContent([]byte(raw))
		if err := writeAtomic(s.objectPath("docs", id), []byte(raw)); err != nil {
			t.Fatal(err)
		}
		if err := s.VerifyStoredDoc(context.Background(), id); err == nil {
			t.Fatalf("accepted %s", raw)
		}
		if _, err := os.Stat(s.docReceiptPath(id)); !os.IsNotExist(err) {
			t.Fatal("invalid CIR certified")
		}
	}
}

func TestDocVerificationCancellationAndConcurrentKeyCreation(t *testing.T) {
	s := verificationStore(t)
	id, err := s.PutDoc(context.Background(), domain.SessionDoc{CIR: bigDoc(5)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.VerifyStoredDoc(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.docReceiptPath(id)); !os.IsNotExist(err) {
		t.Fatal("canceled work certified")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			fresh := NewFileStore(s.repoRoot)
			fresh.EnableDocVerificationCache(s.docProofKeyPath)
			if err := fresh.VerifyStoredDoc(context.Background(), id); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	key, err := s.docVerificationKey()
	if err != nil || !s.matchesDocReceipt(context.Background(), id, key) {
		t.Fatalf("unusable concurrent receipt: %v", err)
	}
	if err := s.VerifyStoredDoc(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatal("warm cancellation ignored", err)
	}
}

func TestDocVerificationKeyCannotBeSuppliedByReplica(t *testing.T) {
	for _, kind := range []string{"inside", "symlink-parent", "symlink-key", "public-key"} {
		t.Run(kind, func(t *testing.T) {
			s := verificationStore(t)
			inside := filepath.Join(s.storeDir(), "private")
			if err := os.MkdirAll(inside, 0o700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "private")
			if err := os.Mkdir(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "inside":
				s.EnableDocVerificationCache(filepath.Join(inside, "key"))
			case "symlink-parent":
				link := filepath.Join(outside, "alias")
				if err := os.Symlink(inside, link); err != nil {
					t.Fatal(err)
				}
				s.EnableDocVerificationCache(filepath.Join(link, "nested", "key"))
			case "symlink-key":
				key := filepath.Join(inside, "key")
				if err := os.WriteFile(key, bytes.Repeat([]byte{1}, 32), 0o600); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(outside, "key")
				if err := os.Symlink(key, link); err != nil {
					t.Fatal(err)
				}
				s.EnableDocVerificationCache(link)
			case "public-key":
				key := filepath.Join(outside, "key")
				if err := os.WriteFile(key, bytes.Repeat([]byte{1}, 32), 0o644); err != nil {
					t.Fatal(err)
				}
				s.EnableDocVerificationCache(key)
			}
			if key, err := s.docVerificationKey(); err == nil || len(key) != 0 {
				t.Fatal("accepted unsafe key")
			}
		})
	}
}

func BenchmarkStoredDocumentVerification(b *testing.B) {
	s := verificationStore(b)
	id, err := s.PutDoc(context.Background(), domain.SessionDoc{CIR: bigDoc(250)}) // 10 MiB, synthetic
	if err != nil {
		b.Fatal(err)
	}
	for _, warm := range []bool{false, true} {
		name := "cold"
		if warm {
			name = "warm-new-store"
		}
		b.Run(name, func(b *testing.B) {
			if err := s.VerifyStoredDoc(context.Background(), id); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				fresh := NewFileStore(s.repoRoot)
				if warm {
					fresh.EnableDocVerificationCache(s.docProofKeyPath)
				}
				if err := fresh.VerifyStoredDoc(context.Background(), id); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
