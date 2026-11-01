//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type docRangeEnvelopeTracePG struct {
	headerBodies atomic.Int64
	forbidden    atomic.Int64
	unbounded    atomic.Int64
}

func (trace *docRangeEnvelopeTracePG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.Contains(query.SQL, "substring(part.bytes") {
		trace.headerBodies.Add(1)
		if len(query.Args) != 10 {
			trace.unbounded.Add(1)
		} else if length, ok := query.Args[9].(int); !ok || length < 1 || length > 64<<10 {
			trace.unbounded.Add(1)
		}
	}
	for _, marker := range []string{"source.bytes", "root.bytes", "b.bytes", "SELECT bytes FROM blobs"} {
		if strings.Contains(query.SQL, marker) {
			trace.forbidden.Add(1)
			break
		}
	}
	return ctx
}

func (*docRangeEnvelopeTracePG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func docRangeEnvelopePublishedPG(test *testing.T, storage *PostgresStore, ctx context.Context, headerBytes int) legacyByteStagePGFixture {
	test.Helper()
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	fixture := legacyByteStagePGDocument(test, storage, ctx, repo, domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
			SessionOriginID: string(repo), Cwd: strings.Repeat("h", headerBytes) + "\ud55c🙂", Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: string(repo) + strings.Repeat("body", 16<<10)}}}},
	})
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	if err := storage.BuildDocRangeJob(ctx, job); err != nil {
		test.Fatal("actual worker did not publish the header", err)
	}
	return fixture
}

func docRangeEnvelopeReaderPG(test *testing.T, storage *PostgresStore, ctx context.Context, trace *docRangeEnvelopeTracePG) *PostgresStore {
	test.Helper()
	config := storage.pool.Config()
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	return &PostgresStore{pool: pool}
}

