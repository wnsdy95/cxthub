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
	"slices"
	"sort"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func installCatalogImage(t *testing.T, st *FileStore, f catalogCacheFixture, expected domain.ContentHash, cp domain.CatalogCheckpoint, entries []domain.CatalogEntry) domain.ContentHash {
	t.Helper()
	before := catalogCacheJSON(t, entries)
	revision, err := st.InstallCatalogImage(context.Background(), expected, f.repo, f.remote, cp, entries)
	if err != nil || revision == expected || domain.ValidateContentHash(revision) != nil {
		t.Fatalf("install revision=%s err=%v", revision, err)
	}
	if !bytes.Equal(before, catalogCacheJSON(t, entries)) {
		t.Fatal("installer mutated caller entries")
	}
	return revision
}

func catalogImageSeedPending(t *testing.T, st *FileStore, f catalogCacheFixture) domain.ContentHash {
	t.Helper()
	r := catalogCacheAppend(t, st, f, "", f.page("baseline", 10, []domain.CatalogEntry{f.protocol(0)}, "baseline"))
	r = catalogCacheAppend(t, st, f, r, f.page("baseline", 10, []domain.CatalogEntry{f.snapshot(t, 1, 0, "old")}, ""))
	r = catalogCacheAppend(t, st, f, r, f.page("delta", 11, []domain.CatalogEntry{f.snapshot(t, 1, 11, "pending")}, "pending-1"))
	return catalogCacheAppend(t, st, f, r, f.page("delta", 11, []domain.CatalogEntry{f.snapshot(t, 2, 11, "pending-2")}, "pending-2"))
}

func TestInstallCatalogImageReplacementAndEpochReset(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset=%t", reset), func(t *testing.T) {
			f := catalogCacheTestFixture()
			st := NewFileStore(t.TempDir())
			revision := catalogImageSeedPending(t, st, f)
			oldHead := catalogCacheHeadFor(t, st, f)
			oldComplete := catalogCacheCompleteFor(t, st, oldHead)
			retained := map[string][]byte{st.catalogCacheCompletePath(oldHead.Completed): catalogCacheJSON(t, oldComplete)}
			for _, hash := range append(oldComplete.Pages, catalogCachePendingFor(t, st, oldHead)...) {
				path := st.catalogCacheRecordPath(hash)
				retained[path] = catalogCacheBytes(t, path)
			}
			cp := *f.page("baseline", 12, nil, "").Checkpoint
			if reset {
				cp.Epoch, cp.Sequence = "00000000-0000-0000-0000-000000000002", 0
			}
			// An image is not a wire page: more than MaxCatalogLimit entries,
			// deliberately unordered, must be split into <=256-entry records.
			entries := []domain.CatalogEntry{}
			for i := 1028; i >= 3; i-- {
				entries = append(entries, f.snapshot(t, i, cp.Sequence, "replacement"))
			}
			entries = append(entries, f.protocol(0))
			revision = installCatalogImage(t, st, f, revision, cp, entries)
			want := slices.Clone(entries)
			sort.Slice(want, func(i, j int) bool { return catalogCacheEntryLess(want[i], want[j]) })
			got := catalogCacheRead(t, st, f)
			if got.Revision != revision || !sameCatalogCheckpoint(got.Checkpoint, &cp) || len(got.Pending) != 0 || !reflect.DeepEqual(got.Entries, want) {
				t.Fatal("replacement merged old/pending entries or lost checkpoint")
			}
			head := catalogCacheHeadFor(t, st, f)
			complete := catalogCacheCompleteFor(t, st, head)
			if head.Generation != oldHead.Generation+1 || head.PendingTail != "" || head.PendingCount != 0 || len(complete.Pages) != 0 || !sameCatalogCheckpoint(complete.ImageCheckpoint, &cp) || len(complete.Image) != 5 {
				t.Fatal("replacement did not publish one image generation")
			}
			count := 0
			for _, hash := range complete.Image {
				record, err := st.readCatalogCacheRecord(context.Background(), f.repo, f.remote, hash)
				if err != nil || record.Format != catalogCacheImageFormat || len(record.Entries) > 256 {
					t.Fatalf("image page: %+v %v", record, err)
				}
				count += len(record.Entries)
			}
			if count != len(entries) {
				t.Fatal("image entry count differs")
			}
			for path, raw := range retained {
				if !bytes.Equal(raw, catalogCacheBytes(t, path)) {
					t.Fatal("old immutable record was changed or removed")
				}
			}
			// The new image is also a valid base for the existing delta protocol.
			f.epoch = cp.Epoch
			entry := f.snapshot(t, 3, cp.Sequence+1, "delta after image")
			catalogCacheAppend(t, st, f, revision, f.page("delta", cp.Sequence+1, []domain.CatalogEntry{entry}, ""))
			want[1] = entry
			if !reflect.DeepEqual(catalogCacheRead(t, st, f).Entries, want) {
				t.Fatal("ordinary delta after replacement failed")
			}
		})
	}
}

