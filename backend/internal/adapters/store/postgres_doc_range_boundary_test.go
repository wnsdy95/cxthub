//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func docRangeBoundaryPGPair(t *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, count, mismatch int) [2]rootReaderFixture {
	t.Helper()
	first := rangeFixture(t, count)
	var second rootReaderFixture
	if err := json.Unmarshal(first.canonical, &second.cir); err != nil {
		t.Fatal(err)
	}
	if mismatch < count {
		second.cir.Events[mismatch].Blocks[0].Text += " changed"
	} else {
		second.cir.Events = append(second.cir.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: count + 1, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "suffix"}}})
	}
	var err error
	second.manifest, second.bodies, err = domain.ConversationManifestForCIR(second.cir)
	if err != nil {
		t.Fatal(err)
	}
	second.hash, err = domain.ConversationManifestHash(second.manifest)
	if err != nil {
		t.Fatal(err)
	}
	second.manifestBytes, err = domain.CanonicalConversationManifest(second.manifest)
	if err != nil {
		t.Fatal(err)
	}
	second.canonical, err = domain.CanonicalBytes(second.cir)
	if err != nil {
		t.Fatal(err)
	}
	pair := [2]rootReaderFixture{first, second}
	for _, fixture := range pair {
		seedRootPG(t, ctx, storage, repo, fixture)
		if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: fixture.hash, RepoID: repo, DocHash: fixture.hash, DocIdentity: domain.DocumentIdentityRootV1, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
			t.Fatal(err)
		}
		if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
			t.Fatal(err)
		}
	}
	return pair
}

