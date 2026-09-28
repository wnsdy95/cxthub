package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type failHookSnapshotDeleteStore struct {
	*storage.FileStore
}

func (s *failHookSnapshotDeleteStore) DeleteSnapshot(context.Context, domain.ContentHash) error {
	return errors.New("snapshot deletion failed")
}

func TestGCHookLeafRequiresSupersedingCapture(t *testing.T) {
	for _, tc := range []struct {
		name           string
		change         func(*domain.CIRDocument)
		child          bool
		failDelete     bool
		attachedMemory bool
		backfill       bool
		wantDeleted    bool
	}{
		{name: "same native session moves worktrees", wantDeleted: true},
		{name: "different provider same native ID", change: func(d *domain.CIRDocument) { d.Envelope.SourceProvider = domain.ProviderCodex }},
		{name: "different session", change: func(d *domain.CIRDocument) { d.Envelope.SessionOriginID = "another-session" }},
		{name: "truncated capture", change: func(d *domain.CIRDocument) { d.Events = d.Events[:1] }},
		{name: "divergent capture", change: func(d *domain.CIRDocument) { d.Events[0].Role = "system" }},
		{name: "unreferenced child still needs parent", child: true},
		{name: "snapshot delete fails", failDelete: true},
		{name: "attached memory absent from successor", attachedMemory: true},
		{name: "historical upload pins superseded capture", backfill: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			st := storage.NewFileStore(root)
			repo := string(domain.HashContent([]byte("gc repo")))
			put := func(cir domain.CIRDocument) domain.ContentHash {
				t.Helper()
				h, err := st.PutDoc(ctx, domain.SessionDoc{CIR: cir})
				if err != nil {
					t.Fatal(err)
				}
				if err := st.PutSnapshot(ctx, domain.Snapshot{ID: h, DocHash: h, RepoID: repo, Branch: cir.Envelope.GitBranch, Provider: cir.Envelope.SourceProvider, SessionID: cir.Envelope.SessionOriginID, Message: domain.HookMessagePrefix + " capture"}); err != nil {
					t.Fatal(err)
				}
				return h
			}
			oldDoc, err := codec.NewClaudeCodec().Decode(ctx, []byte(e2eClaudeSession))
			if err != nil {
				t.Fatal(err)
			}
			old := put(oldDoc)
			if tc.attachedMemory {
				h, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: old, Summary: "independent archived memory"})
				if err != nil {
					t.Fatal(err)
				}
				if err := st.CompareAndSwapSnapshotMemory(ctx, old, "", h); err != nil {
					t.Fatal(err)
				}
			}
			newDoc, err := codec.NewClaudeCodec().Decode(ctx, []byte(e2eClaudeSession+"\n"+`{"type":"user","cwd":"/Users/work/other-worktree","sessionId":"s1","gitBranch":"feature/other","timestamp":"2026-06-30T00:00:02Z","message":{"role":"user","content":"continue"}}`))
			if err != nil {
				t.Fatal(err)
			}
			newDoc.Envelope.Cwd, newDoc.Envelope.GitBranch = "/Users/work/other-worktree", "feature/other"
			if tc.change != nil {
				tc.change(&newDoc)
			}
			current := put(newDoc)
			if tc.child {
				child := domain.HashContent([]byte("unreferenced child"))
				if err := st.PutSnapshot(ctx, domain.Snapshot{ID: child, DocHash: child, RepoID: repo, Parents: []domain.ContentHash{old}}); err != nil {
					t.Fatal(err)
				}
			}
			var sessionStore outbound.SessionStore = st
			if tc.failDelete {
				sessionStore = &failHookSnapshotDeleteStore{FileStore: st}
			}
			svc := newTestSaveService(nil, nil, nil, sessionStore)
			if tc.backfill {
				snap, err := st.GetSnapshot(ctx, old)
				if err != nil {
					t.Fatal(err)
				}
				if err := st.StageBackfills(ctx, repo, []domain.Snapshot{snap}); err != nil {
					t.Fatal(err)
				}
			}
			svc.gcHookLeaf(ctx, repo, old, current)
			jobs, queueErr := st.CaptureCollections(ctx, repo, 32)
			wantJobs := 0
			if tc.failDelete || tc.backfill {
				wantJobs = 1
			}
			if queueErr != nil || len(jobs) != wantJobs {
				t.Fatalf("collection retry state: %+v %v", jobs, queueErr)
			}
			_, snapErr := st.GetSnapshot(ctx, old)
			_, docErr := st.GetDoc(ctx, old)
			if tc.wantDeleted {
				if !errors.Is(snapErr, domain.ErrNotFound) || !errors.Is(docErr, domain.ErrNotFound) {
					t.Fatalf("superseded leaf remains: snapshot=%v doc=%v", snapErr, docErr)
				}
			} else if snapErr != nil || docErr != nil {
				t.Fatalf("original data deleted without proof of supersession: snapshot=%v doc=%v", snapErr, docErr)
			}
			if tc.backfill {
				queued, err := st.ListBackfills(ctx, repo)
				if err != nil || len(queued) != 1 {
					t.Fatal(queued, err)
				}
				if err := st.UpdateBackfill(ctx, queued[0], nil); err != nil {
					t.Fatal(err)
				}
				// A restarted collector can retire a proven duplicate only after
				// the durable upload obligation was acknowledged.
				newTestSaveService(nil, nil, nil, storage.NewFileStore(root)).gcHookLeaf(ctx, repo, old, current)
				if _, err := st.GetDoc(ctx, old); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("completed pin leaked", err)
				}
			}
		})
	}
}
