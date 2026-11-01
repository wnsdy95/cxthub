//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type rootPublicationBatchQueryPG struct {
	sql       string
	wanted    []string
	hashes    []string
	payload   bool
	rows      int
	bytes     int
	maxBody   int
	decodeErr error
}

type rootPublicationBatchTracePG struct {
	pgx.Tx
	queries []*rootPublicationBatchQueryPG
}

func (trace *rootPublicationBatchTracePG) Query(ctx context.Context, query string, arguments ...any) (pgx.Rows, error) {
	rows, err := trace.Tx.Query(ctx, query, arguments...)
	if err != nil || !strings.Contains(query, "FROM repo_blobs rb JOIN blobs b") {
		return rows, err
	}
	measurement := &rootPublicationBatchQueryPG{sql: strings.Join(strings.Fields(query), " ")}
	for _, argument := range arguments {
		if hashes, ok := argument.([]string); ok {
			measurement.wanted = slices.Clone(hashes)
		}
	}
	for _, field := range rows.FieldDescriptions() {
		if field.DataTypeOID == 17 {
			measurement.payload = true
		}
	}
	trace.queries = append(trace.queries, measurement)
	return &rootPublicationBatchObservedRowsPG{Rows: rows, measurement: measurement}, nil
}

func (trace *rootPublicationBatchTracePG) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	rows, err := trace.Query(ctx, query, arguments...)
	return rootPublicationBatchRowPG{rows: rows, err: err}
}

type rootPublicationBatchObservedRowsPG struct {
	pgx.Rows
	measurement *rootPublicationBatchQueryPG
}

func (rows *rootPublicationBatchObservedRowsPG) Next() bool {
	if !rows.Rows.Next() {
		return false
	}
	rows.measurement.rows++
	values, err := rows.Rows.Values()
	if err != nil {
		rows.measurement.decodeErr = err
		return true
	}
	for position, value := range values {
		if position == 0 {
			if hash, ok := value.(string); ok {
				rows.measurement.hashes = append(rows.measurement.hashes, hash)
			}
		}
		if body, ok := value.([]byte); ok {
			rows.measurement.bytes += len(body)
			rows.measurement.maxBody = max(rows.measurement.maxBody, len(body))
		}
	}
	return true
}

type rootPublicationBatchRowPG struct {
	rows pgx.Rows
	err  error
}

