//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// A store handed a context that carries another store's transaction refuses
// every kind of work, instead of doing it outside that transaction.
func TestPGStoreRefusesToWorkInsideAnotherStoresTransaction(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	peer, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	bundle := domain.SettingsBundle{Kind: "claude", Files: []domain.SettingsFile{{Path: "settings.json", ContentB64: "e30="}}}
	doc := domain.HashContent([]byte("a document this repository does not hold"))

	stop := errors.New("synthetic: leave the transaction")
	calls := 0
	err = peer.WithinRepository(ctx, repo, func(bound context.Context) error {
		for name, work := range map[string]func() error{
			// One statement that writes.
			"a write": func() error { return storage.PutSettingsBundle(bound, repo, bundle) },
			// One row read.
			"a read of one row": func() error { _, err := storage.GetSettingsBundle(bound, repo, "claude"); return err },
			// A list of rows.
			"a list": func() error { _, err := storage.HasDocs(bound, repo, []domain.ContentHash{doc}); return err },
			// Work that opens a transaction of its own.
			"a transaction": func() error { return storage.DeleteDoc(bound, repo, doc) },
			// Work that opens one without options: the claim of a read of the Git host.
			"a plain transaction": func() error {
				_, err := storage.ClaimRemoteGitSync(bound, repo, "https://github.com/example/project", time.Now().UTC(), time.Minute)
				return err
			},
		} {
			calls++
			if err := work(); !errors.Is(err, domain.ErrConflict) {
				t.Errorf("%s inside another store's transaction: %v", name, err)
			}
		}
		// The store that owns the transaction works inside it.
		if err := peer.PutSettingsBundle(bound, repo, bundle); err != nil {
			t.Errorf("the owner inside its own transaction: %v", err)
		}
		if got, err := peer.GetSettingsBundle(bound, repo, "claude"); err != nil || got.Kind != "claude" {
			t.Errorf("the owner reads what it wrote in its transaction: %+v %v", got, err)
		}
		return stop
	})
	if !errors.Is(err, stop) || calls != 5 {
		t.Fatalf("the transaction ended with %v after %d refused calls", err, calls)
	}
	// Nothing was written beside the transaction, and what the owner wrote
	// went with the transaction.
	if _, err := storage.GetSettingsBundle(ctx, repo, "claude"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a write escaped the transaction: %v", err)
	}
	// Outside any transaction both stores work.
	if err := storage.PutSettingsBundle(ctx, repo, bundle); err != nil {
		t.Fatal(err)
	}
	if got, err := peer.GetSettingsBundle(ctx, repo, "claude"); err != nil || got.Kind != "claude" {
		t.Fatalf("the other store outside a transaction: %+v %v", got, err)
	}
}
