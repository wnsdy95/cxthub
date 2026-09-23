package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"reflect"
	"strings"
	"testing"
)

type effectiveQuery struct {
	repo     domain.ContentHash
	state    domain.ContentHash
	items    []domain.EffectiveMemoryItem
	received []domain.EffectiveMemoryRequest
}

func (q *effectiveQuery) QueryEffectiveMemory(_ context.Context, repo domain.ContentHash, in domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	if repo != q.repo {
		return domain.EffectiveMemoryPage{}, domain.ErrForbidden
	}
	if in.Selection.Validate() != nil {
		return domain.EffectiveMemoryPage{}, domain.ErrValidation
	}
	q.received = append(q.received, in)
	i := 0
	if in.Cursor != "" {
		if _, err := fmt.Sscanf(in.Cursor, "next-%d", &i); err != nil {
			return domain.EffectiveMemoryPage{}, err
		}
	}
	out := domain.EffectiveMemoryPage{Selection: in.Selection, StateHash: q.state, LineageHash: pageHash(6), Items: []domain.EffectiveMemoryItem{}, Total: len(q.items)}
	if i < len(q.items) {
		out.Items = append(out.Items, q.items[i])
		if i+1 < len(q.items) {
			out.NextCursor = fmt.Sprintf("next-%d", i+1)
		}
	}
	return out, nil
}
func TestEffectiveMemoryMCPFragmentsAndSelection(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	id := pageHash(2)
	f := fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: {{ID: id, RepoID: repo.ID}}}}
	q := &effectiveQuery{repo: repo.ID, state: pageHash(3), items: []domain.EffectiveMemoryItem{
		{ID: pageHash(4), SourceSnapshot: id, Kind: "code", Text: strings.Repeat("\ud55c\uad6d\uc5b4\n\"", 2000), MemoryClaimAssessment: domain.MemoryClaimAssessment{State: "inactive", Reason: "declared_scope_before"}},
		{ID: pageHash(5), SourceSnapshot: id, Kind: "rationale", Text: "Retained reason", MemoryClaimAssessment: domain.MemoryClaimAssessment{State: "retained", Reason: "historical_knowledge"}}}}
	a := toolArgs{Mode: "effective", Ref: string(id), CodeCommit: strings.Repeat("a", 40), MemoryHash: string(pageHash(7))}
	var joined strings.Builder
	var got []domain.EffectiveMemoryItem
	first := ""
	for i := 0; i < 100; i++ {
		s := &Server{context: f}
		s.SetEffectiveMemory(q)
		raw, err := s.memoryPage(systemTestContext(), repo, a)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > pageBytes {
			t.Fatal("MCP byte bound", len(raw))
		}
		var page struct {
			Fragment string `json:"json_fragment"`
			Next     string `json:"next_cursor"`
			Offset   int    `json:"byte_offset"`
			Complete bool   `json:"item_complete"`
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		if page.Offset != joined.Len() {
			t.Fatal("offset mismatch")
		}
		joined.WriteString(page.Fragment)
		if page.Complete {
			var item domain.EffectiveMemoryItem
			if err := json.Unmarshal([]byte(joined.String()), &item); err != nil {
				t.Fatal(err)
			}
			got = append(got, item)
			joined.Reset()
		}
		if i == 0 {
			first = page.Next
		}
		if page.Next == "" {
			break
		}
		a.Cursor = page.Next
	}
	if !reflect.DeepEqual(got, q.items) || first == "" {
		t.Fatal("adapter changed/truncated assessment", len(got))
	}
	for _, in := range q.received {
		if in.Limit != 1 || in.Selection.SnapshotID != id || in.Selection.CodeCommit != a.CodeCommit || string(in.Selection.MemoryHash) != a.MemoryHash {
			t.Fatal("selection changed", in)
		}
	}
	s := &Server{context: f, effectiveMemory: q}
	a.Cursor = first
	changed := a
	changed.CodeCommit = strings.Repeat("b", 40)
	if _, err := s.memoryPage(systemTestContext(), repo, changed); err == nil {
		t.Fatal("cursor accepted different code")
	}
	changed = a
	changed.CodeCommit = ""
	changed.Cursor = ""
	if _, err := s.memoryPage(systemTestContext(), repo, changed); err == nil {
		t.Fatal("code position inferred")
	}
	q.state = pageHash(8)
	if _, err := s.memoryPage(systemTestContext(), repo, a); err == nil {
		t.Fatal("mixed evidence generations")
	}
}