func TestInstallCatalogImageValidationPreservesCompleteAndPending(t *testing.T) {
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	revision := catalogImageSeedPending(t, st, f)
	path, _ := st.catalogCachePath(f.repo, f.remote)
	before := catalogCacheBytes(t, path)
	files := catalogCacheFiles(t, st)
	for _, name := range []string{"foreign checkpoint", "checkpoint version", "epoch", "negative checkpoint", "same epoch rollback", "future entry", "negative entry", "duplicate", "alias", "deleted", "missing protocol", "foreign entry", "bad value"} {
		t.Run(name, func(t *testing.T) {
			cp := *f.page("baseline", 12, nil, "").Checkpoint
			entries := []domain.CatalogEntry{f.protocol(0), f.snapshot(t, 3, 12, "new")}
			switch name {
			case "foreign checkpoint":
				cp.RepoID = domain.HashContent([]byte("foreign"))
			case "checkpoint version":
				cp.Version++
			case "epoch":
				cp.Epoch = "invalid"
			case "negative checkpoint":
				cp.Sequence = -1
			case "same epoch rollback":
				cp.Sequence, entries[1].Sequence = 0, 0
			case "future entry":
				entries[1].Sequence = 13
			case "negative entry":
				entries[1].Sequence = -1
			case "duplicate":
				entries = append(entries, entries[1])
			case "alias":
				ref := domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "main", Target: entries[1].Key}
				for _, key := range []string{`["branch", "main"]`, `["branch","ma\u0069n"]`} {
					entries = append(entries, domain.CatalogEntry{Kind: "ref", Key: key, Value: catalogCacheJSON(t, ref)})
				}
			case "deleted":
				entries[1].Deleted, entries[1].Value = true, nil
			case "missing protocol":
				entries = entries[1:]
			case "foreign entry":
				other := f
				other.repo = domain.HashContent([]byte("other repo"))
				entries[1] = other.snapshot(t, 3, 0, "wrong scope")
			case "bad value":
				entries[1].Value = json.RawMessage(`null`)
			}
			got, err := st.InstallCatalogImage(context.Background(), revision, f.repo, f.remote, cp, entries)
			if got != "" || !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatalf("accepted invalid image: %s %v", got, err)
			}
			if !bytes.Equal(before, catalogCacheBytes(t, path)) || !reflect.DeepEqual(files, catalogCacheFiles(t, st)) {
				t.Fatal("validation failure wrote catalog state")
			}
		})
	}
	// A single noncanonical ref-key spelling is valid and remains unchanged.
	ref := domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "main", Target: f.snapshot(t, 3, 0, "").Key}
	r := domain.CatalogEntry{Kind: "ref", Key: `["branch", "ma\u0069n"]`, Value: catalogCacheJSON(t, ref)}
	installCatalogImage(t, st, f, revision, *f.page("baseline", 12, nil, "").Checkpoint, []domain.CatalogEntry{r, f.protocol(0)})
	if got := catalogCacheRead(t, st, f); got.Entries[1].Key != r.Key {
		t.Fatal("installer rewrote original ref metadata")
	}
}

func TestInstallCatalogImageRejectsOldCorruption(t *testing.T) {
	for _, location := range []string{"head", "descriptor", "completed page", "pending first page", "image", "missing image"} {
		t.Run(location, func(t *testing.T) {
			f := catalogCacheTestFixture()
			st := NewFileStore(t.TempDir())
			revision := catalogImageSeedPending(t, st, f)
			cp := *f.page("baseline", 12, nil, "").Checkpoint
			if location == "image" || location == "missing image" {
				revision = installCatalogImage(t, st, f, revision, cp, []domain.CatalogEntry{f.protocol(0)})
			}
			head := catalogCacheHeadFor(t, st, f)
			complete := catalogCacheCompleteFor(t, st, head)
			headPath, _ := st.catalogCachePath(f.repo, f.remote)
			corrupt := headPath
			switch location {
			case "descriptor":
				corrupt = st.catalogCacheCompletePath(head.Completed)
			case "completed page":
				corrupt = st.catalogCacheRecordPath(complete.Pages[0])
			case "pending first page":
				corrupt = st.catalogCacheRecordPath(catalogCachePendingFor(t, st, head)[0])
			case "image", "missing image":
				corrupt = st.catalogCacheRecordPath(complete.Image[0])
			}
			if location == "missing image" {
				if err := os.Remove(corrupt); err != nil {
					t.Fatal(err)
				}
			} else {
				catalogCacheWrite(t, corrupt, []byte(`{"corrupt":true}`))
			}
			before := catalogCacheBytes(t, headPath)
			files := catalogCacheFiles(t, st)
			// New epoch and fresh valid image do not authorize repairing old data.
			cp.Epoch, cp.Sequence = "00000000-0000-0000-0000-000000000002", 0
			got, err := st.InstallCatalogImage(context.Background(), revision, f.repo, f.remote, cp, []domain.CatalogEntry{f.protocol(0)})
			if got != "" || !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatalf("corrupt old cache accepted: %s %v", got, err)
			}
			if !bytes.Equal(before, catalogCacheBytes(t, headPath)) || !reflect.DeepEqual(files, catalogCacheFiles(t, st)) {
				t.Fatal("old corruption silently repaired")
			}
		})
	}
}

