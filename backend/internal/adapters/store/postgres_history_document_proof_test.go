//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type historyProofFixturePG struct {
	s      *PostgresStore
	ctx    context.Context
	repo   domain.ContentHash
	snap   domain.Snapshot
	chunks map[domain.ContentHash][]byte
	stored map[domain.ContentHash][]byte
	last   domain.ContentHash
}

// A bounded canonical event stream split into 1 KiB chunks exercises multiple
// 16-key batches and duplicate occurrences without a giant fixture or index.
func historyProofFixture(t *testing.T) historyProofFixturePG {
	t.Helper()
	s, ctx := chunkReusePG(t)
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for i := 0; i < 40; i++ {
		text.WriteString(strings.Repeat(fmt.Sprintf("%s/%03d/", repo, i), 14))
	}
	tile := strings.Repeat(string(repo), 16)[:1024]
	text.WriteString(strings.Repeat(tile, 5))
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: string(repo), SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: text.String()}}}}}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	full, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: domain.HashContent(raw), CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := domain.PlanDocChunks(raw)
	if !ok || len(plan.Order) != 1 {
		t.Fatal("expected one small original stream")
	}
	stream := plan.Bodies[plan.Order[0]]
	manifest := domain.DocChunkManifest{Format: domain.ChunkFormatV2, Envelope: plan.Manifest.Envelope}
	chunks := map[domain.ContentHash][]byte{}
	for start := 0; start < len(stream); start += 1024 {
		body := stream[start:min(start+1024, len(stream))]
		hash := domain.HashContent(body)
		manifest.Chunks = append(manifest.Chunks, hash)
		chunks[hash] = body
	}
	if len(chunks) <= 32 || len(chunks) >= len(manifest.Chunks) {
		t.Fatalf("fixture needs three batches and repeated chunks: distinct=%d occurrences=%d", len(chunks), len(manifest.Chunks))
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	stored := map[domain.ContentHash][]byte{full.Hash(): docCompress(encoded)}
	for hash, body := range chunks {
		stored[hash] = docCompress(body)
	}
	for hash, body := range stored {
		if _, err := s.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, hash, body); err != nil {
			t.Fatal(err)
		}
		kind := "chunk"
		if hash == full.Hash() {
			kind = "doc"
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,$2,$3)`, repo, kind, hash); err != nil {
			t.Fatal(err)
		}
	}
	snap := domain.Snapshot{ID: full.Hash(), DocHash: full.Hash(), RepoID: repo, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}
	if err := s.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	snap, err = s.GetSnapshot(ctx, repo, snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(chunks))
	for hash := range chunks {
		keys = append(keys, string(hash))
	}
	sort.Strings(keys)
	f := historyProofFixturePG{s, ctx, repo, snap, chunks, stored, domain.ContentHash(keys[len(keys)-1])}
	t.Cleanup(func() {
		// Restore current bytes/grants even for a deliberately failed proof.
		for hash, body := range stored {
			if _, err := s.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2) ON CONFLICT(hash) DO UPDATE SET bytes=EXCLUDED.bytes`, hash, body); err != nil {
				t.Error(err)
			}
			kind := "chunk"
			if hash == snap.DocHash {
				kind = "doc"
			}
			if _, err := s.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, repo, kind, hash); err != nil {
				t.Error(err)
			}
		}
	})
	return f
}

func (f historyProofFixturePG) prepare(ctx context.Context, p outbound.HistoryDocumentProof, between func() error) error {
	return f.s.WithinReadSnapshot(ctx, func(read context.Context) error {
		proof, err := f.s.VerifyStoredDoc(read, f.repo, f.snap.DocHash)
		if err != nil {
			return err
		}
		if !proof.Matches(f.snap) {
			return domain.ErrIntegrity
		}
		if between != nil {
			if err := between(); err != nil {
				return err
			}
		}
		return p.Capture(read, []domain.Snapshot{f.snap})
	})
}

