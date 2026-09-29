//go:build postgres

package app

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGHistoryTurnsReadOnlyAndMissingIndexNeverPublishes(t *testing.T) {
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
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: historyEnvelope(), Events: []domain.CIREvent{historyMessage(domain.RoleUser, string(repo))}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err = st.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, st, nil, nil, nil)
	req := domain.AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: 4 << 20}
	if err = st.WithinReadSnapshot(ctx, func(read context.Context) error {
		page, err := svc.ReadAgentHistoryPage(read, repo, doc.Hash, req)
		if err == nil && len(page.Turns) != 1 {
			t.Fatal("missing turn")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var original []byte
	if err = conn.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, doc.Hash).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `DELETE FROM doc_read_indexes_v3 WHERE hash=$1`, doc.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req); !errors.Is(err, domain.ErrAgentHistoryUnavailable) {
		t.Fatalf("missing index: %v", err)
	}
	var exists bool
	var after []byte
	if err = conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_read_index_current WHERE hash=$1),bytes FROM blobs WHERE hash=$1`, doc.Hash).Scan(&exists, &after); err != nil {
		t.Fatal(err)
	}
	if exists || string(after) != string(original) {
		t.Fatal("read published an index or changed archive")
	}
}
