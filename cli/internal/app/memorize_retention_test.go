package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type collectingMemoryDistiller struct {
	collector *SaveSessionService
	repo      string
	old, next domain.ContentHash
}

func (d collectingMemoryDistiller) Distill(ctx context.Context, _ domain.CIRDocument, _ *domain.NativeMemory) (domain.MemoryDigest, error) {
	d.collector.gcHookLeaf(ctx, d.repo, d.old, d.next)
	return domain.MemoryDigest{Summary: "memory generated while a capture replaces the pending pointer"}, nil
}

func TestMemorizeRetainsTargetUntilMemoryAttachment(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, peer := storage.NewFileStore(root), storage.NewFileStore(root)
	repo := domain.Repo{ID: string(domain.HashContent([]byte(t.Name()))), DefaultBranch: "main", LocalPath: root}
	put := func(texts ...string) domain.ContentHash {
		cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "session"}}
		for i, text := range texts {
			cir.Events = append(cir.Events, domain.Event{Seq: i, Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}})
		}
		id, err := st.PutDoc(ctx, domain.SessionDoc{CIR: cir})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo.ID, Provider: domain.ProviderCodex, SessionID: "session", Message: domain.HookMessagePrefix + " capture"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	old, next := put("first"), put("first", "next")
	collector := newTestSaveService(nil, nil, nil, peer)
	svc := NewMemorizeService(branchSeedGit{repo: repo}, nil, nil, nil, collectingMemoryDistiller{collector: collector, repo: repo.ID, old: old, next: next}, st)
	out, err := svc.Memorize(ctx, inbound.MemorizeInput{Cwd: root, Ref: string(old), Provider: domain.ProviderCodex})
	if err != nil || out.MemoryHash == "" {
		t.Fatalf("memory target collected during distillation: %+v %v", out, err)
	}
	// The queued collection is re-evaluated after the read reservation ends.
	collector.gcHookLeaf(ctx, repo.ID, old, next)
	snap, err := peer.GetSnapshot(ctx, old)
	if err != nil || snap.MemoryHash != out.MemoryHash {
		t.Fatalf("attached archive was collected: %+v %v", snap, err)
	}
	if _, err := peer.GetDoc(ctx, old); err != nil {
		t.Fatal(err)
	}
	jobs, err := peer.CaptureCollections(ctx, repo.ID, 32)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("collection did not settle: %+v %v", jobs, err)
	}
}
