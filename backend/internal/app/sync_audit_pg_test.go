//go:build postgres

package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPGSyncAuditCreationPersistenceAndConcurrentWriter(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	repo := hh(t.Name() + time.Now().String())
	if _, err = st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/org/project"}); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	e := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: strings.Repeat("1", 32), Branch: "feature", Kind: "birth", GitAfter: sha, CreatedAt: time.Now().UTC(), Creation: &domain.GitCreation{Evidence: "process-argv", Command: []string{"git", "branch", "feature", "main"}, StartRef: "main", StartCommit: sha, OriginBranch: "main", OriginBranchID: domain.LegacyContextBranchID(string(repo), "main")}}
	if err = svc.RecordHistory(ctx, e); err != nil {
		t.Fatal(err)
	}
	events, err := svc.ListHistory(ctx, repo)
	if err != nil || len(events) != 1 || !reflect.DeepEqual(events[0].Creation, e.Creation) {
		t.Fatal(events, err)
	}
	if err = svc.RecordHistory(ctx, e); err != nil {
		t.Fatal("idempotent replay", err)
	}
	changed := e
	c := *e.Creation
	c.StartRef = "other"
	c.Command = []string{"git", "branch", "feature", "other"}
	c.OriginBranch = "other"
	c.OriginBranchID = domain.LegacyContextBranchID(string(repo), "other")
	changed.Creation = &c
	if err = svc.RecordHistory(ctx, changed); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatal("immutable command overwritten", err)
	}
	reader := &auditReader{}
	audit := NewGitSyncAudit(svc, reader)
	if _, err = audit.CheckGitHubSync(ctx, repo, ""); err != nil {
		t.Fatal(err)
	}
	reader.during = func() {
		position := e
		position.ID = strings.Repeat("2", 32)
		position.Kind = "position"
		position.Creation = nil
		if err = svc.RecordHistory(ctx, position); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = audit.CheckGitHubSync(ctx, repo, ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("concurrent evidence accepted", err)
	}
}
