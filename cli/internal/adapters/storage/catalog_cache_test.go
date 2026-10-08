package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type catalogCacheFixture struct{ repo, remote, epoch string }

func catalogCacheTestFixture() catalogCacheFixture {
	return catalogCacheFixture{domain.HashContent([]byte("catalog cache repository")), "https://catalog.example.test", "00000000-0000-0000-0000-000000000001"}
}

func catalogCacheJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f catalogCacheFixture) protocol(sequence int64) domain.CatalogEntry {
	return domain.CatalogEntry{Sequence: sequence, Kind: "protocol", Key: f.repo, Value: json.RawMessage(`{"context_protocol":1}`)}
}

func (f catalogCacheFixture) snapshot(t *testing.T, id int, sequence int64, message string) domain.CatalogEntry {
	t.Helper()
	hash := fmt.Sprintf("sha256:%064x", id)
	snap := domain.Snapshot{ID: hash, DocHash: hash, RepoID: f.repo, Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, Message: message,
		Parents:    []domain.ContentHash{domain.HashContent([]byte("first parent")), domain.HashContent([]byte("second parent"))},
		MemoryHash: domain.HashContent([]byte("memory " + message)), CreatedAt: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)}
	return domain.CatalogEntry{Sequence: sequence, Kind: "snapshot", Key: hash, Value: catalogCacheJSON(t, snap)}
}

func (f catalogCacheFixture) page(mode string, through int64, entries []domain.CatalogEntry, cursor string) domain.CatalogPage {
	p := domain.CatalogPage{Version: 1, RepoID: f.repo, Epoch: f.epoch, Through: through, Mode: mode, Entries: entries, NextCursor: cursor}
	if cursor == "" {
		p.Checkpoint = &domain.CatalogCheckpoint{Version: 1, RepoID: f.repo, Epoch: f.epoch, Sequence: through}
	}
	return p
}

func catalogCacheAppend(t *testing.T, st *FileStore, f catalogCacheFixture, revision domain.ContentHash, page domain.CatalogPage) domain.ContentHash {
	t.Helper()
	before := catalogCacheJSON(t, page)
	next, err := st.AppendCatalogPage(context.Background(), revision, f.repo, f.remote, page)
	if err != nil {
		t.Fatal(err)
	}
	if next == revision || domain.ValidateContentHash(next) != nil || !bytes.Equal(before, catalogCacheJSON(t, page)) {
		t.Fatal("invalid revision or caller mutation")
	}
	return next
}

