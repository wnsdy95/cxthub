//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGTransactionDeferredQuotaRollback(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx := context.Background()
	st, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("quota%d", time.Now().UnixNano())
	u := domain.User{ID: "dev:" + name, Username: name, Name: name, Email: name + "@example.test"}
	if err := st.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	ns := domain.Namespace{ID: domain.NewID("ns_"), Slug: name, Kind: domain.NamespaceUser, UserID: u.ID, CreatedAt: time.Now().UTC()}
	if err := st.CreateNamespace(ctx, ns); err != nil {
		t.Fatal(err)
	}
	repositoryRecord := domain.Repository{ID: domain.NewID("ws_"), Name: name, Slug: "quota", OwnerID: u.ID, OwnerUsername: name, OwnerNamespaceID: ns.ID, CreatedAt: time.Now().UTC()}
	if err := st.CreateRepository(ctx, repositoryRecord); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(repositoryRecord.ID))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: repositoryRecord.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.ConfigureStoragePolicy(ctx, ns.ID, "limit-"+ns.ID, 0, domain.StoragePolicy{Plan: "free", IncludedBytes: 1}, u.ID, "test deferred quota"); err != nil {
		t.Fatal(err)
	}
	body := []byte(string(repo) + "deferred quota test")
	hash := domain.HashContent(body)
	innerFinished := false
	err = st.WithinRepository(ctx, repo, func(tx context.Context) error {
		_, _, err := st.PutChunks(tx, repo, map[domain.ContentHash][]byte{hash: body})
		innerFinished = err == nil
		return err
	})
	if !innerFinished || !errors.Is(err, domain.ErrStorageLimit) {
		t.Fatalf("outer commit contract: inner=%v err=%v", innerFinished, err)
	}
	if _, err := st.GetChunk(ctx, repo, hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("over-quota chunk persisted: %v", err)
	}
	usage, err := st.ReadStorageUsage(ctx, ns.ID, time.Now().Add(-time.Hour), time.Now())
	if err != nil || usage.CurrentBytes != 0 {
		t.Fatalf("rolled-back usage persisted: %+v %v", usage, err)
	}
}

func TestPGTransactionSecretsCASAndRegistration(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx := context.Background()
	st, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(fmt.Sprint(t.Name(), time.Now().UnixNano())))
	if _, err = st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "trunk", GitRemoteURL: "https://github.com/acme/original"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/different"})
	if err != nil || got.DefaultBranch != "trunk" || got.GitRemoteURL != "https://github.com/acme/original" {
		t.Fatalf("registration overwrote established repo: %+v %v", got, err)
	}
	for round := 0; round < 2; round++ {
		expected, err := st.GetSecretsEnvelope(ctx, repo)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- st.CompareAndSwapSecrets(ctx, repo, expected, []byte(fmt.Sprintf(`{"revision":%d}`, round*2+i)))
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		won, lost := 0, 0
		for err := range errs {
			if err == nil {
				won++
			} else if errors.Is(err, domain.ErrRefConflict) {
				lost++
			} else {
				t.Fatal(err)
			}
		}
		if won != 1 || lost != 1 {
			t.Fatalf("secrets winners=%d conflicts=%d", won, lost)
		}
	}
}

func TestPGTransactionDurabilityOverridesAsyncDSN(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	// All CI/test DSNs are URL-form; use a dedicated pool setting below instead
	// of changing global PostgreSQL configuration.
	st, err := NewPostgresStore(context.Background(), dsn+"&synchronous_commit=off")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var mode string
	if err := st.pool.QueryRow(context.Background(), `SHOW synchronous_commit`).Scan(&mode); err != nil || mode != "on" {
		t.Fatalf("durability=%s err=%v", mode, err)
	}
}
