package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type selectionChangingDistiller struct{ change func() error }

func (d selectionChangingDistiller) Distill(context.Context, domain.CIRDocument, *domain.NativeMemory) (domain.MemoryDigest, error) {
	if d.change != nil {
		if err := d.change(); err != nil {
			return domain.MemoryDigest{}, err
		}
	}
	return domain.MemoryDigest{Summary: "new synthetic memory"}, nil
}

func newMemorizePositionFixture(t *testing.T) (*storage.FileStore, domain.Repo, domain.WorkingPosition) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	st := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("a", 40))
	repo := domain.Repo{ID: string(domain.HashContent([]byte(t.Name()))), DefaultBranch: "main", LocalPath: root}
	id, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: "synthetic", SourceProvider: domain.ProviderCodex, CIRVersion: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo.ID, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err = st.PutRef(ctx, domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: "main", Target: id}); err != nil {
		t.Fatal(err)
	}
	if err = st.PutWorkingPosition(ctx, domain.WorkingPosition{RepoID: repo.ID, Branch: "main", Snapshot: id, SharedTarget: id, GitCommit: strings.Repeat("a", 40), MemoryPinned: true}); err != nil {
		t.Fatal(err)
	}
	p, err := st.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return st, repo, p
}

func TestWorkingMemoryCommitFreezesSameTupleUserSelection(t *testing.T) {
	ctx := context.Background()
	st, repo, p := newMemorizePositionFixture(t)
	newer := p
	e := *p.Selection
	e.ID = strings.Repeat("e", 32)
	newer.Selection = &e
	distiller := selectionChangingDistiller{change: func() error { return st.PutWorkingPosition(ctx, newer) }}
	svc := NewMemorizeService(branchSeedGit{repo: repo}, nil, nil, nil, distiller, st)
	_, err := svc.Memorize(ctx, inbound.MemorizeInput{Cwd: repo.LocalPath})
	if !errors.Is(err, domain.ErrSelectionChanged) {
		t.Errorf("stale same-tuple selection accepted: %v", err)
	}
	got, err := st.GetWorkingPosition(ctx)
	if err != nil || !reflect.DeepEqual(got, newer) {
		t.Errorf("user selection overwritten: %v", err)
	}
	snap, err := st.GetSnapshot(ctx, p.Snapshot)
	if err != nil || snap.MemoryHash != "" {
		t.Errorf("stale distillation attached memory: %v", err)
	}
}

func TestWorkingMemoryCommitNormalizesInheritedOwner(t *testing.T) {
	ctx := context.Background()
	st, repo, p := newMemorizePositionFixture(t)
	owner, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: "ancestor"}}})
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: owner, Summary: "inherited"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.PutSnapshot(ctx, domain.Snapshot{ID: owner, DocHash: owner, RepoID: repo.ID, MemoryHash: inherited}); err != nil {
		t.Fatal(err)
	}
	p.MemoryHash, p.MemorySource, p.Selection = inherited, owner, nil
	if err = st.PutWorkingPosition(ctx, p); err != nil {
		t.Fatal(err)
	}
	svc := NewMemorizeService(branchSeedGit{repo: repo}, nil, nil, nil, selectionChangingDistiller{}, st)
	out, err := svc.Memorize(ctx, inbound.MemorizeInput{Cwd: repo.LocalPath})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryHash != out.MemoryHash || got.MemorySource != "" || got.Selection.MemorySource != "" {
		t.Fatalf("self-owned attachment kept inherited provenance: %+v", got)
	}
	digest, err := st.GetMemory(ctx, out.MemoryHash)
	if err != nil || digest.SnapshotID != p.Snapshot || digest.PreviousMemoryHash != "" {
		t.Fatalf("inherited owner became same-owner predecessor: %v", err)
	}
}

// An older independently recorded empty pin is not ordered by the later
// inherited selection. Keep that ambiguous history closed even after a new
// local memorize succeeds.
func TestWorkingMemoryCommitUnlinkedEmptyReplayRemainsClosed(t *testing.T) {
	ctx := context.Background()
	st, repo, p := newMemorizePositionFixture(t)
	id, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: "owner"}}})
	if err != nil {
		t.Fatal(err)
	}
	h, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: id, Summary: "inherited"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo.ID, MemoryHash: h}); err != nil {
		t.Fatal(err)
	}
	p.MemoryHash, p.MemorySource, p.Selection = h, id, nil
	if err = st.PutWorkingPosition(ctx, p); err != nil {
		t.Fatal(err)
	}
	svc := NewMemorizeService(branchSeedGit{repo: repo}, nil, nil, nil, selectionChangingDistiller{}, st)
	if _, err = svc.Memorize(ctx, inbound.MemorizeInput{Cwd: repo.LocalPath}); err != nil {
		t.Fatal(err)
	}
	receipt := domain.HistoryEvent{ID: strings.Repeat("f", 32), RepoID: repo.ID, Kind: "pr-merge", PRCompleted: true, Branch: "integrated", BranchID: "base", SourceBranchID: p.BranchID, Source: p.Snapshot, Target: p.Snapshot, CreatedAt: time.Now().UTC(), PR: &domain.PullRequestMerge{Number: 1, BaseBranch: "integrated", HeadBranch: "main", HeadSHA: p.GitCommit, MergeSHA: strings.Repeat("b", 40)}}
	_, err = NewContextHistoryService(st, st).ResolvePRSourcePosition(ctx, receipt)
	if !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("inherited replay boundary unexpectedly changed: %v", err)
	}
	t.Log("unlinked historical selection remains ambiguous:", err)
}

func TestWorkingMemoryCommitJournalProtectsCaptureCollection(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "corrupt"}[corrupt], func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			st := storage.NewFileStore(root)
			repo := string(domain.HashContent([]byte(t.Name())))
			put := func(texts ...string) domain.ContentHash {
				cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "session"}}
				for i, text := range texts {
					cir.Events = append(cir.Events, domain.Event{Seq: i, Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}})
				}
				id, err := st.PutDoc(ctx, domain.SessionDoc{CIR: cir})
				if err != nil {
					t.Fatal(err)
				}
				if err = st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Provider: domain.ProviderCodex, SessionID: "session", Message: domain.HookMessagePrefix + " capture"}); err != nil {
					t.Fatal(err)
				}
				return id
			}
			old, next := put("first"), put("first", "next")
			h, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: old, Summary: "in flight"})
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(struct {
				Version int                          `json:"version"`
				Commit  outbound.WorkingMemoryCommit `json:"commit"`
			}{1, outbound.WorkingMemoryCommit{RepoID: repo, Snapshot: old, Memory: h}})
			if err != nil {
				t.Fatal(err)
			}
			if corrupt {
				data = []byte("broken redo")
			}
			path := filepath.Join(root, ".cxt", "working-memory.json")
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			newTestSaveService(nil, nil, nil, st).gcHookLeaf(ctx, repo, old, next)
			if _, err = st.GetSnapshot(ctx, old); err != nil {
				t.Fatal("pending target collected", err)
			}
			if _, err = st.GetDoc(ctx, old); err != nil {
				t.Fatal("pending doc collected", err)
			}
			if _, err = os.Stat(path); err != nil {
				t.Fatal("collector consumed redo", err)
			}
			jobs, err := st.CaptureCollections(ctx, repo, 32)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("retention did not defer collection: %v", err)
			}
		})
	}
}