func TestDocRangeEnvelopePGLazyBoundedAndScoped(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	fixture := docRangeEnvelopePublishedPG(test, storage, ctx, 300<<10)
	foreign, _ := docRangeJobPGRepo(test, storage, ctx)
	trace := &docRangeEnvelopeTracePG{}
	peer := docRangeEnvelopeReaderPG(test, storage, ctx, trace)
	var retained *postgresDocRangeGenerationRead
	var retainedContext context.Context
	if err := peer.WithinReadSnapshot(ctx, func(bound context.Context) error {
		read, err := peer.openDocRangeGeneration(bound, fixture.repo, fixture.hash)
		if err != nil {
			return err
		}
		retained, retainedContext = read, bound
		descriptor := read.Descriptor()
		if descriptor.Version != 6 || descriptor.Envelope != nil || descriptor.EnvelopeRef == nil ||
			descriptor.EnvelopeRef.Bytes != len(fixture.envelope) || descriptor.EnvelopeRef.Hash != domain.HashContent(fixture.envelope) {
			test.Fatal("large header was not a reference-only descriptor", descriptor)
		}
		descriptor.EnvelopeRef.Hash = domain.HashContent([]byte("changed caller copy"))
		descriptor.EnvelopeRef.Bytes++
		if got := read.Descriptor(); got.EnvelopeRef.Hash != domain.HashContent(fixture.envelope) || got.EnvelopeRef.Bytes != len(fixture.envelope) {
			test.Fatal("descriptor exposed its reference owner")
		}
		if trace.headerBodies.Load() != 0 {
			test.Fatal("descriptor fetched the header body")
		}
		parts := int64((len(fixture.envelope) + (64 << 10) - 1) / (64 << 10))
		for attempt := int64(1); attempt <= 2; attempt++ {
			envelope, err := read.Envelope(bound)
			if err != nil || !bytes.Equal(envelope, fixture.envelope) {
				test.Fatal("referenced header changed bytes", err)
			}
			envelope[0] = '!'
			if trace.headerBodies.Load() != parts*attempt || len(read.objects) != 0 || read.generation.Envelope != nil {
				test.Fatal("reader retained header bodies or changed the selected read extent", trace.headerBodies.Load(), parts, attempt)
			}
		}
		if trace.forbidden.Load() != 0 || trace.unbounded.Load() != 0 {
			test.Fatal("header read fetched the original source or an unbounded SQL body")
		}
		canceled, cancel := context.WithCancel(bound)
		cancel()
		if body, err := read.Envelope(canceled); !errors.Is(err, context.Canceled) || body != nil {
			test.Fatal("canceled read exposed bytes", err)
		}
		if body, err := read.Envelope(ctx); !errors.Is(err, domain.ErrConflict) || body != nil {
			test.Fatal("unbound read exposed bytes", err)
		}
		if _, err := peer.OpenDocRangeGeneration(bound, foreign, fixture.hash); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("foreign repository opened a referenced header", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if body, err := retained.Envelope(retainedContext); err == nil || body != nil {
		test.Fatal("closed transaction exposed a header", err)
	}
	if err := peer.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if body, err := retained.Envelope(bound); !errors.Is(err, domain.ErrConflict) || body != nil {
			test.Fatal("another transaction reused the retained header reader", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestDocRangeEnvelopePGApplicationReadsPublishedHeaderAndEvents(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	fixture := docRangeEnvelopePublishedPG(test, storage, ctx, 300<<10)
	trace := &docRangeEnvelopeTracePG{}
	peer := docRangeEnvelopeReaderPG(test, storage, ctx, trace)
	peer.ConfigureLegacyByteRangePublication(true)
	peer.ConfigureLegacyDerivedByteRangePublication(true)
	service := app.NewService(peer, peer, nil, nil, peer)
	page, err := service.ReadDocViewEvents(inbound.WithSystemActor(ctx), fixture.repo, fixture.hash, "", 0, 20, "chat")
	if err != nil {
		test.Fatal("application could not read its published referenced-header generation", err)
	}
	var expected domain.CIRDocument
	if err := json.Unmarshal(fixture.raw, &expected); err != nil {
		test.Fatal(err)
	}
	if page.Hash != fixture.hash || page.DocIdentity != domain.DocumentIdentityLegacy || page.Total != 1 || len(page.Events) != 1 ||
		page.Envelope.Cwd != expected.Envelope.Cwd || page.Envelope.SessionOriginID != expected.Envelope.SessionOriginID {
		test.Fatal("application changed header fields, event visibility or document identity", page.Hash, page.Total, len(page.Events))
	}
	if trace.headerBodies.Load() == 0 || trace.forbidden.Load() != 0 || trace.unbounded.Load() != 0 {
		test.Fatal("application bypassed the verified header accessor or fetched the original blob", trace.headerBodies.Load(), trace.forbidden.Load(), trace.unbounded.Load())
	}
}

func TestDocRangeEnvelopePGInlineCopyAndReferenceFences(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	small := docRangeEnvelopePublishedPG(test, storage, ctx, 128)
	large := docRangeEnvelopePublishedPG(test, storage, ctx, 300<<10)
	foreign, _ := docRangeJobPGRepo(test, storage, ctx)
	trace := &docRangeEnvelopeTracePG{}
	peer := docRangeEnvelopeReaderPG(test, storage, ctx, trace)
	if err := peer.WithinReadSnapshot(ctx, func(bound context.Context) error {
		read, err := peer.openDocRangeGeneration(bound, small.repo, small.hash)
		if err != nil {
			return err
		}
		if read.Descriptor().Version != 5 {
			test.Fatal("small legacy header no longer uses version 5")
		}
		for range 2 {
			envelope, err := read.Envelope(bound)
			if err != nil || !bytes.Equal(envelope, small.envelope) {
				test.Fatal("inline envelope lost its private copy", err)
			}
			envelope[0] = '!'
		}
		if trace.headerBodies.Load() != 0 {
			test.Fatal("inline envelope used the header body store")
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	for _, fault := range []string{"repository", "generation", "source-version", "aggregate-hash", "resource-limit"} {
		test.Run(fault, func(test *testing.T) {
			if err := peer.WithinReadSnapshot(ctx, func(bound context.Context) error {
				read, err := peer.openDocRangeGeneration(bound, large.repo, large.hash)
				if err != nil {
					return err
				}
				expected := domain.ErrIntegrity
				before := trace.headerBodies.Load()
				switch fault {
				case "repository":
					read.repo = foreign
				case "generation":
					read.binding.RangeGeneration = domain.HashContent([]byte("another generation"))
				case "source-version":
					read.binding.SourceVersion++
				case "aggregate-hash":
					read.generation.EnvelopeRef.Hash = domain.HashContent([]byte("different envelope"))
				case "resource-limit":
					read.generation.EnvelopeRef.Bytes = maxLegacyReadBytes + 1
					expected = domain.ErrDocumentResourceLimit
				}
				if body, err := read.Envelope(bound); !errors.Is(err, expected) || body != nil {
					test.Fatal("invalid header binding exposed bytes or changed error class", len(body), err, expected)
				}
				if fault == "resource-limit" && trace.headerBodies.Load() != before {
					test.Fatal("resource rejection fetched header bytes")
				}
				return nil
			}); err != nil {
				test.Fatal(err)
			}
		})
	}
}

func TestDocRangeEnvelopePGPublicationGuards(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	fixture := docRangeEnvelopePublishedPG(test, storage, ctx, 300<<10)
	var descriptor []byte
	if err := storage.pool.QueryRow(ctx, `SELECT descriptor FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash).Scan(&descriptor); err != nil {
		test.Fatal(err)
	}
	for _, fault := range []string{"protocol", "inline", "missing-reference", "hash", "small-reference", "oversized-reference", "extent", "foreign-plan", "missing-header", "incomplete-header"} {
		test.Run(fault, func(test *testing.T) {
			transaction, err := storage.pool.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(transaction)
			if fault != "protocol" {
				if _, err := transaction.Exec(ctx, `SET LOCAL cxt.doc_range_worker_protocol='5'`); err != nil {
					test.Fatal(err)
				}
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(descriptor, &payload); err != nil {
				test.Fatal(err)
			}
			reference := domain.DocRangeEnvelopeReference{Hash: domain.HashContent(fixture.envelope), Bytes: len(fixture.envelope)}
			switch fault {
			case "inline":
				payload["envelope"] = json.RawMessage(`{}`)
			case "missing-reference":
				delete(payload, "envelope_ref")
			case "hash":
				reference.Hash = "sha256:invalid"
			case "small-reference":
				reference.Bytes = domain.MaxConversationManifestBytes
			case "oversized-reference":
				reference.Bytes = domain.MaxConversationDocumentBytes + 1
			case "extent":
				reference.Bytes++
			case "missing-header":
				if _, err := transaction.Exec(ctx, `ALTER TABLE doc_legacy_byte_stage_headers_v1 DISABLE TRIGGER guard_legacy_byte_header_parts`); err != nil {
					test.Fatal(err)
				}
				if _, err := transaction.Exec(ctx, `DELETE FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1 AND ordinal=0`, fixture.hash); err != nil {
					test.Fatal(err)
				}
			case "incomplete-header":
				if _, err := transaction.Exec(ctx, `SET LOCAL cxt.legacy_byte_stage_protocol='1'`); err != nil {
					test.Fatal(err)
				}
				if _, err := transaction.Exec(ctx, `ALTER TABLE doc_legacy_byte_stages_v1 DISABLE TRIGGER guard_legacy_byte_header_stage`); err != nil {
					test.Fatal(err)
				}
				if _, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1 SET header_next=header_next-1,header_complete=false WHERE doc_hash=$1`, fixture.hash); err != nil {
					test.Fatal(err)
				}
			}
			if fault != "missing-reference" {
				raw, err := json.Marshal(reference)
				if err != nil {
					test.Fatal(err)
				}
				payload["envelope_ref"] = raw
			}
			changed, err := json.Marshal(payload)
			if err != nil {
				test.Fatal(err)
			}
			if fault == "foreign-plan" {
				_, err = transaction.Exec(ctx, `UPDATE doc_range_generations_v2 SET legacy_byte_plan_hash=$2 WHERE doc_hash=$1`,
					fixture.hash, domain.HashContent([]byte("foreign plan")))
			} else {
				_, err = transaction.Exec(ctx, `UPDATE doc_range_generations_v2 SET descriptor=$2,generation_hash=$3 WHERE doc_hash=$1`,
					fixture.hash, changed, domain.HashContent(changed))
			}
			expected := "23514"
			if fault == "protocol" {
				expected = "0A000"
			}
			var rejected *pgconn.PgError
			if !errors.As(err, &rejected) || rejected.Code != expected {
				test.Fatal("publication bypassed its original or referenced-header guard", err, expected)
			}
		})
	}
}

func TestDocRangeEnvelopePGRejectsChangedHeaderParts(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	for _, fault := range []string{"aggregate", "length", "ordinal", "missing"} {
		test.Run(fault, func(test *testing.T) {
			fixture := docRangeEnvelopePublishedPG(test, storage, ctx, 300<<10)
			transaction, err := storage.pool.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(transaction)
			if _, err := transaction.Exec(ctx, `ALTER TABLE doc_legacy_byte_stage_headers_v1 DISABLE TRIGGER guard_legacy_byte_header_parts`); err != nil {
				test.Fatal(err)
			}
			body := bytes.Clone(fixture.envelope[:64<<10])
			switch fault {
			case "aggregate":
				body[0] = '!'
				_, err = transaction.Exec(ctx, `UPDATE doc_legacy_byte_stage_headers_v1 SET bytes=$2,hash=$3 WHERE doc_hash=$1 AND ordinal=0`,
					fixture.hash, body, domain.HashContent(body))
			case "length":
				body = body[:len(body)-1]
				_, err = transaction.Exec(ctx, `UPDATE doc_legacy_byte_stage_headers_v1 SET bytes=$2,hash=$3 WHERE doc_hash=$1 AND ordinal=0`,
					fixture.hash, body, domain.HashContent(body))
			case "ordinal":
				_, err = transaction.Exec(ctx, `UPDATE doc_legacy_byte_stage_headers_v1 SET ordinal=8191 WHERE doc_hash=$1 AND ordinal=0`, fixture.hash)
			case "missing":
				_, err = transaction.Exec(ctx, `DELETE FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1 AND ordinal=0`, fixture.hash)
			}
			if err != nil {
				test.Fatal("could not seed isolated header damage", err)
			}
			if _, err := transaction.Exec(ctx, `ALTER TABLE doc_legacy_byte_stage_headers_v1 ENABLE TRIGGER guard_legacy_byte_header_parts`); err != nil {
				test.Fatal(err)
			}
			if err := transaction.Commit(ctx); err != nil {
				test.Fatal(err)
			}
			if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
				read, err := storage.openDocRangeGeneration(bound, fixture.repo, fixture.hash)
				if err != nil {
					return err
				}
				if body, err := read.Envelope(bound); !errors.Is(err, domain.ErrIntegrity) || body != nil {
					test.Fatal("damaged selected part exposed a header", len(body), err)
				}
				return nil
			}); err != nil {
				test.Fatal(err)
			}
		})
	}
}
