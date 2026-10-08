package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

const catalogTestEpoch = "e66213bd-003e-47d1-b965-e690036bfb08"

func catalogTestRepo() string { return string(HashContent([]byte("synthetic catalog repo"))) }

func catalogTestSnapshot(name string) Snapshot {
	id := HashContent([]byte(name))
	return Snapshot{ID: id, DocHash: id, RepoID: catalogTestRepo(), Branch: "main", Parents: []ContentHash{}, Provider: ProviderCodex, Fidelity: FidelityFull, Author: TeamIdentity{Name: "Synthetic"}, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func catalogTestRaw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func catalogTestSnapshotEntry(t *testing.T, name string, sequence int64) CatalogEntry {
	t.Helper()
	snapshot := catalogTestSnapshot(name)
	return CatalogEntry{Kind: "snapshot", Key: string(snapshot.ID), Sequence: sequence, Value: catalogTestRaw(t, snapshot)}
}

func catalogTestProtocol(t *testing.T, protocol int) CatalogEntry {
	t.Helper()
	return CatalogEntry{Kind: "protocol", Key: catalogTestRepo(), Value: catalogTestRaw(t, map[string]int{"context_protocol": protocol})}
}

func catalogTestRef(t *testing.T, ref Ref, sequence int64) CatalogEntry {
	t.Helper()
	return CatalogEntry{Kind: "ref", Key: string(catalogTestRaw(t, [2]string{ref.Kind, ref.Name})), Sequence: sequence, Value: catalogTestRaw(t, ref)}
}

func catalogTestPage(entries ...CatalogEntry) CatalogPage {
	cp := CatalogCheckpoint{Version: 1, RepoID: catalogTestRepo(), Epoch: catalogTestEpoch, Sequence: 7}
	return CatalogPage{Version: 1, RepoID: cp.RepoID, Epoch: cp.Epoch, Through: cp.Sequence, Mode: "baseline", Entries: entries, Checkpoint: &cp}
}

func TestCatalogCheckpointValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*CatalogCheckpoint)
		good bool
	}{
		{"zero", func(*CatalogCheckpoint) {}, true},
		{"uppercase_uuid", func(cp *CatalogCheckpoint) { cp.Epoch = strings.ToUpper(cp.Epoch) }, true},
		{"negative", func(cp *CatalogCheckpoint) { cp.Sequence = -1 }, false},
		{"future_version", func(cp *CatalogCheckpoint) { cp.Version++ }, false},
		{"missing_version", func(cp *CatalogCheckpoint) { cp.Version = 0 }, false},
		{"other_repository", func(cp *CatalogCheckpoint) { cp.RepoID = string(HashContent([]byte("another repo"))) }, false},
		{"bad_repository", func(cp *CatalogCheckpoint) { cp.RepoID = "../repo" }, false},
		{"no_epoch", func(cp *CatalogCheckpoint) { cp.Epoch = "" }, false},
		{"epoch_separators", func(cp *CatalogCheckpoint) { cp.Epoch = strings.ReplaceAll(cp.Epoch, "-", "_") }, false},
		{"epoch_hex", func(cp *CatalogCheckpoint) { cp.Epoch = "z" + cp.Epoch[1:] }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := CatalogCheckpoint{Version: 1, RepoID: catalogTestRepo(), Epoch: catalogTestEpoch}
			tc.edit(&cp)
			err := ValidateCatalogCheckpoint(catalogTestRepo(), cp)
			if (err == nil) != tc.good || (!tc.good && !errors.Is(err, ErrHashMismatch)) {
				t.Fatalf("ValidateCatalogCheckpoint() = %v", err)
			}
		})
	}
}

