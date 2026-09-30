package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type inspectionFixture struct {
	store    *FileStore
	root     domain.ContentHash
	tip      domain.ContentHash
	memory   domain.ContentHash
	settings []domain.ContentHash
	event    domain.HistoryEvent
	position domain.WorkingPosition
}

func newInspectionFixture(t *testing.T) inspectionFixture {
	t.Helper()
	ctx := context.Background()
	f := inspectionFixture{store: NewFileStore(t.TempDir())}
	repo := string(domain.HashContent([]byte("synthetic inspection repo")))
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, target := range []*domain.ContentHash{&f.root, &f.tip} {
		cir := sampleCIR(fmt.Sprintf("inspection document %d", i))
		hash, err := f.store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
		if err != nil {
			t.Fatal(err)
		}
		*target = hash
		if i == 0 {
			// Exercise the legacy whole-blob fallback alongside a v2 tip.
			raw, err := domain.CanonicalBytes(cir)
			if err != nil {
				t.Fatal(err)
			}
			inspectionWrite(t, f.store.objectPath("docs", hash), docCompress(raw))
		}
	}
	for _, kind := range []string{"claude", "agents", "codex"} {
		hash, err := f.store.PutSettingsObject(ctx, domain.SettingsBundle{Kind: kind, Files: []domain.SettingsFile{}})
		if err != nil {
			t.Fatal(err)
		}
		f.settings = append(f.settings, hash)
	}
	var err error
	f.memory, err = f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: f.tip, Summary: "synthetic memory"})
	if err != nil {
		t.Fatal(err)
	}
	for i, hash := range []domain.ContentHash{f.root, f.tip} {
		snap := domain.Snapshot{ID: hash, DocHash: hash, RepoID: repo, Branch: "main", CreatedAt: created.Add(time.Duration(i) * time.Second)}
		if hash == f.tip {
			snap.Parents = []domain.ContentHash{f.root}
			snap.ClaudeSettings, snap.AgentsSettings, snap.CodexSettings = f.settings[0], f.settings[1], f.settings[2]
			snap.MemoryHash = f.memory
		}
		if err := f.store.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.PutRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: f.tip}); err != nil {
		t.Fatal(err)
	}
	f.event = domain.HistoryEvent{
		ID: strings.Repeat("a", 32), RepoID: repo, Branch: "main", BranchID: domain.LegacyContextBranchID(repo, "main"),
		Kind: "attach", LocalBranch: "local", CreatedAt: created,
		Source: f.root, Target: f.tip, SharedTarget: f.tip, MemorySource: f.root, MemoryHash: f.memory,
	}
	if err := f.store.PutHistoryEvent(ctx, f.event); err != nil {
		t.Fatal(err)
	}
	f.position = domain.WorkingPosition{
		RepoID: repo, WorktreeID: strings.Repeat("b", 32), Branch: "main", BranchID: f.event.BranchID,
		Snapshot: f.tip, SharedTarget: f.root, MemorySource: f.root, MemoryHash: f.memory, Selection: &f.event,
	}
	inspectionWriteJSON(t, filepath.Join(f.store.storeDir(), "worktrees", f.position.WorktreeID, "position.json"), f.position)
	if err := f.store.BindLocalBranch(ctx, f.event); err != nil {
		t.Fatal(err)
	}
	return f
}

func inspectionWrite(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := writeAtomic(path, raw); err != nil {
		t.Fatal(err)
	}
}

func inspectionWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	inspectionWrite(t, path, raw)
}