func (row rootPublicationBatchRowPG) Scan(destinations ...any) error {
	if row.err != nil {
		return row.err
	}
	defer row.rows.Close()
	if !row.rows.Next() {
		if err := row.rows.Err(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	return row.rows.Scan(destinations...)
}

func rootPublicationBatchTraceContextPG(ctx context.Context) (context.Context, *rootPublicationBatchTracePG) {
	original := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	trace := &rootPublicationBatchTracePG{Tx: original.Tx}
	bound := *original
	bound.Tx = trace
	return context.WithValue(ctx, repositoryTxKey{}, &bound), trace
}

func assertRootPublicationBatchQueriesPG(test *testing.T, trace *rootPublicationBatchTracePG, order []domain.ContentHash, wantPayloads int) []string {
	test.Helper()
	unique := make(map[string]bool)
	for _, hash := range order {
		unique[string(hash)] = true
	}
	wantLocked := make([]string, 0, len(unique))
	for hash := range unique {
		wantLocked = append(wantLocked, hash)
	}
	slices.Sort(wantLocked)
	var locked, requested []string
	metadataCount, payloadCount := 0, 0
	for _, query := range trace.queries {
		if query.decodeErr != nil || query.rows < 1 || query.rows > 64 || len(query.wanted) != query.rows {
			test.Fatalf("invalid bounded query: rows=%d requested=%d decode=%v SQL=%s", query.rows, len(query.wanted), query.decodeErr, query.sql)
		}
		if query.payload {
			payloadCount++
			if metadataCount != (len(unique)+63)/64 || query.bytes > 8<<20 || query.maxBody > 1<<20 {
				test.Fatalf("payload before complete retention or over budget: metadata=%d rows=%d bytes=%d largest=%d", metadataCount, query.rows, query.bytes, query.maxBody)
			}
			for _, hash := range query.wanted {
				if !unique[hash] {
					test.Fatal("payload query requested a non-manifest hash", hash)
				}
			}
			requested = append(requested, query.wanted...)
		} else {
			metadataCount++
			if payloadCount != 0 || query.bytes != 0 || !strings.Contains(query.sql, "FOR SHARE OF rb,b") || !slices.Equal(query.wanted, query.hashes) {
				test.Fatal("retention must lock ordered metadata only before reading bodies", query.sql, query.wanted, query.hashes)
			}
			locked = append(locked, query.hashes...)
		}
	}
	if !slices.Equal(locked, wantLocked) || metadataCount != (len(unique)+63)/64 || payloadCount != wantPayloads {
		test.Fatalf("query budget: unique=%d locked=%d metadata=%d payloads=%d wantPayloads=%d", len(unique), len(locked), metadataCount, payloadCount, wantPayloads)
	}
	test.Logf("unique=%d metadata_queries=%d payload_queries=%d", len(unique), metadataCount, payloadCount)
	return requested
}

func seedRootPublicationBatchPG(test *testing.T, storage *PostgresStore, ctx context.Context, count, size int) (domain.ContentHash, []domain.ContentHash, map[domain.ContentHash][]byte) {
	test.Helper()
	repo := domain.HashContent([]byte(test.Name() + time.Now().String()))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		test.Fatal(err)
	}
	order := make([]domain.ContentHash, 0, count)
	bodies := make(map[domain.ContentHash][]byte, count)
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	for ordinal := 0; ordinal < count; ordinal++ {
		body := bytes.Repeat([]byte("p"), size)
		copy(body, fmt.Sprintf("%s/%04d", repo, ordinal))
		hash := domain.HashContent(body)
		if _, err := transaction.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, hash, body); err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, repo, hash); err != nil {
			test.Fatal(err)
		}
		order = append(order, hash)
		bodies[hash] = body
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	slices.Sort(order)
	slices.Reverse(order)
	return repo, order, bodies
}

func TestRootPublicationBatchPGCountAndByteBounds(test *testing.T) {
	for _, scenario := range []struct {
		name     string
		count    int
		size     int
		payloads int
	}{
		{name: "65-tiny", count: 65, size: 128, payloads: 2},
		{name: "129-tiny", count: 129, size: 128, payloads: 3},
		{name: "17-one-MiB", count: 17, size: 1 << 20, payloads: 3},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			repo, order, bodies := seedRootPublicationBatchPG(test, storage, ctx, scenario.count, scenario.size)
			for _, phase := range []string{"cold", "warm"} {
				err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
					traced, trace := rootPublicationBatchTraceContextPG(bound)
					read, err := storage.retainedRootChunkReader(traced, repo, order)
					if err != nil {
						return err
					}
					assertRootPublicationBatchQueriesPG(test, trace, order, 0)
					for ordinal, hash := range order {
						body, err := read(traced, hash)
						if err != nil || !bytes.Equal(body, bodies[hash]) {
							test.Fatalf("%s manifest-order body %s: %v", phase, hash, err)
						}
						if ordinal == 0 {
							assertRootPublicationBatchQueriesPG(test, trace, order, 1)
						}
					}
					requested := assertRootPublicationBatchQueriesPG(test, trace, order, scenario.payloads)
					for ordinal, hash := range order {
						if len(requested) != len(order) || requested[ordinal] != string(hash) {
							test.Fatal("payload batches did not follow manifest order exactly once", requested)
						}
						updated := bytes.Clone(bodies[hash])
						updated[len(updated)-1] = 'w'
						bodies[hash] = updated
					}
					return nil
				})
				if err != nil {
					test.Fatal(phase, err)
				}
				if phase == "cold" {
					for _, hash := range order {
						if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, bodies[hash]); err != nil {
							test.Fatal(err)
						}
					}
				}
			}
		})
	}
}