func TestCatalogEntryRejectsMalformedMetadata(t *testing.T) {
	base := catalogTestSnapshotEntry(t, "snapshot", 1)
	for _, tc := range []struct {
		name string
		edit func(*CatalogEntry)
	}{
		{"unknown_kind", func(e *CatalogEntry) { e.Kind = "document" }},
		{"negative_sequence", func(e *CatalogEntry) { e.Sequence = -1 }},
		{"bad_key", func(e *CatalogEntry) { e.Key = "bad" }},
		{"different_key", func(e *CatalogEntry) { e.Key = string(HashContent([]byte("other"))) }},
		{"value_missing", func(e *CatalogEntry) { e.Value = nil }},
		{"value_null", func(e *CatalogEntry) { e.Value = json.RawMessage(`null`) }},
		{"value_array", func(e *CatalogEntry) { e.Value = json.RawMessage(`[]`) }},
		{"deleted_with_value", func(e *CatalogEntry) { e.Deleted = true }},
		{"deleted_with_null", func(e *CatalogEntry) { e.Deleted = true; e.Value = json.RawMessage(`null`) }},
		{"unknown_metadata", func(e *CatalogEntry) { e.Value = append(json.RawMessage(`{"future":true,`), e.Value[1:]...) }},
		{"derived_branches", func(e *CatalogEntry) { e.Value = append(json.RawMessage(`{"branches":["main"],`), e.Value[1:]...) }},
		{"duplicate_id", func(e *CatalogEntry) { e.Value = append(json.RawMessage(`{"id":"`+e.Key+`",`), e.Value[1:]...) }},
		{"case_alias", func(e *CatalogEntry) {
			e.Value = json.RawMessage(strings.Replace(string(e.Value), `"repo_id":`, `"Repo_ID":`, 1))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := base
			entry.Value = append(json.RawMessage(nil), base.Value...)
			tc.edit(&entry)
			if err := ValidateCatalogEntry(catalogTestRepo(), entry); !errors.Is(err, ErrHashMismatch) {
				t.Fatalf("ValidateCatalogEntry() = %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		edit func(*Snapshot)
	}{
		{"scope", func(s *Snapshot) { s.RepoID = string(HashContent([]byte("another repo"))) }},
		{"doc_identity", func(s *Snapshot) { s.DocHash = HashContent([]byte("other doc")) }},
		{"memory_hash", func(s *Snapshot) { s.MemoryHash = "wrong" }},
		{"claude_settings", func(s *Snapshot) { s.ClaudeSettings = "wrong" }},
		{"agents_settings", func(s *Snapshot) { s.AgentsSettings = "wrong" }},
		{"codex_settings", func(s *Snapshot) { s.CodexSettings = "wrong" }},
		{"parent", func(s *Snapshot) { s.Parents = []ContentHash{"wrong"} }},
		{"graft", func(s *Snapshot) { s.GraftParents = []ContentHash{"wrong"} }},
		{"graft_overflow", func(s *Snapshot) { s.GraftSeq = MaxGraftSeq + 1 }},
		{"negative_compaction", func(s *Snapshot) { s.CompactionCount = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := catalogTestSnapshot("snapshot")
			tc.edit(&snapshot)
			entry := base
			entry.Value = catalogTestRaw(t, snapshot)
			if err := ValidateCatalogEntry(catalogTestRepo(), entry); !errors.Is(err, ErrHashMismatch) {
				t.Fatalf("ValidateCatalogEntry() = %v", err)
			}
		})
	}
	base.Deleted, base.Value = true, nil
	if err := ValidateCatalogEntry(catalogTestRepo(), base); err != nil {
		t.Fatalf("valid tombstone: %v", err)
	}
}

func TestCatalogEntryRefsHistoryAndProtocol(t *testing.T) {
	repo := catalogTestRepo()
	ref := Ref{RepoID: repo, Kind: RefBranch, Name: "feature/space", BranchID: "branch-identity", Target: catalogTestSnapshot("snapshot").ID}
	entry := catalogTestRef(t, ref, 1)
	entry.Key = " [ \n\t\"branch\", \"feature/space\"  ] "
	if err := ValidateCatalogEntry(repo, entry); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`[]`, `["branch"]`, `["branch","feature/space","extra"]`, `["branch",null]`, `["branch",1]`, `["unknown","feature/space"]`, `["branch","../escape"]`, `["branch","other"]`, `["branch","feature/space"] true`} {
		bad := entry
		bad.Key = key
		if ValidateCatalogEntry(repo, bad) == nil {
			t.Fatalf("accepted key %s", key)
		}
	}
	for _, mutation := range []func(*Ref){
		func(r *Ref) { r.RepoID = "wrong" },
		func(r *Ref) { r.Target = "bad" },
		func(r *Ref) { r.Symbolic = "main" },
		func(r *Ref) { r.BranchID = strings.Repeat("x", 129) },
	} {
		bad := ref
		mutation(&bad)
		changed := entry
		changed.Value = catalogTestRaw(t, bad)
		if ValidateCatalogEntry(repo, changed) == nil {
			t.Fatalf("accepted ref %+v", bad)
		}
	}
	archived, err := NewBranchLifecycleRef(repo, "main", ref.Target, 1, BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	archived.Target = HashContent([]byte("wrong lifecycle target"))
	if ValidateCatalogEntry(repo, catalogTestRef(t, archived, 1)) == nil {
		t.Fatal("accepted lifecycle tag/name target mismatch")
	}
	history := HistoryEvent{ID: strings.Repeat("a", 32), RepoID: repo, BranchID: "source-identity", Branch: "feature/space", Kind: "birth", Target: ref.Target, CreatedAt: catalogTestSnapshot("snapshot").CreatedAt}
	historyEntry := CatalogEntry{Kind: "history", Key: history.ID, Value: catalogTestRaw(t, history)}
	if err := ValidateCatalogEntry(repo, historyEntry); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func(*HistoryEvent){
		func(h *HistoryEvent) { h.RepoID = "wrong" },
		func(h *HistoryEvent) { h.ID = strings.Repeat("b", 32) },
		func(h *HistoryEvent) { h.CreatedAt = time.Time{} },
		func(h *HistoryEvent) { h.Kind = "unknown" },
		func(h *HistoryEvent) { h.BindingParent = "bad" },
	} {
		bad := history
		mutation(&bad)
		changed := historyEntry
		changed.Value = catalogTestRaw(t, bad)
		if ValidateCatalogEntry(repo, changed) == nil {
			t.Fatalf("accepted history %+v", bad)
		}
	}
	for _, kind := range []string{"ref", "history", "protocol"} {
		tombstone := entry
		if kind == "history" {
			tombstone = historyEntry
		} else if kind == "protocol" {
			tombstone = catalogTestProtocol(t, 1)
		}
		tombstone.Deleted, tombstone.Value = true, nil
		if err := ValidateCatalogEntry(repo, tombstone); err != nil {
			t.Fatalf("%s tombstone: %v", kind, err)
		}
	}
	for _, protocol := range []int{-1, 2} {
		if ValidateCatalogEntry(repo, catalogTestProtocol(t, protocol)) == nil {
			t.Fatalf("accepted protocol %d", protocol)
		}
	}
}

func TestCatalogPageOrderAndBoundaries(t *testing.T) {
	repo := catalogTestRepo()
	for _, tc := range []struct {
		name string
		edit func(*CatalogPage)
	}{
		{"entries_null", func(p *CatalogPage) { p.Entries = nil }},
		{"unknown_mode", func(p *CatalogPage) { p.Mode = "other" }},
		{"wrong_repo", func(p *CatalogPage) { p.RepoID = string(HashContent([]byte("wrong repo"))) }},
		{"wrong_version", func(p *CatalogPage) { p.Version = 2 }},
		{"wrong_epoch", func(p *CatalogPage) { p.Epoch = "wrong" }},
		{"negative_through", func(p *CatalogPage) { p.Through = -1 }},
		{"ahead_entry", func(p *CatalogPage) { p.Entries[0].Sequence = 8 }},
		{"negative_entry", func(p *CatalogPage) { p.Entries[0].Sequence = -1 }},
		{"unordered", func(p *CatalogPage) { p.Entries[0], p.Entries[1] = p.Entries[1], p.Entries[0] }},
		{"duplicate", func(p *CatalogPage) { p.Entries = append(p.Entries, p.Entries[1]) }},
		{"baseline_tombstone", func(p *CatalogPage) { p.Entries[1].Deleted, p.Entries[1].Value = true, nil }},
		{"no_progress", func(p *CatalogPage) { p.Checkpoint = nil }},
		{"both_progress", func(p *CatalogPage) { p.NextCursor = "opaque" }},
		{"empty_partial", func(p *CatalogPage) { p.NextCursor, p.Checkpoint, p.Entries = "opaque", nil, []CatalogEntry{} }},
		{"checkpoint_version", func(p *CatalogPage) { p.Checkpoint.Version = 2 }},
		{"checkpoint_repo", func(p *CatalogPage) { p.Checkpoint.RepoID = "wrong" }},
		{"checkpoint_epoch", func(p *CatalogPage) { p.Checkpoint.Epoch = strings.ToUpper(p.Epoch) }},
		{"checkpoint_sequence", func(p *CatalogPage) { p.Checkpoint.Sequence-- }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := catalogTestPage(catalogTestProtocol(t, 1), catalogTestSnapshotEntry(t, "snapshot", 0))
			tc.edit(&page)
			if err := ValidateCatalogPage(repo, nil, nil, page); !errors.Is(err, ErrHashMismatch) {
				t.Fatalf("ValidateCatalogPage()=%v", err)
			}
		})
	}
	// Baselines sort identities, not mutation times. They may mix seq 0 and 7.
	baseline := catalogTestPage(catalogTestProtocol(t, 1), catalogTestSnapshotEntry(t, "snapshot", 0))
	baseline.Entries[0].Sequence = 7
	if err := ValidateCatalogPage(repo, nil, nil, baseline); err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{1000, 1001} {
		page := catalogTestPage()
		page.Entries = []CatalogEntry{}
		for i := range count {
			ref := Ref{RepoID: repo, Kind: RefBranch, Name: fmt.Sprintf("branch-%04d", i), Target: catalogTestSnapshot("snapshot").ID}
			page.Entries = append(page.Entries, catalogTestRef(t, ref, 0))
		}
		if err := ValidateCatalogPage(repo, nil, nil, page); (err == nil) != (count == 1000) {
			t.Fatalf("%d entries: %v", count, err)
		}
	}
}

func TestCatalogPagesSplitTransactionsAndNeverRequireClosure(t *testing.T) {
	repo := catalogTestRepo()
	child := catalogTestSnapshot("child")
	child.Parents = []ContentHash{HashContent([]byte("parent not yet received"))}
	event := HistoryEvent{ID: strings.Repeat("a", 32), RepoID: repo, BranchID: "identity", Branch: "main", Kind: "birth", Target: child.ID, BindingParent: strings.Repeat("b", 32), CreatedAt: child.CreatedAt}
	first := catalogTestPage(CatalogEntry{Kind: "history", Key: event.ID, Sequence: 7, Value: catalogTestRaw(t, event)})
	first.Mode, first.NextCursor, first.Checkpoint = "delta", "opaque1", nil
	second := catalogTestPage(CatalogEntry{Kind: "snapshot", Key: string(child.ID), Sequence: 7, Value: catalogTestRaw(t, child)})
	second.Mode = "delta"
	after := CatalogCheckpoint{Version: 1, RepoID: repo, Epoch: catalogTestEpoch, Sequence: 6}
	if err := ValidateCatalogPage(repo, &after, nil, first); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCatalogPage(repo, &after, &first, second); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*CatalogPage)
	}{
		{"old_sequence", func(p *CatalogPage) { p.Entries[0].Sequence = 6 }},
		{"changed_through", func(p *CatalogPage) { p.Through++; p.Checkpoint.Sequence++ }},
		{"changed_epoch", func(p *CatalogPage) { p.Epoch = strings.ToUpper(p.Epoch); p.Checkpoint.Epoch = p.Epoch }},
		{"changed_mode", func(p *CatalogPage) { p.Mode = "baseline" }},
		{"tail_repeated", func(p *CatalogPage) { p.Entries = append([]CatalogEntry{}, first.Entries...) }},
		{"cursor_repeated", func(p *CatalogPage) { p.Checkpoint, p.NextCursor = nil, first.NextCursor }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := second
			page.Entries = append([]CatalogEntry{}, second.Entries...)
			cp := *second.Checkpoint
			page.Checkpoint = &cp
			tc.edit(&page)
			if ValidateCatalogPage(repo, &after, &first, page) == nil {
				t.Fatal("invalid continuation accepted")
			}
		})
	}
	if ValidateCatalogPage(repo, &after, &second, second) == nil {
		t.Fatal("continued after final checkpoint")
	}
	empty := second
	empty.Entries = []CatalogEntry{}
	if err := ValidateCatalogPage(repo, &after, &first, empty); err != nil {
		t.Fatalf("final empty continuation: %v", err)
	}
	empty.Through, empty.Checkpoint = after.Sequence, &after
	if err := ValidateCatalogPage(repo, &after, nil, empty); err != nil {
		t.Fatalf("unchanged authorized delta: %v", err)
	}
	if ValidateCatalogPage(repo, &after, nil, catalogTestPage()) == nil {
		t.Fatal("accepted baseline for delta request")
	}
}

