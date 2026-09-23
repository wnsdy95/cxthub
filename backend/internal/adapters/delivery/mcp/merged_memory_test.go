package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestMemoryLoadIncludesPreviousMainImmediatelyAfterPromotion(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	base, source := pageHash(2), pageHash(3)
	previous := domain.MemoryDigest{SnapshotID: base, Summary: "previous main decision"}
	feature := domain.MemoryDigest{SnapshotID: source, Summary: "feature decision", Fragments: []domain.MemoryFragment{{SourceSnapshot: source, Summary: "feature decision"}}}
	bh, _ := domain.MemoryDigestHash(previous)
	sh, _ := domain.MemoryDigestHash(feature)
	f := pageBackend{fakeContextBackend: fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: {
		{ID: base, RepoID: repo.ID, MemoryHash: bh},
		{ID: source, RepoID: repo.ID, MemoryHash: sh, GraftSeq: 1, GraftParents: []domain.ContentHash{base}},
	}}, memories: map[domain.ContentHash]domain.MemoryDigest{bh: previous, sh: feature}}}
	s := &Server{context: f}
	got, err := s.memoryPage(systemTestContext(), repo, toolArgs{Ref: string(source)})
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{previous.Summary, feature.Summary} {
		if !strings.Contains(got, marker) {
			t.Fatalf("immediate merged memory omitted %q: %s", marker, got)
		}
	}
}

func TestProjectedMemoryPagesAreStatelessAndRejectDependencyChanges(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	base, source := pageHash(2), pageHash(3)
	memory := domain.MemoryDigest{SnapshotID: base, Summary: strings.Repeat("\uBCD1\uD569 \uAE30\uC5B5", 6000)}
	hash, _ := domain.MemoryDigestHash(memory)
	f := pageBackend{fakeContextBackend: fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: {
		{ID: base, RepoID: repo.ID, MemoryHash: hash}, {ID: source, RepoID: repo.ID, GraftParents: []domain.ContentHash{base}},
	}}, memories: map[domain.ContentHash]domain.MemoryDigest{hash: memory}}}
	a := toolArgs{Ref: string(source)}
	var assembled strings.Builder
	var firstCursor string
	var projection domain.ContentHash
	for i := 0; i < 100; i++ {
		// Each page goes through a fresh MCP server instance (no sticky replica).
		raw, err := (&Server{context: f}).memoryPage(systemTestContext(), repo, a)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Fragment string             `json:"json_fragment"`
			Next     string             `json:"next_cursor"`
			Hash     domain.ContentHash `json:"projection_hash"`
			Memory   domain.ContentHash `json:"memory_hash"`
			Offset   int                `json:"byte_offset"`
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			projection = page.Hash
			firstCursor = page.Next
		}
		if page.Hash != projection || page.Memory != "" || page.Offset != assembled.Len() || len(page.Fragment) > pageBytes {
			t.Fatalf("inconsistent projection page: %s", raw)
		}
		assembled.WriteString(page.Fragment)
		if page.Next == "" {
			break
		}
		a.Cursor = page.Next
	}
	var result domain.MemoryDigest
	if err := json.Unmarshal([]byte(assembled.String()), &result); err != nil {
		t.Fatal(err)
	}
	if result.Summary != memory.Summary || result.SnapshotID != source {
		t.Fatal("projection lost Unicode or source")
	}
	if firstCursor == "" {
		t.Fatal("pagination not exercised")
	}
	cursor, err := cursorFor(string(repo.ID), "memory_load", toolArgs{Ref: string(source), Cursor: firstCursor})
	if err != nil {
		t.Fatal(err)
	}
	if cursor.Version != 2 || cursor.FragmentFormat != "memory-project-v1" {
		t.Fatal("older replicas could misread a projection cursor as stored memory")
	}
	bad := cursor
	bad.FragmentFormat = "future-format"
	if _, err := (&Server{context: f}).memoryPage(systemTestContext(), repo, toolArgs{Ref: string(source), Cursor: encodeCursor(bad)}); err == nil {
		t.Fatal("mixed projection rendering versions")
	}

	changed := memory
	changed.Summary = "new ancestor revision"
	newHash, _ := domain.MemoryDigestHash(changed)
	f.memories[newHash] = changed
	f.snapshots[repo.ID][0].MemoryHash = newHash
	a.Cursor = firstCursor
	if _, err := (&Server{context: f}).memoryPage(systemTestContext(), repo, a); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("mixed projection revisions: %v", err)
	}
	// Explicit history remains the exact archived object after later changes.
	exact, err := (&Server{context: f}).memoryPage(systemTestContext(), repo, toolArgs{Ref: string(base), MemoryHash: string(hash)})
	if err != nil || !strings.Contains(exact, string(hash)) {
		t.Fatalf("historical memory lost: %s %v", exact, err)
	}
}

