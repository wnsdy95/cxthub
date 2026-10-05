package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

var errTrackingListBeforeAcceptance = errors.New("test-owned pre-acceptance list failure")

type trackingListBeforeAcceptance struct{}

func (trackingListBeforeAcceptance) List(context.Context, inbound.ListInput) (inbound.ListOutput, error) {
	return inbound.ListOutput{}, errTrackingListBeforeAcceptance
}

// Resolve B using real fetch/query code, then let the normal position service
// select the remote-known A before CLI replay can accept its storage journal.
func trackingSelectionRetryFixture(t *testing.T, empty bool) *tracking290Fixture {
	t.Helper()
	f := newTracking290Fixture(t, false)
	if empty {
		for i := range f.remote.history {
			f.remote.history[i].MemoryHash, f.remote.history[i].MemorySource = "", ""
		}
	}
	memory := domain.MemoryDigest{SnapshotID: f.b, Summary: "B recorded memory"}
	hash, err := domain.MemoryDigestHash(memory)
	if err != nil {
		t.Fatal(err)
	}
	f.remote.memories[hash] = memory
	e := f.remote.history[1]
	e.ID, e.Target, e.MemoryHash, e.MemorySource = strings.Repeat("d", 32), f.b, hash, f.b
	f.remote.history = append(f.remote.history, e)
	got, err := f.resolve(t, 0)
	if err != nil || got.Target != f.b || got.MemoryHash != hash {
		t.Fatalf("fixture must resolve the dominating B pin: targetB=%t memoryB=%t err=%v", got.Target == f.b, got.MemoryHash == hash, err)
	}
	f.op.Event, f.op.Resolved = got, true
	ctx := context.Background()
	if err := f.store.PutHistoryEvent(ctx, f.remote.history[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateBranchRef(ctx, f.remote.ref); err != nil {
		t.Fatal(err)
	}
	return f
}

func selectTrackingRetryA(t *testing.T, f *tracking290Fixture, hash, source domain.ContentHash) domain.WorkingPosition {
	t.Helper()
	ctx := context.Background()
	if err := f.c.History.SelectPosition(ctx, domain.WorkingPosition{RepoID: f.repo, Branch: "team-task", LocalBranch: "local-task", GitCommit: f.oid, Snapshot: f.a, MemoryHash: hash, MemorySource: source, MemoryPinned: true}); err != nil {
		t.Fatal(err)
	}
	p, err := f.c.History.CurrentPosition(ctx)
	if err != nil || p.BranchID != f.op.Event.BranchID || p.GitBranch() != "local-task" || p.GitCommit != f.oid || p.Snapshot != f.a || p.MemoryHash != hash || !p.MemoryPinned || !p.Rewound || p.Selection == nil {
		t.Fatalf("normal user selection precondition failed: %v", err)
	}
	return p
}

func TestTracking290PreservesCompatibleSelectionBeforeAcceptance(t *testing.T) {
	for _, retry := range []bool{false, true} {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("retry=%t/pinned-empty=%t", retry, empty), func(t *testing.T) {
				f := trackingSelectionRetryFixture(t, empty)
				ctx := context.Background()
				j := f.journal(t)
				replay := func() error {
					return replayBranchOperationsForRefWithPublication(ctx, f.c, f.cwd, f.op.GitRef, func(string) {})
				}
				if retry {
					list := f.c.List
					f.c.List = trackingListBeforeAcceptance{}
					err := replay()
					f.c.List = list
					if !errors.Is(err, errTrackingListBeforeAcceptance) {
						t.Fatalf("pre-acceptance failure not reached: %v", err)
					}
					ops, err := j.List()
					if err != nil || len(ops) != 1 || !ops[0].Resolved || ops[0].Phase != "committed" || ops[0].LastError == "" || !reflect.DeepEqual(ops[0].Tracking, f.op.Tracking) {
						t.Fatalf("failed apply lost frozen pending operation: %v", err)
					}
				}
				for _, path := range []string{"tracking-attachment.json", filepath.Join("tracking-attachments", f.op.Event.ID+".json")} {
					if _, err := os.Stat(filepath.Join(f.cwd, ".cxt", path)); !os.IsNotExist(err) {
						t.Fatalf("fixture already accepted attachment: %s: %v", path, err)
					}
				}
				hash, owner := f.m1, f.a
				if empty {
					hash, owner = "", ""
				}
				before := selectTrackingRetryA(t, f, hash, owner)
				if err := replay(); err != nil {
					t.Fatalf("compatible selection blocked binding: %v", err)
				}
				after, err := f.c.History.CurrentPosition(ctx)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("tracking replay overwrote the newer compatible user selection: beforeA=%t afterB=%t err=%v", before.Snapshot == f.a, after.Snapshot == f.b, err)
				}
				binding, err := f.store.ResolveLocalBranch(ctx, f.repo, "local-task")
				if err != nil || !binding.Tracking || binding.BranchID != f.op.Event.BranchID {
					t.Fatalf("selection preservation skipped binding: %v", err)
				}
				ref, err := f.store.GetRef(ctx, f.repo, domain.RefBranch, "team-task")
				if err != nil || ref != f.remote.ref {
					t.Fatalf("shared identity/ref changed: %v", err)
				}
				ops, err := j.List()
				if err != nil || len(ops) != 1 || ops[0].Phase != "applied" || ops[0].LastError != "" || !reflect.DeepEqual(ops[0].Tracking, f.op.Tracking) || f.sync.calls != 1 {
					t.Fatalf("frozen operation was not acknowledged exactly once: calls=%d err=%v", f.sync.calls, err)
				}
			})
		}
	}
}

func TestTracking290PreservedSelectionStillRequiresCompatibility(t *testing.T) {
	f := trackingSelectionRetryFixture(t, false)
	ctx := context.Background()
	j := f.journal(t)
	hash, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: f.a, Summary: "unpublished user pin"})
	if err != nil {
		t.Fatal(err)
	}
	before := selectTrackingRetryA(t, f, hash, f.a)
	err = replayBranchOperationsForRefWithPublication(ctx, f.c, f.cwd, f.op.GitRef, func(string) {})
	if !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("unpublished pin bypassed store compatibility: %v", err)
	}
	after, readErr := f.c.History.CurrentPosition(ctx)
	if readErr != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed attachment changed user selection: %v", readErr)
	}
	binding, readErr := f.store.ResolveLocalBranch(ctx, f.repo, "local-task")
	if readErr != nil || binding.Tracking {
		t.Fatalf("incompatible attachment published binding: %v", readErr)
	}
	ops, readErr := j.List()
	if readErr != nil || len(ops) != 1 || ops[0].Phase != "committed" || ops[0].LastError == "" || !reflect.DeepEqual(ops[0].Tracking, f.op.Tracking) {
		t.Fatalf("incompatible attachment was acknowledged: %v", readErr)
	}
}