func TestRootPublicationBatchPGRepeatedHashes(test *testing.T) {
	for _, count := range []int{1, 65} {
		test.Run(fmt.Sprintf("unique-%d", count), func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			repo, unique, bodies := seedRootPublicationBatchPG(test, storage, ctx, count, 128)
			var order []domain.ContentHash
			for _, hash := range unique {
				for repeat := 0; repeat < 3; repeat++ {
					order = append(order, hash)
				}
			}
			order = append(order, unique[0], unique[0])
			wantPayloads := 1
			if count > 64 {
				wantPayloads = 4
			}
			if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
				traced, trace := rootPublicationBatchTraceContextPG(bound)
				read, err := storage.retainedRootChunkReader(traced, repo, order)
				if err != nil {
					return err
				}
				for _, hash := range order {
					body, err := read(traced, hash)
					if err != nil || !bytes.Equal(body, bodies[hash]) {
						test.Fatal("repeated hash lost its current body", hash, err)
					}
				}
				assertRootPublicationBatchQueriesPG(test, trace, order, wantPayloads)
				return nil
			}); err != nil {
				test.Fatal(err)
			}
		})
	}
}

func TestRootPublicationBatchPGRepeatedWindowsDoNotAmplifyBytes(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	repo, unique, bodies := seedRootPublicationBatchPG(test, storage, ctx, 32, 512<<10)
	order := slices.Clone(unique)
	for repeat := 0; repeat < 16; repeat++ {
		order = append(order, unique[0], unique[16])
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		traced, trace := rootPublicationBatchTraceContextPG(bound)
		read, err := storage.retainedRootChunkReader(traced, repo, order)
		if err != nil {
			return err
		}
		occurrenceBytes := 0
		for _, hash := range order {
			body, err := read(traced, hash)
			if err != nil || !bytes.Equal(body, bodies[hash]) {
				test.Fatal("alternating repeated window lost current bytes", hash, err)
			}
			occurrenceBytes += len(bodies[hash])
		}
		assertRootPublicationBatchQueriesPG(test, trace, order, 18)
		transferred, payloads := 0, 0
		for _, query := range trace.queries {
			if query.payload {
				payloads++
				transferred += query.bytes
				if payloads > 2 && query.rows != 1 {
					test.Fatal("evicted repeat reloaded an entire prior window", query.rows)
				}
			}
		}
		if transferred > occurrenceBytes {
			test.Fatalf("repeat prefetch amplified transfer: got=%d occurrence_bytes=%d", transferred, occurrenceBytes)
		}
		test.Logf("transferred_bytes=%d occurrence_bytes=%d", transferred, occurrenceBytes)
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestRootPublicationBatchPGInterleavedRepeatPreservesPrefetch(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	repo, unique, bodies := seedRootPublicationBatchPG(test, storage, ctx, 32, 512<<10)
	order := append(slices.Clone(unique[:17]), unique[0])
	order = append(order, unique[17:]...)
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		traced, trace := rootPublicationBatchTraceContextPG(bound)
		read, err := storage.retainedRootChunkReader(traced, repo, order)
		if err != nil {
			return err
		}
		for _, hash := range order {
			body, err := read(traced, hash)
			if err != nil || !bytes.Equal(body, bodies[hash]) {
				test.Fatal("interleaved repeat lost pending bytes", err)
			}
		}
		assertRootPublicationBatchQueriesPG(test, trace, order, 3)
		transferred := 0
		for _, query := range trace.queries {
			transferred += query.bytes
		}
		if transferred != len(order)*(512<<10) {
			test.Fatal("singleton repeat discarded unread prefetch", transferred)
		}
		test.Logf("interleaved_repeat_bytes=%d occurrence_bytes=%d", transferred, len(order)*(512<<10))
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestRootPublicationBatchPGInterleavedRepeatSchedules(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	repo, unique, bodies := seedRootPublicationBatchPG(test, storage, ctx, 129, 128)
	for schedule := 0; schedule < 6; schedule++ {
		var order []domain.ContentHash
		for ordinal, hash := range unique {
			order = append(order, hash)
			for repeat := 0; repeat < (ordinal+schedule)%5; repeat++ {
				order = append(order, unique[(ordinal*31+repeat*17+schedule)%(ordinal+1)])
			}
		}
		if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
			traced, trace := rootPublicationBatchTraceContextPG(bound)
			read, err := storage.retainedRootChunkReader(traced, repo, order)
			if err != nil {
				return err
			}
			for _, hash := range order {
				body, err := read(traced, hash)
				if err != nil || !bytes.Equal(body, bodies[hash]) {
					test.Fatal("repeat schedule changed chunk", schedule, err)
				}
			}
			transferred, payloads := 0, 0
			for _, query := range trace.queries {
				transferred += query.bytes
				if query.payload {
					payloads++
				}
			}
			assertRootPublicationBatchQueriesPG(test, trace, order, payloads)
			if transferred > len(order)*128 {
				test.Fatal("repeat schedule amplified transfer", schedule, transferred, len(order)*128)
			}
			return nil
		}); err != nil {
			test.Fatal(err)
		}
	}
}

func TestRootPublicationBatchPGRejectsLateMissingAndOversizedMetadata(test *testing.T) {
	for _, damage := range []string{"grant", "bytes", "oversized"} {
		test.Run(damage, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			repo, order, _ := seedRootPublicationBatchPG(test, storage, ctx, 65, 128)
			hash := order[0]
			want := domain.ErrNotFound
			if damage == "oversized" {
				want = domain.ErrIntegrity
				if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, bytes.Repeat([]byte("x"), (1<<20)+1)); err != nil {
					test.Fatal(err)
				}
			} else {
				if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, hash); err != nil {
					test.Fatal(err)
				}
				if damage == "bytes" {
					if _, err := storage.pool.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, hash); err != nil {
						test.Fatal(err)
					}
				}
			}
			err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
				traced, trace := rootPublicationBatchTraceContextPG(bound)
				read, err := storage.retainedRootChunkReader(traced, repo, order)
				if !errors.Is(err, want) || read != nil || len(trace.queries) != 2 {
					test.Fatalf("late metadata failure: reader=%t queries=%d err=%v", read != nil, len(trace.queries), err)
				}
				for _, query := range trace.queries {
					if query.payload || query.bytes != 0 {
						test.Fatal("body fetched before complete metadata retention", query.sql)
					}
				}
				return err
			})
			if !errors.Is(err, want) {
				test.Fatal(err)
			}
		})
	}
}