// Include directories, modes, mtimes and file bytes to catch recovery,
// repacking, caches, mutation locks or other writes during inspection.
func inspectionDiskState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var raw []byte
		if !entry.IsDir() {
			raw, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		state[path] = fmt.Sprintf("%v/%d/%s", info.Mode(), info.ModTime().UnixNano(), raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func assertInspectionUnchanged(t *testing.T, store *FileStore, before map[string]string) {
	t.Helper()
	if after := inspectionDiskState(t, store.repoRoot); !reflect.DeepEqual(before, after) {
		t.Fatal("inspection changed synthetic replica files or directories")
	}
}

func TestInspectReplicaCompletedReadOnly(t *testing.T) {
	f := newInspectionFixture(t)
	before := inspectionDiskState(t, f.store.repoRoot)
	r := f.store.InspectReplica(context.Background())
	if !r.Completed || r.Snapshots != 2 || r.DocumentsChecked != 2 || r.HistoryEvents != 1 || len(r.Issues) != 0 {
		t.Fatalf("healthy inspection: %+v", r)
	}
	assertInspectionUnchanged(t, f.store, before)
	raw, err := json.Marshal(r)
	if err != nil || !strings.Contains(string(raw), `"completed":true`) {
		t.Fatalf("completion JSON: %s, %v", raw, err)
	}
}

func TestInspectReplicaCorruptionStillCompleted(t *testing.T) {
	missing := domain.HashContent([]byte("synthetic missing object"))
	cases := []struct {
		name   string
		damage func(*testing.T, inspectionFixture)
		label  func(inspectionFixture) string
	}{
		{"document", func(t *testing.T, f inspectionFixture) {
			inspectionWrite(t, f.store.objectPath("docs", f.tip), []byte("corrupt document"))
		}, func(f inspectionFixture) string { return "document " + string(f.tip) + ": " }},
		{"v2 chunk", func(t *testing.T, f inspectionFixture) {
			raw, err := os.ReadFile(f.store.objectPath("docs", f.tip))
			if err != nil {
				t.Fatal(err)
			}
			body, err := docDecompress(raw)
			if err != nil {
				t.Fatal(err)
			}
			manifest, ok := chunkcas.ParseManifest(body)
			if !ok || manifest.Format != chunkcas.FormatV2 || len(manifest.Chunks) == 0 {
				t.Fatal("fixture must contain a v2 document")
			}
			inspectionWrite(t, f.store.objectPath("chunks", manifest.Chunks[0]), docCompress([]byte("[]")))
		}, func(f inspectionFixture) string { return "document " + string(f.tip) + ": " }},
		{"parent", func(t *testing.T, f inspectionFixture) {
			snap, err := f.store.GetSnapshot(context.Background(), f.tip)
			if err != nil {
				t.Fatal(err)
			}
			snap.Parents = []domain.ContentHash{missing}
			inspectionWriteJSON(t, f.store.objectPath("snapshots", f.tip), snap)
		}, func(inspectionFixture) string { return "parent " + string(missing) + ": " }},
		{"settings", func(t *testing.T, f inspectionFixture) {
			inspectionWrite(t, f.store.objectPath("settingsobjs", f.settings[0]), []byte("{}"))
		}, func(f inspectionFixture) string { return "settings " + string(f.settings[0]) + ": " }},
		{"memory", func(t *testing.T, f inspectionFixture) {
			inspectionWrite(t, f.store.objectPath("memories", f.memory), []byte("{}"))
		}, func(f inspectionFixture) string { return "memory " + string(f.memory) + ": " }},
		{"ref", func(t *testing.T, f inspectionFixture) {
			if err := f.store.PutRef(context.Background(), domain.Ref{RepoID: f.event.RepoID, Kind: domain.RefBranch, Name: "main", Target: missing}); err != nil {
				t.Fatal(err)
			}
		}, func(inspectionFixture) string { return "ref main: " }},
		{"history root", func(t *testing.T, f inspectionFixture) {
			f.event.Source = missing
			inspectionWriteJSON(t, filepath.Join(f.store.storeDir(), "history", f.event.ID+".json"), f.event)
		}, func(inspectionFixture) string { return "history root " + string(missing) + ": " }},
		{"worktree context", func(t *testing.T, f inspectionFixture) {
			f.position.RepoID = string(missing)
			inspectionWriteJSON(t, filepath.Join(f.store.storeDir(), "worktrees", f.position.WorktreeID, "position.json"), f.position)
		}, func(f inspectionFixture) string { return "worktree context " + string(f.tip) + ": " }},
		{"binding path", func(t *testing.T, f inspectionFixture) {
			inspectionWriteJSON(t, filepath.Join(f.store.storeDir(), "branch-bindings", "wrong.json"), localBranchRecord{Event: f.event})
		}, func(inspectionFixture) string { return "local binding path: " }},
		{"pending transaction", func(t *testing.T, f inspectionFixture) {
			inspectionWrite(t, filepath.Join(f.store.storeDir(), "working-commit.json"), []byte("unrecovered synthetic journal"))
		}, func(inspectionFixture) string { return "pending local transaction: working-commit.json" }},
		{"pending checkout transaction", func(t *testing.T, f inspectionFixture) {
			inspectionWrite(t, filepath.Join(f.store.storeDir(), "checkout-transition.json"), []byte("corrupt pending checkout journal"))
		}, func(inspectionFixture) string { return "pending local transaction: checkout-transition.json" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newInspectionFixture(t)
			tc.damage(t, f)
			before := inspectionDiskState(t, f.store.repoRoot)
			r := f.store.InspectReplica(context.Background())
			if !r.Completed || r.Snapshots != 2 || r.DocumentsChecked != 2 || r.HistoryEvents != 1 {
				t.Fatalf("corrupt but fully inspected replica: %+v", r)
			}
			found := false
			for _, issue := range r.Issues {
				found = found || strings.HasPrefix(issue, tc.label(f))
				if strings.HasPrefix(issue, "incomplete inspection") {
					t.Fatalf("damage incorrectly reported as cancellation: %+v", r)
				}
			}
			if !found {
				t.Fatalf("missing issue %q: %+v", tc.label(f), r)
			}
			assertInspectionUnchanged(t, f.store, before)
		})
	}
}

func TestInspectReplicaPreCanceled(t *testing.T) {
	f := newInspectionFixture(t)
	before := inspectionDiskState(t, f.store.repoRoot)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := f.store.inspectReplica(ctx, func(label string) { t.Fatalf("read attempted after pre-cancellation: %s", label) })
	if r.Completed || r.Snapshots != 0 || r.DocumentsChecked != 0 || r.HistoryEvents != 0 || !reflect.DeepEqual(r.Issues, []string{"incomplete inspection at entry: context canceled"}) {
		t.Fatalf("pre-canceled inspection: %+v", r)
	}
	public := f.store.InspectReplica(ctx)
	if !reflect.DeepEqual(public, r) {
		t.Fatalf("public cancellation result: %+v, want %+v", public, r)
	}
	raw, err := json.Marshal(r)
	if err != nil || !strings.Contains(string(raw), `"completed":false`) {
		t.Fatalf("incomplete JSON: %s, %v", raw, err)
	}
	assertInspectionUnchanged(t, f.store, before)
}

func TestInspectReplicaBothPendingTransactionsStayReadOnly(t *testing.T) {
	f := newInspectionFixture(t)
	names := []string{"working-commit.json", "checkout-transition.json"}
	for _, name := range names {
		inspectionWrite(t, filepath.Join(f.store.storeDir(), name), []byte("malformed pending journal"))
	}
	before := inspectionDiskState(t, f.store.repoRoot)
	r := f.store.InspectReplica(context.Background())
	if !r.Completed || r.Snapshots != 2 || r.DocumentsChecked != 2 || r.HistoryEvents != 1 || len(r.Issues) != 2 {
		t.Fatalf("both pending transactions: %+v", r)
	}
	for i, name := range names {
		if r.Issues[i] != "pending local transaction: "+name+" (normal mutation retries recovery)" {
			t.Fatalf("pending transaction not reported: %+v", r)
		}
	}
	assertInspectionUnchanged(t, f.store, before)
}

func TestInspectReplicaExpiredDeadline(t *testing.T) {
	store := NewFileStore(t.TempDir())
	before := inspectionDiskState(t, store.repoRoot)
	// A deadline already in the past avoids scheduler or wall-clock races.
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	r := store.InspectReplica(ctx)
	if r.Completed || r.Snapshots != 0 || r.DocumentsChecked != 0 || r.HistoryEvents != 0 || !reflect.DeepEqual(r.Issues, []string{"incomplete inspection at entry: context deadline exceeded"}) {
		t.Fatalf("expired inspection: %+v", r)
	}
	assertInspectionUnchanged(t, store, before)
}

func TestInspectReplicaCancellationAtEveryReadBoundary(t *testing.T) {
	f := newInspectionFixture(t)
	before := inspectionDiskState(t, f.store.repoRoot)
	var boundaries []string
	r := f.store.inspectReplica(context.Background(), func(label string) { boundaries = append(boundaries, label) })
	if !r.Completed || len(r.Issues) != 0 {
		t.Fatalf("boundary fixture: %+v", r)
	}
	// These prefixes ensure the trace exercises every requested loop family.
	for _, prefix := range []string{"snapshot metadata", "document ", "parent ", "settings ", "memory ", "references", "ref ", "history", "history root ", "history memory ", "worktrees", "worktree ", "worktree context ", "worktree memory ", "branch-bindings", "local binding ", "working-commit.json", "checkout-transition.json"} {
		found := false
		for _, label := range boundaries {
			found = found || strings.HasPrefix(label, prefix)
		}
		if !found {
			t.Fatalf("fixture omitted %q read boundary: %v", prefix, boundaries)
		}
	}
	for cutoff, label := range boundaries {
		t.Run(fmt.Sprintf("%02d %s", cutoff, label), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var visited []string
			r := f.store.inspectReplica(ctx, func(label string) {
				visited = append(visited, label)
				if len(visited) == cutoff+1 {
					cancel()
				}
			})
			wantIssue := "incomplete inspection at " + label + ": context canceled"
			if r.Completed || !reflect.DeepEqual(r.Issues, []string{wantIssue}) {
				t.Fatalf("cancellation at %q: %+v", label, r)
			}
			if !reflect.DeepEqual(visited, boundaries[:cutoff+1]) {
				t.Fatalf("reads continued or changed at cancellation: %v, want %v", visited, boundaries[:cutoff+1])
			}
			wantSnapshots, wantDocuments, wantEvents := 0, 0, 0
			for _, previous := range boundaries[:cutoff] {
				if previous == "snapshot metadata" {
					wantSnapshots = 2
				}
				if previous == "history" {
					wantEvents = 1
				}
				if strings.HasPrefix(previous, "document ") {
					wantDocuments++
				}
			}
			if r.Snapshots != wantSnapshots || r.DocumentsChecked != wantDocuments || r.HistoryEvents != wantEvents {
				t.Fatalf("partial counts = %d/%d/%d, want %d/%d/%d", r.Snapshots, r.DocumentsChecked, r.HistoryEvents, wantSnapshots, wantDocuments, wantEvents)
			}
			assertInspectionUnchanged(t, f.store, before)
		})
	}
}

func TestInspectReplicaCancellationDistinguishesMetadataAndCheckedDocuments(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt=%v", corrupt), func(t *testing.T) {
			f := newInspectionFixture(t)
			if corrupt {
				inspectionWrite(t, f.store.objectPath("docs", f.tip), []byte("corrupt document"))
			}
			before := inspectionDiskState(t, f.store.repoRoot)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			label := "document " + string(f.root)
			r := f.store.inspectReplica(ctx, func(boundary string) {
				if boundary == label {
					cancel()
				}
			})
			if r.Completed || r.Snapshots != 2 || r.DocumentsChecked != 1 || r.HistoryEvents != 0 {
				t.Fatalf("metadata total and checked documents: %+v", r)
			}
			wantIssues := 1
			if corrupt {
				wantIssues++
				if len(r.Issues) == 0 || !strings.HasPrefix(r.Issues[0], "document "+string(f.tip)+": ") {
					t.Fatalf("corrupt checked document missing from partial result: %+v", r)
				}
			}
			if len(r.Issues) != wantIssues || r.Issues[len(r.Issues)-1] != "incomplete inspection at "+label+": context canceled" {
				t.Fatalf("cancellation issue: %+v", r)
			}
			raw, err := json.Marshal(r)
			if err != nil || !strings.Contains(string(raw), `"documents_checked":1`) {
				t.Fatalf("checked documents JSON: %s, %v", raw, err)
			}
			assertInspectionUnchanged(t, f.store, before)
		})
	}
}

