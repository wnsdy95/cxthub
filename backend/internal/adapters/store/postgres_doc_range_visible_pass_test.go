//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docRangeVisiblePassBatchSize struct {
	statements   int
	payloadBytes int
}

type docRangeVisiblePassBatchTx struct {
	pgx.Tx
	testing        *testing.T
	hash           domain.ContentHash
	sizes          []docRangeVisiblePassBatchSize
	rangePages     int
	visibleObjects int
	mixedBatches   int
	failure        error
}

func (transaction *docRangeVisiblePassBatchTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	transaction.testing.Helper()
	size := docRangeVisiblePassBatchSize{statements: batch.Len()}
	rangePages, visibleObjects := 0, 0
	for position, query := range batch.QueuedQueries {
		if len(query.Arguments) != 3 || query.Arguments[0] != transaction.hash {
			transaction.testing.Fatal("unexpected publication arguments", query.SQL, query.Arguments)
		}
		raw, ok := query.Arguments[2].([]byte)
		if !ok || len(raw) == 0 {
			transaction.testing.Fatal("publication payload missing", query.SQL)
		}
		size.payloadBytes += len(raw)
		switch {
		case strings.HasPrefix(query.SQL, "INSERT INTO doc_range_pages_v1"):
			ordinal, valid := query.Arguments[1].(int)
			if !valid || ordinal < 0 || position+1 >= batch.Len() {
				transaction.testing.Fatal("range page pair split or invalid", query.Arguments)
			}
			object := batch.QueuedQueries[position+1]
			if !strings.HasPrefix(object.SQL, "INSERT INTO doc_range_objects_v2") || len(object.Arguments) != 3 || object.Arguments[1] != domain.HashContent(raw) || !reflect.DeepEqual(object.Arguments[2], raw) {
				transaction.testing.Fatal("range page pair lost original hash or bytes", ordinal)
			}
			rangePages++
		case strings.HasPrefix(query.SQL, "INSERT INTO doc_range_objects_v2"):
			if query.Arguments[1] != domain.HashContent(raw) {
				transaction.testing.Fatal("object payload hash changed", query.Arguments[1])
			}
			if bytes.Contains(raw, []byte(`"units":[`)) {
				visibleObjects++
			}
		default:
			transaction.testing.Fatal("unexpected publication statement", query.SQL)
		}
	}
	if size.statements > 128 || size.payloadBytes > 4<<20 {
		transaction.testing.Fatal("mixed batch exceeds SQL or duplicated-payload cap", size)
	}
	transaction.sizes = append(transaction.sizes, size)
	transaction.rangePages += rangePages
	transaction.visibleObjects += visibleObjects
	if rangePages > 0 && visibleObjects > 0 {
		transaction.mixedBatches++
	}
	if transaction.failure != nil || transaction.Tx == nil {
		return docVisibleBatchResult{err: transaction.failure}
	}
	return transaction.Tx.SendBatch(ctx, batch)
}

