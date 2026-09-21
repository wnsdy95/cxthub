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

func TestPGTransactionVisibilityCancellationAndRestart(t *testing.T) {
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
	peer, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	repo := domain.HashContent([]byte(fmt.Sprint(t.Name(), time.Now().UnixNano())))
	if _, err = st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	var ids []domain.ContentHash
	for _, text := range []string{"old", "new"} {
		doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: string(repo) + text}}}}}}
		raw, _ := domain.CanonicalBytes(doc.CIR)
		doc.Hash = domain.HashContent(raw)
		if _, err = st.PutDoc(ctx, repo, doc); err != nil {
			t.Fatal(err)
		}
		if err = st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, doc.Hash)
	}
	ref := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: ids[0]}
	if err = st.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	ref.Target = ids[1]
	if err := st.WithinRepository(ctx, repo, func(tx context.Context) error {
		_, err := peer.GetRepo(tx, repo)
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("foreign store escaped transaction: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = st.WithinRepository(ctx, repo, func(tx context.Context) error {
		if err := st.CompareAndSwapRef(tx, repo, ref, ids[0]); err != nil {
			return err
		}
		observed, err := peer.GetRef(ctx, repo, domain.RefBranch, "main")
		if err != nil {
			return err
		}
		if observed.Target != ids[0] {
			t.Fatal("uncommitted ref visible")
		}
		logs, err := peer.ReadReflog(ctx, repo)
		if err != nil {
			return err
		}
		if len(logs) != 1 {
			t.Fatal("uncommitted reflog visible")
		}
		return errors.New("rollback")
	}); err == nil {
		t.Fatal("rollback error lost")
	}
	if err = st.WithinReadSnapshot(ctx, func(read context.Context) error {
		before, err := st.GetRef(read, repo, domain.RefBranch, "main")
		if err != nil {
			return err
		}
		if err := peer.CompareAndSwapRef(ctx, repo, ref, ids[0]); err != nil {
			return err
		}
		after, err := st.GetRef(read, repo, domain.RefBranch, "main")
		if err != nil {
			return err
		}
		if before.Target != ids[0] || after.Target != before.Target {
			t.Fatal("mixed read snapshot")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Cancelling a writer after its nested storage commit rolls everything back.
	cancelCtx, cancel := context.WithCancel(ctx)
	err = st.WithinRepository(cancelCtx, repo, func(tx context.Context) error {
		ref.Target = ids[0]
		if err := st.CompareAndSwapRef(tx, repo, ref, ids[1]); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if err == nil {
		t.Fatal("cancelled transaction acknowledged")
	}
	reader, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := reader.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || got.Target != ids[1] {
		t.Fatalf("restart state=%+v %v", got, err)
	}
	// Terminating the database connection simulates a process dying mid-operation.
	err = st.WithinRepository(ctx, repo, func(tx context.Context) error {
		if err := st.CompareAndSwapRef(tx, repo, ref, ids[1]); err != nil {
			return err
		}
		var pid int
		if err := st.db(tx).QueryRow(tx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			return err
		}
		var killed bool
		return peer.pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&killed)
	})
	if err == nil {
		t.Fatal("terminated transaction acknowledged")
	}
	got, err = reader.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || got.Target != ids[1] {
		t.Fatalf("terminated writer leaked state: %+v %v", got, err)
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
