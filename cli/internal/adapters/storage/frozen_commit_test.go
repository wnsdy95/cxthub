package storage

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"reflect"
	"strings"
	"testing"
)

func TestFrozenCommitRejectsWorkerStartupCodeSubstitution(t *testing.T) {
	f := newFrozenFixture(t)
	ctx := context.Background()
	before, err := f.store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next := f.op.Position
	next.GitCommit = strings.Repeat("d", 40)
	err = f.store.CommitFrozenSnapshotIfCurrent(ctx, f.op.Ref, f.position.SharedTarget, before, next, nil)
	if !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatalf("stale worker: %v", err)
	}
	after, err := f.store.GetWorkingPosition(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("stale worker changed position", err)
	}
	ref, err := f.store.GetRef(ctx, f.repo, domain.RefBranch, "main")
	if err != nil || ref.Target != f.op.ExpectedRef.Target {
		t.Fatal("stale worker changed ref", err)
	}
}