func TestCatalogRefIdentityWhitespaceAndRepeatedDeltaUpdates(t *testing.T) {
	repo := catalogTestRepo()
	ref := Ref{Kind: RefBranch, Name: "main", RepoID: repo, Target: catalogTestSnapshot("snapshot").ID}
	a, b := catalogTestRef(t, ref, 1), catalogTestRef(t, ref, 1)
	a.Key = `[ "branch", "main" ]`
	escaped := b
	escaped.Key = `["br\u0061nch","ma\u0069n"]`
	if ValidateCatalogEntry(repo, escaped) != nil || CatalogEntryIdentity(a) != CatalogEntryIdentity(b) || CatalogEntryIdentity(b) != CatalogEntryIdentity(escaped) || CatalogEntryIdentity(b) != "ref\x00"+b.Key {
		t.Fatal("identity must canonicalize ref whitespace and JSON escapes")
	}
	page := catalogTestPage(a, b)
	if ValidateCatalogPage(repo, nil, nil, page) == nil {
		t.Fatal("accepted duplicate semantic ref keys")
	}
	first, second := catalogTestPage(a), catalogTestPage(b)
	first.Checkpoint, first.NextCursor = nil, "opaque"
	if ValidateCatalogPage(repo, nil, &first, second) == nil {
		t.Fatal("accepted cross-page duplicate semantic ref keys")
	}
	after := CatalogCheckpoint{Version: 1, RepoID: repo, Epoch: catalogTestEpoch}
	page.Mode = "delta"
	if ValidateCatalogPage(repo, &after, nil, page) == nil {
		t.Fatal("accepted same transaction duplicate identity")
	}
	page.Entries[1].Sequence = 2
	if err := ValidateCatalogPage(repo, &after, nil, page); err != nil {
		t.Fatalf("separate transaction update: %v", err)
	}
	page.Entries[1].Sequence = 0
	if ValidateCatalogPage(repo, &after, nil, page) == nil {
		t.Fatal("accepted out of order/excluded sequence")
	}
}