func TestPGHistoryDocumentProofSameSnapshotCapture(t *testing.T) {
	f := historyProofFixture(t)
	err := f.s.WithinHistoryDocumentProof(f.ctx, f.repo, func(ctx context.Context, p outbound.HistoryDocumentProof) error {
		if err := f.prepare(ctx, p, func() error {
			bad := bytes.Clone(f.chunks[f.last])
			bad[len(bad)-1] ^= 1 // Same-length final-byte change after verification.
			_, err := f.s.pool.Exec(f.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, f.last, docCompress(bad))
			return err
		}); err != nil {
			return err
		}
		// RR capture must retain the old version, not certify the changed bytes.
		return f.s.WithinRepository(ctx, f.repo, p.Pin)
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("verified-old/captured-new race accepted: %v", err)
	}
	if proof, err := f.s.VerifyStoredDoc(f.ctx, f.repo, f.snap.DocHash); !errors.Is(err, domain.ErrIntegrity) || proof.Valid() {
		t.Fatalf("warm physical proof hid current tail corruption: %v %v", proof, err)
	}
}

func TestPGHistoryDocumentProofChangedOrBusyDependency(t *testing.T) {
	for _, mode := range []string{"descriptor", "chunk_repack", "chunk_tail", "grant_missing", "grant_reinsert", "blob_reinsert", "snapshot", "snapshot_reference", "last_blob_busy", "last_grant_busy"} {
		t.Run(mode, func(t *testing.T) {
			f := historyProofFixture(t)
			err := f.s.WithinHistoryDocumentProof(f.ctx, f.repo, func(ctx context.Context, p outbound.HistoryDocumentProof) error {
				if err := f.prepare(ctx, p, nil); err != nil {
					return err
				}
				mutator, err := f.s.pool.Begin(f.ctx)
				if err != nil {
					return err
				}
				defer mutator.Rollback(f.ctx)
				switch mode {
				case "descriptor":
					_, err = mutator.Exec(f.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, f.snap.DocHash)
				case "chunk_repack":
					_, err = mutator.Exec(f.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, f.last, f.chunks[f.last])
				case "chunk_tail":
					bad := bytes.Clone(f.chunks[f.last])
					bad[len(bad)-1] ^= 1
					_, err = mutator.Exec(f.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, f.last, docCompress(bad))
				case "grant_missing", "grant_reinsert", "blob_reinsert":
					_, err = mutator.Exec(f.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, f.repo, f.last)
					if err == nil && mode == "blob_reinsert" {
						_, err = mutator.Exec(f.ctx, `DELETE FROM blobs WHERE hash=$1`, f.last)
						if err == nil {
							_, err = mutator.Exec(f.ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, f.last, f.stored[f.last])
						}
					}
					if err == nil && mode != "grant_missing" {
						_, err = mutator.Exec(f.ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, f.repo, f.last)
					}
				case "snapshot":
					_, err = mutator.Exec(f.ctx, `UPDATE snapshots SET message='changed' WHERE repo_id=$1 AND id=$2`, f.repo, f.snap.ID)
				case "snapshot_reference":
					_, err = mutator.Exec(f.ctx, `UPDATE snapshots SET doc_hash=$3 WHERE repo_id=$1 AND id=$2`, f.repo, f.snap.ID, f.last)
				case "last_blob_busy":
					_, err = mutator.Exec(f.ctx, `SELECT 1 FROM blobs WHERE hash=$1 FOR UPDATE`, f.last)
				case "last_grant_busy":
					_, err = mutator.Exec(f.ctx, `SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2 FOR UPDATE`, f.repo, f.last)
				}
				if err != nil {
					return err
				}
				if !strings.HasSuffix(mode, "_busy") {
					if err := mutator.Commit(f.ctx); err != nil {
						return err
					}
				}
				return f.s.WithinRepository(ctx, f.repo, func(write context.Context) error {
					if err := f.s.AdvanceRepositoryRevision(write, f.repo, false); err != nil {
						return err
					}
					return p.Pin(write)
				})
			})
			if !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("changed/busy dependency accepted: %v", err)
			}
			v, err := f.s.RepositoryRevision(f.ctx, f.repo)
			if err != nil || v.Graph != 0 {
				t.Fatalf("late failure escaped rollback: %+v %v", v, err)
			}
		})
	}
}

