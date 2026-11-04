//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func memoryReadPGFixture(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, count int) (domain.MemoryCaptureBundle, domain.MemoryReadBundle) {
	test.Helper()
	capture := memoryBundlePGFixture(test, storage, ctx, repo)
	header := domain.MemoryReadHeader{
		Version: domain.MemoryReadVersion, Rules: domain.MemoryReadRules, CaptureID: capture.ID,
		Source: capture.Header.Generation, Origin: "memory-read-store-test",
		View: domain.EffectiveMemoryPage{Selection: capture.Header.Selection, Content: capture.Header.Content,
			StateHash: domain.HashContent([]byte("state")), LineageHash: domain.HashContent([]byte("lineage"))},
	}
	items := make([]domain.EffectiveMemoryItem, count)
	for ordinal := range items {
		items[ordinal] = domain.EffectiveMemoryItem{ID: domain.HashContent([]byte(fmt.Sprint(ordinal))), SourceSnapshot: repo,
			Kind: "rationale", Text: fmt.Sprintf("bounded result %04d", ordinal),
			MemoryClaimAssessment: domain.MemoryClaimAssessment{State: "retained", Reason: "historical_knowledge"}}
	}
	return capture, memoryReadPGSeal(test, ctx, header, items)
}

func memoryReadPGSeal(test *testing.T, ctx context.Context, header domain.MemoryReadHeader, items []domain.EffectiveMemoryItem) domain.MemoryReadBundle {
	test.Helper()
	header.Chunks = nil
	header.View.Total = len(items)
	var err error
	header.View.DeliveryStateHash, err = domain.EffectiveMemoryDeliveryStateHash(ctx, header.Source.Graph.RepoID, header.Origin, header.View, items)
	if err != nil {
		test.Fatal(err)
	}
	bundle, err := domain.BuildMemoryReadBundle(ctx, header, items)
	if err != nil {
		test.Fatal(err)
	}
	return bundle
}

func memoryReadPGWrite(test *testing.T, storage *PostgresStore, ctx context.Context, bundle domain.MemoryReadBundle) {
	test.Helper()
	if err := storage.WriteMemoryRead(ctx, bundle); err != nil {
		test.Fatal(err)
	}
}

func memoryReadPGAssert(test *testing.T, storage *PostgresStore, ctx context.Context, bundle domain.MemoryReadBundle) {
	test.Helper()
	repo := bundle.Header.Source.Graph.RepoID
	header, err := storage.ReadMemoryReadHeader(ctx, repo, bundle.ID)
	if err != nil || !reflect.DeepEqual(header, bundle.Header) {
		test.Fatalf("header changed: %v", err)
	}
	for ordinal, expected := range bundle.Chunks {
		actual, err := storage.ReadMemoryReadChunk(ctx, repo, bundle.ID, ordinal)
		if err != nil || !bytes.Equal(actual, expected.Bytes) {
			test.Fatalf("chunk %d changed: %v", ordinal, err)
		}
	}
}