func TestDocRangeVisiblePassMixedBatchBounds(test *testing.T) {
	if docVisibleBatchObjects != 128 || docVisibleBatchBytes != 4<<20 {
		test.Fatal("publication caps changed", docVisibleBatchObjects, docVisibleBatchBytes)
	}
	type emission struct {
		rangePage    bool
		count        int
		payloadBytes int
	}
	for _, scenario := range []struct {
		name      string
		emissions []emission
		want      []docRangeVisiblePassBatchSize
	}{
		{
			name:      "126 visible plus pair exactly fills SQL cap",
			emissions: []emission{{false, 126, 1}, {true, 1, 1}, {false, 1, 1}},
			want:      []docRangeVisiblePassBatchSize{{128, 128}, {1, 1}},
		},
		{
			name:      "127 visible cannot admit a pair",
			emissions: []emission{{false, 127, 1}, {true, 1, 1}, {false, 1, 1}},
			want:      []docRangeVisiblePassBatchSize{{127, 127}, {3, 3}},
		},
		{
			name:      "full visible batch flushes before pair",
			emissions: []emission{{false, 128, 1}, {true, 1, 1}},
			want:      []docRangeVisiblePassBatchSize{{128, 128}, {2, 2}},
		},
		{
			name:      "64 pairs exactly fill SQL cap",
			emissions: []emission{{true, 65, 1}, {false, 1, 1}},
			want:      []docRangeVisiblePassBatchSize{{128, 128}, {3, 3}},
		},
		{
			name:      "maximum range pages exactly fill byte cap",
			emissions: []emission{{true, 16, domain.MaxDocRangePageBytes}, {false, 1, 1}},
			want:      []docRangeVisiblePassBatchSize{{32, 4 << 20}, {1, 1}},
		},
		{
			name: "mixed pair exactly fills byte cap",
			emissions: []emission{
				{true, 15, domain.MaxDocRangePageBytes}, {false, 7, domain.MaxDocVisibleObjectBytes},
				{true, 1, domain.MaxDocVisibleObjectBytes / 2}, {false, 1, 1},
			},
			want: []docRangeVisiblePassBatchSize{{39, 4 << 20}, {1, 1}},
		},
		{
			name: "one-byte visible emissions reach exact byte cap",
			emissions: []emission{
				{true, 15, domain.MaxDocRangePageBytes}, {false, 7, domain.MaxDocVisibleObjectBytes},
				{true, 1, domain.MaxDocVisibleObjectBytes/2 - 1}, {false, 3, 1},
			},
			want: []docRangeVisiblePassBatchSize{{41, 4 << 20}, {1, 1}},
		},
		{
			name: "pair two bytes over remaining capacity flushes first",
			emissions: []emission{
				{true, 15, domain.MaxDocRangePageBytes}, {false, 7, domain.MaxDocVisibleObjectBytes},
				{true, 1, domain.MaxDocVisibleObjectBytes/2 + 1},
			},
			want: []docRangeVisiblePassBatchSize{{37, (4 << 20) - domain.MaxDocVisibleObjectBytes}, {2, domain.MaxDocVisibleObjectBytes + 2}},
		},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			ctx := context.Background()
			transaction := &docRangeVisiblePassBatchTx{testing: test, hash: domain.HashContent([]byte(test.Name()))}
			batch := postgresDocVisibleBatch{transaction: &repositoryTx{Tx: transaction}, hash: transaction.hash}
			ordinal := 0
			for _, step := range scenario.emissions {
				raw := bytes.Repeat([]byte("p"), step.payloadBytes)
				object := domain.DocRangeObject{Hash: domain.HashContent(raw), Bytes: raw}
				for range step.count {
					var err error
					if step.rangePage {
						err = batch.emitRangePage(ctx, ordinal, object)
						ordinal++
					} else {
						err = batch.emit(ctx, object)
					}
					if err != nil {
						test.Fatal(err)
					}
					if batch.batch.Len() > 128 || batch.bytes > 4<<20 {
						test.Fatal("pending mixed batch exceeded caps", batch.batch.Len(), batch.bytes)
					}
				}
			}
			if err := batch.flush(ctx); err != nil {
				test.Fatal(err)
			}
			if !reflect.DeepEqual(transaction.sizes, scenario.want) || transaction.rangePages != ordinal {
				test.Fatal("incorrect exact batch accounting", transaction.sizes, scenario.want, transaction.rangePages, ordinal)
			}
			if batch.bytes != 0 || batch.batch.Len() != 0 {
				test.Fatal("flush retained mixed payloads", batch.bytes, batch.batch.Len())
			}
			if err := batch.flush(ctx); err != nil || len(transaction.sizes) != len(scenario.want) {
				test.Fatal("empty flush sent another batch", transaction.sizes, err)
			}
		})
	}
}

func TestDocRangeVisiblePassRangeEmissionValidation(test *testing.T) {
	raw := bytes.Repeat([]byte("p"), domain.MaxDocRangePageBytes)
	oversized := append(bytes.Clone(raw), 'p')
	valid := domain.DocRangeObject{Hash: domain.HashContent(raw), Bytes: raw}
	for _, scenario := range []struct {
		name    string
		ordinal int
		object  domain.DocRangeObject
	}{
		{"negative ordinal", -1, valid},
		{"empty payload", 0, domain.DocRangeObject{Hash: domain.HashContent(nil)}},
		{"missing hash", 0, domain.DocRangeObject{Bytes: raw}},
		{"wrong hash", 0, domain.DocRangeObject{Hash: domain.HashContent([]byte("wrong")), Bytes: raw}},
		{"oversized payload with matching hash", 0, domain.DocRangeObject{Hash: domain.HashContent(oversized), Bytes: oversized}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			ctx := context.Background()
			transaction := &docRangeVisiblePassBatchTx{testing: test, hash: domain.HashContent([]byte(test.Name()))}
			batch := postgresDocVisibleBatch{transaction: &repositoryTx{Tx: transaction}, hash: transaction.hash}
			visible := domain.DocRangeObject{Hash: domain.HashContent([]byte("v")), Bytes: []byte("v")}
			for range 127 {
				if err := batch.emit(ctx, visible); err != nil {
					test.Fatal(err)
				}
			}
			if err := batch.emitRangePage(ctx, scenario.ordinal, scenario.object); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("invalid range page admitted", err)
			}
			if batch.batch.Len() != 127 || batch.bytes != 127 || len(transaction.sizes) != 0 {
				test.Fatal("invalid range page flushed or changed pending visible rows", batch.batch.Len(), batch.bytes, transaction.sizes)
			}
		})
	}
}

