//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
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