func TestInspectReplicaCancellationPreservesEarlierIssues(t *testing.T) {
	f := newInspectionFixture(t)
	inspectionWrite(t, f.store.objectPath("docs", f.tip), []byte("corrupt document"))
	before := inspectionDiskState(t, f.store.repoRoot)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := f.store.inspectReplica(ctx, func(label string) {
		if label == "references" {
			cancel()
		}
	})
	if r.Completed || r.Snapshots != 2 || r.DocumentsChecked != 2 || r.HistoryEvents != 0 || len(r.Issues) != 2 || !strings.HasPrefix(r.Issues[0], "document "+string(f.tip)+": ") || r.Issues[1] != "incomplete inspection at references: context canceled" {
		t.Fatalf("partial inspection lost damage or duplicated cancellation: %+v", r)
	}
	assertInspectionUnchanged(t, f.store, before)
}

// Arm cancellation for a chosen operation without racing a goroutine against
// disk I/O. Done and Err remain consistent via the underlying cancel function.
type inspectionCancelOnCheckContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (ctx *inspectionCancelOnCheckContext) Err() error {
	if ctx.remaining > 0 {
		ctx.remaining--
		if ctx.remaining == 0 {
			ctx.cancel()
		}
	}
	return ctx.Context.Err()
}

