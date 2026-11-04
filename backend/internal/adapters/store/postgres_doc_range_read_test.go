//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docRangeReadTraceKey struct{}
type docRangeReadCounts struct {
	headers atomic.Int64
	pages   atomic.Int64
}
type docRangeReadTracer struct{}

func (docRangeReadTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if counts, ok := ctx.Value(docRangeReadTraceKey{}).(*docRangeReadCounts); ok {
		if strings.Contains(query.SQL, "i.header") {
			counts.headers.Add(1)
		} else if strings.Contains(query.SQL, "substring(p.bytes") {
			counts.pages.Add(1)
		}
	}
	return ctx
}

func (docRangeReadTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestDocRangePGPinnedHeaderReadCost(t *testing.T) {
	for _, events := range []int{1280, 12800} {
		t.Run(fmt.Sprint(events), func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := rootPGRepo(t, ctx, storage)
			fixture := rangeFixture(t, events)
			seedRootPG(t, ctx, storage, repo, fixture)
			if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
				t.Fatal(err)
			}
			config := storage.pool.Config()
			config.ConnConfig.Tracer = docRangeReadTracer{}
			pool, err := pgxpool.NewWithConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			reader := &PostgresStore{pool: pool}
			defer reader.Close()
			positions := []int{0, 1, events/domain.DocRangePageEvents - 1}
			var expected [][]domain.DocRangeEntry
			legacy := &docRangeReadCounts{}
			if err := reader.WithinReadSnapshot(context.WithValue(ctx, docRangeReadTraceKey{}, legacy), func(read context.Context) error {
				header, err := reader.DocRangeIndex(read, repo, fixture.hash)
				if err != nil {
					return err
				}
				for _, position := range positions {
					entries, err := reader.DocRangePage(read, repo, header, position)
					if err != nil {
						return err
					}
					expected = append(expected, entries)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			pinned := &docRangeReadCounts{}
			if err := reader.WithinReadSnapshot(context.WithValue(ctx, docRangeReadTraceKey{}, pinned), func(read context.Context) error {
				index, err := reader.OpenDocRange(read, repo, fixture.hash)
				if err != nil {
					return err
				}
				for offset, position := range positions {
					entries, err := index.ReadPage(read, position)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(entries, expected[offset]) {
						t.Fatal("pinned reader changed page", position)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if legacy.headers.Load() != 4 || pinned.headers.Load() != 1 || pinned.pages.Load() != 3 {
				t.Fatalf("metadata reads: legacy=%d pinned=%d page-only=%d", legacy.headers.Load(), pinned.headers.Load(), pinned.pages.Load())
			}
			var metadataBytes int64
			if err := pool.QueryRow(ctx, `SELECT octet_length(b.bytes)+octet_length(i.header)
 FROM blobs b JOIN doc_range_indexes_v1 i ON i.hash=b.hash WHERE b.hash=$1`, fixture.hash).Scan(&metadataBytes); err != nil {
				t.Fatal(err)
			}
			t.Logf("events=%d selected_pages=3 root/header_reads=4->1 root/header_bytes=%d->%d", events, 4*metadataBytes, metadataBytes)
		})
	}
}

func TestDocRangePGPinnedScopeAndOwnedHeader(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	foreign, _ := rootPGRepo(t, ctx, storage)
	fixture := rangeFixture(t, 129)
	seedRootPG(t, ctx, storage, repo, fixture)
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.OpenDocRange(ctx, repo, fixture.hash); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("unbound read was allowed", err)
	}
	if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		return storage.WithinReadSnapshot(write, func(read context.Context) error {
			if _, err := storage.OpenDocRange(read, repo, fixture.hash); !errors.Is(err, domain.ErrConflict) {
				t.Fatal("write transaction admitted pinned reader", err)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	peer, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	var retained outbound.DocRangeRead
	var retainedContext context.Context
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		if _, err := peer.OpenDocRange(read, repo, fixture.hash); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("foreign transaction admitted reader", err)
		}
		if _, err := storage.OpenDocRange(read, foreign, fixture.hash); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("foreign repository admitted reader", err)
		}
		index, err := storage.OpenDocRange(read, repo, fixture.hash)
		if err != nil {
			return err
		}
		retained, retainedContext = index, read
		original := index.Header()
		changed := index.Header()
		changed.Pages[0] = domain.HashContent([]byte("forged page"))
		changed.Manifest.Envelope[0] = '!'
		changed.Manifest.Chunks[0].Bytes++
		if !reflect.DeepEqual(index.Header(), original) {
			t.Fatal("caller mutated pinned proof")
		}
		entries, err := index.ReadPage(read, 0)
		if err != nil || len(entries) != domain.DocRangePageEvents {
			t.Fatal("isolated header affected page", err)
		}
		entries[0].Hash = "mutated entry"
		again, err := index.ReadPage(read, 0)
		if err != nil || again[0].Hash == entries[0].Hash {
			t.Fatal("caller mutated later page", err)
		}
		for _, position := range []int{-1, len(original.Pages)} {
			if _, err := index.ReadPage(read, position); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("out-of-range page admitted", position, err)
			}
		}
		if _, err := index.ReadPage(ctx, 0); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("reader escaped its snapshot", err)
		}
		canceled, cancel := context.WithCancel(read)
		cancel()
		if _, err := index.ReadPage(canceled, 0); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled read succeeded", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := retained.ReadPage(retainedContext, 0); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal("reader used a completed transaction", err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		if _, err := retained.ReadPage(read, 0); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("reader reused across requests", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDocRangePGPinnedGenerationAndFreshValidation(t *testing.T) {
	for _, damage := range []string{"root", "header", "page", "missing-page", "ownership"} {
		t.Run(damage, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := rootPGRepo(t, ctx, storage)
			fixture := rangeFixture(t, 129)
			seedRootPG(t, ctx, storage, repo, fixture)
			if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
				t.Fatal(err)
			}
			if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
				index, err := storage.OpenDocRange(read, repo, fixture.hash)
				if err != nil {
					return err
				}
				switch damage {
				case "root":
					_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, []byte("corrupt root"))
				case "header":
					_, err = storage.pool.Exec(ctx, `UPDATE doc_range_indexes_v1 SET header=$2 WHERE hash=$1`, fixture.hash, []byte("{}"))
				case "page":
					_, err = storage.pool.Exec(ctx, `UPDATE doc_range_pages_v1 SET bytes=$2 WHERE doc_hash=$1 AND ordinal=0`, fixture.hash, []byte("[]"))
				case "missing-page":
					_, err = storage.pool.Exec(ctx, `DELETE FROM doc_range_pages_v1 WHERE doc_hash=$1 AND ordinal=0`, fixture.hash)
				case "ownership":
					_, err = storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, fixture.hash)
				}
				if err != nil {
					return err
				}
				entries, err := index.ReadPage(read, 0)
				if err != nil || len(entries) != 128 {
					t.Fatal("mixed generations in an already-pinned read", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
				index, err := storage.OpenDocRange(read, repo, fixture.hash)
				if err != nil {
					return err
				}
				_, err = index.ReadPage(read, 0)
				return err
			})
			if err == nil {
				t.Fatal("a new request reused stale validation")
			}
			if damage == "ownership" && !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("ownership error", err)
			}
		})
	}
}
