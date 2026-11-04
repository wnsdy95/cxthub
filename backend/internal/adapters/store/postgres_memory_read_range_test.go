//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func memoryReadRangePGFixture(test *testing.T, count int) (*PostgresStore, context.Context, domain.MemoryReadBundle) {
	test.Helper()
	storage, ctx, repo := catalogPG(test)
	capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, count)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	memoryReadPGWrite(test, storage, ctx, bundle)
	return storage, ctx, bundle
}

func TestPGMemoryReadRangeParityOwnershipAndScope(test *testing.T) {
	storage, ctx, bundle := memoryReadRangePGFixture(test, 130)
	repo := bundle.Header.Source.Graph.RepoID
	var escaped outbound.MemoryReadChunkReader
	var escapedContext context.Context
	check := func(bound context.Context, header domain.MemoryReadHeader, reader outbound.MemoryReadChunkReader) error {
		if !reflect.DeepEqual(header, bundle.Header) {
			test.Fatal("header mismatch")
		}
		escaped, escapedContext = reader, bound
		header.Chunks[0].Hash = "invalid"
		for ordinal, chunk := range bundle.Chunks {
			expected, err := domain.DecodeMemoryReadChunk(ctx, bundle.Header, ordinal, chunk.Bytes)
			if err != nil {
				return err
			}
			items, err := reader.ReadChunk(bound, ordinal)
			if err != nil || !reflect.DeepEqual(items, expected) {
				test.Fatalf("range parity %d: %v", ordinal, err)
			}
			items[0].Text = "changed"
			again, err := reader.ReadChunk(bound, ordinal)
			if err != nil || !reflect.DeepEqual(again, expected) {
				test.Fatal("mutable output alias", err)
			}
		}
		return nil
	}
	for _, borrowed := range []bool{false, true} {
		read := func(readContext context.Context) error {
			return storage.WithMemoryReadRange(readContext, repo, bundle.ID, check)
		}
		var err error
		if borrowed {
			err = storage.WithinReadSnapshot(ctx, read)
		} else {
			err = read(ctx)
		}
		if err != nil {
			test.Fatal(err)
		}
		if items, err := escaped.ReadChunk(escapedContext, 0); !errors.Is(err, domain.ErrConflict) || items != nil {
			test.Fatal("escaped reader succeeded", err)
		}
	}
	if err := storage.WithMemoryReadRange(ctx, repo, bundle.ID, nil); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("nil callback", err)
	}
	if err := storage.WithMemoryReadRange(ctx, domain.HashContent([]byte("foreign")), bundle.ID, check); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("foreign repository", err)
	}
	if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		if err := storage.WithMemoryReadRange(write, repo, bundle.ID, check); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("write scope admitted", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		peer := &PostgresStore{pool: storage.pool}
		if err := peer.WithMemoryReadRange(read, repo, bundle.ID, check); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("foreign owner admitted", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestPGMemoryReadRangeFailureCannotBeSuppressed(test *testing.T) {
	storage, ctx, bundle := memoryReadRangePGFixture(test, 1)
	repo := bundle.Header.Source.Graph.RepoID
	for _, scenario := range []string{"ordinal", "scope", "cancel", "callback", "panic"} {
		test.Run(scenario, func(test *testing.T) {
			var retained outbound.MemoryReadChunkReader
			var retainedContext context.Context
			var recovered any
			callbackError := errors.New("callback failed")
			wanted := domain.ErrValidation
			if scenario == "scope" {
				wanted = domain.ErrConflict
			} else if scenario == "cancel" {
				wanted = context.Canceled
			} else if scenario == "callback" {
				wanted = callbackError
			}
			var result error
			func() {
				defer func() { recovered = recover() }()
				result = storage.WithMemoryReadRange(ctx, repo, bundle.ID, func(bound context.Context, _ domain.MemoryReadHeader, reader outbound.MemoryReadChunkReader) error {
					retained, retainedContext = reader, bound
					switch scenario {
					case "ordinal":
						_, _ = reader.ReadChunk(bound, -1)
					case "scope":
						_, _ = reader.ReadChunk(ctx, 0)
					case "cancel":
						canceled, cancel := context.WithCancel(bound)
						cancel()
						_, _ = reader.ReadChunk(canceled, 0)
					case "callback":
						return callbackError
					case "panic":
						panic(callbackError)
					}
					if items, err := reader.ReadChunk(bound, 0); !errors.Is(err, wanted) || items != nil {
						test.Fatal("failed scope subsequently returned data", err)
					}
					return nil
				})
			}()
			if scenario == "panic" {
				if recovered != callbackError {
					test.Fatal("panic changed", recovered)
				}
			} else if !errors.Is(result, wanted) {
				test.Fatal("callback suppressed failed read", result)
			}
			if items, err := retained.ReadChunk(retainedContext, 0); !errors.Is(err, domain.ErrConflict) || items != nil {
				test.Fatal("failed scope remains usable", err)
			}
		})
	}
}

func TestPGMemoryReadRangeIntegrityBoundaries(test *testing.T) {
	for _, scenario := range []string{"body", "hash", "start", "count", "missing", "header", "total", "capture", "unselected"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, bundle := memoryReadRangePGFixture(test, 65)
			repo := bundle.Header.Source.Graph.RepoID
			query := ""
			switch scenario {
			case "body":
				query = `UPDATE memory_read_chunks SET body=set_byte(body,10,120) WHERE repo_id=$1 AND read_id=$2 AND ordinal=0`
			case "hash":
				query = `UPDATE memory_read_chunks SET chunk_hash='sha256:'||repeat('a',64) WHERE repo_id=$1 AND read_id=$2 AND ordinal=0`
			case "start":
				query = `UPDATE memory_read_chunks SET start_item=start_item+1 WHERE repo_id=$1 AND read_id=$2 AND ordinal=0`
			case "count":
				query = `UPDATE memory_read_chunks SET item_count=item_count-1 WHERE repo_id=$1 AND read_id=$2 AND ordinal=0`
			case "missing":
				query = `DELETE FROM memory_read_chunks WHERE repo_id=$1 AND read_id=$2 AND ordinal=0`
			case "header":
				query = `UPDATE memory_read_headers SET header=set_byte(header,10,120) WHERE repo_id=$1 AND read_id=$2`
			case "total":
				query = `UPDATE memory_read_headers SET body_bytes=body_bytes+1 WHERE repo_id=$1 AND read_id=$2`
			case "capture":
				query = `UPDATE memory_read_headers SET capture_id='sha256:'||repeat('a',64) WHERE repo_id=$1 AND read_id=$2`
			case "unselected":
				query = `DELETE FROM memory_read_chunks WHERE repo_id=$1 AND read_id=$2 AND ordinal=1`
			}
			memoryBundlePGFault(test, storage, ctx, query, string(repo), string(bundle.ID))
			err := storage.WithMemoryReadRange(ctx, repo, bundle.ID, func(bound context.Context, _ domain.MemoryReadHeader, reader outbound.MemoryReadChunkReader) error {
				_, err := reader.ReadChunk(bound, 0)
				return err
			})
			if scenario == "unselected" {
				if err != nil {
					test.Fatal("selected range scanned unrelated index", err)
				}
				if _, err := storage.ReadMemoryReadHeader(ctx, repo, bundle.ID); !errors.Is(err, domain.ErrIntegrity) {
					test.Fatal("full audit ignored missing unselected row", err)
				}
				return
			}
			if !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("corrupt range accepted", err)
			}
		})
	}
}

