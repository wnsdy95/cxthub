//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// A read snapshot belongs to the store that opened it. The owner nests inside
// it without opening another; a second store handed its context neither reads
// inside it nor runs the work beside it.
func TestPGReadSnapshotIsNotSharedBetweenStores(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	peer, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	nested, foreign := 0, 0
	err = peer.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if err := peer.WithinReadSnapshot(bound, func(inner context.Context) error {
			nested++
			if inner != bound {
				t.Error("the owner nested in another snapshot than its own")
			}
			return nil
		}); err != nil {
			t.Errorf("the owner inside its own snapshot: %v", err)
		}
		if err := storage.WithinReadSnapshot(bound, func(context.Context) error { foreign++; return nil }); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("another store inside the snapshot: %v", err)
		}
		return nil
	})
	if err != nil || nested != 1 || foreign != 0 {
		t.Fatalf("the snapshot ended with %v; the owner nested %d times, the other store ran %d times", err, nested, foreign)
	}
	// What the work answers is the snapshot's answer.
	stop := errors.New("synthetic: the read failed")
	if err := storage.WithinReadSnapshot(ctx, func(context.Context) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("the work's own error: %v", err)
	}
	// A store that cannot begin a snapshot does not run the work.
	closed, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	ran := false
	if err := closed.WithinReadSnapshot(ctx, func(context.Context) error { ran = true; return nil }); err == nil || ran {
		t.Fatalf("a closed store: %v, work ran: %v", err, ran)
	}
}

// A visible generation is stored for a document only together with the range
// generation it names. Where that pair is not there, nothing is stored and
// the caller is told that the index is not available.
func TestPGVisibleGenerationNeedsItsRangeGeneration(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	document := domain.HashContent([]byte("a document without a range generation"))
	ranges := domain.HashContent([]byte("a range generation nobody stored"))
	descriptor := []byte(`{"synthetic":"descriptor"}`)
	err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		transaction := bound.Value(repositoryTxKey{}).(*repositoryTx)
		if err := writeVisibleGenerationPG(bound, transaction, document, ranges, domain.HashContent(descriptor), descriptor); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
			t.Errorf("a visible generation without its range generation: %v", err)
		}
		// A caller that gave up is told so, not that the index is missing.
		gone, cancel := context.WithCancel(bound)
		cancel()
		if err := writeVisibleGenerationPG(gone, transaction, document, ranges, domain.HashContent(descriptor), descriptor); !errors.Is(err, context.Canceled) {
			t.Errorf("a caller that gave up: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var stored int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_visible_generations_v1 WHERE doc_hash=$1`, document).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("a refused visible generation was stored: %d %v", stored, err)
	}
}
