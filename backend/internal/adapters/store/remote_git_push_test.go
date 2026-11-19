package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type remotePushTestStore interface {
	outbound.RemoteGitStore
	outbound.RemotePushStore
}

// checkRemotePushPersistence is the contract both stores keep.
func checkRemotePushPersistence(t *testing.T, ctx context.Context, storage remotePushTestStore, repo domain.ContentHash) {
	t.Helper()
	a, b := remoteGitStoreSHA("a"), remoteGitStoreSHA("b")
	lease := 2 * time.Minute
	if _, err := storage.ClaimRemoteGitSync(ctx, repo, remoteGitStoreOrigin, remoteGitStoreTime(0), lease); err != nil {
		t.Fatal(err)
	}
	if pushes, err := storage.ListRemotePushes(ctx, repo, []string{"main"}); err != nil || pushes == nil || len(pushes) != 0 {
		t.Fatalf("no push yet: %#v %v", pushes, err)
	}
	if err := storage.RecordRemotePushes(ctx, repo, nil); err != nil {
		t.Fatalf("recording no push: %v", err)
	}
	first := []domain.RemoteBranchPush{{Branch: "main", SHA: a, SeenAt: remoteGitStoreTime(10)}, {Branch: "topic", SHA: a, SeenAt: remoteGitStoreTime(10)}}
	if err := storage.RecordRemotePushes(ctx, repo, first); err != nil {
		t.Fatal(err)
	}
	// A branch that goes back to a commit it was at before was pushed again.
	later := []domain.RemoteBranchPush{{Branch: "main", SHA: b, SeenAt: remoteGitStoreTime(20)}, {Branch: "main", SHA: a, SeenAt: remoteGitStoreTime(30)}}
	if err := storage.RecordRemotePushes(ctx, repo, later); err != nil {
		t.Fatal(err)
	}
	// The same sighting again adds nothing.
	if err := storage.RecordRemotePushes(ctx, repo, first); err != nil {
		t.Fatal(err)
	}
	if pushes, err := storage.ListRemotePushes(ctx, repo, []string{"main", "release"}); err != nil || !reflect.DeepEqual(pushes, []domain.RemoteBranchPush{first[0], later[0], later[1]}) {
		t.Fatalf("pushes of one branch, oldest first: %+v %v", pushes, err)
	}
	if pushes, err := storage.ListRemotePushes(ctx, repo, []string{"topic", "main"}); err != nil || len(pushes) != 4 || pushes[0] != first[0] || pushes[1] != first[1] {
		t.Fatalf("pushes of two branches: %+v %v", pushes, err)
	}
	if pushes, err := storage.ListRemotePushes(ctx, repo, nil); err != nil || len(pushes) != 0 {
		t.Fatalf("pushes of no branch: %+v %v", pushes, err)
	}
	for name, refused := range map[string]domain.RemoteBranchPush{
		"a push without a branch":        {SHA: a, SeenAt: remoteGitStoreTime(40)},
		"a push without a commit number": {Branch: "main", SHA: "main", SeenAt: remoteGitStoreTime(40)},
		"a push without its time":        {Branch: "main", SHA: a},
	} {
		if err := storage.RecordRemotePushes(ctx, repo, []domain.RemoteBranchPush{refused}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s was stored: %v", name, err)
		}
	}
	// The record belongs to the origin it was read from.
	if _, err := storage.ClaimRemoteGitSync(ctx, repo, remoteGitStoreOrigin+"-moved", remoteGitStoreTime(250), lease); err != nil {
		t.Fatal(err)
	}
	if pushes, err := storage.ListRemotePushes(ctx, repo, []string{"main", "topic"}); err != nil || len(pushes) != 0 {
		t.Fatalf("pushes of the first origin were kept: %+v %v", pushes, err)
	}
}

func TestFSRemotePushPersistence(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	checkRemotePushPersistence(t, context.Background(), storage, domain.HashContent([]byte(t.Name())))
}