func TestInspectReplicaCancellationAfterReadIsNotDamage(t *testing.T) {
	f := newInspectionFixture(t)
	inspectionWrite(t, f.store.objectPath("memories", f.memory), []byte("{}"))
	before := inspectionDiskState(t, f.store.repoRoot)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &inspectionCancelOnCheckContext{Context: base, cancel: cancel}
	label := "memory " + string(f.memory)
	lastRead := ""
	r := f.store.inspectReplica(ctx, func(boundary string) {
		lastRead = boundary
		if boundary == label {
			// The first check admits the memory read; GetMemory currently does
			// not inspect ctx. The next check cancels after the read returns.
			ctx.remaining = 2
		}
	})
	if lastRead != label || r.Completed || r.Snapshots != 2 || r.DocumentsChecked != 1 || r.HistoryEvents != 0 || !reflect.DeepEqual(r.Issues, []string{"incomplete inspection at " + label + ": context canceled"}) {
		t.Fatalf("canceled read was reported as damage or continued: %+v, last read %q", r, lastRead)
	}
	assertInspectionUnchanged(t, f.store, before)
}

func TestInspectReplicaEmptyStoreDoesNotCreateDirectories(t *testing.T) {
	store := NewFileStore(t.TempDir())
	before := inspectionDiskState(t, store.repoRoot)
	r := store.InspectReplica(context.Background())
	if !r.Completed || r.Snapshots != 0 || r.DocumentsChecked != 0 || r.HistoryEvents != 0 || len(r.Issues) != 0 {
		t.Fatalf("empty replica: %+v", r)
	}
	assertInspectionUnchanged(t, store, before)
}
