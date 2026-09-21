//go:build postgres

package store

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPostgresOwnershipMigrationPreservesMultiRepositoryAccess(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx := context.Background()
	admin, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.pool.Close()
	database := domain.NewID("ownership_")
	if _, err = admin.pool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer admin.pool.Exec(ctx, "DROP DATABASE "+pgx.Identifier{database}.Sanitize())
	databaseURL, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	databaseURL.Path = "/" + database
	st, err := NewPostgresStore(ctx, databaseURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer st.pool.Close()
	legacy := t.TempDir()
	migrations := "../../../../schemas/db/migrations"
	files, err := os.ReadDir(migrations)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Name() >= "0052" {
			continue
		}
		data, e := os.ReadFile(filepath.Join(migrations, f.Name()))
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(legacy, f.Name()), data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if _, err = st.ApplyMigrations(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	boundary := "ws_" + strings.Repeat("a", 32)
	exec := func(sql string, args ...any) {
		if _, e := st.pool.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO users(id,email,name,username) VALUES('owner','owner@example.test','Owner','alice'),('reader','reader@example.test','Reader','reader')`)
	exec(`INSERT INTO workspaces(id,name,owner_id,owner_username,slug,visibility,secrets_policy) VALUES($1,'Project','owner','alice','project','private','owner')`, boundary)
	exec(`INSERT INTO memberships(workspace_id,user_id,role) VALUES($1,'owner','owner'),($1,'reader','puller')`, boundary)
	token := "inv_" + strings.Repeat("b", 32)
	exec(`INSERT INTO invites(token,workspace_id,created_by,role,status) VALUES($1,$2,'owner','member','pending')`, token, boundary)
	var ids []domain.ContentHash
	for _, name := range []string{"api", "web"} {
		remote := "https://example.test/alice/project/" + name
		id := domain.HashContent([]byte(remote))
		ids = append(ids, id)
		exec(`INSERT INTO repos(id,remote_url,default_branch,team,workspace_id) VALUES($1,$2,'main','default',$3)`, string(id), remote, boundary)
	}
	organizationID, namespaceID := domain.NewID("ent_"), domain.NewID("ns_")
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range []string{
		`INSERT INTO enterprises(id,namespace_id,name,slug,created_by) VALUES($1,$2,'Legacy Company','legacy-company','owner')`,
		`INSERT INTO namespaces(id,slug,kind,enterprise_id) VALUES($2,'legacy-company','enterprise',$1)`,
		`INSERT INTO enterprise_memberships(enterprise_id,user_id,role) VALUES($1,'owner','owner'),($1,'reader','admin')`,
		`INSERT INTO enterprise_policies(enterprise_id,workspace_creation,default_workspace_visibility,allow_public_workspaces) VALUES($1,'members','private',false)`,
	} {
		// Each statement accepts both IDs to keep extended-protocol parameter counts exact.
		args := []any{organizationID, namespaceID}
		if !strings.Contains(statement, "$2") {
			args = args[:1]
		}
		if _, err = tx.Exec(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Two independently starting servers must serialize migration, including
	// the data split. The second startup observes completion, not a partial plan.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := st.ApplyMigrations(ctx, migrations); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	organization, err := st.GetOrganization(ctx, organizationID)
	if err != nil || organization.NamespaceID != namespaceID || organization.Name != "Legacy Company" {
		t.Fatalf("company migration: %+v %v", organization, err)
	}
	policy, err := st.GetOrganizationPolicy(ctx, organizationID)
	if err != nil || policy.RepositoryCreation != domain.OrganizationRepositoryMembers || policy.AllowPublicRepositories {
		t.Fatalf("company policy migration: %+v %v", policy, err)
	}
	admins, err := st.ListOrganizationMembers(ctx, organizationID)
	if err != nil || len(admins) != 2 {
		t.Fatalf("company membership migration: %+v %v", admins, err)
	}
	namespace, err := st.GetNamespace(ctx, namespaceID)
	if err != nil || namespace.Kind != domain.NamespaceOrganization || namespace.OrganizationID != organizationID {
		t.Fatalf("company namespace migration: %+v %v", namespace, err)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		r, e := st.GetRepo(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		seen[r.RepositoryID] = true
		w, e := st.GetRepository(ctx, r.RepositoryID)
		if e != nil {
			t.Fatal(e)
		}
		if w.Visibility != domain.VisibilityPrivate || w.SecretsPolicy != "owner" {
			t.Fatal("policy changed")
		}
		members, e := st.ListMembers(ctx, w.ID)
		if e != nil {
			t.Fatal(e)
		}
		if len(members) != 2 {
			t.Fatalf("collaborator lost: %+v", members)
		}
		for _, m := range members {
			if m.UserID == "reader" && m.Role != domain.RolePuller {
				t.Fatal("reader role changed")
			}
		}
		bound, e := st.GetBoundRepo(ctx, w.ID)
		if e != nil || bound.ID != id {
			t.Fatal("content binding changed", e)
		}
		alias, e := st.GetRepositoryByPath(ctx, "alice", "project/"+w.Slug)
		if e != nil || alias.ID != w.ID {
			t.Fatal("old path no longer resolves", e)
		}
	}
	if len(seen) != 2 || !seen[boundary] {
		t.Fatal("boundary not flattened")
	}
	targets, err := st.InviteTargets(ctx, token)
	if err != nil || len(targets) != 2 {
		t.Fatal("invitation scope lost", err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO repos(id,remote_url,team,repository_id) VALUES($1,'https://example.test/squat','default',$2)`, string(domain.HashContent([]byte("other"))), boundary); err == nil {
		t.Fatal("two content repositories claimed one boundary")
	}
}

func TestPostgresOwnershipBindingCannotSplitOrMove(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	st, err := NewPostgresStore(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ApplyMigrations(context.Background(), "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	runRepositoryBindingContract(t, st)
}
