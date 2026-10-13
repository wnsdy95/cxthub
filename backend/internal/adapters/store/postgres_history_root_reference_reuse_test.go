//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGHistoryRootReferenceEvidenceBinding(test *testing.T) {
	fixture := rootFixture(test, "reference evidence")
	verified, err := domain.VerifyConversationManifestReference(context.Background(), fixture.hash, fixture.manifest, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		return fixture.bodies[hash], nil
	})
	if err != nil {
		test.Fatal(err)
	}
	legacy, err := domain.VerifyStoredDocBytes(domain.HashContent(fixture.canonical), fixture.canonical)
	if err != nil {
		test.Fatal(err)
	}
	for _, mode := range []string{"valid", "no-scope", "inactive", "attempted", "foreign-store", "mismatched-repo", "no-tx", "foreign-tx", "write-tx", "canceled", "invalid-proof", "legacy-proof"} {
		test.Run(mode, func(test *testing.T) {
			owner := &PostgresStore{}
			caller := owner
			repo := domain.HashContent([]byte(test.Name()))
			private := &historyDocumentProofPG{owner: owner, repo: repo, active: true}
			tx := &repositoryTx{owner: owner, readOnly: true}
			ctx := context.WithValue(context.Background(), historyDocumentProofKeyPG{}, private)
			ctx = context.WithValue(ctx, repositoryTxKey{}, tx)
			proof := verified
			switch mode {
			case "no-scope":
				ctx = context.WithValue(context.Background(), repositoryTxKey{}, tx)
			case "inactive":
				private.active = false
			case "attempted":
				private.attempted = true
			case "foreign-store":
				caller = &PostgresStore{}
			case "mismatched-repo":
				repo = domain.HashContent([]byte("another repository"))
			case "no-tx":
				ctx = context.WithValue(context.Background(), historyDocumentProofKeyPG{}, private)
			case "foreign-tx":
				tx.owner = &PostgresStore{}
			case "write-tx":
				tx.readOnly, tx.repo = false, repo
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			case "invalid-proof":
				proof = domain.VerifiedDocReference{}
			case "legacy-proof":
				proof = legacy
			}
			caller.rememberHistoryRootReference(ctx, repo, proof)
			if mode != "valid" {
				if len(private.rootReferences) != 0 {
					test.Fatal("unbound evidence retained")
				}
				return
			}
			recorded, present := private.rootReferences[verified.DocumentRef()]
			if !present || len(private.rootReferences) != 1 || recorded.tx != tx || recorded.proof != verified {
				test.Fatal("missing exact transaction/reference evidence")
			}
		})
	}
	owner := &PostgresStore{}
	repo := domain.HashContent([]byte(test.Name()))
	marker := errors.New("scope failed")
	var retained *historyDocumentProofPG
	err = owner.WithinHistoryDocumentProof(context.Background(), repo, func(ctx context.Context, proof outbound.HistoryDocumentProof) error {
		retained = proof.(*historyDocumentProofPG)
		read := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{owner: owner, readOnly: true})
		owner.rememberHistoryRootReference(read, repo, verified)
		if len(retained.rootReferences) != 1 {
			test.Fatal("missing in-scope evidence")
		}
		return marker
	})
	if err != marker || retained.active || retained.rootReferences != nil {
		test.Fatal("failed scope retained request evidence", err)
	}
}

type historyRootReferenceReadKeyPG struct{}

type historyRootReferenceReadsPG struct {
	documents atomic.Int32
	chunks    atomic.Int32
}

func (reads *historyRootReferenceReadsPG) reset() {
	reads.documents.Store(0)
	reads.chunks.Store(0)
}

type historyRootReferenceTracePG struct{}

func (historyRootReferenceTracePG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	reads, ok := ctx.Value(historyRootReferenceReadKeyPG{}).(*historyRootReferenceReadsPG)
	if !ok || !strings.Contains(query.SQL, "b.bytes") {
		return ctx
	}
	if strings.Contains(query.SQL, "rb.kind='chunk'") || len(query.Args) > 2 && query.Args[1] == "chunk" {
		reads.chunks.Add(1)
	} else {
		reads.documents.Add(1)
	}
	return ctx
}