func docRangeBoundaryPGEnqueue(t *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, pair [2]rootReaderFixture) domain.DocRangeIndexJob {
	t.Helper()
	job, err := storage.EnqueueDocRangeBoundary(ctx, repo, pair[0].hash, pair[1].hash, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func docRangeBoundaryPGClaim(t *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, pair [2]rootReaderFixture) domain.DocRangeIndexJob {
	t.Helper()
	job := docRangeJobPGClaim(t, storage, ctx, repo, pair[0].hash)
	if job.BaseHash != pair[1].hash {
		t.Fatal("wrong base claimed", job)
	}
	return job
}

func docRangeBoundaryPGGet(t *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, pair [2]rootReaderFixture) domain.DocRangeIndexJob {
	t.Helper()
	job, err := scanDocRangeJob(storage.pool.QueryRow(ctx, `SELECT `+docRangeJobColumns+` FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, repo, pair[0].hash, pair[1].hash))
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func docRangeBoundaryPGRead(t *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, pair [2]rootReaderFixture, want int, wantErr error) {
	t.Helper()
	err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		got, err := storage.DocRangeBoundary(read, repo, pair[0].hash, pair[1].hash)
		if !errors.Is(err, wantErr) || got != want {
			t.Fatalf("boundary=%d error=%v, want=%d error=%v", got, err, want, wantErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDocRangeBoundaryPGExactAndSameDocument(t *testing.T) {
	for _, mismatch := range []int{0, 1, 127, 128, 256, 257} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := docRangeJobPGRepo(t, storage, ctx)
			pair := docRangeBoundaryPGPair(t, storage, ctx, repo, 257, mismatch)
			docRangeBoundaryPGRead(t, storage, ctx, repo, pair, 0, domain.ErrDocRangeIndexUnavailable)
			docRangeBoundaryPGRead(t, storage, ctx, repo, [2]rootReaderFixture{pair[0], pair[0]}, 257, nil)
			docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
			claim := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
			if err := storage.BuildDocRangeJob(ctx, claim); err != nil {
				t.Fatal(err)
			}
			docRangeBoundaryPGRead(t, storage, ctx, repo, pair, mismatch, nil)
			receipt := docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
			if receipt.State != "completed" || receipt.Attempts != 1 || receipt.Version != claim.Version {
				t.Fatal("completed proof was not reused", receipt)
			}
			if err := storage.BuildDocRangeJob(ctx, claim); !errors.Is(err, domain.ErrConflict) {
				t.Fatal("duplicate claim published", err)
			}
			docRangeBoundaryPGRead(t, storage, ctx, repo, [2]rootReaderFixture{pair[1], pair[0]}, 0, domain.ErrDocRangeIndexUnavailable)
		})
	}
}

type docRangeBoundaryReadTracer struct {
	objects   atomic.Int64
	queries   atomic.Int64
	unbounded atomic.Bool
}

func (trace *docRangeBoundaryReadTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	trace.queries.Add(1)
	if strings.Contains(query.SQL, "FROM doc_range_objects_v2") {
		trace.objects.Add(1)
	}
	if strings.Contains(query.SQL, "doc_range_indexes_v1") || strings.Contains(query.SQL, "doc_read_indexes_v3") || strings.Contains(query.SQL, "blobs") && strings.Contains(query.SQL, "bytes") {
		trace.unbounded.Store(true)
	}
	return ctx
}

func (*docRangeBoundaryReadTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func docRangeBoundaryPGTracedStore(t *testing.T, storage *PostgresStore, ctx context.Context, trace pgx.QueryTracer) *PostgresStore {
	t.Helper()
	config := storage.pool.Config()
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	peer := &PostgresStore{pool: pool}
	t.Cleanup(peer.Close)
	return peer
}

func TestDocRangeBoundaryPGBoundedResumptionAcrossStores(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	batchEvents := domain.DocRangePrefixBatchPages * domain.DocRangePageEvents
	want := 2*batchEvents + 9
	pair := docRangeBoundaryPGPair(t, storage, ctx, repo, want+8, want)
	docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
	for batch := 1; batch <= 3; batch++ {
		trace := &docRangeBoundaryReadTracer{}
		peer := docRangeBoundaryPGTracedStore(t, storage, ctx, trace)
		claim := docRangeBoundaryPGClaim(t, peer, ctx, repo, pair)
		if err := peer.BuildDocRangeJob(ctx, claim); err != nil {
			t.Fatal(err)
		}
		checkpoint, exists, err := readDocRangeBoundaryCheckpoint(ctx, storage.pool, repo, pair[0].hash, pair[1].hash, false)
		if err != nil || !exists || checkpoint.events != min(batch*batchEvents, want) || checkpoint.complete != (batch == 3) || checkpoint.jobVersion != claim.Version {
			t.Fatalf("batch %d checkpoint: %+v %v", batch, checkpoint, err)
		}
		if trace.unbounded.Load() || trace.objects.Load() > 2*(domain.DocRangePrefixBatchPages+4) {
			t.Fatalf("unbounded batch reads: old=%v objects=%d", trace.unbounded.Load(), trace.objects.Load())
		}
		t.Logf("batch=%d checkpoint=%d complete=%v authenticated-object-queries=%d", batch, checkpoint.events, checkpoint.complete, trace.objects.Load())
		job := docRangeBoundaryPGGet(t, storage, ctx, repo, pair)
		attempts := 0
		if batch == 3 {
			attempts = 1
		}
		if job.Attempts != attempts || job.Version != claim.Version || !job.LeaseUntil.IsZero() {
			t.Fatal("claim accounting", job)
		}
		if batch < 3 {
			if job.State != "waiting" || time.Until(job.NextAttempt) > time.Second {
				t.Fatal("incomplete batch did not immediately requeue", job)
			}
			docRangeBoundaryPGRead(t, storage, ctx, repo, pair, 0, domain.ErrDocRangeIndexUnavailable)
		} else {
			if job.State != "completed" {
				t.Fatal(job)
			}
			docRangeBoundaryPGRead(t, storage, ctx, repo, pair, want, nil)
		}
	}
}

func TestDocRangeBoundaryPGWarmReadCostIndependentOfPrefix(t *testing.T) {
	for _, count := range []int{1000, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := docRangeJobPGRepo(t, storage, ctx)
			pair := docRangeBoundaryPGPair(t, storage, ctx, repo, count, count-5)
			docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
			for range 2 {
				claim := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
				if err := storage.BuildDocRangeJob(ctx, claim); err != nil {
					t.Fatal(err)
				}
				if docRangeBoundaryPGGet(t, storage, ctx, repo, pair).State == "completed" {
					break
				}
			}
			trace := &docRangeBoundaryReadTracer{}
			peer := docRangeBoundaryPGTracedStore(t, storage, ctx, trace)
			if err := peer.WithinReadSnapshot(ctx, func(read context.Context) error {
				trace.queries.Store(0)
				for range 3 {
					got, err := peer.DocRangeBoundary(read, repo, pair[0].hash, pair[1].hash)
					if err != nil || got != count-5 {
						t.Fatal("warm exact boundary", got, err)
					}
				}
				if trace.queries.Load() != 6 || trace.objects.Load() != 0 || trace.unbounded.Load() {
					t.Fatalf("prefix replayed: queries=%d objects=%d bodies=%v", trace.queries.Load(), trace.objects.Load(), trace.unbounded.Load())
				}
				t.Logf("events=%d warm-reads=3 queries=6 object-queries=0 root-or-chunk-body-queries=0", count)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDocRangeBoundaryPGTransactionGuards(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	pair := docRangeBoundaryPGPair(t, storage, ctx, repo, 129, 128)
	docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
	claim := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
	peer := &PostgresStore{pool: storage.pool}
	checkWrites := func(bound context.Context, owner *PostgresStore, want error) {
		t.Helper()
		if _, err := owner.EnqueueDocRangeBoundary(bound, repo, pair[0].hash, pair[1].hash, time.Now()); !errors.Is(err, want) {
			t.Fatal("enqueue escaped guard", err)
		}
		if err := owner.BuildDocRangeJob(bound, claim); !errors.Is(err, want) {
			t.Fatal("build escaped guard", err)
		}
	}
	if _, err := storage.DocRangeBoundary(ctx, repo, pair[0].hash, pair[1].hash); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("reader accepted unbound context", err)
	}
	checkWrites(outbound.WithDocReadOnly(ctx), storage, domain.ErrConflict)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	checkWrites(canceled, storage, context.Canceled)
	var closed context.Context
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		closed = read
		checkWrites(read, storage, domain.ErrConflict)
		checkWrites(read, peer, domain.ErrConflict)
		if _, err := peer.DocRangeBoundary(read, repo, pair[0].hash, pair[1].hash); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("reader accepted foreign context", err)
		}
		_, err := storage.DocRangeBoundary(read, repo, pair[0].hash, pair[0].hash)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.DocRangeBoundary(closed, repo, pair[0].hash, pair[0].hash); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal("warm reader escaped closed transaction", err)
	}
	if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		checkWrites(write, storage, domain.ErrConflict)
		if _, err := storage.DocRangeBoundary(write, repo, pair[0].hash, pair[1].hash); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("reader accepted writable context", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if current := docRangeBoundaryPGGet(t, storage, ctx, repo, pair); !reflect.DeepEqual(current, docRangeJobPGReceipt(claim)) {
		t.Fatal("guard modified job", current)
	}
}

func TestDocRangeBoundaryPGCurrentCorruptionFailsClosed(t *testing.T) {
	for _, damage := range []string{"descriptor", "event page", "directory", "missing generation", "checkpoint"} {
		t.Run(damage, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := docRangeJobPGRepo(t, storage, ctx)
			pair := docRangeBoundaryPGPair(t, storage, ctx, repo, 129, 128)
			docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
			claim := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
			if damage == "checkpoint" {
				if err := storage.BuildDocRangeJob(ctx, claim); err != nil {
					t.Fatal(err)
				}
				if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_boundaries_v1 SET events=1000 WHERE repo_id=$1`, repo); err != nil {
					t.Fatal(err)
				}
				docRangeBoundaryPGRead(t, storage, ctx, repo, pair, 0, domain.ErrIntegrity)
				if _, err := storage.EnqueueDocRangeBoundary(ctx, repo, pair[0].hash, pair[1].hash, time.Now()); !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("malformed completed proof requeued", err)
				}
				return
			}
			var object domain.ContentHash
			if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
				reader, err := storage.OpenDocRangeGeneration(read, repo, pair[1].hash)
				if err != nil {
					return err
				}
				if damage == "event page" {
					object, err = reader.PageHash(read, 1)
				} else {
					object = reader.Descriptor().EventPages.Hash
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			statement, args := `UPDATE doc_range_objects_v2 SET bytes=$3 WHERE doc_hash=$1 AND hash=$2`, []any{pair[1].hash, object, []byte("damaged")}
			want := domain.ErrIntegrity
			switch damage {
			case "descriptor":
				statement, args = `UPDATE doc_range_generations_v2 SET descriptor=$2 WHERE doc_hash=$1`, []any{pair[1].hash, []byte("damaged")}
			case "missing generation":
				statement, args = `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, []any{pair[1].hash}
				want = domain.ErrDocRangeIndexUnavailable
			}
			if _, err := storage.pool.Exec(ctx, statement, args...); err != nil {
				t.Fatal(err)
			}
			if err := storage.BuildDocRangeJob(ctx, claim); !errors.Is(err, want) {
				t.Fatal("corrupt selected source accepted", err)
			}
			checkpoint, exists, err := readDocRangeBoundaryCheckpoint(ctx, storage.pool, repo, pair[0].hash, pair[1].hash, false)
			if err != nil || exists {
				t.Fatal("failed batch published progress", checkpoint, err)
			}
		})
	}
}