func TestInstallCatalogImageCASAndGeneration(t *testing.T) {
	for _, seeded := range []bool{false, true} {
		t.Run(fmt.Sprintf("seeded=%t", seeded), func(t *testing.T) {
			f := catalogCacheTestFixture()
			st := NewFileStore(t.TempDir())
			var expected domain.ContentHash
			if seeded {
				expected = catalogImageSeedPending(t, st, f)
			}
			cp := *f.page("baseline", 12, nil, "").Checkpoint
			type result struct {
				revision domain.ContentHash
				entry    domain.CatalogEntry
				err      error
			}
			start := make(chan struct{})
			results := make(chan result, 8)
			for i := range 8 {
				entry := f.snapshot(t, 3, 12, fmt.Sprintf("writer %d", i))
				go func() {
					<-start
					r, err := NewFileStore(st.repoRoot).InstallCatalogImage(context.Background(), expected, f.repo, f.remote, cp, []domain.CatalogEntry{f.protocol(0), entry})
					results <- result{r, entry, err}
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
				} else if errors.Is(r.err, domain.ErrSyncConflict) && r.revision == "" {
					conflicts++
				} else {
					t.Errorf("unexpected loser: %+v", r)
				}
			}
			if wins != 1 || conflicts != 7 {
				t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
			}
			got := catalogCacheRead(t, st, f)
			entries := []domain.CatalogEntry{f.protocol(0), winner.entry}
			if got.Revision != winner.revision || !reflect.DeepEqual(got.Entries, entries) || len(got.Pending) != 0 {
				t.Fatal("loser changed winner image")
			}
			head := catalogCacheHeadFor(t, st, f)
			next := installCatalogImage(t, st, f, winner.revision, cp, entries)
			if next == expected || catalogCacheHeadFor(t, st, f).Generation != head.Generation+1 {
				t.Fatal("identical image reused CAS generation")
			}
			path, _ := st.catalogCachePath(f.repo, f.remote)
			before := catalogCacheBytes(t, path)
			if _, err := st.InstallCatalogImage(context.Background(), winner.revision, f.repo, f.remote, cp, entries); !errors.Is(err, domain.ErrSyncConflict) || !bytes.Equal(before, catalogCacheBytes(t, path)) {
				t.Fatalf("stale CAS changed image: %v", err)
			}
			head = catalogCacheHeadFor(t, st, f)
			head.Generation = ^uint64(0)
			catalogCacheSaveHead(t, st, f, head)
			head = catalogCacheHeadFor(t, st, f)
			before = catalogCacheBytes(t, path)
			if _, err := st.InstallCatalogImage(context.Background(), head.Revision, f.repo, f.remote, cp, entries); !errors.Is(err, domain.ErrHashMismatch) || !bytes.Equal(before, catalogCacheBytes(t, path)) {
				t.Fatalf("generation overflow changed image: %v", err)
			}
		})
	}
}