func memoryReadPGCounts(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash) [2]int {
	test.Helper()
	var counts [2]int
	if err := storage.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM memory_read_headers WHERE repo_id=$1),
		(SELECT count(*) FROM memory_read_chunks WHERE repo_id=$1)`, string(repo)).Scan(&counts[0], &counts[1]); err != nil {
		test.Fatal(err)
	}
	return counts
}

func TestPGMemoryReadRestartIdempotenceAndIsolation(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, 130)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	before := memorySourceGenerationPG(test, ctx, storage, repo)
	var workers sync.WaitGroup
	failures := make(chan error, 3)
	for range 3 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			failures <- storage.WriteMemoryRead(ctx, bundle)
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			test.Fatal(err)
		}
	}
	if counts := memoryReadPGCounts(test, storage, ctx, repo); counts != [2]int{1, 3} {
		test.Fatal("duplicate publication", counts)
	}
	if after := memorySourceGenerationPG(test, ctx, storage, repo); after != before {
		test.Fatal("result storage changed source generation")
	}
	foreign := memorySourceRepoPG(test, ctx, storage, "result foreign")
	if _, err := storage.ReadMemoryReadHeader(ctx, foreign, bundle.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("foreign header", err)
	}
	if _, err := storage.ReadMemoryReadChunk(ctx, foreign, bundle.ID, 0); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("foreign chunk", err)
	}
	storage.Close()
	peer, err := NewPostgresStore(ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(peer.Close)
	memoryReadPGAssert(test, peer, ctx, bundle)
	memoryReadPGWrite(test, peer, ctx, bundle)
	if applied, err := peer.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil || applied != 0 {
		test.Fatal("migration restart", applied, err)
	}
}

func TestPGMemoryReadEmptyAndInvalidOrdinals(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, 0)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	memoryReadPGWrite(test, storage, ctx, bundle)
	memoryReadPGWrite(test, storage, ctx, bundle)
	memoryReadPGAssert(test, storage, ctx, bundle)
	if counts := memoryReadPGCounts(test, storage, ctx, repo); counts != [2]int{1, 0} {
		test.Fatal("empty result", counts)
	}
	for _, ordinal := range []int{-1, 0, domain.MaxMemoryReadChunks} {
		if raw, err := storage.ReadMemoryReadChunk(ctx, repo, bundle.ID, ordinal); !errors.Is(err, domain.ErrValidation) || raw != nil {
			test.Fatal("invalid ordinal", ordinal, err)
		}
	}
}

func TestPGMemoryReadBorrowedTransactions(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, 1)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	memoryReadPGWrite(test, storage, ctx, bundle)
	for _, readonly := range []bool{false, true} {
		check := func(bound context.Context) error {
			peer := &PostgresStore{pool: storage.pool}
			for _, candidate := range []*PostgresStore{storage, peer} {
				if err := candidate.WriteMemoryRead(bound, bundle); !errors.Is(err, domain.ErrConflict) {
					test.Fatal("borrowed write", err)
				}
				if readonly && candidate == storage {
					memoryReadPGAssert(test, candidate, bound, bundle)
					continue
				}
				if _, err := candidate.ReadMemoryReadHeader(bound, repo, bundle.ID); !errors.Is(err, domain.ErrConflict) {
					test.Fatal("borrowed header", err)
				}
				if raw, err := candidate.ReadMemoryReadChunk(bound, repo, bundle.ID, 0); !errors.Is(err, domain.ErrConflict) || raw != nil {
					test.Fatal("borrowed chunk", err)
				}
			}
			return nil
		}
		var err error
		if readonly {
			err = storage.WithinReadSnapshot(ctx, check)
		} else {
			err = storage.WithinRepository(ctx, repo, check)
		}
		if err != nil {
			test.Fatal(err)
		}
	}
	transaction := catalogTxPG(test, ctx, storage)
	for _, borrowed := range []*repositoryTx{nil, {owner: storage, readOnly: true},
		{Tx: transaction, owner: storage, readOnly: true, repo: domain.HashContent([]byte("other"))}} {
		bound := context.WithValue(ctx, repositoryTxKey{}, borrowed)
		if err := storage.WriteMemoryRead(bound, bundle); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("invalid borrowed write", err)
		}
		if _, err := storage.ReadMemoryReadHeader(bound, repo, bundle.ID); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("invalid borrowed header", err)
		}
	}
}

func TestPGMemoryReadSourceSelectionContentAssociation(test *testing.T) {
	for _, scenario := range []string{"source", "selection", "content", "capture"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			capture, original := memoryReadPGFixture(test, storage, ctx, repo, 0)
			memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
			header := original.Header
			switch scenario {
			case "source":
				header.Source.Sequence++
			case "selection":
				header.View.Selection.CodeCommit = strings.Repeat("b", 40)
			case "content":
				header.View.Content = "prompt"
			case "capture":
				header.CaptureID = domain.HashContent([]byte("missing capture"))
			}
			bundle := memoryReadPGSeal(test, ctx, header, nil)
			expected := domain.ErrIntegrity
			if scenario == "capture" {
				expected = domain.ErrNotFound
			}
			if err := storage.WriteMemoryRead(ctx, bundle); !errors.Is(err, expected) {
				test.Fatal("mismatched association accepted", err)
			}
			if counts := memoryReadPGCounts(test, storage, ctx, repo); counts != [2]int{} {
				test.Fatal("mismatch left rows", counts)
			}
		})
	}
}

func TestPGMemoryReadCaptureExpiryAndCascade(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, 65)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	memoryReadPGWrite(test, storage, ctx, bundle)
	memoryBundlePGExpire(test, storage, ctx, repo, capture.ID)
	if _, err := storage.ReadMemoryReadHeader(ctx, repo, bundle.ID); !errors.Is(err, domain.ErrMemoryCaptureExpired) {
		test.Fatal("expired header", err)
	}
	if raw, err := storage.ReadMemoryReadChunk(ctx, repo, bundle.ID, 0); !errors.Is(err, domain.ErrMemoryCaptureExpired) || raw != nil {
		test.Fatal("expired chunk", err)
	}
	if err := storage.WriteMemoryRead(ctx, bundle); !errors.Is(err, domain.ErrMemoryCaptureExpired) {
		test.Fatal("expired retry", err)
	}
	next := bundle
	next.Header.Origin += "next"
	next = memoryReadPGSeal(test, ctx, next.Header, nil)
	if err := storage.WriteMemoryRead(ctx, next); !errors.Is(err, domain.ErrMemoryCaptureExpired) {
		test.Fatal("new result on expired capture", err)
	}
	if count, err := storage.PruneMemoryCaptures(ctx, repo, 1); err != nil || count != 1 {
		test.Fatal("prune", count, err)
	}
	if counts := memoryReadPGCounts(test, storage, ctx, repo); counts != [2]int{} {
		test.Fatal("capture left result orphans", counts)
	}
	if _, err := storage.ReadMemoryReadHeader(ctx, repo, bundle.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("pruned header", err)
	}
}
