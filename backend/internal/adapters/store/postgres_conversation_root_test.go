//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func seedRootPG(t *testing.T, ctx context.Context, st *PostgresStore, repo domain.ContentHash, f rootReaderFixture) {
	t.Helper()
	for h, body := range f.bodies {
		if _, err := st.db(ctx).Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, h, docCompress(body)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db(ctx).Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2) ON CONFLICT DO NOTHING`, repo, h); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db(ctx).Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, f.hash, docCompress(f.manifestBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db(ctx).Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2) ON CONFLICT DO NOTHING`, repo, f.hash); err != nil {
		t.Fatal(err)
	}
}

func rootPGRepo(t *testing.T, ctx context.Context, st *PostgresStore) (domain.ContentHash, string) {
	t.Helper()
	name := fmt.Sprintf("root%d", time.Now().UnixNano())
	user := domain.User{ID: "dev:" + name, Username: name, Name: name, Email: name + "@example.test"}
	if err := st.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	ns := domain.Namespace{ID: domain.NewID("ns_"), Slug: name, Kind: domain.NamespaceUser, UserID: user.ID, CreatedAt: time.Now().UTC()}
	if err := st.CreateNamespace(ctx, ns); err != nil {
		t.Fatal(err)
	}
	record := domain.Repository{ID: domain.NewID("ws_"), OwnerID: user.ID, OwnerUsername: name, OwnerNamespaceID: ns.ID, Name: name, Slug: "roots", CreatedAt: time.Now().UTC()}
	if err := st.CreateRepository(ctx, record); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(record.ID))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID}); err != nil {
		t.Fatal(err)
	}
	return repo, ns.ID
}