func TestInstallCatalogImageCancellationBeforePublication(t *testing.T) {
	for _, stage := range []string{"before call", "first image page", "complete descriptor"} {
		t.Run(stage, func(t *testing.T) {
			f := catalogCacheTestFixture()
			st := NewFileStore(t.TempDir())
			expected := catalogImageSeedPending(t, st, f)
			before := catalogCacheRead(t, st, f)
			headPath, _ := st.catalogCachePath(f.repo, f.remote)
			beforeHead := catalogCacheBytes(t, headPath)
			oldHead := catalogCacheHeadFor(t, st, f)
			cp := *f.page("baseline", 12, nil, "").Checkpoint
			entries := []domain.CatalogEntry{f.protocol(0)}
			for i := 3; i < 516; i++ {
				entries = append(entries, f.snapshot(t, i, 12, "new"))
			}
			first := catalogCacheRecord{Format: catalogCacheImageFormat, Version: 1, RepoID: f.repo, Remote: f.remote, Entries: entries[:256]}
			firstPath := st.catalogCacheRecordPath(domain.HashContent(catalogCacheJSON(t, first)))
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			fired := stage == "before call"
			if fired {
				cancel()
			}
			ctx := metadataCheckpointCheckContext{Context: base, check: func() {
				if fired || !fileExists(firstPath) {
					return
				}
				if stage == "complete descriptor" {
					files, err := os.ReadDir(filepath.Dir(st.catalogCacheCompletePath(oldHead.Completed)))
					if err != nil {
						t.Fatal(err)
					}
					if len(files) < 2 {
						return
					}
				}
				fired = true
				if !reflect.DeepEqual(before, catalogCacheRead(t, st, f)) {
					t.Fatal("partial replacement became visible")
				}
				cancel()
			}}
			got, err := st.InstallCatalogImage(ctx, expected, f.repo, f.remote, cp, entries)
			if !fired || got != "" || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation revision=%s err=%v fired=%t", got, err, fired)
			}
			if !bytes.Equal(beforeHead, catalogCacheBytes(t, headPath)) || !reflect.DeepEqual(before, catalogCacheRead(t, st, f)) {
				t.Fatal("cancellation changed complete or pending cache")
			}
			installCatalogImage(t, st, f, expected, cp, entries)
		})
	}
}

func TestInstallCatalogImageDoesNotApplyMetadata(t *testing.T) {
	ctx := context.Background()
	f := catalogCacheTestFixture()
	st := NewFileStore(t.TempDir())
	entry := f.snapshot(t, 1, 0, "local original")
	var snapshot domain.Snapshot
	if err := json.Unmarshal(entry.Value, &snapshot); err != nil {
		t.Fatal(err)
	}
	// The catalog fixture advertises an unfetched attachment. Seed a valid
	// local snapshot without it; acquisition must not attach that remote memory.
	snapshot.MemoryHash = ""
	if err := st.PutSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	ref := domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "main", Target: snapshot.ID}
	if err := st.PutRef(ctx, ref); err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"", "main"} {
		observation := outbound.RemoteObservation{Version: 1, RepoID: f.repo, Remote: f.remote, Branch: branch, Refs: []domain.Ref{ref}, Snapshots: []domain.Snapshot{snapshot}}
		if err := st.CompareAndSwapRemoteObservation(ctx, "", observation); err != nil {
			t.Fatal(err)
		}
	}
	catalogCacheWrite(t, filepath.Join(st.storeDir(), "working-selection.json"), []byte("preserved selection fixture"))
	workingFiles := func() map[string][]byte {
		out := map[string][]byte{}
		err := filepath.WalkDir(st.storeDir(), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if path == filepath.Join(st.storeDir(), catalogCacheNamespace) || path == filepath.Join(st.storeDir(), "locks") {
					return filepath.SkipDir
				}
			} else {
				out[path] = catalogCacheBytes(t, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := workingFiles()
	newRef := ref
	newRef.Target = f.snapshot(t, 2, 0, "").Key
	entries := []domain.CatalogEntry{f.protocol(0), {Kind: "ref", Key: `["branch","main"]`, Value: catalogCacheJSON(t, newRef)}, f.snapshot(t, 1, 12, "remote changed"), f.snapshot(t, 2, 12, "remote only")}
	cp := *f.page("baseline", 12, nil, "").Checkpoint
	revision := installCatalogImage(t, st, f, "", cp, entries)
	if !reflect.DeepEqual(before, workingFiles()) {
		t.Fatal("metadata acquisition changed original refs, observations, snapshot or selection bytes")
	}
	for _, scope := range []catalogCacheFixture{{repo: f.repo, remote: f.remote + "/other", epoch: f.epoch}, {repo: domain.HashContent([]byte("other repo")), remote: f.remote, epoch: f.epoch}} {
		otherCP := *scope.page("baseline", 0, nil, "").Checkpoint
		if _, err := st.InstallCatalogImage(ctx, revision, scope.repo, scope.remote, otherCP, []domain.CatalogEntry{scope.protocol(0)}); !errors.Is(err, domain.ErrSyncConflict) {
			t.Fatalf("foreign revision accepted: %v", err)
		}
		if got := catalogCacheRead(t, st, scope); got.Checkpoint != nil || got.Revision != "" {
			t.Fatal("installation leaked into another scope")
		}
	}
}
