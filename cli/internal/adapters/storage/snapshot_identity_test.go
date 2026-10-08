package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestSnapshotDocumentIdentityImmutable(t *testing.T) {
	ctx := context.Background()
	for _, original := range []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1} {
		for _, operation := range []string{"replay", "promotion", "reconcile"} {
			t.Run(string(original)+"/"+operation, func(t *testing.T) {
				store := NewFileStore(t.TempDir())
				id := domain.HashContent([]byte("identity-metadata-only"))
				base := domain.Snapshot{ID: id, DocHash: id, DocIdentity: original, Branch: domain.StashBranchLabel, Message: domain.HookMessagePrefix + "capture"}
				if err := store.PutSnapshot(ctx, base); err != nil {
					t.Fatal(err)
				}
				before, err := readCxtFile(store.objectPath("snapshots", id))
				if err != nil {
					t.Fatal(err)
				}
				if original == domain.DocumentIdentityLegacy && bytes.Contains(before, []byte("doc_identity")) {
					t.Fatal("legacy disk JSON gained identity field")
				}
				incoming := base
				incoming.DocIdentity = domain.DocumentIdentityRootV1
				if original == domain.DocumentIdentityRootV1 {
					incoming.DocIdentity = domain.DocumentIdentityLegacy
				}
				write := store.PutSnapshot
				if operation != "replay" {
					incoming.Branch, incoming.Message = "main", "promoted"
					incoming.Grafted, incoming.GraftSeq = true, 2
					incoming.GraftParents = []domain.ContentHash{domain.HashContent([]byte("graft"))}
				}
				if operation == "reconcile" {
					write = store.ReconcileGraftState
				}
				if err := write(ctx, incoming); !errors.Is(err, domain.ErrHashMismatch) {
					t.Fatalf("identity mutation accepted: %v", err)
				}
				after, err := readCxtFile(store.objectPath("snapshots", id))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("rejected mutation changed stored metadata", err)
				}
				incoming.DocIdentity = original
				if err := write(ctx, incoming); err != nil {
					t.Fatal("matching identity rejected", err)
				}
				got, err := store.GetSnapshot(ctx, id)
				if err != nil || got.DocumentRef() != base.DocumentRef() || got.GraftSeq != incoming.GraftSeq {
					t.Fatal("identity lost during valid metadata update", got, err)
				}
				if fileExists(store.objectPath("docs", id)) {
					t.Fatal("metadata update created a document body")
				}
			})
		}
	}
}

func TestSnapshotRejectsUnknownDocumentIdentityBeforeEffects(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	id := domain.HashContent([]byte("unknown-identity"))
	snapshot := domain.Snapshot{ID: id, DocHash: id, DocIdentity: "future"}
	if err := validateSnapshotRefs(snapshot); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		t.Fatalf("reference validation accepted unknown identity: %v", err)
	}
	if err := store.PutSnapshot(ctx, snapshot); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		t.Fatalf("write accepted unknown identity: %v", err)
	}
	if _, err := os.Stat(store.storeDir()); !os.IsNotExist(err) {
		t.Fatalf("invalid identity created replica files: %v", err)
	}
	// A corrupt stored discriminator must not be silently replaced by replay.
	snapshot.DocIdentity = domain.DocumentIdentityRootV1
	if err := store.PutSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(domain.DocumentIdentityRootV1), []byte("future"), 1)
	path := store.objectPath("snapshots", id)
	if err := writeAtomic(path, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSnapshot(ctx, id); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		t.Fatalf("read accepted corrupt identity: %v", err)
	}
	if err := store.PutSnapshot(ctx, snapshot); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		t.Fatalf("replay repaired corrupt identity: %v", err)
	}
	after, err := readCxtFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("corrupt metadata changed on rejection", err)
	}
}

func TestConcurrentSnapshotDocumentIdentityOneWinner(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	id := domain.HashContent([]byte("concurrent-snapshot-identity"))
	type result struct {
		identity domain.DocumentIdentity
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, identity := range []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1} {
		go func(identity domain.DocumentIdentity) {
			store := NewFileStore(root)
			<-start
			results <- result{identity, store.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, DocIdentity: identity, Branch: "main"})}
		}(identity)
	}
	close(start)
	var winner domain.DocumentIdentity
	successes, conflicts := 0, 0
	for range 2 {
		got := <-results
		switch {
		case got.err == nil:
			successes++
			winner = got.identity
		case errors.Is(got.err, domain.ErrHashMismatch):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent result: %v", got.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	got, err := NewFileStore(root).GetSnapshot(ctx, id)
	if err != nil || got.DocIdentity != winner {
		t.Fatal("stored identity differs from successful writer", got, err)
	}
}
