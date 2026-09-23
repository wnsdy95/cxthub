package mcp

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"strings"
	"testing"
	"time"
)

type branchPageBackend struct {
	pageBackend
	ref        domain.Ref
	calls      int
	code       string
	generation string
	requested  []domain.ContextSelection
}

func (f *branchPageBackend) ListRefs(context.Context, domain.ContentHash) ([]domain.Ref, error) {
	return []domain.Ref{f.ref}, nil
}
func (f *branchPageBackend) QueryBranchMemory(_ context.Context, _ domain.ContentHash, branch string, snapshot domain.ContentHash, code string) (domain.MemoryProjection, error) {
	f.calls++
	f.code = code
	if branch != "main" || snapshot != f.ref.Target {
		panic("wrong branch selection")
	}
	return domain.MemoryProjection{Found: true, StateHash: pageHash(9), Digest: domain.MemoryDigest{SnapshotID: snapshot, Summary: "retained PR contribution"}, Inclusion: &domain.BranchContext{CodeCommit: code, Merges: []domain.BranchContextMerge{{State: "included"}}}}, nil
}
func (f *branchPageBackend) QueryContext(_ context.Context, _ domain.ContentHash, in domain.ContextSelection) (domain.ContextQueryView, error) {
	f.requested = append(f.requested, in)
	return domain.ContextQueryView{Branch: "main", Position: f.ref.Target, StateHash: domain.HashContent([]byte(f.generation)), Snapshots: []domain.Snapshot{{ID: pageHash(3), DocHash: pageHash(3), Message: "needle one", CreatedAt: time.Unix(1, 0)}, {ID: pageHash(4), DocHash: pageHash(4), Message: "needle two", CreatedAt: time.Unix(3, 0)}, {ID: pageHash(5), DocHash: pageHash(5), Message: "needle three", CreatedAt: time.Unix(2, 0)}}}, nil
}
func TestMemoryLoadBranchUsesCloudInclusionButHashReadsArchive(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1), DefaultBranch: "main"}
	id := pageHash(2)
	f := &branchPageBackend{ref: domain.Ref{Kind: domain.RefBranch, Name: "main", Target: id}, pageBackend: pageBackend{fakeContextBackend: fakeContextBackend{snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: {{ID: id, RepoID: repo.ID}}}}}}
	s := &Server{context: f}
	code := ""
	raw, err := s.memoryPage(systemTestContext(), repo, toolArgs{Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 || f.code != code || !strings.Contains(raw, "retained PR contribution") || !strings.Contains(raw, "inclusion") {
		t.Fatal(raw, f.calls)
	}
	if _, err := s.memoryPage(systemTestContext(), repo, toolArgs{Ref: string(id)}); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatal("explicit snapshot became branch aggregate")
	}
}
func TestContextListUsesGitOrderCursorInsteadOfCaptureClock(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	f := &branchPageBackend{ref: domain.Ref{Target: pageHash(2)}, generation: "one"}
	s := &Server{context: f}
	a := toolArgs{Scope: "current", Position: "main", Limit: 1}
	var ids []domain.ContentHash
	var first string
	for i := 0; i < 3; i++ {
		raw, err := s.contextPage(systemTestContext(), repo, a)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Snapshots []domain.Snapshot `json:"snapshots"`
			Next      string            `json:"next_cursor"`
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Snapshots) != 1 {
			t.Fatal(raw)
		}
		ids = append(ids, page.Snapshots[0].ID)
		a.Cursor = page.Next
		if i == 0 {
			first = a.Cursor
		}
	}
	if ids[0] != pageHash(3) || ids[1] != pageHash(4) || ids[2] != pageHash(5) || a.Cursor != "" {
		t.Fatal(ids, a.Cursor)
	}
	f.generation = "two"
	a.Cursor = first
	if _, err := s.contextPage(systemTestContext(), repo, a); err == nil {
		t.Fatal("mixed timeline generations accepted")
	}
}

type indexedBranchBackend struct{ *branchPageBackend }

func (b indexedBranchBackend) ReadDocEvents(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int, int) (domain.DocEventPage, error) {
	return domain.DocEventPage{}, nil
}
func (b indexedBranchBackend) SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error) {
	return nil, nil
}
func TestBranchSearchKeepsIncludedSourcesAcrossPages(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		f := &branchPageBackend{ref: domain.Ref{Target: pageHash(2)}, generation: "one", pageBackend: pageBackend{docs: map[domain.ContentHash]domain.SessionDoc{pageHash(3): {}, pageHash(4): {}, pageHash(5): {}}}}
		s := &Server{context: f}
		if indexed {
			s.context = indexedBranchBackend{f}
		}
		a := toolArgs{Scope: "current", Position: "main", Query: "needle", Limit: 1}
		var found []domain.ContentHash
		for calls := 0; calls < 10; calls++ {
			raw, err := s.searchPage(systemTestContext(), domain.Repo{ID: pageHash(1)}, a)
			if err != nil {
				t.Fatal(err)
			}
			var page struct {
				Hits []struct {
					Snapshot domain.ContentHash `json:"snapshot_id"`
				} `json:"hits"`
				Next string `json:"next_cursor"`
			}
			if err = json.Unmarshal([]byte(raw), &page); err != nil {
				t.Fatal(err)
			}
			for _, h := range page.Hits {
				found = append(found, h.Snapshot)
			}
			if page.Next == "" {
				break
			}
			a.Cursor = page.Next
		}
		if len(found) != 3 || found[0] != pageHash(3) || found[1] != pageHash(4) || found[2] != pageHash(5) {
			t.Fatal("clock order hid included search results", indexed, found)
		}
		for _, q := range f.requested[1:] {
			if q.Branch != "main" {
				t.Fatal("cursor lost branch identity")
			}
		}
		f.generation = "changed"
		if _, err := s.searchPage(systemTestContext(), domain.Repo{ID: pageHash(1)}, a); err == nil {
			t.Fatal("changed inclusion accepted")
		}
	}
}