func (historyRootReferenceTracePG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func historyRootReferenceFixturePG(test *testing.T) (historyProofFixturePG, *historyRootReferenceReadsPG) {
	test.Helper()
	fixture := historyRootProofFixture(test)
	config := fixture.s.pool.Config()
	config.ConnConfig.Tracer = historyRootReferenceTracePG{}
	pool, err := pgxpool.NewWithConfig(fixture.ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	fixture.s = &PostgresStore{pool: pool}
	reads := &historyRootReferenceReadsPG{}
	fixture.ctx = context.WithValue(fixture.ctx, historyRootReferenceReadKeyPG{}, reads)
	return fixture, reads
}

func TestPGHistoryRootReferenceReuseSameRR(test *testing.T) {
	fixture, reads := historyRootReferenceFixturePG(test)
	for _, phase := range []string{"cold", "warm"} {
		test.Run(phase, func(test *testing.T) {
			var retained *historyDocumentProofPG
			err := fixture.s.WithinHistoryDocumentProof(fixture.ctx, fixture.repo, func(ctx context.Context, proof outbound.HistoryDocumentProof) error {
				retained = proof.(*historyDocumentProofPG)
				err := fixture.s.WithinReadSnapshot(ctx, func(read context.Context) error {
					reads.reset()
					verified, err := fixture.s.VerifyStoredDoc(read, fixture.repo, fixture.snap.DocHash)
					if err != nil {
						return err
					}
					if !verified.Matches(fixture.snap) || reads.chunks.Load() == 0 || len(retained.rootReferences) != 1 {
						test.Fatal("preflight did not fully verify and retain reference evidence")
					}
					preflightReads := reads.chunks.Load()
					reads.reset()
					if err := proof.Capture(read, []domain.Snapshot{fixture.snap}); err != nil {
						return err
					}
					if reads.chunks.Load() != 0 || reads.documents.Load() != 1 || retained.rootReferences != nil {
						test.Fatal("capture duplicated closure bodies or retained transaction evidence")
					}
					if len(retained.snapshots) != 1 || len(retained.blobs) != len(fixture.chunks)+1 || len(retained.grants["chunk"]) != len(fixture.chunks) || len(retained.grants["doc"]) != 1 {
						test.Fatal("capture lost complete deduplicated closure metadata")
					}
					test.Logf("preflight_chunk_queries=%d capture_chunk_queries=%d capture_descriptor_queries=%d", preflightReads, reads.chunks.Load(), reads.documents.Load())
					return nil
				})
				if err != nil {
					return err
				}
				reads.reset()
				if err := fixture.s.WithinRepository(ctx, fixture.repo, proof.Pin); err != nil {
					return err
				}
				if reads.chunks.Load() != 0 || reads.documents.Load() != 0 {
					test.Fatal("pin read document bodies")
				}
				return nil
			})
			if err != nil || retained.active || retained.rootReferences != nil {
				test.Fatal("scope completion", err)
			}
		})
	}
}

func TestPGHistoryRootReferenceReuseFallback(test *testing.T) {
	for _, mode := range []string{"no-verification", "standalone-verification", "cross-rr", "copied-wrapper", "cross-rr-corrupt-tail", "direct-corrupt-tail", "failed-tail", "canceled", "foreign-store", "mismatched-repo", "other-reference"} {
		test.Run(mode, func(test *testing.T) {
			fixture, reads := historyRootReferenceFixturePG(test)
			corrupt := func() error {
				bad := bytes.Clone(fixture.chunks[fixture.last])
				bad[len(bad)-1] ^= 1
				_, err := fixture.s.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.last, docCompress(bad))
				return err
			}
			otherRepo := domain.HashContent([]byte(string(fixture.repo) + "/other"))
			var otherHash domain.ContentHash
			if mode == "mismatched-repo" {
				if _, err := fixture.s.PutRepo(fixture.ctx, domain.Repo{ID: otherRepo}); err != nil {
					test.Fatal(err)
				}
				if _, err := fixture.s.pool.Exec(fixture.ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) SELECT $2,kind,hash FROM repo_blobs WHERE repo_id=$1`, fixture.repo, otherRepo); err != nil {
					test.Fatal(err)
				}
			}
			if mode == "other-reference" {
				other := uniqueRootPublicationFixturePG(test, "another root")
				seedRootPG(test, fixture.ctx, fixture.s, fixture.repo, other)
				otherHash = other.hash
			}
			err := fixture.s.WithinHistoryDocumentProof(fixture.ctx, fixture.repo, func(ctx context.Context, proof outbound.HistoryDocumentProof) error {
				private := proof.(*historyDocumentProofPG)
				verify := func(read context.Context) error {
					_, err := fixture.s.VerifyStoredDoc(read, fixture.repo, fixture.snap.DocHash)
					return err
				}
				switch mode {
				case "standalone-verification":
					if err := verify(ctx); err != nil {
						return err
					}
					if len(private.rootReferences) != 0 {
						test.Fatal("internally completed transaction left reusable evidence")
					}
				case "cross-rr", "cross-rr-corrupt-tail":
					if err := fixture.s.WithinReadSnapshot(ctx, verify); err != nil {
						return err
					}
					if len(private.rootReferences) != 1 {
						test.Fatal("cross-RR regression needs prior transaction evidence")
					}
				}
				corruptTail := mode == "cross-rr-corrupt-tail" || mode == "direct-corrupt-tail" || mode == "failed-tail"
				if corruptTail {
					if err := corrupt(); err != nil {
						return err
					}
				}
				err := fixture.s.WithinReadSnapshot(ctx, func(read context.Context) error {
					switch mode {
					case "copied-wrapper":
						if err := verify(read); err != nil {
							return err
						}
						copied := *read.Value(repositoryTxKey{}).(*repositoryTx)
						read = context.WithValue(read, repositoryTxKey{}, &copied)
					case "failed-tail":
						if err := verify(read); !errors.Is(err, domain.ErrIntegrity) {
							test.Fatal("failed verification contract", err)
						}
					case "canceled":
						canceled, cancel := context.WithCancel(read)
						cancel()
						if err := verify(canceled); !errors.Is(err, context.Canceled) {
							test.Fatal("canceled verification contract", err)
						}
					case "foreign-store":
						other := &PostgresStore{pool: fixture.s.pool}
						if verified, err := other.VerifyStoredDoc(read, fixture.repo, fixture.snap.DocHash); !errors.Is(err, domain.ErrConflict) || verified.Valid() {
							test.Fatal("foreign store accepted transaction", err)
						}
					case "mismatched-repo":
						if _, err := fixture.s.VerifyStoredDoc(read, otherRepo, fixture.snap.DocHash); err != nil {
							return err
						}
					case "other-reference":
						if _, err := fixture.s.VerifyStoredDoc(read, fixture.repo, otherHash); err != nil {
							return err
						}
					}
					if mode == "failed-tail" || mode == "canceled" || mode == "foreign-store" || mode == "mismatched-repo" {
						if len(private.rootReferences) != 0 {
							test.Fatal("unusable verification left evidence")
						}
					}
					reads.reset()
					err := proof.Capture(read, []domain.Snapshot{fixture.snap})
					if reads.chunks.Load() == 0 || reads.documents.Load() != 1 || private.rootReferences != nil {
						test.Fatal("fallback skipped full current verification or retained evidence")
					}
					test.Logf("fallback_chunk_queries=%d capture_descriptor_queries=%d", reads.chunks.Load(), reads.documents.Load())
					return err
				})
				if corruptTail {
					if !errors.Is(err, domain.ErrIntegrity) {
						test.Fatal("fallback accepted current corrupt tail", err)
					}
					if pinErr := fixture.s.WithinRepository(ctx, fixture.repo, proof.Pin); !errors.Is(pinErr, domain.ErrConflict) {
						test.Fatal("failed fallback left usable pins", pinErr)
					}
					return nil
				}
				if err != nil {
					return err
				}
				return fixture.s.WithinRepository(ctx, fixture.repo, proof.Pin)
			})
			if err != nil {
				test.Fatal(err)
			}
		})
	}
}

func TestPGHistoryRootReferenceReuseSameRRMutationFailsPin(test *testing.T) {
	fixture, reads := historyRootReferenceFixturePG(test)
	err := fixture.s.WithinHistoryDocumentProof(fixture.ctx, fixture.repo, func(ctx context.Context, proof outbound.HistoryDocumentProof) error {
		if err := fixture.s.WithinReadSnapshot(ctx, func(read context.Context) error {
			if _, err := fixture.s.VerifyStoredDoc(read, fixture.repo, fixture.snap.DocHash); err != nil {
				return err
			}
			bad := bytes.Clone(fixture.chunks[fixture.last])
			bad[len(bad)-1] ^= 1
			if _, err := fixture.s.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.last, docCompress(bad)); err != nil {
				return err
			}
			reads.reset()
			if err := proof.Capture(read, []domain.Snapshot{fixture.snap}); err != nil {
				return err
			}
			if reads.chunks.Load() != 0 || reads.documents.Load() != 1 {
				test.Fatal("same-RR regression did not exercise reference reuse")
			}
			return nil
		}); err != nil {
			return err
		}
		return fixture.s.WithinRepository(ctx, fixture.repo, func(write context.Context) error {
			if err := fixture.s.AdvanceRepositoryRevision(write, fixture.repo, false); err != nil {
				return err
			}
			return proof.Pin(write)
		})
	})
	if !errors.Is(err, domain.ErrConflict) {
		test.Fatal("old verified reference pinned changed current bytes", err)
	}
	revision, err := fixture.s.RepositoryRevision(fixture.ctx, fixture.repo)
	if err != nil || revision.Graph != 0 {
		test.Fatal("pin conflict failed to roll back publication", revision, err)
	}
}