func TestCatalogManifestProjectsProtocolAndPreservesImage(t *testing.T) {
	repo := catalogTestRepo()
	snapshot := catalogTestSnapshot("snapshot")
	snapshot.MemoryHash = HashContent([]byte("memory"))
	snapshot.Parents = []ContentHash{HashContent([]byte("z parent")), HashContent([]byte("a parent"))}
	snapshot.GraftParents = []ContentHash{HashContent([]byte("z graft")), HashContent([]byte("a graft"))}
	snapshot.GraftSeq = 2
	refs := []Ref{
		{Kind: RefHEAD, Name: "HEAD", RepoID: repo, Symbolic: "refs/heads/feature"},
		{Kind: RefBranch, Name: "feature", RepoID: repo, Target: snapshot.ID, BranchID: "retained-identity"},
	}
	archive, err := NewBranchLifecycleRef(repo, "feature", snapshot.ID, 1, BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	refs = append(refs, archive)
	for _, protocol := range []int{0, 1} {
		entries := []CatalogEntry{catalogTestProtocol(t, protocol), {Kind: "snapshot", Key: string(snapshot.ID), Value: catalogTestRaw(t, snapshot)}}
		for _, ref := range refs {
			entries = append(entries, catalogTestRef(t, ref, 0))
		}
		before := string(catalogTestRaw(t, entries))
		manifest, snapshots, err := CatalogManifest(repo, entries)
		if err != nil {
			t.Fatal(err)
		}
		if string(catalogTestRaw(t, entries)) != before || !reflect.DeepEqual(snapshots, []Snapshot{snapshot}) {
			t.Fatal("projection mutated raw entries or parent order")
		}
		wantRefs := append([]Ref{}, refs...)
		if protocol == 0 {
			wantRefs, err = ProjectBranchLifecycleRefs(refs)
			if err != nil {
				t.Fatal(err)
			}
			if len(manifest.Refs) != 2 || manifest.Refs[0].Symbolic != "" || manifest.Refs[0].Target != snapshot.ID {
				t.Fatal("legacy archive must hide branch and detach HEAD")
			}
		} else if len(manifest.Refs) != 3 {
			t.Fatal("protocol1 must retain the raw branch")
		}
		sort.Slice(wantRefs, func(i, j int) bool { return wantRefs[i].Kind < wantRefs[j].Kind })
		if !reflect.DeepEqual(wantRefs, manifest.Refs) {
			t.Fatalf("protocol %d refs differ: %+v", protocol, manifest.Refs)
		}
		state, err := SnapshotStateHash(snapshot)
		if err != nil || manifest.SnapshotStates[snapshot.ID] != state || manifest.MemoryAttachments[snapshot.ID] != snapshot.MemoryHash || !reflect.DeepEqual(manifest.SnapshotIndex, []ContentHash{snapshot.ID}) || manifest.ContextProtocol != protocol {
			t.Fatalf("incorrect manifest projection: %+v (%v)", manifest, err)
		}
	}
	for _, entries := range [][]CatalogEntry{
		nil,
		{catalogTestSnapshotEntry(t, "snapshot", 0)},
		{catalogTestProtocol(t, 1), catalogTestProtocol(t, 1)},
		{catalogTestProtocol(t, 1), catalogTestSnapshotEntry(t, "snapshot", 0), catalogTestSnapshotEntry(t, "snapshot", 1)},
		{catalogTestProtocol(t, 1), {Kind: "snapshot", Key: string(snapshot.ID), Deleted: true}},
	} {
		if _, _, err := CatalogManifest(repo, entries); err == nil {
			t.Fatal("accepted incomplete or non-live catalog image")
		}
	}
	manifest, snapshots, err := CatalogManifest(repo, []CatalogEntry{catalogTestProtocol(t, 0)})
	if err != nil || manifest.Refs == nil || manifest.SnapshotIndex == nil || manifest.MemoryAttachments == nil || manifest.SnapshotStates == nil || snapshots == nil {
		t.Fatalf("empty repository projection: %+v %+v %v", manifest, snapshots, err)
	}
}
