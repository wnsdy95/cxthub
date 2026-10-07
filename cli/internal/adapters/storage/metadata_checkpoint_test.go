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
	"sort"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func metadataCheckpointFixture() outbound.MetadataCheckpoint {
	hash := func(label string) domain.ContentHash { return domain.HashContent([]byte(label)) }
	repo := hash("metadata checkpoint repository")
	return outbound.MetadataCheckpoint{Version: 1, RepoID: repo, Remote: "https://checkpoint.example.test", Snapshots: []domain.Snapshot{{
		ID: hash("fetched document"), RepoID: repo, DocHash: hash("fetched document"),
		Branch: "main", Parents: []domain.ContentHash{hash("parent"), hash("second parent")},
		MemoryHash: hash("memory"), ClaudeSettings: hash("claude settings"), AgentsSettings: hash("agents settings"), CodexSettings: hash("codex settings"),
		Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, Message: "Fetched metadata",
		Author:    domain.TeamIdentity{Name: "Synthetic Author", Email: "author@example.test", Team: "test"},
		CreatedAt: time.Date(2026, time.October, 1, 2, 3, 4, 0, time.UTC),
		Grafted:   true, GraftParents: []domain.ContentHash{hash("graft parent"), hash("second graft")}, GraftSeq: 2,
		SessionID: "synthetic-session", Models: []string{"synthetic-model"}, CompactionCount: 1,
	}}}
}

func metadataCheckpointJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func metadataCheckpointWrite(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func metadataCheckpointReadBytes(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func metadataCheckpointHeadFor(t *testing.T, st *FileStore, value outbound.MetadataCheckpoint) metadataCheckpointHead {
	t.Helper()
	head, err := st.readMetadataCheckpointHead(context.Background(), value.RepoID, value.Remote)
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func metadataCheckpointSaveHead(t *testing.T, st *FileStore, head metadataCheckpointHead) metadataCheckpointHead {
	t.Helper()
	if head.Generation == 0 {
		head.Generation = 1
	}
	var err error
	head.Revision, err = metadataCheckpointHeadRevision(head.metadataCheckpointHeadPayload)
	if err != nil {
		t.Fatal(err)
	}
	path, err := st.metadataCheckpointPath(head.RepoID, head.Remote)
	if err != nil {
		t.Fatal(err)
	}
	metadataCheckpointWrite(t, path, metadataCheckpointJSON(t, head))
	return head
}

func metadataCheckpointSeedPage(t *testing.T, st *FileStore, value outbound.MetadataCheckpoint) domain.ContentHash {
	t.Helper()
	raw, err := encodeMetadataCheckpointPage(context.Background(), value.RepoID, value.Remote, value.Snapshots, nil)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(raw)
	metadataCheckpointWrite(t, st.metadataCheckpointPagePath(hash), raw)
	return hash
}

func metadataCheckpointAppend(t *testing.T, st *FileStore, expected domain.ContentHash, value outbound.MetadataCheckpoint) domain.ContentHash {
	t.Helper()
	revision, err := st.AppendMetadataCheckpoint(context.Background(), expected, value.RepoID, value.Remote, value.Snapshots, nil)
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

func TestMetadataCheckpointAppendRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := NewFileStore(t.TempDir())
	v := metadataCheckpointFixture()
	missing, err := st.ReadMetadataCheckpoint(ctx, v.RepoID, v.Remote)
	if err != nil || !reflect.DeepEqual(missing, outbound.MetadataCheckpoint{Version: 1, RepoID: v.RepoID, Remote: v.Remote}) {
		t.Fatalf("absent=%+v %v", missing, err)
	}
	if _, err := os.Stat(st.storeDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created state: %v", err)
	}
	second := v.Snapshots[0]
	second.ID = domain.HashContent([]byte("second document"))
	second.DocHash = second.ID
	v.Snapshots = append(v.Snapshots, second)
	sort.Slice(v.Snapshots, func(i, j int) bool { return v.Snapshots[i].ID > v.Snapshots[j].ID })
	before := metadataCheckpointJSON(t, v)
	revision := metadataCheckpointAppend(t, st, "", v)
	if domain.ValidateContentHash(revision) != nil || !bytes.Equal(before, metadataCheckpointJSON(t, v)) {
		t.Fatal("invalid revision or mutated caller")
	}
	got, err := NewFileStore(st.repoRoot).ReadMetadataCheckpoint(ctx, v.RepoID, v.Remote)
	if err != nil || got.Revision != revision || len(got.Snapshots) != 2 || got.Snapshots[0].ID >= got.Snapshots[1].ID {
		t.Fatalf("roundtrip=%+v %v", got, err)
	}
	want := append([]domain.Snapshot(nil), v.Snapshots...)
	sort.Slice(want, func(i, j int) bool { return want[i].ID < want[j].ID })
	if !reflect.DeepEqual(got.Snapshots, want) {
		t.Fatal("snapshot fields changed")
	}
	head := metadataCheckpointHeadFor(t, st, v)
	if head.PageCount != 1 || head.CompactAfter != 128 || head.Revision != revision || head.Generation != 1 {
		t.Fatalf("head=%+v", head)
	}
	pagePath := st.metadataCheckpointPagePath(head.Pages[0])
	raw := metadataCheckpointReadBytes(t, pagePath)
	if domain.HashContent(raw) != head.Pages[0] {
		t.Fatal("page hash is not canonical payload checksum")
	}
	v.Snapshots[0], v.Snapshots[1] = v.Snapshots[1], v.Snapshots[0]
	revision = metadataCheckpointAppend(t, st, revision, v)
	head = metadataCheckpointHeadFor(t, st, v)
	if head.PageCount != 2 || head.Pages[0] != head.Pages[1] || !bytes.Equal(raw, metadataCheckpointReadBytes(t, pagePath)) {
		t.Fatal("reordered batch changed immutable page")
	}
	// Parent order and every other snapshot field are preserved in the page hash.
	v.Snapshots = v.Snapshots[:1]
	v.Snapshots[0].Parents = append([]domain.ContentHash(nil), v.Snapshots[0].Parents...)
	v.Snapshots[0].Parents[0], v.Snapshots[0].Parents[1] = v.Snapshots[0].Parents[1], v.Snapshots[0].Parents[0]
	v.Snapshots[0].MemoryHash = domain.HashContent([]byte("updated memory"))
	revision = metadataCheckpointAppend(t, st, revision, v)
	got, err = st.ReadMetadataCheckpoint(ctx, v.RepoID, v.Remote)
	if err != nil || got.Revision != revision || len(got.Snapshots) != 2 || !reflect.DeepEqual(got.Snapshots[0], v.Snapshots[0]) {
		t.Fatalf("latest override=%+v %v", got, err)
	}
	if _, err := st.AppendMetadataCheckpoint(ctx, "", v.RepoID, v.Remote, v.Snapshots, nil); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("stale append=%v", err)
	}
	if _, err := os.Stat(filepath.Join(st.storeDir(), "objects")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("applied objects created: %v", err)
	}
}

func TestMetadataCheckpointHeadCorruptionNeverRepaired(t *testing.T) {
	st := NewFileStore(t.TempDir())
	v := metadataCheckpointFixture()
	metadataCheckpointAppend(t, st, "", v)
	v.Snapshots[0].Message = "another page"
	metadataCheckpointAppend(t, st, metadataCheckpointHeadFor(t, st, v).Revision, v)
	head := metadataCheckpointHeadFor(t, st, v)
	path, _ := st.metadataCheckpointPath(v.RepoID, v.Remote)
	original := metadataCheckpointJSON(t, head)
	for name, mutate := range map[string]func(*metadataCheckpointHead){
		"format":     func(h *metadataCheckpointHead) { h.Format = "other" },
		"version":    func(h *metadataCheckpointHead) { h.Version++ },
		"repo":       func(h *metadataCheckpointHead) { h.RepoID = domain.HashContent([]byte("foreign repo")) },
		"remote":     func(h *metadataCheckpointHead) { h.Remote += "/other" },
		"count":      func(h *metadataCheckpointHead) { h.PageCount++ },
		"threshold":  func(h *metadataCheckpointHead) { h.CompactAfter++ },
		"order":      func(h *metadataCheckpointHead) { h.Pages[0], h.Pages[1] = h.Pages[1], h.Pages[0] },
		"revision":   func(h *metadataCheckpointHead) { h.Revision = "" },
		"generation": func(h *metadataCheckpointHead) { h.Generation++ },
	} {
		t.Run(name, func(t *testing.T) {
			var corrupt metadataCheckpointHead
			if err := json.Unmarshal(original, &corrupt); err != nil {
				t.Fatal(err)
			}
			mutate(&corrupt)
			metadataCheckpointAssertBadHead(t, st, v, path, metadataCheckpointJSON(t, corrupt))
		})
	}
	for name, raw := range map[string][]byte{
		"unknown":   bytes.Replace(original, []byte(`"version":1`), []byte(`"version":1,"unknown":0`), 1),
		"duplicate": bytes.Replace(original, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"trailing":  append(append([]byte(nil), original...), []byte(` {}`)...),
		"malformed": []byte(`{"version":`),
	} {
		t.Run(name, func(t *testing.T) { metadataCheckpointAssertBadHead(t, st, v, path, raw) })
	}
	for _, mutate := range []func(*metadataCheckpointHead){
		func(h *metadataCheckpointHead) { h.Format = "wrong" },
		func(h *metadataCheckpointHead) { h.Version = 2 },
		func(h *metadataCheckpointHead) { h.Generation = 0 },
		func(h *metadataCheckpointHead) { h.PageCount = 0 },
		func(h *metadataCheckpointHead) { h.CompactAfter = 127 },
		func(h *metadataCheckpointHead) { h.Pages = []domain.ContentHash{"invalid"}; h.PageCount = 1 },
	} {
		copy := head
		mutate(&copy)
		copy.Revision, _ = metadataCheckpointHeadRevision(copy.metadataCheckpointHeadPayload)
		metadataCheckpointAssertBadHead(t, st, v, path, metadataCheckpointJSON(t, copy))
	}
}

func metadataCheckpointAssertBadHead(t *testing.T, st *FileStore, v outbound.MetadataCheckpoint, path string, raw []byte) {
	t.Helper()
	metadataCheckpointWrite(t, path, raw)
	if got, err := st.ReadMetadataCheckpoint(context.Background(), v.RepoID, v.Remote); !errors.Is(err, domain.ErrHashMismatch) || !reflect.DeepEqual(got, outbound.MetadataCheckpoint{}) {
		t.Fatalf("corrupt head read=%+v %v", got, err)
	}
	if _, err := st.AppendMetadataCheckpoint(context.Background(), "", v.RepoID, v.Remote, v.Snapshots, nil); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("corrupt head append=%v", err)
	}
	if !bytes.Equal(raw, metadataCheckpointReadBytes(t, path)) {
		t.Fatal("head repaired")
	}
}

func TestMetadataCheckpointPageTamperAndHeadOnlyAppend(t *testing.T) {
	st := NewFileStore(t.TempDir())
	v := metadataCheckpointFixture()
	revision := metadataCheckpointAppend(t, st, "", v)
	head := metadataCheckpointHeadFor(t, st, v)
	path := st.metadataCheckpointPagePath(head.Pages[0])
	original := metadataCheckpointReadBytes(t, path)
	for _, field := range []string{"id", "repo_id", "doc_hash", "branch", "parents", "memory_hash", "claude_settings", "agents_settings", "codex_settings", "provider", "fidelity", "message", "author", "created_at", "grafted", "graft_parents", "graft_seq", "session_id", "models", "compaction_count"} {
		t.Run(field, func(t *testing.T) {
			var page map[string]json.RawMessage
			if err := json.Unmarshal(original, &page); err != nil {
				t.Fatal(err)
			}
			var snapshots []map[string]json.RawMessage
			if err := json.Unmarshal(page["snapshots"], &snapshots); err != nil {
				t.Fatal(err)
			}
			delete(snapshots[0], field)
			page["snapshots"] = metadataCheckpointJSON(t, snapshots)
			raw := metadataCheckpointJSON(t, page)
			metadataCheckpointWrite(t, path, raw)
			if _, err := st.ReadMetadataCheckpoint(context.Background(), v.RepoID, v.Remote); !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatalf("tampered old page read=%v", err)
			}
			if _, err := st.AppendMetadataCheckpoint(context.Background(), revision, v.RepoID, v.Remote, v.Snapshots, nil); !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatalf("existing corrupt page accepted: %v", err)
			}
			if !bytes.Equal(raw, metadataCheckpointReadBytes(t, path)) {
				t.Fatal("immutable page silently repaired")
			}
		})
	}
	// An ordinary append must not scan historical pages. Full reads still fail.
	v.Snapshots[0].Message = "new fetched batch"
	metadataCheckpointAppend(t, st, revision, v)
	if _, err := st.ReadMetadataCheckpoint(context.Background(), v.RepoID, v.Remote); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("old corruption hidden by newer page: %v", err)
	}
}

