//go:build postgres

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// The command an operator runs: it lists what is stored, shows what a move
// would change, and moves only when told to.
func TestRepoAddressPGCommandListsLooksAndMoves(test *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CXT_TEST_DSN"))
	if dsn == "" {
		test.Skip("CXT_TEST_DSN unset; requires a disposable local PostgreSQL fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer storage.Close()
	const migrations = "../../../schemas/db/migrations"
	if _, err := storage.ApplyMigrations(ctx, migrations); err != nil {
		test.Fatal(err)
	}
	now, suffix := time.Now().UTC(), domain.NewID("")[:10]
	owner := domain.User{ID: domain.NewID("u_"), Username: "operator" + suffix, Email: "operator" + suffix + "@example.test", Name: "Operator"}
	if err := storage.UpsertUser(ctx, owner); err != nil {
		test.Fatal(err)
	}
	namespace := domain.Namespace{ID: domain.NewID("ns_"), Slug: owner.Username, Kind: domain.NamespaceUser, UserID: owner.ID, CreatedAt: now}
	if err := storage.CreateNamespace(ctx, namespace); err != nil {
		test.Fatal(err)
	}
	workspace := domain.Repository{ID: domain.NewID("ws_"), Name: "Tool", OwnerID: owner.ID, OwnerUsername: owner.Username, OwnerNamespaceID: namespace.ID, Slug: "tool", CreatedAt: now}
	if err := storage.CreateRepository(ctx, workspace); err != nil {
		test.Fatal(err)
	}
	from, to := "http://localhost:5173/"+owner.Username+"/tool", "https://cxthub.example/"+owner.Username+"/tool"
	fromID, toID := domain.HashContent([]byte(domain.NormalizeGitRemoteURL(from))), domain.HashContent([]byte(domain.NormalizeGitRemoteURL(to)))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: fromID, RemoteURL: from, RepositoryID: workspace.ID, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	test.Setenv("CXT_POSTGRES_DSN", dsn)
	test.Setenv("CXT_MIGRATIONS_DIR", migrations)
	run := func(args ...string) (string, string, error) {
		test.Helper()
		var stdout, stderr bytes.Buffer
		err := runRepoAddress(ctx, args, &stdout, &stderr)
		return stdout.String(), stderr.String(), err
	}
	listed := func() map[domain.ContentHash]repoAddressListed {
		test.Helper()
		out, _, err := run("--list")
		if err != nil {
			test.Fatal(err)
		}
		var entries []repoAddressListed
		if err := json.Unmarshal([]byte(out), &entries); err != nil {
			test.Fatal(err)
		}
		byID := map[domain.ContentHash]repoAddressListed{}
		for _, entry := range entries {
			byID[entry.ID] = entry
		}
		return byID
	}
	if entry := listed()[fromID]; entry.Address != from || entry.Workspace != workspace.ID || entry.Path != owner.Username+"/tool" {
		test.Fatalf("the stored repository is listed as %+v", entry)
	}
	out, note, err := run("--from", from, "--to", to)
	var preview domain.RepoAddressMove
	if err != nil || json.Unmarshal([]byte(out), &preview) != nil || preview.Applied || preview.From != fromID || preview.To != toID || !strings.Contains(note, "nothing was changed") {
		test.Fatalf("looking at the move: %v %s %q", err, out, note)
	}
	if _, ok := listed()[toID]; ok {
		test.Fatal("looking at the move moved the repository")
	}
	out, note, err = run("--from", from, "--to", to, "--apply")
	var moved domain.RepoAddressMove
	if err != nil || json.Unmarshal([]byte(out), &moved) != nil || !moved.Applied || !strings.Contains(note, "cxt setup "+to) {
		test.Fatalf("the move: %v %s %q", err, out, note)
	}
	after := listed()
	if _, ok := after[fromID]; ok || after[toID].Address != to || after[toID].Path != owner.Username+"/tool" {
		test.Fatalf("after the move: %+v", after[toID])
	}
	if _, _, err := run("--from", from, "--to", to, "--apply"); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		test.Fatalf("moving it again: %v", err)
	}
	// A database of another release is refused before anything is read.
	test.Setenv("CXT_MIGRATIONS_DIR", test.TempDir())
	if _, _, err := run("--list"); err == nil || !strings.Contains(err.Error(), "run cxt-admin migrate first") {
		test.Fatalf("another release: %v", err)
	}
}
