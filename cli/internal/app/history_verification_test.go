package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func historyVerificationFixture(t testing.TB, size int) (*storage.FileStore, domain.HistoryEvent, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	store := storage.NewFileStore(root)
	store.EnableDocVerificationCache(filepath.Join(t.TempDir(), "private", "key"))
	cir := domain.CIRDocument{Envelope: domain.Envelope{SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CIRVersion: "1"}, Events: []domain.Event{{Seq: 1, Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("synthetic context ", size/18+1)}}}}}
	id, err := store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	repo := string(domain.HashContent([]byte("history-verification")))
	memory, err := store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: id, Summary: "verified project rules"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main", MemoryHash: memory}); err != nil {
		t.Fatal(err)
	}
	e := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo, Branch: "main", BranchID: "generation", Kind: "position", Source: id, Target: id, SharedTarget: id, MemorySource: id, CreatedAt: time.Unix(1, 0).UTC()}
	return store, e, root
}

type historyReadCounter struct {
	outbound.SessionStore
	reads     int
	wrongHash bool
}

func (s *historyReadCounter) GetDoc(ctx context.Context, id domain.ContentHash) (domain.SessionDoc, error) {
	s.reads++
	d, err := s.SessionStore.GetDoc(ctx, id)
	if s.wrongHash {
		d.Hash = domain.HashContent([]byte("foreign"))
	}
	return d, err
}

type historyProofCounter struct {
	*historyReadCounter
	verifier   outbound.StoredDocumentVerifier
	proofs     int
	proofError error
}

func (s *historyProofCounter) VerifyStoredDoc(ctx context.Context, id domain.ContentHash) error {
	s.proofs++
	if s.proofError != nil {
		return s.proofError
	}
	return s.verifier.VerifyStoredDoc(ctx, id)
}

func TestHistorySourceReusesExactProofWithinOneOperation(t *testing.T) {
	store, e, _ := historyVerificationFixture(t, 1024)
	count := &historyProofCounter{historyReadCounter: &historyReadCounter{SessionStore: store}, verifier: store}
	svc := NewContextHistoryService(count, store)
	for i := 1; i <= 2; i++ {
		got, err := svc.ValidateHistorySource(context.Background(), e)
		if err != nil {
			t.Fatal(err)
		}
		if got.MemoryHash == "" || !got.MemoryPinned {
			t.Fatal("memory provenance lost")
		}
		if count.reads != 0 || count.proofs != i {
			t.Fatalf("verification calls after operation %d: materialized=%d verified=%d", i, count.reads, count.proofs)
		}
	}
	// A subsequent operation must verify current bytes, never a process-lifetime bool.
	count.proofError = domain.ErrHashMismatch
	if _, err := svc.ValidateHistorySource(context.Background(), e); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("stale successful proof reused: %v", err)
	}
}

func TestHistorySourceFallbackAndRepositoryIsolation(t *testing.T) {
	store, e, _ := historyVerificationFixture(t, 1024)
	counter := &historyReadCounter{SessionStore: store}
	svc := NewContextHistoryService(counter, store)
	if _, err := svc.ValidateHistorySource(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if counter.reads != 1 {
		t.Fatalf("fallback decoded one document %d times", counter.reads)
	}
	counter.wrongHash = true
	if _, err := svc.ValidateHistorySource(context.Background(), e); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("wrong document accepted: %v", err)
	}
	count := &historyProofCounter{historyReadCounter: &historyReadCounter{SessionStore: store}, verifier: store}
	e.RepoID = string(domain.HashContent([]byte("foreign repository")))
	if _, err := NewContextHistoryService(count, store).ValidateHistorySource(context.Background(), e); !errors.Is(err, domain.ErrHashMismatch) || count.proofs != 0 {
		t.Fatalf("ownership bypass: %d %v", count.proofs, err)
	}
}

func TestHistorySourceWarmReceiptRejectsModifiedStorage(t *testing.T) {
	for _, remove := range []bool{false, true} {
		name := "changed bytes with same size and time"
		if remove {
			name = "deleted"
		}
		t.Run(name, func(t *testing.T) {
			store, e, root := historyVerificationFixture(t, 1<<20)
			svc := NewContextHistoryService(store, store)
			ctx := context.Background()
			if _, err := svc.ValidateHistorySource(ctx, e); err != nil {
				t.Fatal(err)
			}
			receipt := filepath.Join(root, ".cxt", "doc-verification", strings.TrimPrefix(string(e.Source), "sha256:")+".json")
			if _, err := os.Stat(receipt); err != nil {
				t.Fatalf("not a warm receipt: %v", err)
			}
			if remove {
				if err := store.DeleteDoc(ctx, e.Source); err != nil {
					t.Fatal(err)
				}
			} else {
				path := filepath.Join(root, ".cxt", "objects", "docs", strings.TrimPrefix(string(e.Source), "sha256:"))
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw[0] ^= 1
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := svc.ValidateHistorySource(ctx, e); err == nil {
				t.Fatal("modified document accepted through receipt")
			}
		})
	}
}

func TestHistorySourceCancellationCannotCertifyDocument(t *testing.T) {
	store, e, _ := historyVerificationFixture(t, 1024)
	count := &historyProofCounter{historyReadCounter: &historyReadCounter{SessionStore: store}, verifier: store}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewContextHistoryService(count, store).ValidateHistorySource(ctx, e); !errors.Is(err, context.Canceled) || count.proofs != 0 {
		t.Fatalf("cancelled validation: proofs=%d err=%v", count.proofs, err)
	}
}

func BenchmarkHistorySourceVerification(b *testing.B) {
	for _, warm := range []bool{false, true} {
		name := "cold"
		if warm {
			name = "warm"
		}
		b.Run(name, func(b *testing.B) {
			store, e, root := historyVerificationFixture(b, 16<<20)
			if !warm {
				store.EnableDocVerificationCache("")
			} else if err := store.VerifyStoredDoc(context.Background(), e.Source); err != nil {
				b.Fatal(err)
			}
			if warm {
				if _, err := os.Stat(filepath.Join(root, ".cxt", "doc-verification", strings.TrimPrefix(string(e.Source), "sha256:")+".json")); err != nil {
					b.Fatalf("warm fixture has no authenticated receipt: %v", err)
				}
			}
			svc := NewContextHistoryService(store, store)
			b.ReportAllocs()
			b.SetBytes(16 << 20)
			b.ResetTimer()
			for b.Loop() {
				if _, err := svc.ValidateHistorySource(context.Background(), e); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