func TestMetadataCheckpointBatchAndPageValidation(t *testing.T) {
	other := domain.HashContent([]byte("different hash"))
	for name, mutate := range map[string]func(*outbound.MetadataCheckpoint){
		"nil":   func(v *outbound.MetadataCheckpoint) { v.Snapshots = nil },
		"empty": func(v *outbound.MetadataCheckpoint) { v.Snapshots = []domain.Snapshot{} },
		"too large": func(v *outbound.MetadataCheckpoint) {
			for len(v.Snapshots) < 257 {
				v.Snapshots = append(v.Snapshots, v.Snapshots[0])
			}
		},
		"duplicate":      func(v *outbound.MetadataCheckpoint) { v.Snapshots = append(v.Snapshots, v.Snapshots[0]) },
		"foreign":        func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].RepoID = other },
		"id":             func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].ID = "invalid" },
		"doc":            func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].DocHash = "invalid" },
		"identity":       func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].DocHash = other },
		"memory":         func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].MemoryHash = "invalid" },
		"claude":         func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].ClaudeSettings = "invalid" },
		"agents":         func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].AgentsSettings = "invalid" },
		"codex":          func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].CodexSettings = "invalid" },
		"parent":         func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].Parents = []domain.ContentHash{"invalid"} },
		"graft":          func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].GraftParents = []domain.ContentHash{"invalid"} },
		"graft sequence": func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].GraftSeq = domain.MaxGraftSeq + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			st := NewFileStore(t.TempDir())
			v := metadataCheckpointFixture()
			mutate(&v)
			if _, err := st.AppendMetadataCheckpoint(context.Background(), "", v.RepoID, v.Remote, v.Snapshots, nil); !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatalf("invalid append=%v", err)
			}
			if _, err := os.Stat(st.storeDir()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid batch created state")
			}
			raw := metadataCheckpointJSON(t, metadataCheckpointPage{metadataCheckpointPageFormat, 1, v.RepoID, v.Remote, v.Snapshots, nil})
			metadataCheckpointAssertInvalidPage(t, st, v, raw)
		})
	}
	for name, mutate := range map[string]func(*metadataCheckpointPage){
		"format":  func(p *metadataCheckpointPage) { p.Format = "other" },
		"version": func(p *metadataCheckpointPage) { p.Version = 2 },
		"repo":    func(p *metadataCheckpointPage) { p.RepoID = other },
		"remote":  func(p *metadataCheckpointPage) { p.Remote += "/other" },
		"unsorted": func(p *metadataCheckpointPage) {
			p.Snapshots = append(p.Snapshots, p.Snapshots[0])
			p.Snapshots[1].ID = other
			p.Snapshots[1].DocHash = other
			sort.Slice(p.Snapshots, func(i, j int) bool { return p.Snapshots[i].ID > p.Snapshots[j].ID })
		},
	} {
		t.Run(name, func(t *testing.T) {
			v := metadataCheckpointFixture()
			page := metadataCheckpointPage{metadataCheckpointPageFormat, 1, v.RepoID, v.Remote, v.Snapshots, nil}
			mutate(&page)
			metadataCheckpointAssertInvalidPage(t, NewFileStore(t.TempDir()), v, metadataCheckpointJSON(t, page))
		})
	}
	v := metadataCheckpointFixture()
	raw, err := encodeMetadataCheckpointPage(context.Background(), v.RepoID, v.Remote, v.Snapshots, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, corrupt := range map[string][]byte{
		"unknown":          bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"unknown":1`), 1),
		"unknown snapshot": bytes.Replace(raw, []byte(`"branch":"main"`), []byte(`"branch":"main","unknown":1`), 1),
		"duplicate key":    bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"trailing":         append(append([]byte(nil), raw...), []byte(` {}`)...),
	} {
		t.Run(name, func(t *testing.T) { metadataCheckpointAssertInvalidPage(t, NewFileStore(t.TempDir()), v, corrupt) })
	}
}

func metadataCheckpointAssertInvalidPage(t *testing.T, st *FileStore, v outbound.MetadataCheckpoint, raw []byte) {
	t.Helper()
	hash := domain.HashContent(raw)
	metadataCheckpointWrite(t, st.metadataCheckpointPagePath(hash), raw)
	head := metadataCheckpointHead{metadataCheckpointHeadPayload: metadataCheckpointHeadPayload{metadataCheckpointHeadFormat, 1, v.RepoID, v.Remote, []domain.ContentHash{hash}, 1, 128, 1}}
	metadataCheckpointSaveHead(t, st, head)
	if got, err := st.ReadMetadataCheckpoint(context.Background(), v.RepoID, v.Remote); !errors.Is(err, domain.ErrHashMismatch) || !reflect.DeepEqual(got, outbound.MetadataCheckpoint{}) {
		t.Fatalf("hash-valid malformed page=%+v %v", got, err)
	}
}

func TestMetadataCheckpointCompactionLatestOverridesAndRetainsPages(t *testing.T) {
	st := NewFileStore(t.TempDir())
	v := metadataCheckpointFixture()
	old := metadataCheckpointSeedPage(t, st, v)
	v.Snapshots[0].Message = "latest existing"
	latest := metadataCheckpointSeedPage(t, st, v)
	head := metadataCheckpointHeadFor(t, st, v)
	for range 64 {
		head.Pages = append(head.Pages, old, latest)
	}
	head.PageCount = len(head.Pages)
	head = metadataCheckpointSaveHead(t, st, head)
	v.Snapshots[0].MemoryHash = domain.HashContent([]byte("latest append memory"))
	revision := metadataCheckpointAppend(t, st, head.Revision, v)
	after := metadataCheckpointHeadFor(t, st, v)
	if after.PageCount != 1 || after.CompactAfter != 128 || after.Generation != head.Generation+1 || after.Revision == head.Revision {
		t.Fatalf("compacted head=%+v", after)
	}
	got, err := st.ReadMetadataCheckpoint(context.Background(), v.RepoID, v.Remote)
	if err != nil || got.Revision != revision || !reflect.DeepEqual(got.Snapshots, v.Snapshots) {
		t.Fatalf("compaction lost newest fields: %+v %v", got, err)
	}
	for _, hash := range []domain.ContentHash{old, latest} {
		if _, err := os.Stat(st.metadataCheckpointPagePath(hash)); err != nil {
			t.Fatal("unreferenced page was removed", err)
		}
	}
}

func TestMetadataCheckpointCompactionThresholdGrowsGeometrically(t *testing.T) {
	st := NewFileStore(t.TempDir())
	v := metadataCheckpointFixture()
	head := metadataCheckpointHeadFor(t, st, v)
	batch := func(start int) []domain.Snapshot {
		out := make([]domain.Snapshot, 256)
		for i := range out {
			id := fmt.Sprintf("sha256:%064x", start+i+1)
			out[i] = domain.Snapshot{ID: id, DocHash: id, RepoID: v.RepoID, Parents: []domain.ContentHash{}}
		}
		return out
	}
	// Seed sorted, disjoint canonical pages without timing 128 durable appends.
	for i := range 128 {
		v.Snapshots = batch(i * 256)
		head.Pages = append(head.Pages, metadataCheckpointSeedPage(t, st, v))
	}
	head.PageCount = len(head.Pages)
	head = metadataCheckpointSaveHead(t, st, head)
	v.Snapshots = batch(128 * 256)
	revision := metadataCheckpointAppend(t, st, head.Revision, v)
	compacted := metadataCheckpointHeadFor(t, st, v)
	if compacted.PageCount != 129 || compacted.CompactAfter != 258 {
		t.Fatalf("geometric threshold=%+v", compacted)
	}
	v.Snapshots = batch(129 * 256)[:1]
	revision = metadataCheckpointAppend(t, st, revision, v)
	next := metadataCheckpointHeadFor(t, st, v)
	if next.PageCount != 130 || next.CompactAfter != 258 || !reflect.DeepEqual(next.Pages[:129], compacted.Pages) {
		t.Fatal("next append immediately compacted again")
	}
	got, err := st.ReadMetadataCheckpoint(context.Background(), v.RepoID, v.Remote)
	if err != nil || len(got.Snapshots) != 129*256+1 || got.Revision != revision {
		t.Fatalf("compacted count=%d %v", len(got.Snapshots), err)
	}
}

func TestMetadataCheckpointCompactionRejectsOldCorruption(t *testing.T) {
	st := NewFileStore(t.TempDir())
	v := metadataCheckpointFixture()
	page := metadataCheckpointSeedPage(t, st, v)
	head := metadataCheckpointHeadFor(t, st, v)
	for range 128 {
		head.Pages = append(head.Pages, page)
	}
	head.PageCount = 128
	head = metadataCheckpointSaveHead(t, st, head)
	path, _ := st.metadataCheckpointPath(v.RepoID, v.Remote)
	before := metadataCheckpointReadBytes(t, path)
	metadataCheckpointWrite(t, st.metadataCheckpointPagePath(page), []byte("corrupt old page"))
	v.Snapshots[0].Message = "different new page"
	if _, err := st.AppendMetadataCheckpoint(context.Background(), head.Revision, v.RepoID, v.Remote, v.Snapshots, nil); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("compaction accepted corruption: %v", err)
	}
	if !bytes.Equal(before, metadataCheckpointReadBytes(t, path)) {
		t.Fatal("failed compaction advanced head")
	}
}

func TestMetadataCheckpointConcurrentAppendHasExactlyOneWinner(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			st := NewFileStore(t.TempDir())
			v := metadataCheckpointFixture()
			var expected domain.ContentHash
			if existing {
				expected = metadataCheckpointAppend(t, st, "", v)
			}
			type result struct {
				revision domain.ContentHash
				message  string
				err      error
			}
			results := make(chan result, 12)
			start := make(chan struct{})
			for i := range 12 {
				go func() {
					next := metadataCheckpointFixture()
					next.Snapshots[0].Message = fmt.Sprint(i)
					<-start
					revision, err := NewFileStore(st.repoRoot).AppendMetadataCheckpoint(context.Background(), expected, next.RepoID, next.Remote, next.Snapshots, nil)
					results <- result{revision, next.Snapshots[0].Message, err}
				}()
			}
			close(start)
			wins, conflicts := 0, 0
			var winner result
			for range 12 {
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
			if wins != 1 || conflicts != 11 {
				t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
			}
			got, err := st.ReadMetadataCheckpoint(context.Background(), v.RepoID, v.Remote)
			if err != nil || got.Revision != winner.revision || got.Snapshots[0].Message != winner.message {
				t.Fatalf("winner lost=%+v %v", got, err)
			}
		})
	}
}

type metadataCheckpointCheckContext struct {
	context.Context
	check func()
}

func (c metadataCheckpointCheckContext) Err() error { c.check(); return c.Context.Err() }

func TestMetadataCheckpointCancellationAndPublicationFailure(t *testing.T) {
	for _, failure := range []string{"cancel", "head symlink"} {
		t.Run(failure, func(t *testing.T) {
			st := NewFileStore(t.TempDir())
			v := metadataCheckpointFixture()
			revision := metadataCheckpointAppend(t, st, "", v)
			headPath, _ := st.metadataCheckpointPath(v.RepoID, v.Remote)
			before := metadataCheckpointReadBytes(t, headPath)
			v.Snapshots[0].Message = "unpublished new page"
			raw, err := encodeMetadataCheckpointPage(context.Background(), v.RepoID, v.Remote, v.Snapshots, nil)
			if err != nil {
				t.Fatal(err)
			}
			pagePath := st.metadataCheckpointPagePath(domain.HashContent(raw))
			outside := filepath.Join(t.TempDir(), "untouched")
			metadataCheckpointWrite(t, outside, before)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			fired := false
			ctx := metadataCheckpointCheckContext{Context: base, check: func() {
				if fired || !fileExists(pagePath) {
					return
				}
				fired = true
				if failure == "cancel" {
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
			got, err := st.AppendMetadataCheckpoint(ctx, revision, v.RepoID, v.Remote, v.Snapshots, nil)
			wantErr := error(context.Canceled)
			if failure == "head symlink" {
				wantErr = domain.ErrHashMismatch
			}
			if !fired || got != "" || !errors.Is(err, wantErr) {
				t.Fatalf("publication fault=%s %v fired=%t", got, err, fired)
			}
			if !bytes.Equal(before, metadataCheckpointReadBytes(t, outside)) {
				t.Fatal("outside target changed")
			}
			if failure == "head symlink" {
				if _, err := os.Readlink(headPath); err != nil {
					t.Fatal("head symlink overwritten", err)
				}
				if err := os.Remove(headPath); err != nil {
					t.Fatal(err)
				}
				metadataCheckpointWrite(t, headPath, before)
			}
			if !bytes.Equal(before, metadataCheckpointReadBytes(t, headPath)) || !bytes.Equal(raw, metadataCheckpointReadBytes(t, pagePath)) {
				t.Fatal("failure changed progress or page")
			}
			previous, err := st.ReadMetadataCheckpoint(context.Background(), v.RepoID, v.Remote)
			if err != nil || previous.Revision != revision || previous.Snapshots[0].Message == v.Snapshots[0].Message {
				t.Fatalf("orphan became visible=%+v %v", previous, err)
			}
			metadataCheckpointAppend(t, st, revision, v)
		})
	}
	st := NewFileStore(t.TempDir())
	v := metadataCheckpointFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.ReadMetadataCheckpoint(ctx, v.RepoID, v.Remote); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := st.AppendMetadataCheckpoint(ctx, "", v.RepoID, v.Remote, v.Snapshots, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.storeDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled call created state")
	}
	revision := metadataCheckpointAppend(t, st, "", v)
	path, _ := st.metadataCheckpointPath(v.RepoID, v.Remote)
	err := st.withMutationLock(context.Background(), metadataCheckpointNamespace, filepath.Base(path), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := st.AppendMetadataCheckpoint(ctx, revision, v.RepoID, v.Remote, v.Snapshots, nil)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("waiting append=%v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	probe := metadataCheckpointCheckContext{Context: context.Background(), check: func() { checks++ }}
	if _, err := st.ReadMetadataCheckpoint(probe, v.RepoID, v.Remote); err != nil {
		t.Fatal(err)
	}
	for at := 1; at <= checks; at++ {
		base, cancel := context.WithCancel(context.Background())
		n := 0
		ctx := metadataCheckpointCheckContext{Context: base, check: func() {
			n++
			if n == at {
				cancel()
			}
		}}
		got, err := st.ReadMetadataCheckpoint(ctx, v.RepoID, v.Remote)
		cancel()
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, outbound.MetadataCheckpoint{}) {
			t.Fatalf("partial canceled read at %d: %+v %v", at, got, err)
		}
	}
}

func TestMetadataCheckpointSymlinks(t *testing.T) {
	for _, location := range []string{"head", "dangling head", "page", "dangling page", "pages", "namespace", ".cxt", "lock"} {
		t.Run(location, func(t *testing.T) {
			st := NewFileStore(t.TempDir())
			v := metadataCheckpointFixture()
			headPath, _ := st.metadataCheckpointPath(v.RepoID, v.Remote)
			raw, err := encodeMetadataCheckpointPage(context.Background(), v.RepoID, v.Remote, v.Snapshots, nil)
			if err != nil {
				t.Fatal(err)
			}
			hash := domain.HashContent(raw)
			pagePath := st.metadataCheckpointPagePath(hash)
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "sentinel")
			metadataCheckpointWrite(t, sentinel, []byte("untouched"))
			link, target := headPath, sentinel
			var expected domain.ContentHash
			if location == "page" || location == "dangling page" || location == "pages" {
				head := metadataCheckpointHeadFor(t, st, v)
				head.Pages, head.PageCount = []domain.ContentHash{hash}, 1
				expected = metadataCheckpointSaveHead(t, st, head).Revision
			}
			switch location {
			case "dangling head":
				target = filepath.Join(outside, "absent")
			case "page":
				link = pagePath
			case "dangling page":
				link, target = pagePath, filepath.Join(outside, "absent")
			case "pages":
				link, target = filepath.Dir(pagePath), outside
			case "namespace":
				link, target = filepath.Dir(headPath), outside
			case ".cxt":
				link, target = st.storeDir(), outside
			case "lock":
				link = filepath.Join(st.storeDir(), "locks", metadataCheckpointNamespace, filepath.Base(headPath)+".flock")
			}
			if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if location != "lock" {
				if _, err := st.ReadMetadataCheckpoint(context.Background(), v.RepoID, v.Remote); !errors.Is(err, domain.ErrHashMismatch) {
					t.Fatalf("symlink read=%v", err)
				}
			}
			if _, err := st.AppendMetadataCheckpoint(context.Background(), expected, v.RepoID, v.Remote, v.Snapshots, nil); !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatalf("symlink append=%v", err)
			}
			if got, err := os.Readlink(link); err != nil || got != target {
				t.Fatalf("symlink replaced=%s %v", got, err)
			}
			if string(metadataCheckpointReadBytes(t, sentinel)) != "untouched" {
				t.Fatal("outside target changed")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 1 {
				t.Fatalf("write escaped store=%v %v", entries, err)
			}
		})
	}
}

func TestMetadataCheckpointScopeAndVerifiedObservationIsolation(t *testing.T) {
	ctx := context.Background()
	st := NewFileStore(t.TempDir())
	v := metadataCheckpointFixture()
	verified := outbound.RemoteObservation{Version: 1, RepoID: v.RepoID, Remote: v.Remote, Refs: []domain.Ref{{RepoID: v.RepoID, Kind: domain.RefBranch, Name: "main", Target: v.Snapshots[0].ID}}}
	if err := st.CompareAndSwapRemoteObservation(ctx, "", verified); err != nil {
		t.Fatal(err)
	}
	obsPath, _ := st.remoteObservationPath(v.RepoID, v.Remote)
	before := metadataCheckpointReadBytes(t, obsPath)
	revision := metadataCheckpointAppend(t, st, "", v)
	head := metadataCheckpointHeadFor(t, st, v)
	for _, scope := range []struct{ repo, remote string }{{v.RepoID, v.Remote + "/other"}, {domain.HashContent([]byte("foreign repo")), v.Remote}} {
		got, err := st.ReadMetadataCheckpoint(ctx, scope.repo, scope.remote)
		if err != nil || got.Revision != "" || len(got.Snapshots) != 0 {
			t.Fatalf("scope leaked=%+v %v", got, err)
		}
		foreign := metadataCheckpointFixture()
		foreign.RepoID = scope.repo
		foreign.Remote = scope.remote
		foreign.Snapshots[0].RepoID = scope.repo
		if _, err := st.AppendMetadataCheckpoint(ctx, revision, scope.repo, scope.remote, foreign.Snapshots, nil); !errors.Is(err, domain.ErrSyncConflict) {
			t.Fatalf("foreign revision accepted: %v", err)
		}
		metadataCheckpointAppend(t, st, "", foreign)
		otherHead := metadataCheckpointHeadFor(t, st, foreign)
		if otherHead.Pages[0] == head.Pages[0] {
			t.Fatal("page hash omits endpoint/repo identity")
		}
		otherHead.Pages = head.Pages
		metadataCheckpointSaveHead(t, st, otherHead)
		if _, err := st.ReadMetadataCheckpoint(ctx, scope.repo, scope.remote); !errors.Is(err, domain.ErrHashMismatch) {
			t.Fatalf("foreign page accepted: %v", err)
		}
	}
	for _, scope := range []struct{ repo, remote string }{{"../repo", v.Remote}, {v.RepoID, ""}} {
		if _, err := st.ReadMetadataCheckpoint(ctx, scope.repo, scope.remote); err == nil {
			t.Fatal("invalid read scope")
		}
		if _, err := st.AppendMetadataCheckpoint(ctx, "", scope.repo, scope.remote, v.Snapshots, nil); err == nil {
			t.Fatal("invalid append scope")
		}
	}
	got, err := st.ReadMetadataCheckpoint(ctx, v.RepoID, v.Remote)
	if err != nil || got.Revision != revision {
		t.Fatalf("original scope changed=%+v %v", got, err)
	}
	if !bytes.Equal(before, metadataCheckpointReadBytes(t, obsPath)) {
		t.Fatal("verified observation changed")
	}
}

func TestMetadataCheckpointTombstonesAndReappearance(t *testing.T) {
	st := NewFileStore(t.TempDir())
	v := metadataCheckpointFixture()
	revision := metadataCheckpointAppend(t, st, "", v)
	id := v.Snapshots[0].ID
	oldToken, err := domain.SnapshotStateHash(v.Snapshots[0])
	if err != nil {
		t.Fatal(err)
	}
	revision, err = st.AppendMetadataCheckpoint(context.Background(), revision, v.RepoID, v.Remote, nil, []domain.ContentHash{id})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.ReadMetadataCheckpoint(context.Background(), v.RepoID, v.Remote)
	if err != nil || got.Revision != revision || len(got.Snapshots) != 0 {
		t.Fatalf("removal not replayed: %+v %v", got, err)
	}
	// Immutable IDs and mutable tokens can match across disappearance and later
	// reappearance. The new complete metadata must replace the deleted record.
	v.Snapshots[0].CreatedAt = v.Snapshots[0].CreatedAt.Add(time.Hour)
	newToken, err := domain.SnapshotStateHash(v.Snapshots[0])
	if err != nil || newToken != oldToken {
		t.Fatalf("fixture token changed: %v", err)
	}
	revision = metadataCheckpointAppend(t, st, revision, v)
	got, err = st.ReadMetadataCheckpoint(context.Background(), v.RepoID, v.Remote)
	if err != nil || !reflect.DeepEqual(got.Snapshots, v.Snapshots) {
		t.Fatalf("reappearance retained old metadata: %+v %v", got, err)
	}
	other := v.Snapshots[0]
	other.ID = domain.HashContent([]byte("replacement document"))
	other.DocHash = other.ID
	revision, err = st.AppendMetadataCheckpoint(context.Background(), revision, v.RepoID, v.Remote, []domain.Snapshot{other}, []domain.ContentHash{id})
	if err != nil {
		t.Fatal(err)
	}
	got, err = st.ReadMetadataCheckpoint(context.Background(), v.RepoID, v.Remote)
	if err != nil || got.Revision != revision || !reflect.DeepEqual(got.Snapshots, []domain.Snapshot{other}) {
		t.Fatalf("mixed upsert/removal: %+v %v", got, err)
	}
}

func TestMetadataCheckpointTombstoneValidationAndCanonicalOrder(t *testing.T) {
	v := metadataCheckpointFixture()
	id := v.Snapshots[0].ID
	for name, input := range map[string]struct {
		snapshots []domain.Snapshot
		removed   []domain.ContentHash
	}{
		"bad hash":          {nil, []domain.ContentHash{"invalid"}},
		"duplicate removal": {nil, []domain.ContentHash{id, id}},
		"overlap":           {v.Snapshots, []domain.ContentHash{id}},
		"too many removals": {nil, make([]domain.ContentHash, 257)},
		"combined limit":    {v.Snapshots, make([]domain.ContentHash, 256)},
	} {
		t.Run(name, func(t *testing.T) {
			st := NewFileStore(t.TempDir())
			if _, err := st.AppendMetadataCheckpoint(context.Background(), "", v.RepoID, v.Remote, input.snapshots, input.removed); !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatalf("invalid tombstones accepted: %v", err)
			}
			raw := metadataCheckpointJSON(t, metadataCheckpointPage{metadataCheckpointPageFormat, 1, v.RepoID, v.Remote, input.snapshots, input.removed})
			metadataCheckpointAssertInvalidPage(t, st, v, raw)
		})
	}
	removed := []domain.ContentHash{domain.HashContent([]byte("first removal")), domain.HashContent([]byte("second removal"))}
	sort.Sort(sort.Reverse(sort.StringSlice(removed)))
	before := append([]domain.ContentHash(nil), removed...)
	raw, err := encodeMetadataCheckpointPage(context.Background(), v.RepoID, v.Remote, nil, removed)
	if err != nil || !reflect.DeepEqual(removed, before) {
		t.Fatalf("removal input changed: %v", err)
	}
	removed[0], removed[1] = removed[1], removed[0]
	ordered, err := encodeMetadataCheckpointPage(context.Background(), v.RepoID, v.Remote, nil, removed)
	if err != nil || !bytes.Equal(raw, ordered) {
		t.Fatalf("removal order changes canonical page: %v", err)
	}
	metadataCheckpointAssertInvalidPage(t, NewFileStore(t.TempDir()), v, metadataCheckpointJSON(t, metadataCheckpointPage{metadataCheckpointPageFormat, 1, v.RepoID, v.Remote, nil, before}))
	// Exactly 256 total entries is valid, even when most entries are removals.
	removed = nil
	for i := range 255 {
		removed = append(removed, fmt.Sprintf("sha256:%064x", i+1))
	}
	st := NewFileStore(t.TempDir())
	if _, err := st.AppendMetadataCheckpoint(context.Background(), "", v.RepoID, v.Remote, v.Snapshots, removed); err != nil {
		t.Fatal(err)
	}
	got, err := st.ReadMetadataCheckpoint(context.Background(), v.RepoID, v.Remote)
	if err != nil || !reflect.DeepEqual(got.Snapshots, v.Snapshots) {
		t.Fatalf("valid boundary batch: %+v %v", got, err)
	}
}

func TestMetadataCheckpointGenerationPreventsABAAndOverflow(t *testing.T) {
	st := NewFileStore(t.TempDir())
	v := metadataCheckpointFixture()
	empty := metadataCheckpointHeadFor(t, st, v)
	if empty.Generation != 0 {
		t.Fatal("absent head has generation")
	}
	empty.Generation = 2
	empty = metadataCheckpointSaveHead(t, st, empty)
	oldRevision := empty.Revision
	// Simulate two compacted states with exactly the same live page list.
	empty.Generation = 3
	empty = metadataCheckpointSaveHead(t, st, empty)
	if empty.Revision == oldRevision {
		t.Fatal("generation omitted from checksum")
	}
	if _, err := st.AppendMetadataCheckpoint(context.Background(), oldRevision, v.RepoID, v.Remote, v.Snapshots, nil); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("ABA accepted old empty head: %v", err)
	}
	revision := metadataCheckpointAppend(t, st, empty.Revision, v)
	head := metadataCheckpointHeadFor(t, st, v)
	if head.Generation != 4 || head.Revision != revision {
		t.Fatalf("generation not incremented: %+v", head)
	}
	head.Generation = ^uint64(0)
	head = metadataCheckpointSaveHead(t, st, head)
	path, _ := st.metadataCheckpointPath(v.RepoID, v.Remote)
	before := metadataCheckpointReadBytes(t, path)
	v.Snapshots[0].Message = "must not publish or create a page"
	raw, err := encodeMetadataCheckpointPage(context.Background(), v.RepoID, v.Remote, v.Snapshots, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendMetadataCheckpoint(context.Background(), head.Revision, v.RepoID, v.Remote, v.Snapshots, nil); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("generation wrapped: %v", err)
	}
	if !bytes.Equal(before, metadataCheckpointReadBytes(t, path)) || fileExists(st.metadataCheckpointPagePath(domain.HashContent(raw))) {
		t.Fatal("overflow changed head or wrote a page")
	}
}

func TestMetadataCheckpointZeroLiveCompaction(t *testing.T) {
	st := NewFileStore(t.TempDir())
	v := metadataCheckpointFixture()
	page := metadataCheckpointSeedPage(t, st, v)
	head := metadataCheckpointHeadFor(t, st, v)
	for range 128 {
		head.Pages = append(head.Pages, page)
	}
	head.PageCount = 128
	head = metadataCheckpointSaveHead(t, st, head)
	revision, err := st.AppendMetadataCheckpoint(context.Background(), head.Revision, v.RepoID, v.Remote, nil, []domain.ContentHash{v.Snapshots[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	compacted := metadataCheckpointHeadFor(t, st, v)
	if compacted.PageCount != 0 || len(compacted.Pages) != 0 || compacted.CompactAfter != 128 || compacted.Revision != revision || revision == "" {
		t.Fatalf("empty compacted head: %+v", compacted)
	}
	got, err := st.ReadMetadataCheckpoint(context.Background(), v.RepoID, v.Remote)
	if err != nil || got.Revision != revision || len(got.Snapshots) != 0 {
		t.Fatalf("empty compacted read: %+v %v", got, err)
	}
	if _, err := os.Stat(st.metadataCheckpointPagePath(page)); err != nil {
		t.Fatal("unreferenced page deleted", err)
	}
	v.Snapshots[0].CreatedAt = v.Snapshots[0].CreatedAt.Add(time.Hour)
	revision = metadataCheckpointAppend(t, st, revision, v)
	got, err = st.ReadMetadataCheckpoint(context.Background(), v.RepoID, v.Remote)
	if err != nil || got.Revision != revision || !reflect.DeepEqual(got.Snapshots, v.Snapshots) {
		t.Fatalf("reappearance after empty compaction: %+v %v", got, err)
	}
}