func catalogCacheRead(t *testing.T, st *FileStore, f catalogCacheFixture) outbound.CatalogCache {
	t.Helper()
	got, err := NewFileStore(st.repoRoot).ReadCatalogCache(context.Background(), f.repo, f.remote)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func catalogCacheHeadFor(t *testing.T, st *FileStore, f catalogCacheFixture) catalogCacheHead {
	t.Helper()
	head, err := st.readCatalogCacheHead(context.Background(), f.repo, f.remote)
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func catalogCacheCompleteFor(t *testing.T, st *FileStore, head catalogCacheHead) catalogCacheComplete {
	t.Helper()
	complete, err := st.readCatalogCacheComplete(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	return complete
}

func catalogCachePendingFor(t *testing.T, st *FileStore, head catalogCacheHead) []domain.ContentHash {
	t.Helper()
	hashes, _, err := st.readCatalogCachePending(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	return hashes
}

func catalogCacheSeedRecord(t *testing.T, st *FileStore, record catalogCacheRecord) domain.ContentHash {
	t.Helper()
	raw := catalogCacheJSON(t, record)
	hash := domain.HashContent(raw)
	catalogCacheWrite(t, st.catalogCacheRecordPath(hash), raw)
	return hash
}

func catalogCacheBytes(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func catalogCacheWrite(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func catalogCacheSaveHead(t *testing.T, st *FileStore, f catalogCacheFixture, head catalogCacheHead) {
	t.Helper()
	var err error
	head.Revision, err = catalogCacheRevision(head.catalogCacheHeadPayload)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := st.catalogCachePath(f.repo, f.remote)
	catalogCacheWrite(t, path, catalogCacheJSON(t, head))
}

func TestCatalogCacheInterruptedBaselineAndTransactionPromotion(t *testing.T) {
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	got := catalogCacheRead(t, st, f)
	if !reflect.DeepEqual(got, outbound.CatalogCache{Version: 1, RepoID: f.repo, Remote: f.remote}) {
		t.Fatalf("absent cache: %+v", got)
	}
	if _, err := os.Stat(st.storeDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created state: %v", err)
	}
	first := f.page("baseline", 10, []domain.CatalogEntry{f.protocol(0)}, "opaque baseline cursor")
	revision := catalogCacheAppend(t, st, f, "", first)
	got = catalogCacheRead(t, st, f)
	if got.Checkpoint != nil || len(got.Entries) != 0 || !reflect.DeepEqual(got.Pending, []domain.CatalogPage{first}) || got.Revision != revision {
		t.Fatalf("partial baseline acknowledged: %+v", got)
	}
	a, b := f.snapshot(t, 1, 0, "original a"), f.snapshot(t, 2, 0, "original b")
	last := f.page("baseline", 10, []domain.CatalogEntry{a, b}, "")
	revision = catalogCacheAppend(t, NewFileStore(st.repoRoot), f, revision, last)
	complete := catalogCacheRead(t, st, f)
	if !sameCatalogCheckpoint(complete.Checkpoint, last.Checkpoint) || len(complete.Pending) != 0 || !reflect.DeepEqual(complete.Entries, []domain.CatalogEntry{f.protocol(0), a, b}) {
		t.Fatalf("completed baseline: %+v", complete)
	}
	updatedA, updatedB := f.snapshot(t, 1, 11, "updated a"), f.snapshot(t, 2, 11, "updated b")
	partial := f.page("delta", 11, []domain.CatalogEntry{updatedA}, "transaction split")
	revision = catalogCacheAppend(t, st, f, revision, partial)
	got = catalogCacheRead(t, st, f)
	if !sameCatalogCheckpoint(got.Checkpoint, complete.Checkpoint) || !reflect.DeepEqual(got.Entries, complete.Entries) || len(got.Pending) != 1 {
		t.Fatal("half transaction exposed")
	}
	final := f.page("delta", 11, []domain.CatalogEntry{updatedB}, "")
	revision = catalogCacheAppend(t, NewFileStore(st.repoRoot), f, revision, final)
	got = catalogCacheRead(t, st, f)
	if got.Revision != revision || !sameCatalogCheckpoint(got.Checkpoint, final.Checkpoint) || len(got.Pending) != 0 || !reflect.DeepEqual(got.Entries, []domain.CatalogEntry{f.protocol(0), updatedA, updatedB}) {
		t.Fatalf("split transaction not promoted: %+v", got)
	}
	for _, path := range []string{"objects", "snapshots", "refs", "remote-observations", "metadata-checkpoints", "working-selection.json"} {
		if _, err := os.Stat(filepath.Join(st.storeDir(), path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unrelated repository state created: %s %v", path, err)
		}
	}
}

func TestCatalogCacheDeletionReinsertAndRefIdentity(t *testing.T) {
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	a, b := f.snapshot(t, 1, 0, "a"), f.snapshot(t, 2, 0, "b")
	ref := domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "main", Target: a.Key}
	r := domain.CatalogEntry{Kind: "ref", Key: `["branch", "main"]`, Value: catalogCacheJSON(t, ref)}
	revision := catalogCacheAppend(t, st, f, "", f.page("baseline", 10, []domain.CatalogEntry{f.protocol(0), r, a, b}, ""))
	deletedA := domain.CatalogEntry{Sequence: 11, Kind: a.Kind, Key: a.Key, Deleted: true}
	revision = catalogCacheAppend(t, st, f, revision, f.page("delta", 12, []domain.CatalogEntry{deletedA}, "delete first"))
	a = f.snapshot(t, 1, 12, "reinserted")
	deletedB := domain.CatalogEntry{Sequence: 12, Kind: b.Kind, Key: b.Key, Deleted: true}
	revision = catalogCacheAppend(t, st, f, revision, f.page("delta", 12, []domain.CatalogEntry{a, deletedB}, ""))
	got := catalogCacheRead(t, st, f)
	if !reflect.DeepEqual(got.Entries, []domain.CatalogEntry{f.protocol(0), r, a}) {
		t.Fatalf("deletion/reinsert: %+v", got.Entries)
	}
	// Equivalent ref key encodings identify the same entity across runs.
	ref.Target = a.Key
	r = domain.CatalogEntry{Sequence: 13, Kind: "ref", Key: `["branch","main"]`, Value: catalogCacheJSON(t, ref)}
	revision = catalogCacheAppend(t, st, f, revision, f.page("delta", 13, []domain.CatalogEntry{r}, ""))
	if got := catalogCacheRead(t, st, f); !reflect.DeepEqual(got.Entries, []domain.CatalogEntry{f.protocol(0), r, a}) {
		t.Fatal("ref update created a duplicate entity")
	}
	deletedRef := domain.CatalogEntry{Sequence: 14, Kind: "ref", Key: `["branch", "main"]`, Deleted: true}
	catalogCacheAppend(t, st, f, revision, f.page("delta", 14, []domain.CatalogEntry{deletedRef}, ""))
	if got := catalogCacheRead(t, st, f); !reflect.DeepEqual(got.Entries, []domain.CatalogEntry{f.protocol(0), a}) {
		t.Fatal("ref deletion depended on JSON whitespace")
	}
}

func TestCatalogCacheResetPreservesCompleteAndBaselineReplacesEpoch(t *testing.T) {
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	revision := catalogCacheAppend(t, st, f, "", f.page("baseline", 10, []domain.CatalogEntry{f.protocol(0), f.snapshot(t, 1, 0, "old")}, ""))
	complete := catalogCacheRead(t, st, f)
	revision = catalogCacheAppend(t, st, f, revision, f.page("delta", 11, []domain.CatalogEntry{f.snapshot(t, 1, 11, "pending")}, "unfinished"))
	oldHead := catalogCacheHeadFor(t, st, f)
	retained := append(catalogCacheCompleteFor(t, st, oldHead).Pages, catalogCachePendingFor(t, st, oldHead)...)
	reset, err := st.ResetCatalogRun(context.Background(), revision, f.repo, f.remote)
	if err != nil || reset == revision || reset == complete.Revision {
		t.Fatalf("reset revision: %s %v", reset, err)
	}
	got := catalogCacheRead(t, st, f)
	if !sameCatalogCheckpoint(got.Checkpoint, complete.Checkpoint) || !reflect.DeepEqual(got.Entries, complete.Entries) || len(got.Pending) != 0 {
		t.Fatal("reset discarded completed state")
	}
	if _, err := st.ResetCatalogRun(context.Background(), revision, f.repo, f.remote); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("stale reset: %v", err)
	}
	again, err := st.ResetCatalogRun(context.Background(), reset, f.repo, f.remote)
	if err != nil || again == reset || catalogCacheHeadFor(t, st, f).Generation != oldHead.Generation+2 {
		t.Fatalf("empty reset ABA: %v", err)
	}
	f.epoch = "00000000-0000-0000-0000-000000000002"
	revision = catalogCacheAppend(t, st, f, again, f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, "new epoch"))
	got = catalogCacheRead(t, st, f)
	if !sameCatalogCheckpoint(got.Checkpoint, complete.Checkpoint) || !reflect.DeepEqual(got.Entries, complete.Entries) {
		t.Fatal("replacement exposed before final")
	}
	final := f.page("baseline", 0, []domain.CatalogEntry{}, "")
	catalogCacheAppend(t, st, f, revision, final)
	got = catalogCacheRead(t, st, f)
	if !sameCatalogCheckpoint(got.Checkpoint, final.Checkpoint) || !reflect.DeepEqual(got.Entries, []domain.CatalogEntry{f.protocol(0)}) {
		t.Fatalf("new baseline retained old entries: %+v", got)
	}
	for _, hash := range retained {
		if _, err := os.Stat(st.catalogCacheRecordPath(hash)); err != nil {
			t.Fatal("unreferenced page removed", err)
		}
	}
}

func TestCatalogCacheRejectsBadPageProgressWithoutAdvancing(t *testing.T) {
	f := catalogCacheTestFixture()
	for _, failure := range []string{"duplicate", "reorder", "epoch", "through", "mode", "final checkpoint", "missing protocol"} {
		t.Run(failure, func(t *testing.T) {
			st := NewFileStore(t.TempDir())
			first := f.page("baseline", 10, []domain.CatalogEntry{f.protocol(0), f.snapshot(t, 2, 0, "first")}, "first")
			if failure == "missing protocol" {
				first.Entries = first.Entries[1:]
			}
			revision := catalogCacheAppend(t, st, f, "", first)
			bad := f.page("baseline", 10, []domain.CatalogEntry{f.snapshot(t, 3, 0, "last")}, "")
			switch failure {
			case "duplicate":
				bad = first
			case "reorder":
				bad.Entries = []domain.CatalogEntry{f.snapshot(t, 1, 0, "out of order")}
			case "epoch":
				bad.Epoch = "00000000-0000-0000-0000-000000000002"
				bad.Checkpoint.Epoch = bad.Epoch
			case "through":
				bad.Through++
				bad.Checkpoint.Sequence++
			case "mode":
				bad.Mode = "delta"
			case "final checkpoint":
				bad.Checkpoint.Sequence++
			}
			path, _ := st.catalogCachePath(f.repo, f.remote)
			before := catalogCacheBytes(t, path)
			if next, err := st.AppendCatalogPage(context.Background(), revision, f.repo, f.remote, bad); next != "" || !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatalf("invalid page: %s %v", next, err)
			}
			if !bytes.Equal(before, catalogCacheBytes(t, path)) {
				t.Fatal("failed page changed head")
			}
			if got := catalogCacheRead(t, st, f); got.Checkpoint != nil || len(got.Pending) != 1 {
				t.Fatal("invalid page acknowledged")
			}
		})
	}
	st := NewFileStore(t.TempDir())
	if _, err := st.AppendCatalogPage(context.Background(), "", f.repo, f.remote, f.page("delta", 10, []domain.CatalogEntry{}, "")); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("delta without baseline: %v", err)
	}
	if _, err := st.AppendCatalogPage(context.Background(), "", f.repo, f.remote, f.page("baseline", 0, []domain.CatalogEntry{}, "")); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("empty baseline without protocol: %v", err)
	}
	if got := catalogCacheRead(t, st, f); got.Revision != "" {
		t.Fatal("invalid initial image published")
	}
	revision := catalogCacheAppend(t, st, f, "", f.page("baseline", 10, []domain.CatalogEntry{f.protocol(0)}, ""))
	deletedProtocol := domain.CatalogEntry{Sequence: 11, Kind: "protocol", Key: f.repo, Deleted: true}
	if _, err := st.AppendCatalogPage(context.Background(), revision, f.repo, f.remote, f.page("delta", 11, []domain.CatalogEntry{deletedProtocol}, "")); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("required metadata deletion: %v", err)
	}
	if got := catalogCacheRead(t, st, f); got.Revision != revision {
		t.Fatal("invalid complete delta published")
	}
}

