package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type savedMemoryHistoryRemote struct {
	outbound.RemoteSync
	refs    []domain.Ref
	history []domain.HistoryEvent
}

func (*savedMemoryHistoryRemote) PullCapabilities(context.Context, string) (outbound.PullCapabilities, error) {
	return outbound.PullCapabilities{}, nil
}
func (r *savedMemoryHistoryRemote) Pull(context.Context, string, map[domain.ContentHash]domain.ContentHash, []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	return nil, nil, r.refs, nil
}
func (r *savedMemoryHistoryRemote) PullHistoryEvents(context.Context, string) ([]domain.HistoryEvent, error) {
	return r.history, nil
}
func (*savedMemoryHistoryRemote) PushHistoryEvent(context.Context, domain.HistoryEvent) error {
	return errors.New("unexpected history publication")
}

// Exercise the common Save producer through both native capture/codec adapters,
// then pass its unmodified history through the real fetch consumer. The remote
// port supplies only history and refs; immutable objects are already cached.
func TestSaveMemoryPinRemainsFetchable(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		for _, mode := range []string{"branch", "detached", "pending-empty"} {
			t.Run(string(provider)+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				t.Setenv("HOME", t.TempDir())
				root := t.TempDir()
				repo := domain.Repo{ID: string(domain.HashContent([]byte("save memory provenance"))), LocalPath: root, DefaultBranch: "main"}
				gitBranch := "main"
				if mode == "detached" {
					gitBranch = "HEAD"
				}
				g := &stagingGit{repo: repo, branch: gitBranch, sha: strings.Repeat("a", 40)}
				st := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), gitBranch, g.sha)
				seed := pullDoc(t, "selected project context")
				if _, err := st.PutDoc(ctx, seed); err != nil {
					t.Fatal(err)
				}
				var pin domain.ContentHash
				if mode != "pending-empty" {
					var err error
					pin, err = st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: seed.Hash, Summary: "retained project knowledge"})
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := st.PutSnapshot(ctx, domain.Snapshot{RepoID: repo.ID, ID: seed.Hash, DocHash: seed.Hash, Branch: "main", MemoryHash: pin}); err != nil {
					t.Fatal(err)
				}
				ref := domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: "main", Target: seed.Hash, BranchID: domain.LegacyContextBranchID(repo.ID, "main")}
				if _, err := st.CreateBranchRef(ctx, ref); err != nil {
					t.Fatal(err)
				}
				position := domain.WorkingPosition{RepoID: repo.ID, Branch: "main", BranchID: ref.BranchID, GitCommit: g.sha, Snapshot: seed.Hash, SharedTarget: seed.Hash, MemoryHash: pin, MemorySource: seed.Hash, MemoryPinned: true}
				if mode == "detached" {
					position.Branch = ""
				}
				if err := st.PutWorkingPosition(ctx, position); err != nil {
					t.Fatal(err)
				}
				before, err := st.GetWorkingPosition(ctx)
				if err != nil {
					t.Fatal(err)
				}
				raw := strings.ReplaceAll(e2eClaudeSession, "hello", "new capture inherits memory")
				if provider == domain.ProviderCodex {
					raw = `{"timestamp":"2026-06-30T01:00:00Z","type":"session_meta","payload":{"id":"save-memory-codex","cwd":"/work/proj","model":"gpt-5-codex"}}
{"timestamp":"2026-06-30T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"new capture inherits memory"}]}}`
				}
				session := filepath.Join(root, "session.jsonl")
				if err := os.WriteFile(session, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
				svc := newTestSaveService(g,
					map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: capture.NewClaudeCapture(), domain.ProviderCodex: capture.NewCodexCapture()},
					map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec(), domain.ProviderCodex: codec.NewCodexCodec()}, st)
				in := inbound.SaveInput{Cwd: root, Provider: provider, SessionPath: session}
				if mode == "pending-empty" {
					pending := in
					pending.Pending = true
					if _, err := svc.Save(ctx, pending); err != nil {
						t.Fatal(err)
					}
					got, err := st.GetWorkingPosition(ctx)
					if err != nil || !reflect.DeepEqual(got, before) {
						t.Fatalf("pending capture changed selection: %+v %v", got, err)
					}
				}
				out, err := svc.Save(ctx, in)
				if err != nil {
					t.Fatal(err)
				}
				p, err := st.GetWorkingPosition(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var owner domain.ContentHash
				if mode != "pending-empty" {
					memory, err := st.GetMemory(ctx, p.MemoryHash)
					if err != nil {
						t.Fatal(err)
					}
					owner = memory.SnapshotID
					if owner != out.SnapshotID || memory.Summary != "retained project knowledge" || memory.PreviousMemoryHash != "" {
						t.Fatalf("capture did not clone selected memory: %+v", memory)
					}
				} else if p.MemoryHash != "" {
					t.Fatalf("empty selection acquired memory: %s", p.MemoryHash)
				}
				if p.Snapshot != out.SnapshotID || p.MemorySource != owner || !p.MemoryPinned {
					t.Errorf("working pin: snapshot=%s hash=%s owner=%s pinned=%v; want snapshot=%s owner=%s", p.Snapshot, p.MemoryHash, p.MemorySource, p.MemoryPinned, out.SnapshotID, owner)
				}
				if p.Selection == nil {
					t.Fatal("capture lacks a position event")
				}
				if p.Selection.MemoryHash != p.MemoryHash || p.Selection.MemorySource != owner || !p.Selection.MemoryPinned {
					t.Errorf("history pin: hash=%s owner=%s pinned=%v; want hash=%s owner=%s", p.Selection.MemoryHash, p.Selection.MemorySource, p.Selection.MemoryPinned, p.MemoryHash, owner)
				}
				events, err := st.ListHistoryEvents(ctx, repo.ID)
				if err != nil {
					t.Fatal(err)
				}
				current, err := st.GetRef(ctx, repo.ID, domain.RefBranch, "main")
				if err != nil {
					t.Fatal(err)
				}
				if mode == "detached" && current != ref {
					t.Fatal("detached save moved the shared branch")
				}
				remote := &savedMemoryHistoryRemote{refs: []domain.Ref{current}, history: events}
				sync := newTestSyncService(st, remote, nil)
				if _, err := sync.ResolveRemoteBranchObservation(ctx, inbound.SyncInput{RepoID: repo.ID}, "main"); err != nil {
					t.Errorf("freshly saved history must be fetchable: %v", err)
				}
				if owner != "" {
					// A forged explicit owner must still fail even though Source or
					// Target identifies the correct digest owner.
					for i := range remote.history {
						if remote.history[i].ID == p.Selection.ID {
							remote.history[i].MemorySource = seed.Hash
						}
					}
					if _, err := sync.ResolveRemoteBranchObservation(ctx, inbound.SyncInput{RepoID: repo.ID}, "main"); !errors.Is(err, domain.ErrHashMismatch) {
						t.Errorf("wrong explicit owner accepted: %v", err)
					}
				}
				if mode == "detached" {
					return
				}
				again, err := svc.Save(ctx, in)
				if err != nil || again.SnapshotID != out.SnapshotID {
					t.Fatalf("unchanged-session retry: %+v %v", again, err)
				}
				after, err := st.GetWorkingPosition(ctx)
				if err != nil || !reflect.DeepEqual(after, p) {
					t.Fatalf("retry changed the accepted position: %+v %v", after, err)
				}
			})
		}
	}
}