func rootPGImage(t *testing.T, ctx context.Context, st *PostgresStore, repo domain.ContentHash, ns string) string {
	t.Helper()
	var image string
	err := st.db(ctx).QueryRow(ctx, `SELECT jsonb_build_object(
 'objects',(SELECT jsonb_agg(jsonb_build_array(rb.kind,rb.hash,encode(b.bytes,'hex'),rb.xmin::text,b.xmin::text) ORDER BY rb.kind,rb.hash) FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash WHERE repo_id=$1),
 'usage',(SELECT jsonb_agg(to_jsonb(u) ORDER BY object_key) FROM storage_usage_objects u WHERE namespace_id=$2),
 'ledger',(SELECT jsonb_agg(to_jsonb(u) ORDER BY seq) FROM storage_usage_ledger u WHERE namespace_id=$2),
 'index',(SELECT jsonb_agg(to_jsonb(i) ORDER BY hash) FROM doc_read_indexes_v3 i WHERE hash IN (SELECT hash FROM repo_blobs WHERE repo_id=$1 AND kind='doc')),
 'jobs',(SELECT jsonb_agg(to_jsonb(j)) FROM doc_finalization_jobs j WHERE repo_id=$1)
 )::text`, repo, ns).Scan(&image)
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func TestRootPGReaderCurrentBytesOwnershipAndNoWrites(t *testing.T) {
	st, ctx := chunkReusePG(t)
	repo, ns := rootPGRepo(t, ctx, st)
	f := rootFixture(t, strings.Repeat("x", 3*domain.ConversationManifestChunkBytes))
	seedRootPG(t, ctx, st, repo, f)
	before := rootPGImage(t, ctx, st, repo, ns)
	wantBytes := int64(len(docCompress(f.manifestBytes)))
	for _, body := range f.bodies {
		wantBytes += int64(len(docCompress(body)))
	}
	usage, err := st.ReadStorageUsage(ctx, ns, time.Now().Add(-time.Hour), time.Now())
	if err != nil || usage.CurrentBytes != wantBytes {
		t.Fatalf("unique compressed-byte accounting: got=%d want=%d err=%v", usage.CurrentBytes, wantBytes, err)
	}
	for i := 0; i < 2; i++ {
		doc, err := st.ReadVerifiedDoc(ctx, repo, f.hash)
		if err != nil {
			t.Fatal(err)
		}
		assertRootProof(t, doc, f)
		ref, err := st.VerifyStoredDoc(ctx, repo, f.hash)
		if err != nil || ref.DocumentRef() != doc.DocumentRef() {
			t.Fatal(ref, err)
		}
	}
	peer, err := NewPostgresStore(ctx, st.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if _, err := peer.ReadVerifiedDoc(ctx, repo, f.hash); err != nil {
		t.Fatal(err)
	}
	if len(st.docProofs.proofs) != 0 || len(peer.docProofs.proofs) != 0 {
		t.Fatal("root admitted legacy physical cache")
	}
	if _, err := st.GetDoc(ctx, repo, f.hash); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		t.Fatal("legacy GetDoc", err)
	}
	if _, err := st.GetDocManifest(ctx, repo, f.hash); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		t.Fatal("legacy manifest", err)
	}
	if after := rootPGImage(t, ctx, st, repo, ns); after != before {
		t.Fatal("reader changed grants/bytes/usage/index/job state")
	}
	if _, err := st.ReadVerifiedDoc(ctx, domain.HashContent([]byte("foreign")), f.hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("foreign repo", err)
	}
	for _, chunk := range f.manifest.Chunks {
		if _, err := st.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, chunk.Hash); err != nil {
			t.Fatal(err)
		}
		if _, err := st.VerifyStoredDoc(ctx, repo, f.hash); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("revoked chunk grant", err)
		}
		if _, err := st.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, repo, chunk.Hash); err != nil {
			t.Fatal(err)
		}
		body := bytes.Clone(f.bodies[chunk.Hash])
		body[len(body)-1] ^= 1
		if _, err := st.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, chunk.Hash, docCompress(body)); err != nil {
			t.Fatal(err)
		}
		_, readErr := st.ReadVerifiedDoc(ctx, repo, f.hash)
		_, proofErr := st.VerifyStoredDoc(ctx, repo, f.hash)
		if _, err := st.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, chunk.Hash, docCompress(f.bodies[chunk.Hash])); err != nil {
			t.Fatal(err)
		}
		if readErr == nil || proofErr == nil {
			t.Fatal("warm root hid same-length corruption", readErr, proofErr)
		}
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, f.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadVerifiedDoc(ctx, repo, f.hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("revoked document grant", err)
	}
	seedRootPG(t, ctx, st, repo, f)
	if err := st.DeleteDoc(ctx, repo, f.hash); err != nil {
		t.Fatal(err)
	}
	var chunks int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1 AND kind='chunk'`, repo).Scan(&chunks); err != nil || chunks != len(f.bodies) {
		t.Fatal("doc deletion collected root chunks", chunks, err)
	}
}

func TestRootPGReaderCallerTransaction(t *testing.T) {
	st, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, st)
	f := rootFixture(t, "transaction-local root")
	peer, err := NewPostgresStore(ctx, st.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	stop := errors.New("rollback root fixture")
	err = st.WithinRepository(ctx, repo, func(bound context.Context) error {
		seedRootPG(t, bound, st, repo, f)
		doc, err := st.ReadVerifiedDoc(bound, repo, f.hash)
		if err != nil {
			return err
		}
		assertRootProof(t, doc, f)
		if _, err := peer.ReadVerifiedDoc(bound, repo, f.hash); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("foreign context escaped", err)
		}
		if _, err := peer.ReadVerifiedDoc(ctx, repo, f.hash); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("uncommitted grant escaped", err)
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if _, err := st.ReadVerifiedDoc(ctx, repo, f.hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("rolled-back root proof survived", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if doc, err := st.ReadVerifiedDoc(cancelled, repo, f.hash); !errors.Is(err, context.Canceled) || doc.Valid() {
		t.Fatal("canceled root reader", err)
	}
}

type rootPGReadTracer struct {
	reads            atomic.Int32
	paused           atomic.Bool
	entered, release chan struct{}
}

func (tr *rootPGReadTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "FROM repo_blobs rb JOIN blobs b") {
		tr.reads.Add(1)
	}
	if tr.entered != nil && strings.Contains(data.SQL, "rb.kind='chunk'") && tr.paused.CompareAndSwap(false, true) {
		close(tr.entered)
		select {
		case <-tr.release:
		case <-ctx.Done():
		}
	}
	return ctx
}
func (*rootPGReadTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestRootPGReaderSnapshotAndCurrentByteReads(t *testing.T) {
	st, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, st)
	f := rootFixture(t, "snapshot-owned chunks")
	seedRootPG(t, ctx, st, repo, f)
	tr := &rootPGReadTracer{entered: make(chan struct{}), release: make(chan struct{})}
	cfg := st.pool.Config()
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	reader := &PostgresStore{pool: pool}
	defer reader.Close()
	type result struct {
		doc domain.VerifiedSessionDoc
		err error
	}
	done := make(chan result, 1)
	go func() { doc, err := reader.ReadVerifiedDoc(ctx, repo, f.hash); done <- result{doc, err} }()
	select {
	case <-tr.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Root metadata has established the read snapshot. Revoke one current grant
	// from another connection before the chunk SELECT executes.
	_, revokeErr := st.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, f.manifest.Chunks[0].Hash)
	close(tr.release)
	got := <-done
	if revokeErr != nil {
		t.Fatal(revokeErr)
	}
	if got.err != nil {
		t.Fatal("one read did not retain a coherent snapshot", got.err)
	}
	assertRootProof(t, got.doc, f)
	if _, err := reader.ReadVerifiedDoc(ctx, repo, f.hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("next read reused revoked authorization", err)
	}
	seedRootPG(t, ctx, st, repo, f)
	for _, phase := range []string{"cold", "warm"} {
		tr.reads.Store(0)
		if _, err := reader.VerifyStoredDoc(ctx, repo, f.hash); err != nil {
			t.Fatal(err)
		}
		if tr.reads.Load() != 2 {
			t.Fatal("current root+chunk reads", phase, tr.reads.Load())
		}
	}
}

func TestRootPGProofCannotPublishLegacy(t *testing.T) {
	for _, text := range []string{"", "root"} {
		f := rootFixture(t, text)
		var verifier domain.CanonicalDocVerifier
		doc, err := verifier.VerifyConversationManifestDoc(context.Background(), f.hash, f.manifest, func(_ context.Context, h domain.ContentHash) ([]byte, error) { return f.bodies[h], nil })
		if err != nil {
			t.Fatal(err)
		}
		// A nil pool is intentional: every writer/preparer must reject the valid
		// root proof before even attempting a DB operation or cache insertion.
		st := &PostgresStore{}
		repo := domain.HashContent([]byte(t.Name()))
		if _, err := st.PutVerifiedDoc(context.Background(), repo, doc); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
			t.Fatal(err)
		}
		if _, err := st.PutDoc(context.Background(), repo, domain.SessionDoc{Hash: f.hash, Identity: domain.DocumentIdentityRootV1, CIR: f.cir}); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
			t.Fatal(err)
		}
		if _, err := st.PrepareDocJob(context.Background(), doc); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
			t.Fatal(err)
		}
		if err := st.CompleteDocJob(context.Background(), domain.DocFinalizationJob{}, doc, time.Now()); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
			t.Fatal(err)
		}
		p := pgDocPublication{store: st, doc: preparedDocPG{doc: doc}}
		if err := p.Complete(context.Background(), domain.DocFinalizationJob{}, time.Now()); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
			t.Fatal(err)
		}
		if _, err := st.putPreparedDoc(context.Background(), repo, p.doc); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
			t.Fatal(err)
		}
		if len(st.docProofs.proofs) != 0 {
			t.Fatal("rejected root entered legacy cache")
		}
		if err := verifyFrozenDoc(context.Background(), nil, repo, f.hash, f.manifestBytes); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
			t.Fatal("import root gate", err)
		}
	}
}