func TestDocRangeVisiblePassRangeEmissionCancellationAndFailure(test *testing.T) {
	for _, operation := range []string{"canceled emission", "canceled flush", "failed reservation flush"} {
		test.Run(operation, func(test *testing.T) {
			ctx := context.Background()
			transaction := &docRangeVisiblePassBatchTx{testing: test, hash: domain.HashContent([]byte(test.Name()))}
			batch := postgresDocVisibleBatch{transaction: &repositoryTx{Tx: transaction}, hash: transaction.hash}
			visible := domain.DocRangeObject{Hash: domain.HashContent([]byte("v")), Bytes: []byte("v")}
			for range 127 {
				if err := batch.emit(ctx, visible); err != nil {
					test.Fatal(err)
				}
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			want := error(context.Canceled)
			var err error
			if operation == "canceled flush" {
				err = batch.flush(canceled)
			} else if operation == "canceled emission" {
				err = batch.emitRangePage(canceled, 0, visible)
			} else {
				transaction.failure = errors.New("mixed batch flush failed")
				want = transaction.failure
				err = batch.emitRangePage(ctx, 0, visible)
			}
			if !errors.Is(err, want) || batch.batch.Len() != 127 || batch.bytes != 127 {
				test.Fatal("failed emission consumed pending rows or queued a partial pair", err, batch.batch.Len(), batch.bytes)
			}
			wantSends := 0
			if operation == "failed reservation flush" {
				wantSends = 1
			}
			if len(transaction.sizes) != wantSends || transaction.rangePages != 0 {
				test.Fatal("unexpected send after cancellation or flush failure", transaction.sizes, transaction.rangePages)
			}
		})
	}
}

type docRangeVisiblePassPageBuffer struct {
	domain.DocRangePageBuffer
	pages [][]byte
	reads []int
}

func (buffer *docRangeVisiblePassPageBuffer) Append(ctx context.Context, raw []byte) error {
	if err := buffer.DocRangePageBuffer.Append(ctx, raw); err != nil {
		return err
	}
	buffer.pages = append(buffer.pages, bytes.Clone(raw))
	buffer.reads = append(buffer.reads, 0)
	return nil
}

func (buffer *docRangeVisiblePassPageBuffer) Read(ctx context.Context, ordinal int) ([]byte, error) {
	if ordinal < 0 || ordinal >= len(buffer.reads) {
		return nil, domain.ErrIntegrity
	}
	buffer.reads[ordinal]++
	return buffer.DocRangePageBuffer.Read(ctx, ordinal)
}

func TestDocRangeVisiblePassPGAtomicPublicationAndFences(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	repo, _ := rootPGRepo(test, ctx, storage)
	fixture := rangeFixture(test, 257)
	seedRootPG(test, ctx, storage, repo, fixture)
	spool, err := newDocRangeSpool()
	if err != nil {
		test.Fatal(err)
	}
	defer spool.Close()
	buffer := &docRangeVisiblePassPageBuffer{DocRangePageBuffer: spool}
	var verifier domain.CanonicalDocVerifier
	index, err := verifier.VerifyConversationRangeIndex(ctx, fixture.hash, fixture.manifest, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		return fixture.bodies[hash], nil
	}, buffer)
	if err != nil {
		test.Fatal(err)
	}
	before := docVisiblePGImage(test, storage, ctx, fixture.hash)
	err = storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		transaction := *bound.Value(repositoryTxKey{}).(*repositoryTx)
		recorder := &docRangeVisiblePassBatchTx{Tx: transaction.Tx, testing: test, hash: fixture.hash}
		transaction.Tx = recorder
		write := context.WithValue(bound, repositoryTxKey{}, &transaction)
		if err := storage.putRangeIndex(write, repo, index); err != nil {
			return err
		}
		if recorder.rangePages != len(buffer.pages) || recorder.mixedBatches == 0 {
			test.Fatal("range pages and visible objects did not share publication batches", recorder.rangePages, recorder.mixedBatches)
		}
		for ordinal, expected := range buffer.pages {
			if buffer.reads[ordinal] != 1 {
				test.Fatal("publication read range page more than once", ordinal, buffer.reads[ordinal])
			}
			var page, object []byte
			if err := storage.db(write).QueryRow(write, `SELECT page.bytes,object.bytes
 FROM doc_range_pages_v1 page JOIN doc_range_objects_v2 object ON object.doc_hash=page.doc_hash AND object.hash=$3
 WHERE page.doc_hash=$1 AND page.ordinal=$2`, fixture.hash, ordinal, domain.HashContent(expected)).Scan(&page, &object); err != nil {
				return err
			}
			if !bytes.Equal(page, expected) || !bytes.Equal(object, expected) {
				test.Fatal("mixed publication changed original range page bytes", ordinal)
			}
		}
		var current bool
		if err := storage.db(write).QueryRow(write, `SELECT root.doc_range_source_version=ranges.source_version
 AND root.doc_range_source_version=visible.source_version AND ranges.generation_hash=visible.range_generation_hash
 FROM blobs root JOIN doc_range_generations_v2 ranges ON ranges.doc_hash=root.hash
 JOIN doc_visible_generations_v1 visible ON visible.doc_hash=root.hash WHERE root.hash=$1`, fixture.hash).Scan(&current); err != nil {
			return err
		}
		if !current {
			test.Fatal("mixed publication lost source or range-generation fence")
		}
		if _, err := storage.readRangeGeneration(write, repo, fixture.hash); err != nil {
			return err
		}
		if _, err := storage.readVisibleGeneration(write, repo, fixture.hash); err != nil {
			return err
		}
		if after := docVisiblePGImage(test, storage, ctx, fixture.hash); after != before {
			test.Fatal("uncommitted mixed publication became externally visible")
		}
		docVisiblePGRead(test, storage, ctx, repo, fixture.hash, domain.ErrDocRangeIndexUnavailable)
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	assertRangePages(test, ctx, storage, repo, fixture)
	docVisiblePGRead(test, storage, ctx, repo, fixture.hash, nil)
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		ranges, err := storage.OpenDocRangeGeneration(bound, repo, fixture.hash)
		if err != nil {
			return err
		}
		visible, err := storage.OpenDocVisibleGeneration(bound, repo, fixture.hash)
		if err != nil {
			return err
		}
		for _, ordinal := range []int{0, 128, 256} {
			if _, err := ranges.ReadPage(bound, ordinal/domain.DocRangePageEvents); err != nil {
				return err
			}
			for _, mode := range []string{"all", "chat", "prompts"} {
				unit, position, err := visible.Unit(bound, mode, ordinal, false)
				if err != nil || position != ordinal || unit.From != ordinal || unit.To != ordinal+1 || unit.EventIndex != ordinal {
					test.Fatal("mixed publication reader mismatch", mode, ordinal, unit, position, err)
				}
			}
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	foreign, _ := rootPGRepo(test, ctx, storage)
	published := docVisiblePGImage(test, storage, ctx, fixture.hash)
	docVisiblePGRead(test, storage, ctx, foreign, fixture.hash, domain.ErrNotFound)
	if _, err := storage.readRangeGeneration(ctx, foreign, fixture.hash); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("mixed publication granted foreign range access", err)
	}
	if after := docVisiblePGImage(test, storage, ctx, fixture.hash); after != published {
		test.Fatal("unauthorized reads changed mixed publication")
	}
	seedRootPG(test, ctx, storage, foreign, fixture)
	if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, fixture.hash); err != nil {
		test.Fatal(err)
	}
	docVisiblePGRead(test, storage, ctx, repo, fixture.hash, domain.ErrNotFound)
	docVisiblePGRead(test, storage, ctx, foreign, fixture.hash, nil)
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash); err != nil {
		test.Fatal(err)
	}
	docVisiblePGRead(test, storage, ctx, foreign, fixture.hash, domain.ErrDocRangeIndexUnavailable)
	if _, err := storage.readRangeGeneration(ctx, foreign, fixture.hash); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		test.Fatal("source update retained mixed range generation", err)
	}
}

