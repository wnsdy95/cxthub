//go:build postgres

package app

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// This exercises the production read-only transaction, including documents
// written before the current derived event index existed. Reading a projection
// must not require a writable transaction or republish an immutable document.
func TestPGContextSegmentReadsLegacyIndexWithoutPublication(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err = st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: "codex", SessionOriginID: "legacy-index-query"}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: string(repo)}}}}}}
	raw, _ := domain.CanonicalBytes(doc.CIR)
	doc.Hash = domain.HashContent(raw)
	if _, err = st.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	if err = st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err = conn.Exec(ctx, `DELETE FROM doc_read_indexes_v3 WHERE hash=$1`, doc.Hash); err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, st, nil, nil, nil)
	got, err := svc.QueryContext(ctx, repo, domain.ContextSelection{SegmentLimit: 1})
	if err != nil {
		t.Fatalf("read-only legacy segment projection: %v", err)
	}
	if got.Segments == nil || len(got.Segments.Entries) != 1 || got.Segments.Entries[0].Kind != "full_source" || got.Segments.Entries[0].Segment.EventEnd != 1 {
		t.Fatalf("legacy source coverage: %+v", got.Segments)
	}
	err = st.WithinReadSnapshot(ctx, func(read context.Context) error {
		hits, e := st.SearchDocEvents(read, repo, doc.Hash, string(repo), -1, 10)
		if e != nil {
			return e
		}
		if len(hits) != 1 {
			t.Fatalf("legacy read-only search lost source: %d", len(hits))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM doc_read_indexes_v3 WHERE hash=$1`, doc.Hash).Scan(&count); err != nil || count != 0 {
		t.Fatalf("read republished an index: %d %v", count, err)
	}
}