func TestCatalogCacheRejectsCursorCycleAndNonadjacentRefAlias(t *testing.T) {
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	revision := catalogCacheAppend(t, st, f, "", f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, "a"))
	revision = catalogCacheAppend(t, st, f, revision, f.page("baseline", 0, []domain.CatalogEntry{f.snapshot(t, 1, 0, "one")}, "b"))
	// Partial staging validates only its predecessor. A nonadjacent cycle may
	// be staged, but full reads and final promotion must reject the whole run.
	revision = catalogCacheAppend(t, st, f, revision, f.page("baseline", 0, []domain.CatalogEntry{f.snapshot(t, 2, 0, "two")}, "a"))
	catalogCacheAssertCorrupt(t, st, f, revision, f.page("baseline", 0, []domain.CatalogEntry{}, ""))
	st = NewFileStore(t.TempDir())
	refEntry := func(name, key string) domain.CatalogEntry {
		return domain.CatalogEntry{Kind: "ref", Key: key, Value: catalogCacheJSON(t, domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: name, Target: domain.HashContent([]byte("target"))})}
	}
	revision = catalogCacheAppend(t, st, f, "", f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0), refEntry("main", `["branch", "main"]`)}, "r1"))
	revision = catalogCacheAppend(t, st, f, revision, f.page("baseline", 0, []domain.CatalogEntry{refEntry("zzz", `["branch", "zzz"]`)}, "r2"))
	if _, err := st.AppendCatalogPage(context.Background(), revision, f.repo, f.remote, f.page("baseline", 0, []domain.CatalogEntry{refEntry("main", `["branch","main"]`)}, "")); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("nonadjacent ref alias: %v", err)
	}
}

func TestCatalogCacheCASHasOneWinner(t *testing.T) {
	f := catalogCacheTestFixture()
	for _, stage := range []bool{false, true} {
		t.Run(fmt.Sprint(stage), func(t *testing.T) {
			st := NewFileStore(t.TempDir())
			var expected domain.ContentHash
			if stage {
				expected = catalogCacheAppend(t, st, f, "", f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, "pending"))
			}
			type result struct {
				revision domain.ContentHash
				entry    domain.CatalogEntry
				err      error
			}
			start := make(chan struct{})
			results := make(chan result, 8)
			for i := range 8 {
				entry := f.snapshot(t, 1, 0, fmt.Sprintf("writer %d", i))
				page := f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0), entry}, "")
				if stage {
					page.Entries = []domain.CatalogEntry{entry}
				}
				go func() {
					<-start
					revision, err := NewFileStore(st.repoRoot).AppendCatalogPage(context.Background(), expected, f.repo, f.remote, page)
					results <- result{revision, entry, err}
				}()
			}
			close(start)
			wins, conflicts := 0, 0
			var winner result
			for range 8 {
				r := <-results
				if r.err == nil {
					wins++
					winner = r
				} else if errors.Is(r.err, domain.ErrSyncConflict) {
					conflicts++
				} else {
					t.Error(r.err)
				}
			}
			if wins != 1 || conflicts != 7 {
				t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
			}
			got := catalogCacheRead(t, st, f)
			if got.Revision != winner.revision || !reflect.DeepEqual(got.Entries, []domain.CatalogEntry{f.protocol(0), winner.entry}) || len(got.Pending) != 0 {
				t.Fatal("CAS winner lost")
			}
		})
	}
}

