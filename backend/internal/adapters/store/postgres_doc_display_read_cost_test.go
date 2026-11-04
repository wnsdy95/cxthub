//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocDisplayGenerationSourceCurrentRows(test *testing.T) {
	source := domain.DocDisplayStringSource{Version: 1, Profile: domain.DocDisplayStringProfile,
		EventHash: domain.HashContent([]byte("event")), EventBytes: 2, Token: domain.DocFindSpan{From: 0, To: 2}, Empty: true}
	raw, err := source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	hash := domain.HashContent(raw)
	generation := domain.DocDisplayStringGeneration{Version: 1, Source: source}
	target := domain.DocDisplayTarget{Event: domain.DocEventIndex{Hash: source.EventHash, Length: source.EventBytes}}
	if err := verifyDocDisplayGenerationSource(target, generation, hash, &hash, raw); err != nil {
		test.Fatal("valid fresh rows rejected", err)
	}
	for _, scenario := range []string{"missing-source", "empty-source", "oversized-source", "changed-bytes", "changed-hash", "null-source", "event-hash", "event-length", "source-binding", "generation-source"} {
		test.Run(scenario, func(test *testing.T) {
			body := bytes.Clone(raw)
			current := hash
			currentHash := &current
			expectedHash, selected, saved := hash, target, generation
			switch scenario {
			case "missing-source":
				currentHash, body = nil, nil
			case "empty-source":
				body = nil
			case "oversized-source":
				body = bytes.Repeat([]byte(" "), domain.MaxDocDisplaySourceBytes+1)
				current = domain.HashContent(body)
			case "changed-bytes":
				body[0] = '['
			case "changed-hash":
				current = domain.HashContent([]byte("other"))
			case "null-source":
				body = []byte("null")
				current = domain.HashContent(body)
			case "event-hash":
				selected.Event.Hash = domain.HashContent([]byte("other"))
			case "event-length":
				selected.Event.Length++
			case "source-binding":
				expectedHash = domain.HashContent([]byte("other"))
			case "generation-source":
				saved.Source.EventHash = domain.HashContent([]byte("other"))
			}
			if err := verifyDocDisplayGenerationSource(selected, saved, expectedHash, currentHash, body); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("unverified source row accepted", err)
			}
		})
	}
}

func TestDocDisplayGenerationReadTransactionGate(test *testing.T) {
	storage, foreign := &PostgresStore{}, &PostgresStore{}
	for _, scenario := range []string{"missing", "writeable", "foreign", "cancelled"} {
		test.Run(scenario, func(test *testing.T) {
			ctx := context.Background()
			want := domain.ErrConflict
			switch scenario {
			case "writeable":
				ctx = context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{owner: storage})
			case "foreign":
				ctx = context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{owner: foreign, readOnly: true})
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx, want = cancelled, context.Canceled
			}
			generation, err := storage.ReadDocDisplayGeneration(ctx, domain.DocDisplayTarget{})
			if !errors.Is(err, want) || generation != (domain.DocDisplayStringGeneration{}) {
				test.Fatal("unpinned read reached database work", generation, err)
			}
		})
	}
}

type docDisplayReadCostTrace struct {
	settings int
	joined   int
	separate int
	bounded  bool
}

func (trace *docDisplayReadCostTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.Contains(query.SQL, "current_setting('transaction_isolation')") {
		trace.settings++
	}
	if strings.Contains(query.SQL, "FROM doc_display_generations_v1 generation LEFT JOIN doc_display_sources_v1 source") {
		trace.joined++
		trace.bounded = len(query.Args) == 3 && query.Args[2] == domain.MaxDocDisplaySourceBytes+1 &&
			strings.Contains(query.SQL, "substring(generation.bytes FROM 1 FOR $3)") && strings.Contains(query.SQL, "substring(source.bytes FROM 1 FOR $3)")
	}
	if strings.Contains(query.SQL, "FROM doc_display_sources_v1 WHERE") {
		trace.separate++
	}
	return ctx
}

func (*docDisplayReadCostTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestDocDisplayReadPGBoundedFreshRows(test *testing.T) {
	fixture := docDisplayPG(test)
	fixture.enqueue(test, 0)
	job := fixture.claim(test)
	prepared := fixture.done(test, job)
	trace := &docDisplayReadCostTrace{}
	config := fixture.storage.pool.Config()
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(fixture.ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	reader := &PostgresStore{pool: pool}
	if err := reader.WithinReadSnapshot(fixture.ctx, func(ctx context.Context) error {
		generation, err := reader.ReadDocDisplayGeneration(ctx, job.Target)
		if !errors.Is(err, domain.ErrNotFound) || generation != (domain.DocDisplayStringGeneration{}) {
			test.Fatal("missing generation changed its not-found classification", generation, err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.CompleteDocDisplay(fixture.ctx, job, prepared.Checkpoint, time.Now()); err != nil {
		test.Fatal(err)
	}
	for repeat := 0; repeat < 2; repeat++ {
		*trace = docDisplayReadCostTrace{}
		if err := reader.WithinReadSnapshot(fixture.ctx, func(ctx context.Context) error {
			page, err := reader.ReadDocDisplayPage(ctx, job.Target, 0)
			if err != nil || page.Index != 0 || page.From != 0 || page.Text == "" {
				test.Fatal("fresh page read failed", page, err)
			}
			return nil
		}); err != nil {
			test.Fatal(err)
		}
		if trace.settings != 1 || trace.joined != 1 || trace.separate != 0 || !trace.bounded {
			test.Fatal("page read repeated proof queries, reused prior transaction rows or lost byte caps", repeat, *trace)
		}
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_sources_v1 SET bytes=set_byte(bytes,0,91) WHERE source_hash=$1`, prepared.State.Source); err != nil {
		test.Fatal(err)
	}
	if err := reader.WithinReadSnapshot(fixture.ctx, func(ctx context.Context) error {
		page, err := reader.ReadDocDisplayPage(ctx, job.Target, 0)
		if !errors.Is(err, domain.ErrIntegrity) || page.Text != "" || page.Runs != nil {
			test.Fatal("a previous successful transaction hid current source corruption", page, err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestDocDisplayReadPGActualTransactionSettings(test *testing.T) {
	fixture := docDisplayPG(test)
	for _, options := range []pgx.TxOptions{
		{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
		{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadWrite},
		{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
	} {
		test.Run(string(options.IsoLevel)+"/"+string(options.AccessMode), func(test *testing.T) {
			transaction, err := fixture.storage.pool.BeginTx(fixture.ctx, options)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(transaction)
			ctx := context.WithValue(fixture.ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: fixture.storage, readOnly: true})
			generation, err := fixture.storage.ReadDocDisplayGeneration(ctx, fixture.targets[0])
			if !errors.Is(err, domain.ErrConflict) || generation != (domain.DocDisplayStringGeneration{}) {
				test.Fatal("actual transaction settings were replaced by the caller's readOnly flag", generation, err)
			}
		})
	}
}
