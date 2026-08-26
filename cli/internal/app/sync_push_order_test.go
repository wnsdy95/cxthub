package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type pushOrderGit struct{ repo domain.Repo }

func (g pushOrderGit) CurrentRepo(context.Context, string) (domain.Repo, error) { return g.repo, nil }
func (g pushOrderGit) CurrentBranch(context.Context, string) (string, error)    { return "main", nil }

type pushOrderRemote struct {
	outbound.RemoteSync
	events          []string
	objectSnapshots int
	objectDocs      int
	grafted         bool
	failGraft       error
}

func (r *pushOrderRemote) RegisterRepo(_ context.Context, repo domain.Repo) (domain.Repo, error) {
	return repo, nil
}

func (r *pushOrderRemote) Push(_ context.Context, _ string, snaps []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, _, _ bool) error {
	switch {
	case len(snaps) > 0 || len(docs) > 0:
		if len(refs) > 0 {
			return errors.New("objects and refs were published in one phase")
		}
		r.objectSnapshots += len(snaps)
		r.objectDocs += len(docs)
		r.events = append(r.events, "objects")
	case len(refs) > 0:
		if !r.grafted {
			return errors.New("refs published before graft")
		}
		r.events = append(r.events, "refs")
	}
	return nil
}

func (r *pushOrderRemote) GraftSnapshotParents(_ context.Context, _ string, _ domain.ContentHash, _ []domain.ContentHash, _ uint64) error {
	r.events = append(r.events, "graft")
	if r.failGraft != nil {
		return r.failGraft
	}
	r.grafted = true
	return nil
}

func (r *pushOrderRemote) DeleteUnsyncRemote(context.Context, string, string) error { return nil }

func setupPushOrder(t *testing.T) (*SyncRepoService, *pushOrderRemote, string, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	st := storage.NewFileStore(root)
	repoID := string(domain.HashContent([]byte("push-order-repo")))
	put := func(origin string, graft []domain.ContentHash, seq uint64) domain.ContentHash {
		doc := domain.SessionDoc{CIR: domain.CIRDocument{}}
		doc.CIR.Envelope.CIRVersion = "1"
		doc.CIR.Envelope.SessionOriginID = origin
		id, err := st.PutDoc(ctx, doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutSnapshot(ctx, domain.Snapshot{
			ID: id, RepoID: repoID, Branch: "main", DocHash: id,
			Grafted: len(graft) > 0, GraftParents: graft, GraftSeq: seq,
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	base := put("base", nil, 0)
	head := put("head", []domain.ContentHash{base}, 1)
	if err := st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repoID, Target: head}); err != nil {
		t.Fatal(err)
	}
	if err := queueGraft(root, head, base, 0); err != nil {
		t.Fatal(err)
	}
	remote := &pushOrderRemote{}
	repo := domain.Repo{ID: repoID, LocalPath: root, DefaultBranch: "main"}
	return NewSyncRepoService(st, remote, pushOrderGit{repo: repo}), remote, root, repoID
}

func TestPushPublishesObjectsThenGraftsThenRefs(t *testing.T) {
	svc, remote, root, _ := setupPushOrder(t)
	if _, err := svc.Push(context.Background(), inbound.SyncInput{Cwd: root}); err != nil {
		t.Fatal(err)
	}
	want := []string{"objects", "graft", "refs"}
	if len(remote.events) != len(want) {
		t.Fatalf("publish order=%v want=%v", remote.events, want)
	}
	for i := range want {
		if remote.events[i] != want[i] {
			t.Fatalf("publish order=%v want=%v", remote.events, want)
		}
	}
	if remote.objectSnapshots != 2 || remote.objectDocs != 2 {
		t.Fatalf("legacy negotiator fallback snapshots=%d docs=%d, want 2/2", remote.objectSnapshots, remote.objectDocs)
	}
}

func TestPushDoesNotPublishRefsWhenGraftFlushFails(t *testing.T) {
	svc, remote, root, _ := setupPushOrder(t)
	remote.failGraft = errors.New("server unavailable")
	if _, err := svc.Push(context.Background(), inbound.SyncInput{Cwd: root}); err == nil {
		t.Fatal("graft failed but push succeeded")
	}
	want := []string{"objects", "graft"}
	if len(remote.events) != len(want) {
		t.Fatalf("publish order=%v want=%v", remote.events, want)
	}
	for i := range want {
		if remote.events[i] != want[i] {
			t.Fatalf("publish order=%v want=%v", remote.events, want)
		}
	}
	state, err := readGraftQueue(root, ".cxt/grafts.json")
	if err != nil || len(state.Events) != 1 {
		t.Fatalf("failure after graft queue lost: %+v err=%v", state.Events, err)
	}
}