func TestCatalogCacheHeadCorruptionAndScopeFailClosed(t *testing.T) {
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	page := f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, "")
	revision := catalogCacheAppend(t, st, f, "", page)
	path, _ := st.catalogCachePath(f.repo, f.remote)
	original := catalogCacheBytes(t, path)
	for name, raw := range map[string][]byte{
		"checksum":   bytes.Replace(original, []byte(`"generation":1`), []byte(`"generation":2`), 1),
		"duplicate":  bytes.Replace(original, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"unknown":    bytes.Replace(original, []byte(`"version":1`), []byte(`"version":1,"unknown":1`), 1),
		"trailing":   append(append([]byte(nil), original...), []byte(` {}`)...),
		"case alias": bytes.Replace(original, []byte(`"generation":1`), []byte(`"Generation":1`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			catalogCacheWrite(t, path, raw)
			catalogCacheAssertCorrupt(t, st, f, revision, page)
			if !bytes.Equal(raw, catalogCacheBytes(t, path)) {
				t.Fatal("corrupt head repaired")
			}
		})
	}
	catalogCacheWrite(t, path, original)
	head := catalogCacheHeadFor(t, st, f)
	for name, mutate := range map[string]func(*catalogCacheHead){
		"format":             func(h *catalogCacheHead) { h.Format = "bad" },
		"version":            func(h *catalogCacheHead) { h.Version++ },
		"generation":         func(h *catalogCacheHead) { h.Generation = 0 },
		"pending count":      func(h *catalogCacheHead) { h.PendingCount = h.Generation + 1 },
		"scope":              func(h *catalogCacheHead) { h.Remote += "/foreign" },
		"missing checkpoint": func(h *catalogCacheHead) { h.Checkpoint = nil },
		"bad hash":           func(h *catalogCacheHead) { h.Completed = "invalid" },
		"foreign checkpoint": func(h *catalogCacheHead) {
			cp := *h.Checkpoint
			cp.RepoID = domain.HashContent([]byte("foreign"))
			h.Checkpoint = &cp
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := head
			mutate(&bad)
			catalogCacheSaveHead(t, st, f, bad)
			catalogCacheAssertCorrupt(t, st, f, revision, page)
		})
	}
	catalogCacheWrite(t, path, original)
	for _, foreign := range []catalogCacheFixture{{f.repo, f.remote + "/other", f.epoch}, {domain.HashContent([]byte("other repo")), f.remote, f.epoch}} {
		if got := catalogCacheRead(t, st, foreign); got.Revision != "" {
			t.Fatal("cache crossed endpoint/repository")
		}
		foreignPath, _ := st.catalogCachePath(foreign.repo, foreign.remote)
		catalogCacheWrite(t, foreignPath, original)
		if _, err := st.ReadCatalogCache(context.Background(), foreign.repo, foreign.remote); !errors.Is(err, domain.ErrHashMismatch) {
			t.Fatalf("copied foreign head: %v", err)
		}
	}
}

func catalogCacheAssertCorrupt(t *testing.T, st *FileStore, f catalogCacheFixture, revision domain.ContentHash, page domain.CatalogPage) {
	t.Helper()
	if got, err := st.ReadCatalogCache(context.Background(), f.repo, f.remote); !errors.Is(err, domain.ErrHashMismatch) || !reflect.DeepEqual(got, outbound.CatalogCache{}) {
		t.Fatalf("corrupt read: %+v %v", got, err)
	}
	if _, err := st.AppendCatalogPage(context.Background(), revision, f.repo, f.remote, page); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("corrupt append: %v", err)
	}
	if _, err := st.ResetCatalogRun(context.Background(), revision, f.repo, f.remote); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("corrupt reset: %v", err)
	}
}

func TestCatalogCacheReferencedPageCorruptionAndSymlinks(t *testing.T) {
	f := catalogCacheTestFixture()
	for _, where := range []string{"complete", "descriptor", "pending", "head", "directory"} {
		for _, damage := range []string{"corrupt", "missing", "symlink"} {
			if where == "directory" && damage != "symlink" || where == "head" && damage == "missing" {
				continue
			}
			t.Run(where+"/"+damage, func(t *testing.T) {
				st := NewFileStore(t.TempDir())
				page := f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, "")
				revision := catalogCacheAppend(t, st, f, "", page)
				revision = catalogCacheAppend(t, st, f, revision, f.page("delta", 1, []domain.CatalogEntry{f.snapshot(t, 1, 1, "pending")}, "pending"))
				head := catalogCacheHeadFor(t, st, f)
				path := st.catalogCacheRecordPath(catalogCacheCompleteFor(t, st, head).Pages[0])
				if where == "pending" {
					path = st.catalogCacheRecordPath(catalogCachePendingFor(t, st, head)[0])
				}
				if where == "head" {
					path, _ = st.catalogCachePath(f.repo, f.remote)
				}
				if where == "descriptor" {
					path = st.catalogCacheCompletePath(head.Completed)
				}
				outside := filepath.Join(t.TempDir(), "outside")
				if where == "directory" {
					path = filepath.Join(st.storeDir(), catalogCacheNamespace, "pages")
					if err := os.Rename(path, outside); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, path); err != nil {
						t.Fatal(err)
					}
				} else {
					original := catalogCacheBytes(t, path)
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					switch damage {
					case "corrupt":
						catalogCacheWrite(t, path, []byte("tampered"))
					case "symlink":
						catalogCacheWrite(t, outside, original)
						if err := os.Symlink(outside, path); err != nil {
							t.Fatal(err)
						}
					}
				}
				catalogCacheAssertCorrupt(t, st, f, revision, page)
				if damage == "symlink" {
					if _, err := os.Readlink(path); err != nil {
						t.Fatal("symlink replaced")
					}
				}
			})
		}
	}
}