func TestPGMemoryReadRangeSnapshotAndFinalExpiry(test *testing.T) {
	for _, scenario := range []string{"prune", "expiry", "empty-expiry", "cancel"} {
		test.Run(scenario, func(test *testing.T) {
			count := 1
			if scenario == "empty-expiry" {
				count = 0
			}
			storage, ctx, bundle := memoryReadRangePGFixture(test, count)
			repo := bundle.Header.Source.Graph.RepoID
			if strings.Contains(scenario, "expiry") {
				memoryReadPGExpireSoon(test, storage, ctx, repo, bundle.Header.CaptureID)
			}
			parent, cancel := context.WithCancel(ctx)
			defer cancel()
			err := storage.WithMemoryReadRange(parent, repo, bundle.ID, func(bound context.Context, _ domain.MemoryReadHeader, reader outbound.MemoryReadChunkReader) error {
				if scenario == "prune" {
					catalogExecPG(test, ctx, storage.pool, `DELETE FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(bundle.Header.CaptureID))
				}
				if count > 0 {
					items, err := reader.ReadChunk(bound, 0)
					if err != nil || len(items) != count {
						test.Fatal("snapshot fractured", err)
					}
				}
				if scenario == "cancel" {
					cancel()
				} else if strings.Contains(scenario, "expiry") {
					memoryReadPGWaitExpiry(test, storage, ctx, repo, bundle.Header.CaptureID)
				}
				return nil
			})
			if scenario == "prune" && err != nil {
				test.Fatal("owned repeatable read lost pruned result", err)
			}
			if strings.Contains(scenario, "expiry") && !errors.Is(err, domain.ErrMemoryCaptureExpired) {
				test.Fatal("expired operation returned success", err)
			}
			if scenario == "cancel" && !errors.Is(err, context.Canceled) {
				test.Fatal("canceled operation returned success", err)
			}
		})
	}
}

func TestPGMemoryReadRangeConcurrentReadsAndWorkCap(test *testing.T) {
	storage, ctx, bundle := memoryReadRangePGFixture(test, 130)
	repo := bundle.Header.Source.Graph.RepoID
	if err := storage.WithMemoryReadRange(ctx, repo, bundle.ID, func(bound context.Context, _ domain.MemoryReadHeader, reader outbound.MemoryReadChunkReader) error {
		var workers sync.WaitGroup
		failures := make(chan error, 12)
		for index := range 12 {
			workers.Go(func() {
				_, err := reader.ReadChunk(bound, index%len(bundle.Chunks))
				failures <- err
			})
		}
		workers.Wait()
		close(failures)
		for err := range failures {
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	err := storage.WithMemoryReadRange(ctx, repo, bundle.ID, func(bound context.Context, _ domain.MemoryReadHeader, reader outbound.MemoryReadChunkReader) error {
		for range memoryReadRangeChunkLimit {
			if _, err := reader.ReadChunk(bound, 2); err != nil {
				test.Fatal("premature read cap", err)
			}
		}
		if _, err := reader.ReadChunk(bound, 2); !errors.Is(err, domain.ErrMemoryProjectionLimit) {
			test.Fatal("read cap not enforced", err)
		}
		return nil
	})
	if !errors.Is(err, domain.ErrMemoryProjectionLimit) {
		test.Fatal("read cap was suppressible", err)
	}
}