type docRangeVisiblePassFailureTx struct {
	pgx.Tx
	testing        *testing.T
	hash           domain.ContentHash
	mixedBatches   int
	partialPages   int
	partialVisible int
}

func (transaction *docRangeVisiblePassFailureTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	transaction.testing.Helper()
	rangePage, visibleObject := false, false
	for _, query := range batch.QueuedQueries {
		if strings.HasPrefix(query.SQL, "INSERT INTO doc_range_pages_v1") {
			rangePage = true
		}
		if len(query.Arguments) == 3 {
			if raw, ok := query.Arguments[2].([]byte); ok && bytes.Contains(raw, []byte(`"units":[`)) {
				visibleObject = true
			}
		}
	}
	if rangePage && visibleObject {
		transaction.mixedBatches++
		if transaction.mixedBatches == 2 {
			if err := transaction.Tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM doc_range_pages_v1 WHERE doc_hash=$1),
 (SELECT count(*) FROM doc_range_objects_v2 WHERE doc_hash=$1 AND position($2::bytea in bytes)>0)`, transaction.hash, []byte(`"units":[`)).Scan(&transaction.partialPages, &transaction.partialVisible); err != nil {
				transaction.testing.Fatal(err)
			}
			if transaction.partialPages == 0 || transaction.partialVisible == 0 {
				transaction.testing.Fatal("failure did not follow a real partial mixed flush", transaction.partialPages, transaction.partialVisible)
			}
			broken := &pgx.Batch{}
			for position, query := range batch.QueuedQueries {
				if position == batch.Len()-1 {
					broken.Queue(`SELECT 1/0`)
				} else {
					broken.Queue(query.SQL, query.Arguments...)
				}
			}
			return transaction.Tx.SendBatch(ctx, broken)
		}
	}
	return transaction.Tx.SendBatch(ctx, batch)
}

func TestDocRangeVisiblePassPGPartialMixedFlushRollback(test *testing.T) {
	for _, existing := range []bool{false, true} {
		test.Run(fmt.Sprintf("existing-%v", existing), func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			repo, _ := rootPGRepo(test, ctx, storage)
			fixture := rangeFixture(test, 26*domain.DocRangePageEvents)
			seedRootPG(test, ctx, storage, repo, fixture)
			if existing {
				if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
					test.Fatal(err)
				}
			}
			index, err := domain.BuildDocRangeIndex(ctx, rootPublicationProof(test, fixture))
			if err != nil {
				test.Fatal(err)
			}
			before := docVisiblePGImage(test, storage, ctx, fixture.hash)
			err = storage.WithinRepository(ctx, repo, func(bound context.Context) error {
				transaction := *bound.Value(repositoryTxKey{}).(*repositoryTx)
				fault := &docRangeVisiblePassFailureTx{Tx: transaction.Tx, testing: test, hash: fixture.hash}
				recorder := &docRangeVisiblePassBatchTx{Tx: fault, testing: test, hash: fixture.hash}
				transaction.Tx = recorder
				write := context.WithValue(bound, repositoryTxKey{}, &transaction)
				err := storage.putRangeIndex(write, repo, index)
				var postgresError *pgconn.PgError
				if !errors.As(err, &postgresError) || postgresError.Code != "22012" || fault.mixedBatches != 2 || recorder.mixedBatches != 2 {
					test.Fatal("publication did not propagate PostgreSQL failure after partial mixed flush", err, fault.mixedBatches, recorder.mixedBatches)
				}
				if after := docVisiblePGImage(test, storage, ctx, fixture.hash); after != before {
					test.Fatal("failed mixed batch escaped its transaction")
				}
				return err
			})
			var postgresError *pgconn.PgError
			if !errors.As(err, &postgresError) || postgresError.Code != "22012" {
				test.Fatal("outer transaction lost PostgreSQL batch error", err)
			}
			if after := docVisiblePGImage(test, storage, ctx, fixture.hash); after != before {
				test.Fatal("mixed rollback changed committed range or visible rows")
			}
			want := error(domain.ErrDocRangeIndexUnavailable)
			if existing {
				want = nil
			}
			docVisiblePGRead(test, storage, ctx, repo, fixture.hash, want)
		})
	}
}

func docRangeVisiblePassPGRangeImage(test *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash, objectHashes []string) string {
	test.Helper()
	var image string
	if err := storage.db(ctx).QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT jsonb_build_array(header.ctid::text,header.xmin::text,to_jsonb(header)) FROM doc_range_indexes_v1 header WHERE hash=$1),
 (SELECT jsonb_build_array(ranges.ctid::text,ranges.xmin::text,to_jsonb(ranges)) FROM doc_range_generations_v2 ranges WHERE doc_hash=$1),
 (SELECT jsonb_agg(jsonb_build_array(page.ctid::text,page.xmin::text,to_jsonb(page)) ORDER BY ordinal) FROM doc_range_pages_v1 page WHERE doc_hash=$1),
 (SELECT jsonb_agg(jsonb_build_array(object.ctid::text,object.xmin::text,to_jsonb(object)) ORDER BY hash) FROM doc_range_objects_v2 object WHERE doc_hash=$1 AND hash=ANY($2::text[]))
 )::text`, hash, objectHashes).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return image
}

func TestDocRangeVisiblePassPGVisibleOnlyRepairPreservesRangeTuples(test *testing.T) {
	storage, ctx, repo, fixture := docVisiblePGFixture(test, 257)
	index, err := domain.BuildDocRangeIndex(ctx, rootPublicationProof(test, fixture))
	if err != nil {
		test.Fatal(err)
	}
	proof, objects, err := domain.BuildVerifiedDocRangeGeneration(ctx, index)
	if err != nil {
		test.Fatal(err)
	}
	objectHashes := make([]string, 0, len(objects)+len(index.Header().Pages))
	for _, object := range objects {
		objectHashes = append(objectHashes, string(object.Hash))
	}
	for _, hash := range index.Header().Pages {
		objectHashes = append(objectHashes, string(hash))
	}
	before := docRangeVisiblePassPGRangeImage(test, storage, ctx, fixture.hash, objectHashes)
	var missing domain.ContentHash
	want, err := domain.BuildDocVisibleGeneration(ctx, proof, func(_ context.Context, object domain.DocRangeObject) error {
		if missing == "" {
			missing = object.Hash
		}
		return nil
	})
	if err != nil || missing == "" {
		test.Fatal("visible repair fixture emitted no object", err)
	}
	for _, hash := range objectHashes {
		if hash == string(missing) {
			test.Fatal("repair fixture selected a range object")
		}
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if _, err := storage.db(bound).Exec(bound, `DELETE FROM doc_visible_generations_v1 WHERE doc_hash=$1`, fixture.hash); err != nil {
			return err
		}
		tag, err := storage.db(bound).Exec(bound, `DELETE FROM doc_range_objects_v2 WHERE doc_hash=$1 AND hash=$2`, fixture.hash, missing)
		if err == nil && tag.RowsAffected() != 1 {
			test.Fatal("repair fixture did not remove a visible object", tag.RowsAffected())
		}
		return err
	}); err != nil {
		test.Fatal(err)
	}
	docVisiblePGRead(test, storage, ctx, repo, fixture.hash, domain.ErrDocRangeIndexUnavailable)
	if after := docRangeVisiblePassPGRangeImage(test, storage, ctx, fixture.hash, objectHashes); after != before {
		test.Fatal("visible damage changed range tuples")
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		transaction := *bound.Value(repositoryTxKey{}).(*repositoryTx)
		recorder := &docRangeVisiblePassBatchTx{Tx: transaction.Tx, testing: test, hash: fixture.hash}
		transaction.Tx = recorder
		write := context.WithValue(bound, repositoryTxKey{}, &transaction)
		if err := storage.putVisibleGeneration(write, repo, proof); err != nil {
			return err
		}
		if recorder.rangePages != 0 || recorder.visibleObjects == 0 {
			test.Fatal("visible-only repair emitted range pages or no visible objects", recorder.rangePages, recorder.visibleObjects)
		}
		if after := docRangeVisiblePassPGRangeImage(test, storage, write, fixture.hash, objectHashes); after != before {
			test.Fatal("visible-only repair rewrote range row identity or bytes")
		}
		docVisiblePGRead(test, storage, ctx, repo, fixture.hash, domain.ErrDocRangeIndexUnavailable)
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if after := docRangeVisiblePassPGRangeImage(test, storage, ctx, fixture.hash, objectHashes); after != before {
		test.Fatal("visible-only repair committed rewritten range tuples")
	}
	got, err := storage.readVisibleGeneration(ctx, repo, fixture.hash)
	if err != nil || !reflect.DeepEqual(got, want) {
		test.Fatal("visible-only repair changed visible descriptor", got, want, err)
	}
	docVisiblePGRead(test, storage, ctx, repo, fixture.hash, nil)
}