func TestCatalogCacheChecksummedInvalidRecordsAndProgressAreRejected(t *testing.T) {
	f := catalogCacheTestFixture()
	for _, fault := range []string{"scope", "record format", "wire scope", "short chain", "reordered pages", "repeated link", "missing link", "count", "premature checkpoint"} {
		t.Run(fault, func(t *testing.T) {
			st := NewFileStore(t.TempDir())
			first := f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, "first")
			revision := catalogCacheAppend(t, st, f, "", first)
			revision = catalogCacheAppend(t, st, f, revision, f.page("baseline", 0, []domain.CatalogEntry{f.snapshot(t, 1, 0, "one")}, "second"))
			head := catalogCacheHeadFor(t, st, f)
			hashes := catalogCachePendingFor(t, st, head)
			tail, err := st.readCatalogCacheRecord(context.Background(), f.repo, f.remote, head.PendingTail)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "short chain":
				head.PendingTail = hashes[0]
			case "count":
				head.PendingCount--
			case "premature checkpoint":
				head.Checkpoint = &domain.CatalogCheckpoint{Version: 1, RepoID: f.repo, Epoch: f.epoch, Sequence: 0}
			case "repeated link":
				tail.Previous = head.PendingTail
				head.PendingTail = catalogCacheSeedRecord(t, st, tail)
			case "missing link":
				tail.Previous = domain.HashContent([]byte("missing link"))
				head.PendingTail = catalogCacheSeedRecord(t, st, tail)
			case "reordered pages":
				reorderedFirst := tail
				reorderedFirst.Previous = ""
				reorderedFirst.Index = 1
				reorderedLast := catalogCacheRecord{Format: catalogCachePageFormat, Version: 1, RepoID: f.repo, Remote: f.remote, Index: 2, Page: &first, Previous: catalogCacheSeedRecord(t, st, reorderedFirst)}
				head.PendingTail = catalogCacheSeedRecord(t, st, reorderedLast)
			default:
				record := catalogCacheRecord{Format: catalogCachePageFormat, Version: 1, RepoID: f.repo, Remote: f.remote, Index: 1, Page: &first}
				switch fault {
				case "scope":
					record.Remote += "/foreign"
				case "record format":
					record.Format = "unknown"
				case "wire scope":
					record.Page.RepoID = domain.HashContent([]byte("foreign"))
				}
				tail.Previous = catalogCacheSeedRecord(t, st, record)
				head.PendingTail = catalogCacheSeedRecord(t, st, tail)
			}
			catalogCacheSaveHead(t, st, f, head)
			catalogCacheAssertCorrupt(t, st, f, revision, f.page("baseline", 0, []domain.CatalogEntry{}, ""))
		})
	}
}

func TestCatalogCacheImmutableOrphanIsNeverRepaired(t *testing.T) {
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	page := f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, "")
	record := catalogCacheRecord{Format: catalogCachePageFormat, Version: 1, RepoID: f.repo, Remote: f.remote, Index: 1, Page: &page}
	path := st.catalogCacheRecordPath(domain.HashContent(catalogCacheJSON(t, record)))
	corrupt := []byte("corrupt unreferenced page")
	catalogCacheWrite(t, path, corrupt)
	if _, err := st.AppendCatalogPage(context.Background(), "", f.repo, f.remote, page); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("corrupt immutable destination overwritten: %v", err)
	}
	if !bytes.Equal(catalogCacheBytes(t, path), corrupt) || catalogCacheRead(t, st, f).Revision != "" {
		t.Fatal("orphan repaired or published")
	}
}

func TestCatalogCacheGenerationOverflowAndReadCancellation(t *testing.T) {
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	catalogCacheAppend(t, st, f, "", f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, ""))
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	ctx := metadataCheckpointCheckContext{Context: base, check: func() {
		calls++
		if calls == 5 {
			cancel()
		}
	}}
	if got, err := st.ReadCatalogCache(ctx, f.repo, f.remote); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, outbound.CatalogCache{}) {
		t.Fatalf("canceled partial read leaked: %+v %v", got, err)
	}
	head := catalogCacheHeadFor(t, st, f)
	head.Generation = ^uint64(0)
	catalogCacheSaveHead(t, st, f, head)
	head = catalogCacheHeadFor(t, st, f)
	path, _ := st.catalogCachePath(f.repo, f.remote)
	before := catalogCacheBytes(t, path)
	if _, err := st.AppendCatalogPage(context.Background(), head.Revision, f.repo, f.remote, f.page("delta", 0, []domain.CatalogEntry{}, "")); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("append generation overflow: %v", err)
	}
	if _, err := st.ResetCatalogRun(context.Background(), head.Revision, f.repo, f.remote); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("reset generation overflow: %v", err)
	}
	if !bytes.Equal(before, catalogCacheBytes(t, path)) {
		t.Fatal("overflow changed head")
	}
}

func TestCatalogCachePublicationFailureAndCancellation(t *testing.T) {
	f := catalogCacheTestFixture()
	for _, fault := range []string{"cancel", "head symlink"} {
		t.Run(fault, func(t *testing.T) {
			st := NewFileStore(t.TempDir())
			revision := catalogCacheAppend(t, st, f, "", f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, ""))
			page := f.page("delta", 1, []domain.CatalogEntry{f.snapshot(t, 1, 1, "new")}, "")
			record := catalogCacheRecord{Format: catalogCachePageFormat, Version: 1, RepoID: f.repo, Remote: f.remote, Index: 1, Page: &page}
			recordPath := st.catalogCacheRecordPath(domain.HashContent(catalogCacheJSON(t, record)))
			headPath, _ := st.catalogCachePath(f.repo, f.remote)
			before := catalogCacheBytes(t, headPath)
			outside := filepath.Join(t.TempDir(), "outside")
			catalogCacheWrite(t, outside, before)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			fired := false
			ctx := metadataCheckpointCheckContext{Context: base, check: func() {
				if fired || !fileExists(recordPath) {
					return
				}
				fired = true
				if fault == "cancel" {
					cancel()
					return
				}
				if err := os.Remove(headPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, headPath); err != nil {
					t.Fatal(err)
				}
			}}
			got, err := st.AppendCatalogPage(ctx, revision, f.repo, f.remote, page)
			want := error(context.Canceled)
			if fault == "head symlink" {
				want = domain.ErrHashMismatch
			}
			if !fired || got != "" || !errors.Is(err, want) {
				t.Fatalf("publication fault: %s %v fired=%t", got, err, fired)
			}
			if !bytes.Equal(before, catalogCacheBytes(t, outside)) {
				t.Fatal("outside target modified")
			}
			if fault == "head symlink" {
				if _, err := os.Readlink(headPath); err != nil {
					t.Fatal("symlink overwritten")
				}
				if err := os.Remove(headPath); err != nil {
					t.Fatal(err)
				}
				catalogCacheWrite(t, headPath, before)
			}
			if got := catalogCacheRead(t, st, f); got.Revision != revision || len(got.Entries) != 1 {
				t.Fatal("orphan page published")
			}
			catalogCacheAppend(t, st, f, revision, page)
		})
	}
	st := NewFileStore(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.ReadCatalogCache(ctx, f.repo, f.remote); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := st.ResetCatalogRun(ctx, "", f.repo, f.remote); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := st.AppendCatalogPage(ctx, "", f.repo, f.remote, f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, "")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.storeDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled request created state")
	}
}

