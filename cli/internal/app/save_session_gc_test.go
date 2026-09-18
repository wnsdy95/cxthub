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
		name        string
		change      func(*domain.CIRDocument)
		child       bool
		failDelete  bool
		wantDeleted bool
	}{
		{name: "same native session moves worktrees", wantDeleted: true},
		{name: "different provider same native ID", change: func(d *domain.CIRDocument) { d.Envelope.SourceProvider = domain.ProviderCodex }},
		{name: "different session", change: func(d *domain.CIRDocument) { d.Envelope.SessionOriginID = "another-session" }},
		{name: "truncated capture", change: func(d *domain.CIRDocument) { d.Events = d.Events[:1] }},
		{name: "divergent capture", change: func(d *domain.CIRDocument) { d.Events[0].Role = "system" }},
		{name: "unreferenced child still needs parent", child: true},
		{name: "snapshot delete fails", failDelete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := storage.NewFileStore(t.TempDir())
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
			svc := NewSaveSessionService(nil, nil, nil, sessionStore)
			svc.gcHookLeaf(ctx, repo, old, current)
			jobs, queueErr := st.CaptureCollections(ctx, repo, 32)
			wantJobs := 0
			if tc.failDelete {
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
		})
	}
}