func TestPGHistoryDocumentProofPinsLastUntilTransactionEnds(t *testing.T) {
	f := historyProofFixture(t)
	err := f.s.WithinHistoryDocumentProof(f.ctx, f.repo, func(ctx context.Context, p outbound.HistoryDocumentProof) error {
		if err := f.prepare(ctx, p, nil); err != nil {
			return err
		}
		return f.s.WithinRepository(ctx, f.repo, func(write context.Context) error {
			if err := p.Pin(write); err != nil {
				return err
			}
			for _, query := range []string{
				`SELECT 1 FROM snapshots WHERE repo_id=$1 AND id=$2 FOR UPDATE NOWAIT`,
				`SELECT 1 FROM blobs WHERE hash=$2 AND $1::text IS NOT NULL FOR UPDATE NOWAIT`,
				`SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND hash=$2 FOR UPDATE NOWAIT`,
			} {
				for _, hash := range []domain.ContentHash{f.snap.ID, f.last} {
					if strings.Contains(query, "snapshots") && hash != f.snap.ID {
						continue
					}
					tx, err := f.s.pool.Begin(f.ctx)
					if err != nil {
						return err
					}
					_, err = tx.Exec(f.ctx, query, f.repo, hash)
					tx.Rollback(f.ctx)
					var pg *pgconn.PgError
					if !errors.As(err, &pg) || pg.Code != "55P03" {
						return fmt.Errorf("dependency not pinned against concurrent mutation: %v", err)
					}
				}
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(f.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, f.last); err != nil {
		t.Fatal("pin outlived completed transaction", err)
	}
}

func TestPGHistoryDocumentProofScopeAndCancellation(t *testing.T) {
	f := historyProofFixture(t)
	foreign := &PostgresStore{pool: f.s.pool}
	var retained outbound.HistoryDocumentProof
	var retainedContext context.Context
	err := f.s.WithinHistoryDocumentProof(f.ctx, f.repo, func(ctx context.Context, p outbound.HistoryDocumentProof) error {
		retained, retainedContext = p, ctx
		if err := p.Capture(ctx, []domain.Snapshot{f.snap}); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("capture without RR", err)
		}
		if err := f.prepare(ctx, p, nil); err != nil {
			return err
		}
		if err := foreign.WithinRepository(ctx, f.repo, p.Pin); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("foreign-store proof", err)
		}
		if err := f.s.WithinRepository(ctx, domain.HashContent([]byte("another")), p.Pin); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("foreign-repo proof", err)
		}
		if err := f.s.WithinReadSnapshot(ctx, func(read context.Context) error { return p.Capture(read, []domain.Snapshot{f.snap}) }); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("duplicate capture", err)
		}
		return f.s.WithinRepository(ctx, f.repo, func(write context.Context) error {
			canceled, cancel := context.WithCancel(write)
			cancel()
			if err := p.Pin(canceled); !errors.Is(err, context.Canceled) {
				t.Fatal("canceled pin", err)
			}
			return p.Pin(write)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{f.ctx, retainedContext} {
		if err := f.s.WithinRepository(ctx, f.repo, retained.Pin); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("cross-request proof reuse", err)
		}
	}
}

type historyProofTracePG struct {
	reads, pins int
	chunkKeys   map[string]bool
	err         error
}

func (tr *historyProofTracePG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if strings.Contains(q.SQL, "b.bytes") {
		tr.reads++
		if strings.Contains(q.SQL, "rb.kind='chunk'") {
			for _, key := range q.Args[1].([]string) {
				tr.chunkKeys[key] = true
			}
		}
	}
	if strings.HasSuffix(q.SQL, "FOR SHARE NOWAIT") {
		tr.pins++
		if len(q.Args[len(q.Args)-1].([]string)) > 16 {
			tr.err = errors.New("unbounded pin query")
		}
	}
	return ctx
}
func (*historyProofTracePG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPGHistoryDocumentProofCompleteBoundedClosure(t *testing.T) {
	f := historyProofFixture(t)
	tr := &historyProofTracePG{}
	cfg := f.s.pool.Config()
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	f.s = &PostgresStore{pool: pool}
	for _, phase := range []string{"cold", "warm"} {
		tr.reads, tr.pins, tr.err, tr.chunkKeys = 0, 0, nil, map[string]bool{}
		err := f.s.WithinHistoryDocumentProof(f.ctx, f.repo, func(ctx context.Context, p outbound.HistoryDocumentProof) error {
			if err := f.prepare(ctx, p, nil); err != nil {
				return err
			}
			private := p.(*historyDocumentProofPG)
			if len(private.blobs) != len(f.chunks)+1 || len(private.grants["chunk"]) != len(f.chunks) {
				return errors.New("incomplete or duplicate dependency closure")
			}
			before := tr.reads
			if err := f.s.WithinRepository(ctx, f.repo, p.Pin); err != nil {
				return err
			}
			if tr.reads != before {
				return errors.New("write transaction reread object bodies")
			}
			return nil
		})
		wantReads := 2 + (len(f.chunks)+15)/16 // Full verification plus descriptor capture.
		wantPins := 2 + (len(f.chunks)+16)/16 + (len(f.chunks)+15)/16
		if err != nil || tr.err != nil || tr.reads != wantReads || tr.pins != wantPins || len(tr.chunkKeys) != len(f.chunks) {
			t.Fatalf("%s: err=%v trace=%+v want reads=%d pins=%d chunks=%d", phase, err, tr, wantReads, wantPins, len(f.chunks))
		}
		t.Logf("%s: distinct_chunks=%d all_chunk_keys_read=%d byte_queries=%d bounded_pin_queries=%d", phase, len(f.chunks), len(tr.chunkKeys), tr.reads, tr.pins)
	}
}

type historyProofCommitRaceTracePG struct {
	target            string
	start, committing chan struct{}
}

func (tr *historyProofCommitRaceTracePG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if strings.Contains(q.SQL, "FROM blobs") && strings.HasSuffix(q.SQL, "FOR SHARE NOWAIT") {
		for _, key := range q.Args[0].([]string) {
			if key == tr.target {
				// Synchronize at the actual final-blob locking query, after
				// earlier batches, not merely at the start of Pin.
				close(tr.start)
				select {
				case <-tr.committing:
				case <-ctx.Done():
				}
			}
		}
	}
	return ctx
}
func (*historyProofCommitRaceTracePG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestPGHistoryDocumentProofCommitRacesPin(t *testing.T) {
	f := historyProofFixture(t)
	for attempt := 0; attempt < 3; attempt++ {
		tr := &historyProofCommitRaceTracePG{target: string(f.last), start: make(chan struct{}), committing: make(chan struct{})}
		cfg := f.s.pool.Config()
		cfg.ConnConfig.Tracer = tr
		pool, err := pgxpool.NewWithConfig(f.ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		current := f
		current.s = &PostgresStore{pool: pool}
		err = current.s.WithinHistoryDocumentProof(f.ctx, f.repo, func(ctx context.Context, p outbound.HistoryDocumentProof) error {
			if err := current.prepare(ctx, p, nil); err != nil {
				return err
			}
			mutator, err := f.s.pool.Begin(f.ctx)
			if err != nil {
				return err
			}
			defer mutator.Rollback(f.ctx)
			if _, err := mutator.Exec(f.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, f.last); err != nil {
				return err
			}
			// Start the locking read and the updater's commit concurrently.
			// NOWAIT must reject the old locked row, or compare the newly
			// committed tuple and reject its version. Neither outcome may pin.
			committed := make(chan error, 1)
			go func() {
				<-tr.start
				close(tr.committing)
				committed <- mutator.Commit(f.ctx)
			}()
			err = current.s.WithinRepository(ctx, f.repo, p.Pin)
			select {
			case <-tr.start:
			default:
				close(tr.start) // Also release the peer if an earlier query failed.
			}
			if commitErr := <-committed; commitErr != nil {
				return commitErr
			}
			return err
		})
		pool.Close()
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("attempt %d: commit racing pin accepted: %v", attempt, err)
		}
	}
}

func TestPGHistoryDocumentProofSharedChunksAndFailedCapture(t *testing.T) {
	f := historyProofFixture(t)
	doc, err := f.s.GetDoc(f.ctx, f.repo, f.snap.DocHash)
	if err != nil {
		t.Fatal(err)
	}
	doc.CIR.Envelope.SessionOriginID += "another-envelope"
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	otherHash := domain.HashContent(raw)
	plan, ok := domain.PlanDocChunks(raw)
	if !ok {
		t.Fatal("shared fixture plan")
	}
	encoded, err := docDecompress(f.stored[f.snap.DocHash])
	if err != nil {
		t.Fatal(err)
	}
	var manifest domain.DocChunkManifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Envelope = plan.Manifest.Envelope
	encoded, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(f.ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, otherHash, docCompress(encoded)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(f.ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, f.repo, otherHash); err != nil {
		t.Fatal(err)
	}
	other := f.snap
	other.ID, other.DocHash = otherHash, otherHash
	if err := f.s.PutSnapshot(f.ctx, other); err != nil {
		t.Fatal(err)
	}
	other, err = f.s.GetSnapshot(f.ctx, f.repo, otherHash)
	if err != nil {
		t.Fatal(err)
	}
	for _, failed := range []bool{false, true} {
		err := f.s.WithinHistoryDocumentProof(f.ctx, f.repo, func(ctx context.Context, p outbound.HistoryDocumentProof) error {
			err := f.s.WithinReadSnapshot(ctx, func(read context.Context) error {
				for _, snap := range []domain.Snapshot{f.snap, other} {
					proof, err := f.s.VerifyStoredDoc(read, f.repo, snap.DocHash)
					if err != nil || !proof.Matches(snap) {
						return fmt.Errorf("shared fixture verification: %v", err)
					}
				}
				evidence := []domain.Snapshot{f.snap, other}
				if failed {
					evidence[1].Message = "not the verified snapshot"
				}
				return p.Capture(read, evidence)
			})
			if failed {
				if !errors.Is(err, domain.ErrConflict) {
					return fmt.Errorf("bad capture should fail: %v", err)
				}
				return f.s.WithinRepository(ctx, f.repo, p.Pin)
			}
			if err != nil {
				return err
			}
			private := p.(*historyDocumentProofPG)
			if len(private.snapshots) != 2 || len(private.blobs) != len(f.chunks)+2 || len(private.grants["chunk"]) != len(f.chunks) {
				return errors.New("shared closure did not deduplicate exactly")
			}
			return f.s.WithinRepository(ctx, f.repo, p.Pin)
		})
		if failed && !errors.Is(err, domain.ErrConflict) {
			t.Fatal("partial failed capture could pin", err)
		} else if !failed && err != nil {
			t.Fatal(err)
		}
	}
}