func TestMemoryModeValidationAndLegacyCursor(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	id := pageHash(2)
	d := domain.MemoryDigest{SnapshotID: id, Summary: strings.Repeat("original", 3000)}
	hash, _ := domain.MemoryDigestHash(d)
	f := fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: {{ID: id, RepoID: repo.ID, MemoryHash: hash}}}, memories: map[domain.ContentHash]domain.MemoryDigest{hash: d}}
	s := &Server{context: f}
	for _, a := range []toolArgs{{Mode: "invalid"}, {Mode: "project", MemoryHash: string(hash)}} {
		if _, err := s.memoryPage(systemTestContext(), repo, a); err == nil {
			t.Fatal("invalid mode accepted")
		}
	}
	a := toolArgs{Ref: string(id)}
	cur, _ := cursorFor(string(repo.ID), "memory_load", a)
	cur.Snapshot = id
	cur.Memory = hash
	cur.Offset = pageBytes
	a.Cursor = encodeCursor(cur)
	got, err := s.memoryPage(systemTestContext(), repo, a)
	if err != nil || !strings.Contains(got, `"mode":"stored"`) {
		t.Fatalf("pre-upgrade exact cursor broken: %s %v", got, err)
	}
}

func TestMergedConversationsRemainDiscoverableAndFetchable(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	base, feature := pageHash(2), pageHash(3)
	f := pageBackend{fakeContextBackend: fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: {
		{ID: base, DocHash: base, RepoID: repo.ID}, {ID: feature, DocHash: feature, RepoID: repo.ID, GraftParents: []domain.ContentHash{base}},
	}}}, refs: []domain.Ref{{Kind: domain.RefBranch, Name: "main", Target: feature}}, docs: map[domain.ContentHash]domain.SessionDoc{}}
	for _, id := range []domain.ContentHash{base, feature} {
		f.docs[id] = domain.SessionDoc{Hash: id, CIR: domain.CIRDocument{Events: []domain.CIREvent{{Seq: 0, Kind: "message", Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "conversation " + string(id)}}}}}}
	}
	s := &Server{context: f}
	raw, err := s.contextPage(systemTestContext(), repo, toolArgs{Scope: "current", Position: "main"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []domain.ContentHash{base, feature} {
		if !strings.Contains(raw, string(id)) {
			t.Fatalf("merged context not discoverable: %s", raw)
		}
		body, err := s.eventPage(systemTestContext(), repo, toolArgs{Ref: string(id)})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(body, "conversation "+string(id)) {
			t.Fatalf("merged transcript not fetchable: %s", body)
		}
	}
}

func TestProjectMemoryCollapsesRepeatedProviderGenerationsWithoutChangingArchive(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	id := pageHash(2)
	prefix := "This session is being continued from a previous conversation that ran out of context.\n"
	d := domain.MemoryDigest{SnapshotID: id, Summary: prefix + "OLD GENERATION\n" + prefix + "LATEST DECISION"}
	hash, _ := domain.MemoryDigestHash(d)
	f := fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: {{ID: id, RepoID: repo.ID, MemoryHash: hash}}}, memories: map[domain.ContentHash]domain.MemoryDigest{hash: d}}
	server := &Server{context: f}
	got, err := server.memoryPage(systemTestContext(), repo, toolArgs{Ref: string(id)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "OLD GENERATION") || !strings.Contains(got, "LATEST DECISION") {
		t.Fatalf("recursive generation leaked into active memory: %s", got)
	}
	stored, err := server.memoryPage(systemTestContext(), repo, toolArgs{Ref: string(id), MemoryHash: string(hash)})
	if err != nil || !strings.Contains(stored, "OLD GENERATION") {
		t.Fatalf("original generation lost: %s %v", stored, err)
	}
}
