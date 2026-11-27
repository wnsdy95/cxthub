//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
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
