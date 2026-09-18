//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type rejectPublicationMemoryPG struct{ *store.PostgresStore }

func (s *rejectPublicationMemoryPG) CompareAndSwapSnapshotMemory(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, domain.ContentHash) error {
	return domain.ErrIntegrity
}

func TestPGMemoryPublicationCollectionAndRollback(t *testing.T) {
	svc, st, repo := collaborationPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	username := fmt.Sprintf("memory%d", time.Now().UnixNano())
	user := domain.User{ID: "dev:" + username, Name: "Memory", Username: username, Email: username + "@example.com"}
	if err := st.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	ws := domain.Workspace{ID: domain.NewID("ws_"), Name: "Memory", Slug: "memory", OwnerID: user.ID, OwnerUsername: username, CreatedAt: time.Now().UTC()}
	if err := st.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, WorkspaceID: ws.ID}); err != nil {
		t.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	other := NewService(peer, peer, auth.NewTeamTokenAuth(), gitengine.NewEngine(peer), peer)
	makeArchive := func(session string, texts ...string) inbound.MemoryPublication {
		cir := pendingGCCIR(domain.ProviderCodex, texts...)
		cir.Envelope.SessionOriginID = session
		raw, err := domain.CanonicalBytes(cir)
		if err != nil {
			t.Fatal(err)
		}
		id := domain.HashContent(raw)
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Provider: domain.ProviderCodex, SessionID: session, Message: domain.HookMessagePrefix + " capture", Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC()}
		return inbound.MemoryPublication{Objects: inbound.CommitInput{RepoID: repo, Snapshots: []domain.Snapshot{snap}, Docs: []domain.SessionDoc{{Hash: id, CIR: cir}}}, Memory: domain.MemoryDigest{SnapshotID: id, Summary: "independent memory"}}
	}
	for i := 0; i < 6; i++ {
		session := fmt.Sprintf("%s/%d", repo, i)
		old, next := makeArchive(session, "first"), makeArchive(session, "first", "next")
		for _, in := range []inbound.MemoryPublication{old, next} {
			if _, err := svc.Commit(ctx, in.Objects); err != nil {
				t.Fatal(err)
			}
		}
		p := domain.Pending{Provider: domain.ProviderCodex, SessionID: session, Target: old.Memory.SnapshotID}
		if err := svc.PutPending(ctx, repo, session, p); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, e := svc.PublishMemoryArchive(ctx, old); results <- e }()
		go func() {
			defer wg.Done()
			<-start
			p.Target = next.Memory.SnapshotID
			results <- other.PutPending(ctx, repo, session, p)
		}()
		close(start)
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatal(err)
			}
		}
		got, err := other.GetMemoryDigest(ctx, repo, old.Memory.SnapshotID)
		if err != nil || got.Summary != old.Memory.Summary {
			t.Fatalf("concurrent collection lost memory: %+v %v", got, err)
		}
		if _, err := peer.GetDoc(ctx, repo, old.Memory.SnapshotID); err != nil {
			t.Fatal(err)
		}
	}
	// Fail after doc/snapshot/blob writes, at the final pointer CAS. The other
	// replica must see none of the transaction's archive objects or revision.
	before, err := peer.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	in := makeArchive(string(repo)+"/rollback", "rollback")
	reject := &rejectPublicationMemoryPG{PostgresStore: st}
	failing := NewService(reject, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(reject), st)
	if _, err := failing.PublishMemoryArchive(ctx, in); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("injected failure: %v", err)
	}
	if _, err := peer.GetSnapshot(ctx, repo, in.Memory.SnapshotID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("snapshot escaped rollback: %v", err)
	}
	if _, err := peer.GetDoc(ctx, repo, in.Memory.SnapshotID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("document escaped rollback: %v", err)
	}
	after, err := peer.RepositoryRevision(ctx, repo)
	if err != nil || after != before {
		t.Fatalf("revision escaped rollback: %+v %+v %v", before, after, err)
	}
	hash, _ := domain.MemoryDigestHash(in.Memory)
	if _, err := peer.GetMemory(ctx, repo, hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("memory escaped rollback: %v", err)
	}
}