func TestRootPublicationBatchPGRequiresMatchingWritableTransaction(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	repo, order, _ := seedRootPublicationBatchPG(test, storage, ctx, 2, 128)
	peer := &PostgresStore{pool: storage.pool}
	otherRepo := domain.HashContent([]byte("other:" + string(repo)))
	assertRejected := func(candidate context.Context, owner *PostgresStore, repository domain.ContentHash) {
		test.Helper()
		read, err := owner.retainedRootChunkReader(candidate, repository, order)
		if !errors.Is(err, domain.ErrConflict) || read != nil {
			test.Fatalf("unbound/borrowed reader admitted: reader=%t err=%v", read != nil, err)
		}
	}
	assertRejected(ctx, storage, repo)
	if err := storage.WithinReadSnapshot(ctx, func(snapshot context.Context) error {
		assertRejected(snapshot, storage, repo)
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	var escaped storedChunkReader
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		traced, trace := rootPublicationBatchTraceContextPG(bound)
		assertRejected(traced, peer, repo)
		assertRejected(traced, storage, otherRepo)
		assertRejected(outbound.WithDocReadOnly(traced), storage, repo)
		if len(trace.queries) != 0 {
			test.Fatal("rejected transaction read chunk metadata")
		}
		read, err := storage.retainedRootChunkReader(traced, repo, order)
		if err != nil {
			return err
		}
		escaped = read
		borrowed := *traced.Value(repositoryTxKey{}).(*repositoryTx)
		for _, warm := range []bool{false, true} {
			if warm {
				if _, err := read(traced, order[0]); err != nil {
					return err
				}
			}
			before := len(trace.queries)
			for _, rejected := range []context.Context{ctx, bound, outbound.WithDocReadOnly(traced), context.WithValue(traced, repositoryTxKey{}, &borrowed)} {
				body, err := read(rejected, order[0])
				if !errors.Is(err, domain.ErrConflict) || body != nil {
					test.Fatalf("reader escaped retained transaction: warm=%t bytes=%d err=%v", warm, len(body), err)
				}
			}
			canceled, cancel := context.WithCancel(traced)
			cancel()
			if body, err := read(canceled, order[0]); !errors.Is(err, context.Canceled) || body != nil {
				test.Fatal("canceled reader returned bytes", err)
			}
			if len(trace.queries) != before {
				test.Fatal("rejected read performed SQL")
			}
		}
		if body, err := read(traced, domain.HashContent([]byte("undeclared"))); err == nil || body != nil {
			test.Fatal("reader admitted a non-manifest hash")
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, repo, func(next context.Context) error {
		if body, err := escaped(next, order[0]); !errors.Is(err, domain.ErrConflict) || body != nil {
			test.Fatal("cached body escaped into a later transaction", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func assertRootPublicationBatchLockedPG(test *testing.T, storage *PostgresStore, ctx context.Context, repo, hash domain.ContentHash) {
	test.Helper()
	for _, mutation := range []struct {
		query string
		args  []any
	}{
		{query: `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, args: []any{repo, hash}},
		{query: `UPDATE blobs SET bytes=$2 WHERE hash=$1`, args: []any{hash, []byte("blocked current-byte mutation")}},
	} {
		probe, err := storage.pool.Begin(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := probe.Exec(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
			rollbackPG(probe)
			test.Fatal(err)
		}
		_, err = probe.Exec(ctx, mutation.query, mutation.args...)
		rollbackPG(probe)
		var postgresError *pgconn.PgError
		if !errors.As(err, &postgresError) || postgresError.Code != "55P03" {
			test.Fatalf("dependency mutable before final commit: %s: %v", mutation.query, err)
		}
	}
}

func TestRootPublicationBatchPGRetainsUnreadLastBatchThroughCommit(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	repo, order, _ := seedRootPublicationBatchPG(test, storage, ctx, 65, 128)
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		read, err := storage.retainedRootChunkReader(bound, repo, order)
		if err != nil {
			return err
		}
		assertRootPublicationBatchLockedPG(test, storage, ctx, repo, order[0])
		if _, err := read(bound, order[len(order)-1]); err != nil {
			return err
		}
		assertRootPublicationBatchLockedPG(test, storage, ctx, repo, order[0])
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if tag, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, order[0], []byte("after commit")); err != nil || tag.RowsAffected() != 1 {
		test.Fatal("body remained locked after commit", err)
	}
	if tag, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, order[0]); err != nil || tag.RowsAffected() != 1 {
		test.Fatal("grant remained locked after commit", err)
	}
}

func prepareRootPublicationBatchPG(test *testing.T, storage *PostgresStore, ctx context.Context, fixture rootReaderFixture, repo domain.ContentHash, streaming bool) (domain.DocFinalizationJob, func(context.Context) error) {
	test.Helper()
	job, doc := rootPublicationClaimPG(test, storage, ctx, fixture, repo)
	if streaming {
		publication := prepareRootStreamingPG(test, storage, ctx, job, rangeSpoolTempDir(test))
		return job, func(write context.Context) error { return publication.Complete(write, job, time.Now()) }
	}
	publication, err := storage.PrepareDocJob(ctx, doc)
	if err != nil {
		test.Fatal(err)
	}
	return job, func(write context.Context) error { return publication.Complete(write, job, time.Now()) }
}

func TestRootPublicationBatchPGBothPublishersUseCurrentReads(test *testing.T) {
	for _, streaming := range []bool{false, true} {
		test.Run(fmt.Sprintf("streaming-%t", streaming), func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			ctx = rootPublicationContext(ctx)
			nonce := test.Name() + time.Now().String()
			first := strings.Repeat(nonce+"a", 1+domain.ConversationManifestChunkBytes/(len(nonce)+1))[:domain.ConversationManifestChunkBytes]
			second := strings.Repeat(nonce+"b", 1+domain.ConversationManifestChunkBytes/(len(nonce)+1))[:domain.ConversationManifestChunkBytes]
			fixture := uniqueRootPublicationFixturePG(test, first+second)
			if len(fixture.bodies) < 3 {
				test.Fatal("fixture must exercise multiple distinct dependencies")
			}
			job, document := rootPublicationClaimPG(test, storage, ctx, fixture, "")
			var complete func(context.Context) error
			var prepared outbound.PreparedRootDocPublication
			directory := ""
			if streaming {
				directory = rangeSpoolTempDir(test)
				prepared = prepareRootStreamingPG(test, storage, ctx, job, directory)
				complete = func(write context.Context) error { return prepared.Complete(write, job, time.Now()) }
			} else {
				materialized, err := storage.PrepareDocJob(ctx, document)
				if err != nil {
					test.Fatal(err)
				}
				complete = func(write context.Context) error { return materialized.Complete(write, job, time.Now()) }
			}
			for hash, body := range fixture.bodies {
				if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, body); err != nil {
					test.Fatal(err)
				}
			}
			if streaming {
				if err := complete(ctx); !errors.Is(err, domain.ErrConflict) {
					test.Fatal("changed storage generation reused an old preparation", err)
				}
				assertRootStreamingUnpublishedPG(test, storage, ctx, job)
				if err := prepared.Close(); err != nil {
					test.Fatal(err)
				}
				prepared = prepareRootStreamingPG(test, storage, ctx, job, directory)
			}
			if err := storage.WithinRepository(ctx, job.RepoID, func(bound context.Context) error {
				traced, trace := rootPublicationBatchTraceContextPG(bound)
				if err := complete(traced); err != nil {
					return err
				}
				if streaming {
					assertRootPublicationBatchQueriesPG(test, trace, conversationChunkOrder(fixture.manifest), 0)
				} else {
					if len(trace.queries) != len(fixture.manifest.Chunks) {
						test.Fatal("materialized publisher no longer uses its compatible current-byte reader", len(trace.queries))
					}
					for _, query := range trace.queries {
						if !query.payload || query.rows != 1 || !strings.Contains(query.sql, "FOR SHARE OF rb,b") {
							test.Fatal("materialized publisher did not retain current chunk bytes", query.sql)
						}
					}
				}
				assertRootStreamingUnpublishedPG(test, storage, ctx, job)
				assertRootPublicationBatchLockedPG(test, storage, ctx, job.RepoID, fixture.manifest.Chunks[len(fixture.manifest.Chunks)-1].Hash)
				return nil
			}); err != nil {
				test.Fatal(err)
			}
			current, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
			if err != nil || current.State != "completed" || current.DocumentRef() != job.DocumentRef() {
				test.Fatal("completion receipt not committed", current, err)
			}
			assertRangePages(test, ctx, storage, job.RepoID, fixture)
			verified, err := storage.ReadVerifiedDoc(ctx, job.RepoID, job.DocHash)
			if err != nil {
				test.Fatal(err)
			}
			assertRootProof(test, verified, fixture)
			hash := fixture.manifest.Chunks[0].Hash
			if tag, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, []byte("after publication")); err != nil || tag.RowsAffected() != 1 {
				test.Fatal("committed publication retained body lock", err)
			}
			if tag, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, job.RepoID, hash); err != nil || tag.RowsAffected() != 1 {
				test.Fatal("committed publication retained grant lock", err)
			}
		})
	}
}

func TestRootPublicationBatchPGBothPublishersRollbackDamagedClosure(test *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, damage := range []string{"revoked", "foreign-only", "missing", "corrupt", "oversized"} {
			test.Run(fmt.Sprintf("streaming-%t/%s", streaming, damage), func(test *testing.T) {
				storage, ctx := chunkReusePG(test)
				ctx = rootPublicationContext(ctx)
				repo, namespace := rootPGRepo(test, ctx, storage)
				fixture := uniqueRootPublicationFixturePG(test, "current-byte rollback "+test.Name())
				job, complete := prepareRootPublicationBatchPG(test, storage, ctx, fixture, repo, streaming)
				hash := fixture.manifest.Chunks[len(fixture.manifest.Chunks)-1].Hash
				var err error
				switch damage {
				case "revoked", "foreign-only", "missing":
					if _, err = storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, hash); err != nil {
						test.Fatal(err)
					}
					if damage == "missing" {
						_, err = storage.pool.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, hash)
					} else if damage == "foreign-only" {
						foreign, _ := rootPGRepo(test, ctx, storage)
						_, err = storage.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, foreign, hash)
					}
				case "corrupt":
					_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, []byte("corrupt current bytes"))
				case "oversized":
					_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, bytes.Repeat([]byte("x"), (1<<20)+1))
				}
				if err != nil {
					test.Fatal(err)
				}
				before := rootPGImage(test, ctx, storage, repo, namespace)
				want := domain.ErrNotFound
				if damage == "corrupt" || damage == "oversized" {
					want = domain.ErrIntegrity
				}
				err = storage.WithinRepository(ctx, repo, func(bound context.Context) error {
					traced, trace := rootPublicationBatchTraceContextPG(bound)
					err := complete(traced)
					if !errors.Is(err, want) {
						test.Fatalf("damaged closure admitted: got=%v want=%v", err, want)
					}
					if len(trace.queries) == 0 {
						test.Fatal("publication did not inspect current dependencies")
					}
					for _, query := range trace.queries {
						if streaming && damage != "corrupt" && (query.payload || query.bytes != 0) {
							test.Fatal("invalid metadata admitted payload reads", query.sql)
						}
					}
					return err
				})
				if !errors.Is(err, want) {
					test.Fatal(err)
				}
				assertRootStreamingUnpublishedPG(test, storage, ctx, job)
				if after := rootPGImage(test, ctx, storage, repo, namespace); after != before {
					test.Fatal("failed publication changed ownership, retained bytes, accounting, or receipt")
				}
				if damage == "missing" {
					var count int
					if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE hash=$1`, hash).Scan(&count); err != nil || count != 0 {
						test.Fatal("publication repaired missing chunk bytes", count, err)
					}
				}
			})
		}
	}
}

func TestRootPublicationBatchPGMaterializedPaddedBodyCompatibility(test *testing.T) {
	for _, streaming := range []bool{false, true} {
		test.Run(fmt.Sprintf("streaming-%t", streaming), func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			ctx = rootPublicationContext(ctx)
			fixture := uniqueRootPublicationFixturePG(test, "valid skippable padding "+test.Name())
			job, complete := prepareRootPublicationBatchPG(test, storage, ctx, fixture, "", streaming)
			chunk := fixture.manifest.Chunks[0]
			padding := make([]byte, (1<<20)+8)
			binary.LittleEndian.PutUint32(padding, 0x184d2a50)
			binary.LittleEndian.PutUint32(padding[4:], uint32(len(padding)-8))
			stored := append(docCompress(fixture.bodies[chunk.Hash]), padding...)
			decoded, err := rootChunkBytes(ctx, stored, chunk.Bytes)
			if err != nil || !bytes.Equal(decoded, fixture.bodies[chunk.Hash]) || len(stored) <= 1<<20 {
				test.Fatal("fixture is not valid oversized stored zstd padding", len(stored), err)
			}
			if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, chunk.Hash, stored); err != nil {
				test.Fatal(err)
			}
			err = storage.WithinRepository(ctx, job.RepoID, func(bound context.Context) error {
				traced, trace := rootPublicationBatchTraceContextPG(bound)
				err := complete(traced)
				if len(trace.queries) != 1 {
					test.Fatal("unexpected dependency query count", len(trace.queries))
				}
				query := trace.queries[0]
				if streaming {
					if !errors.Is(err, domain.ErrIntegrity) || query.payload || query.bytes != 0 {
						test.Fatal("streaming publisher fetched or accepted oversized padding", query.bytes, err)
					}
				} else if err != nil || !query.payload || query.bytes != len(stored) {
					test.Fatal("materialized publisher rejected valid padded bytes", query.bytes, err)
				}
				return err
			})
			if streaming {
				if !errors.Is(err, domain.ErrIntegrity) {
					test.Fatal(err)
				}
				assertRootStreamingUnpublishedPG(test, storage, ctx, job)
			} else {
				if err != nil {
					test.Fatal(err)
				}
				current, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
				if err != nil || current.State != "completed" {
					test.Fatal("padded materialized publication did not complete", current.State, err)
				}
				assertRangePages(test, ctx, storage, job.RepoID, fixture)
				verified, err := storage.ReadVerifiedDoc(ctx, job.RepoID, job.DocHash)
				if err != nil {
					test.Fatal(err)
				}
				assertRootProof(test, verified, fixture)
			}
			var retained []byte
			if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, chunk.Hash).Scan(&retained); err != nil || !bytes.Equal(retained, stored) {
				test.Fatal("publication rewrote padded dependency", err)
			}
		})
	}
}
