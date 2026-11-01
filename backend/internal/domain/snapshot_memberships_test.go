package domain

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestProjectSnapshotBranchMemberships(t *testing.T) {
	root, next := HashContent([]byte("root")), HashContent([]byte("next"))
	snapshots := []Snapshot{{ID: root, Branch: "main", Branches: []string{"untrusted"}}}
	logs := []RefLogEntry{
		{Kind: RefBranch, Name: "topic", Old: root, New: next},
		{Kind: RefBranch, Name: "HEAD", Old: root},
		{Kind: RefTag, Name: "tag", Old: root},
	}
	got, err := ProjectSnapshotBranchMemberships(context.Background(), snapshots, logs)
	want := map[ContentHash]map[string]bool{root: {"main": true, "topic": true}, next: {"topic": true}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, err)
	}
	if !reflect.DeepEqual(snapshots[0].Branches, []string{"untrusted"}) {
		t.Fatal("projection mutated inputs")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ProjectSnapshotBranchMemberships(ctx, snapshots, logs); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
