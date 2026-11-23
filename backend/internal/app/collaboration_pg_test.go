//go:build postgres

package app

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func collaborationDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	// Saturate a small pool on every machine. A store helper escaping the
	// transaction would otherwise appear correct with many spare connections.
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("pool_max_conns", "2")
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " pool_max_conns=2"
}

func collaborationPG(t *testing.T) (*Service, *store.PostgresStore, domain.ContentHash) {
	t.Helper()
	dsn := collaborationDSN(t)
	ctx := systemTestContext()
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	repo := hh(fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/proj.git"}); err != nil {
		t.Fatal(err)
	}
	return NewService(st, st, auth.NewTeamTokenAuth(), st), st, repo
}

type collaborationGitEvidencePG struct {
	*store.PostgresStore
	repo domain.ContentHash
}

func (storage *collaborationGitEvidencePG) ListRepos(ctx context.Context, _ string) ([]domain.Repo, error) {
	repo, err := storage.GetRepo(ctx, storage.repo)
	if err != nil {
		return nil, err
	}
	return []domain.Repo{repo}, nil
}
