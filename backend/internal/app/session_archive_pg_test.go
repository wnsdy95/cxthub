//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type archiveRevisionFailure struct{ *store.PostgresStore }

func (storage archiveRevisionFailure) AdvanceRepositoryRevision(context.Context, domain.ContentHash, bool) error {
	return errors.New("injected revision failure")
}

func TestPGSessionArchiveAtomicAndConcurrent(t *testing.T) {
	service, storage, repo := collaborationPG(t)
	ctx := systemTestContext()
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	other := NewService(peer, peer, nil, gitengine.NewEngine(peer), peer)
	var snapshots []domain.ContentHash
	for index := 0; index < 4; index++ {
		cir := domain.CIRDocument{Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("%s-%d", repo, index)}}}}}
		raw, err := domain.CanonicalBytes(cir)
		if err != nil {
			t.Fatal(err)
		}
		hash := domain.HashContent(raw)
		if _, err := storage.PutDoc(ctx, repo, domain.SessionDoc{Hash: hash, CIR: cir}); err != nil {
			t.Fatal(err)
		}
		if err := storage.PutSnapshot(ctx, domain.Snapshot{RepoID: repo, ID: hash, DocHash: hash, Provider: domain.ProviderCodex, SessionID: "same-session", Fidelity: domain.FidelityFull, Branch: "main", CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, hash)
	}
	brokenStore := archiveRevisionFailure{storage}
	broken := NewService(brokenStore, storage, nil, gitengine.NewEngine(storage), storage)
	if err := broken.SetSessionArchived(ctx, repo, snapshots[0], true); err == nil {
		t.Fatal("failed transaction accepted")
	}
	records, err := storage.ListSessionArchives(ctx, repo)
	if err != nil || len(records) != 0 {
		t.Fatalf("archive escaped failed transaction: %+v %v", records, err)
	}
	var group sync.WaitGroup
	results := make(chan error, len(snapshots))
	for index, snapshot := range snapshots {
		group.Add(1)
		go func(index int, snapshot domain.ContentHash) {
			defer group.Done()
			worker := service
			if index%2 == 1 {
				worker = other
			}
			results <- worker.SetSessionArchived(ctx, repo, snapshot, true)
		}(index, snapshot)
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	records, err = storage.ListSessionArchives(ctx, repo)
	if err != nil || len(records) != 1 {
		t.Fatalf("duplicate concurrent archive: %+v %v", records, err)
	}
	if err := broken.SetSessionArchived(ctx, repo, snapshots[0], false); err == nil {
		t.Fatal("failed restore accepted")
	}
	after, err := storage.ListSessionArchives(ctx, repo)
	if err != nil || len(after) != 1 || after[0].SnapshotID != records[0].SnapshotID {
		t.Fatalf("restore escaped rollback: %+v %v", after, err)
	}
	if err := other.SetSessionArchived(ctx, repo, snapshots[1], false); err != nil {
		t.Fatal(err)
	}
	if records, err = storage.ListSessionArchives(ctx, repo); err != nil || len(records) != 0 {
		t.Fatalf("restore failed: %+v %v", records, err)
	}
	for _, snapshot := range snapshots {
		if _, err := storage.GetSnapshot(ctx, repo, snapshot); err != nil {
			t.Fatal("original snapshot lost", err)
		}
	}
}