// Count newly created immutable bytes plus each replacement head, without wall
// clock thresholds or an implementation-specific test write hook.
func catalogCacheFiles(t *testing.T, st *FileStore) map[string]int64 {
	t.Helper()
	out := make(map[string]int64)
	err := filepath.WalkDir(filepath.Join(st.storeDir(), catalogCacheNamespace), func(path string, entry os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			out[path] = info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCatalogCacheBoundedWritesAndWarmEmptyDelta(t *testing.T) {
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	entries := []domain.CatalogEntry{f.protocol(0)}
	for i := 1; i <= 3000; i++ {
		entries = append(entries, f.snapshot(t, i, 0, "baseline payload"))
	}
	var revision domain.ContentHash
	var total int64
	headPath, _ := st.catalogCachePath(f.repo, f.remote)
	before := catalogCacheFiles(t, st)
	for start := 0; start < len(entries); start += 250 {
		end := min(start+250, len(entries))
		cursor := ""
		if end < len(entries) {
			cursor = fmt.Sprintf("page-%d", end)
		}
		revision = catalogCacheAppend(t, st, f, revision, f.page("baseline", 0, entries[start:end], cursor))
		after := catalogCacheFiles(t, st)
		for path, size := range after {
			if _, existed := before[path]; !existed || path == headPath {
				total += size
			}
		}
		before = after
	}
	imageBytes := int64(len(catalogCacheJSON(t, entries)))
	if total > imageBytes*2 {
		t.Fatalf("cumulative baseline writes=%d image=%d", total, imageBytes)
	}
	head := catalogCacheHeadFor(t, st, f)
	oldPageBytes := make(map[domain.ContentHash][]byte)
	for _, hash := range catalogCacheCompleteFor(t, st, head).Pages {
		oldPageBytes[hash] = catalogCacheBytes(t, st.catalogCacheRecordPath(hash))
	}
	for range 3 {
		revision = catalogCacheAppend(t, st, f, revision, f.page("delta", 0, []domain.CatalogEntry{}, ""))
	}
	if next := catalogCacheHeadFor(t, st, f); next.Completed != head.Completed || next.Generation != head.Generation+3 {
		t.Fatal("empty delta grew/rebuilt complete image")
	}
	before = catalogCacheFiles(t, st)
	revision = catalogCacheAppend(t, st, f, revision, f.page("delta", 1, []domain.CatalogEntry{f.snapshot(t, 1, 1, "single change")}, ""))
	after := catalogCacheFiles(t, st)
	var written int64
	for path, size := range after {
		if _, existed := before[path]; !existed || path == headPath {
			written += size
		}
	}
	if written > 16*1024 {
		t.Fatalf("single change rewrote image: bytes=%d", written)
	}
	for hash, raw := range oldPageBytes {
		if !bytes.Equal(raw, catalogCacheBytes(t, st.catalogCacheRecordPath(hash))) {
			t.Fatal("immutable page changed")
		}
	}
	if got := catalogCacheRead(t, st, f); got.Revision != revision || len(got.Entries) != len(entries) {
		t.Fatal("bounded write lost data")
	}
}

// The file reader checks cancellation immediately before and after its read.
// Count those entry-point probes rather than add a production I/O test hook.
// Calls made by head validation and page writes are deliberately excluded.
type catalogCacheReadProbe struct {
	context.Context
	recordChecks   int
	completeChecks int
}

func (c *catalogCacheReadProbe) Err() error {
	pc, _, _, ok := runtime.Caller(1)
	if ok {
		name := runtime.FuncForPC(pc).Name()
		if strings.HasSuffix(name, ".readCatalogCacheRecord") {
			c.recordChecks++
		}
		if strings.HasSuffix(name, ".readCatalogCacheComplete") {
			c.completeChecks++
		}
	}
	return c.Context.Err()
}

func TestCatalogCachePartialStagingReadsOneTailNotCumulativePages(t *testing.T) {
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	ctx := &catalogCacheReadProbe{Context: context.Background()}
	var revision domain.ContentHash
	const pages = 40
	for i := range pages {
		entries := []domain.CatalogEntry{f.snapshot(t, i+1, 0, "page")}
		if i == 0 {
			entries = append([]domain.CatalogEntry{f.protocol(0)}, entries...)
		}
		before := ctx.recordChecks
		var err error
		revision, err = st.AppendCatalogPage(ctx, revision, f.repo, f.remote, f.page("baseline", 0, entries, fmt.Sprintf("cursor-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		want := 2
		if i == 0 {
			want = 0
		}
		if reads := ctx.recordChecks - before; reads != want {
			t.Fatalf("page %d: immutable-page read probes=%d want=%d", i, reads, want)
		}
	}
	beforeFinal := ctx.recordChecks
	_, err := st.AppendCatalogPage(ctx, revision, f.repo, f.remote, f.page("baseline", 0, []domain.CatalogEntry{}, ""))
	if err != nil {
		t.Fatal(err)
	}
	if ctx.recordChecks-beforeFinal != 2*pages {
		t.Fatalf("final did not reread all staged pages: probes=%d", ctx.recordChecks-beforeFinal)
	}
	if reads := ctx.recordChecks / 2; reads != 2*pages-1 {
		t.Fatalf("bootstrap reread is not linear: %d page reads", reads)
	}
	t.Logf("%d partial pages plus final: %d immutable page reads (one predecessor per partial, one full final replay)", pages, ctx.recordChecks/2)
}

func TestCatalogCachePendingHeadWritesScaleLinearly(t *testing.T) {
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	// Existing complete metadata must not be rewritten or read while staging.
	revision := catalogCacheAppend(t, st, f, "", f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, ""))
	before := catalogCacheHeadFor(t, st, f)
	descriptorPath := st.catalogCacheCompletePath(before.Completed)
	descriptorBytes := catalogCacheBytes(t, descriptorPath)
	headPath, _ := st.catalogCachePath(f.repo, f.remote)
	ctx := &catalogCacheReadProbe{Context: context.Background()}
	var total, maxHead, total500, max500 int64
	for i := 0; i < 1000; i++ {
		entries := []domain.CatalogEntry{f.snapshot(t, i+1, 0, "small entry")}
		if i == 0 {
			entries = append([]domain.CatalogEntry{f.protocol(0)}, entries...)
		}
		var err error
		revision, err = st.AppendCatalogPage(ctx, revision, f.repo, f.remote, f.page("baseline", 0, entries, fmt.Sprintf("page-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		stat, err := os.Stat(headPath)
		if err != nil {
			t.Fatal(err)
		}
		total += stat.Size()
		maxHead = max(maxHead, stat.Size())
		if i == 499 {
			total500, max500 = total, maxHead
		}
	}
	if total500 == 0 || total*10 > total500*22 {
		t.Fatalf("quadratic cumulative head writes: 500=%d 1000=%d", total500, total)
	}
	// Only bounded uint64 decimal widths can grow; no array may enter the head.
	if maxHead > 1024 || maxHead-max500 > 4 {
		t.Fatalf("unbounded head: max500=%d max1000=%d", max500, maxHead)
	}
	if ctx.completeChecks != 0 || ctx.recordChecks != 2*999 {
		t.Fatalf("partial reads: descriptor checks=%d page checks=%d", ctx.completeChecks, ctx.recordChecks)
	}
	if !bytes.Equal(descriptorBytes, catalogCacheBytes(t, descriptorPath)) {
		t.Fatal("partial staging rewrote completed descriptor")
	}
	after := catalogCacheHeadFor(t, st, f)
	if after.Completed != before.Completed || !sameCatalogCheckpoint(after.Checkpoint, before.Checkpoint) || after.PendingCount != 1000 {
		t.Fatal("partial staging changed complete state")
	}
	got := catalogCacheRead(t, st, f)
	if len(got.Pending) != 1000 || len(got.Entries) != 1 {
		t.Fatal("long linked run did not survive read/restart")
	}
	catalogCacheAppend(t, st, f, revision, f.page("baseline", 0, []domain.CatalogEntry{}, ""))
	if got := catalogCacheRead(t, st, f); len(got.Pending) != 0 || len(got.Entries) != 1001 {
		t.Fatal("long linked run did not promote")
	}
	t.Logf("partial_pages=500 cumulative_head_bytes=%d max_head_bytes=%d", total500, max500)
	t.Logf("partial_pages=1000 cumulative_head_bytes=%d max_head_bytes=%d ratio=%.4f", total, maxHead, float64(total)/float64(total500))
}

func TestCatalogCacheDescriptorAndCheckpointMustAgreeOnFullRead(t *testing.T) {
	f := catalogCacheTestFixture()
	for _, fault := range []string{"scope", "format", "threshold", "checkpoint", "missing page", "reordered completed pages"} {
		t.Run(fault, func(t *testing.T) {
			st := NewFileStore(t.TempDir())
			revision := catalogCacheAppend(t, st, f, "", f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, "initial"))
			revision = catalogCacheAppend(t, st, f, revision, f.page("baseline", 0, []domain.CatalogEntry{f.snapshot(t, 1, 0, "initial")}, ""))
			head := catalogCacheHeadFor(t, st, f)
			complete := catalogCacheCompleteFor(t, st, head)
			switch fault {
			case "scope":
				complete.Remote += "/wrong"
			case "format":
				complete.Format = "wrong"
			case "threshold":
				complete.CompactAfter = 1
			case "checkpoint":
				complete.Checkpoint.Sequence++
			case "missing page":
				complete.Pages[0] = domain.HashContent([]byte("missing complete page"))
			case "reordered completed pages":
				complete.Pages[0], complete.Pages[1] = complete.Pages[1], complete.Pages[0]
			}
			raw := catalogCacheJSON(t, complete)
			head.Completed = domain.HashContent(raw)
			catalogCacheWrite(t, st.catalogCacheCompletePath(head.Completed), raw)
			catalogCacheSaveHead(t, st, f, head)
			catalogCacheAssertCorrupt(t, st, f, revision, f.page("delta", 1, []domain.CatalogEntry{}, ""))
		})
	}
}

func TestCatalogCachePartialStagingDoesNotReadOrRepairCompleteDescriptor(t *testing.T) {
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	revision := catalogCacheAppend(t, st, f, "", f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, ""))
	head := catalogCacheHeadFor(t, st, f)
	path := st.catalogCacheCompletePath(head.Completed)
	corrupt := []byte("damaged completed descriptor")
	catalogCacheWrite(t, path, corrupt)
	revision = catalogCacheAppend(t, st, f, revision, f.page("delta", 1, []domain.CatalogEntry{f.snapshot(t, 1, 1, "pending")}, "pending"))
	if next := catalogCacheHeadFor(t, st, f); next.Completed != head.Completed || !sameCatalogCheckpoint(next.Checkpoint, head.Checkpoint) {
		t.Fatal("partial acknowledged a corrupted complete image")
	}
	catalogCacheAssertCorrupt(t, st, f, revision, f.page("delta", 1, []domain.CatalogEntry{}, ""))
	if !bytes.Equal(corrupt, catalogCacheBytes(t, path)) {
		t.Fatal("descriptor repaired")
	}
}

func TestCatalogCachePartialNeverAcknowledgesUndetectedOldCorruption(t *testing.T) {
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	revision := catalogCacheAppend(t, st, f, "", f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, ""))
	complete := catalogCacheHeadFor(t, st, f)
	revision = catalogCacheAppend(t, st, f, revision, f.page("delta", 1, []domain.CatalogEntry{f.snapshot(t, 1, 1, "first")}, "one"))
	revision = catalogCacheAppend(t, st, f, revision, f.page("delta", 1, []domain.CatalogEntry{f.snapshot(t, 2, 1, "second")}, "two"))
	head := catalogCacheHeadFor(t, st, f)
	corruptPath := st.catalogCacheRecordPath(catalogCachePendingFor(t, st, head)[0])
	catalogCacheWrite(t, corruptPath, []byte("corrupt older received page"))
	revision = catalogCacheAppend(t, st, f, revision, f.page("delta", 1, []domain.CatalogEntry{f.snapshot(t, 3, 1, "third")}, "three"))
	if got := catalogCacheHeadFor(t, st, f); !sameCatalogCheckpoint(got.Checkpoint, complete.Checkpoint) || got.Completed != complete.Completed {
		t.Fatal("partial staging acknowledged corrupt data")
	}
	catalogCacheAssertCorrupt(t, st, f, revision, f.page("delta", 1, []domain.CatalogEntry{}, ""))
	if !bytes.Equal(catalogCacheBytes(t, corruptPath), []byte("corrupt older received page")) {
		t.Fatal("corruption repaired")
	}
}

func TestCatalogCacheCompactionPreservesImageAndRetainsOldPages(t *testing.T) {
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	revision := catalogCacheAppend(t, st, f, "", f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, ""))
	var retained []domain.ContentHash
	for i := int64(1); i < 128; i++ {
		head := catalogCacheHeadFor(t, st, f)
		retained = append(retained, catalogCacheCompleteFor(t, st, head).Pages...)
		revision = catalogCacheAppend(t, st, f, revision, f.page("delta", i, []domain.CatalogEntry{f.snapshot(t, 1, i, fmt.Sprint(i))}, ""))
	}
	head := catalogCacheCompleteFor(t, st, catalogCacheHeadFor(t, st, f))
	if len(head.Image) != 1 || len(head.Pages) != 0 || head.CompactAfter != 128 || !sameCatalogCheckpoint(head.Checkpoint, head.ImageCheckpoint) {
		t.Fatalf("compacted head: %+v", head)
	}
	got := catalogCacheRead(t, st, f)
	if got.Revision != revision || !reflect.DeepEqual(got.Entries, []domain.CatalogEntry{f.protocol(0), f.snapshot(t, 1, 127, "127")}) {
		t.Fatal("compaction lost latest update")
	}
	for _, hash := range retained {
		if _, err := os.Stat(st.catalogCacheRecordPath(hash)); err != nil {
			t.Fatal("historical page removed")
		}
	}
	// Compacted base and later deltas compose, including a tombstone.
	revision = catalogCacheAppend(t, st, f, revision, f.page("delta", 128, []domain.CatalogEntry{{Sequence: 128, Kind: "snapshot", Key: fmt.Sprintf("sha256:%064x", 1), Deleted: true}}, ""))
	if got := catalogCacheRead(t, st, f); got.Revision != revision || !reflect.DeepEqual(got.Entries, []domain.CatalogEntry{f.protocol(0)}) {
		t.Fatal("delta after compacted base failed")
	}
	catalogCacheWrite(t, st.catalogCacheRecordPath(head.Image[0]), []byte("corrupt compacted image"))
	catalogCacheAssertCorrupt(t, st, f, revision, f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, ""))
}

func TestCatalogCacheCompactionThresholdGrowsWithLiveImage(t *testing.T) {
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	head := catalogCacheHead{catalogCacheHeadPayload: catalogCacheHeadPayload{Format: catalogCacheHeadFormat, Version: 1, RepoID: f.repo, Remote: f.remote}}
	// Seed a valid interrupted baseline without timing 128 fsyncs of the head.
	// Ref metadata exercises the same live-page threshold without repeatedly
	// recomputing thousands of unrelated snapshot state hashes under -race.
	entries := []domain.CatalogEntry{f.protocol(0)}
	for i := 1; i < 129*catalogCacheImageSize; i++ {
		name := fmt.Sprintf("branch-%06d", i)
		ref := domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: name, Target: fmt.Sprintf("sha256:%064x", i)}
		entries = append(entries, domain.CatalogEntry{Kind: "ref", Key: string(catalogCacheJSON(t, []string{"branch", name})), Value: catalogCacheJSON(t, ref)})
	}
	for start := 0; start < len(entries)-catalogCacheImageSize; start += catalogCacheImageSize {
		page := f.page("baseline", 0, entries[start:start+catalogCacheImageSize], fmt.Sprint(start))
		record := catalogCacheRecord{Format: catalogCachePageFormat, Version: 1, RepoID: f.repo, Remote: f.remote, Previous: head.PendingTail, Index: head.PendingCount + 1, Page: &page}
		raw := catalogCacheJSON(t, record)
		hash := domain.HashContent(raw)
		catalogCacheWrite(t, st.catalogCacheRecordPath(hash), raw)
		head.PendingTail = hash
		head.PendingCount++
		head.Generation++
	}
	catalogCacheSaveHead(t, st, f, head)
	head = catalogCacheHeadFor(t, st, f)
	revision := catalogCacheAppend(t, st, f, head.Revision, f.page("baseline", 0, entries[len(entries)-catalogCacheImageSize:], ""))
	compacted := catalogCacheCompleteFor(t, st, catalogCacheHeadFor(t, st, f))
	if len(compacted.Image) != 129 || compacted.CompactAfter != 258 {
		t.Fatalf("adaptive threshold: image=%d threshold=%d", len(compacted.Image), compacted.CompactAfter)
	}
	updated := entries[1]
	updated.Sequence = 1
	revision = catalogCacheAppend(t, st, f, revision, f.page("delta", 1, []domain.CatalogEntry{updated}, ""))
	next := catalogCacheCompleteFor(t, st, catalogCacheHeadFor(t, st, f))
	if len(next.Pages) != 1 || !reflect.DeepEqual(next.Image, compacted.Image) || next.CompactAfter != 258 {
		t.Fatal("immediate repeated compaction")
	}
	got := catalogCacheRead(t, st, f)
	if got.Revision != revision || len(got.Entries) != len(entries) || !sort.SliceIsSorted(got.Entries, func(i, j int) bool { return catalogCacheEntryLess(got.Entries[i], got.Entries[j]) }) {
		t.Fatal("large image corrupted")
	}
	// A much smaller fresh baseline must not inherit a huge old log threshold.
	catalogCacheAppend(t, st, f, revision, f.page("baseline", 0, []domain.CatalogEntry{f.protocol(0)}, ""))
	replaced := catalogCacheCompleteFor(t, st, catalogCacheHeadFor(t, st, f))
	if replaced.ImageCheckpoint != nil || len(replaced.Image) != 0 || len(replaced.Pages) != 1 || replaced.CompactAfter != 128 {
		t.Fatal("baseline retained compacted image or its old threshold")
	}
	if got := catalogCacheRead(t, st, f); !reflect.DeepEqual(got.Entries, []domain.CatalogEntry{f.protocol(0)}) {
		t.Fatal("baseline did not replace compacted image")
	}
}
