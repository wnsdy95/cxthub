package mcp

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"strings"
	"testing"
)

type evidenceQuery struct{ job domain.GitChangeJob }

func (q *evidenceQuery) Get(ctx context.Context, repo domain.ContentHash, id string) (domain.GitChangeJob, error) {
	if repo != q.job.RepoID || id != q.job.ID {
		return domain.GitChangeJob{}, domain.ErrNotFound
	}
	return q.job, nil
}
func (q *evidenceQuery) List(ctx context.Context, repo domain.ContentHash, cursor string, limit int) (domain.GitChangePage, error) {
	if repo != q.job.RepoID {
		return domain.GitChangePage{}, domain.ErrNotFound
	}
	return domain.GitChangePage{Items: []domain.GitChangeSummary{q.job.Summary()}}, nil
}
func TestGitChangesMCPBoundedFragmentsAndSelectionBinding(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	q := &evidenceQuery{job: domain.GitChangeJob{ID: strings.Repeat("a", 64), RepoID: repo.ID, State: "completed", Result: &domain.GitReversalEvidence{Coverage: "partial", Paths: []string{strings.Repeat("\ubcc0\uacbd\ub41c-\ud30c\uc77c/", 5000)}, UnverifiedPaths: []string{"needs-review.go"}}}}
	s := &Server{}
	s.SetGitChanges(q)
	a := toolArgs{Repository: string(repo.ID), ChangeID: q.job.ID}
	var assembled strings.Builder
	firstCursor := ""
	count := 0
	for {
		raw, err := s.gitChangesPage(context.Background(), repo, a)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > pageBytes*2 {
			t.Fatalf("unbounded page %d", len(raw))
		}
		var page struct {
			Fragment string `json:"json_fragment"`
			Next     string `json:"next_cursor"`
			Offset   int    `json:"byte_offset"`
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		if page.Offset != assembled.Len() {
			t.Fatal("fragment skipped or duplicated bytes")
		}
		assembled.WriteString(page.Fragment)
		count++
		if firstCursor == "" {
			firstCursor = page.Next
		}
		if page.Next == "" {
			break
		}
		a.Cursor = page.Next
	}
	expected, _ := json.Marshal(q.job)
	if assembled.String() != string(expected) || count < 2 {
		t.Fatal("evidence cannot reassemble exactly")
	}
	a.Cursor = firstCursor
	a.ChangeID = strings.Repeat("b", 64)
	if _, err := s.gitChangesPage(context.Background(), repo, a); err == nil {
		t.Fatal("cursor rebound to another proof")
	}
	a.ChangeID = q.job.ID
	q.job.Version++
	if _, err := s.gitChangesPage(context.Background(), repo, a); err == nil {
		t.Fatal("mixed mutable verification generations")
	}
	raw, err := s.gitChangesPage(context.Background(), repo, toolArgs{Repository: string(repo.ID)})
	if err != nil || strings.Contains(raw, "\ubcc0\uacbd\ub41c") || !strings.Contains(raw, "partial") {
		t.Fatalf("summary leaked unbounded paths %d %v", len(raw), err)
	}
}

type scanQuery struct{ repo domain.ContentHash }

func (q scanQuery) ListScans(ctx context.Context, repo domain.ContentHash, cursor string, limit int) (domain.GitScanPage, error) {
	if repo != q.repo || limit > 20 {
		return domain.GitScanPage{}, domain.ErrForbidden
	}
	return domain.GitScanPage{Items: []domain.GitScanJob{{ID: strings.Repeat("a", 64), Commit: strings.Repeat("b", 40), State: "retrying", Reason: "temporary_provider_or_storage_failure"}}, NextCursor: strings.Repeat("a", 64)}, nil
}
func TestGitObservationsMCPReadOnlyCursorBoundToRepository(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	s := &Server{}
	s.SetGitScans(scanQuery{repo: repo.ID})
	a := toolArgs{Repository: string(repo.ID)}
	raw, err := s.gitScansPage(context.Background(), repo, a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "retrying") || strings.Contains(raw, "git_origin") {
		t.Fatal(raw)
	}
	var page struct {
		Cursor string `json:"next_cursor"`
	}
	json.Unmarshal([]byte(raw), &page)
	if page.Cursor == "" {
		t.Fatal("lost pagination")
	}
	a.Cursor = page.Cursor
	if _, err = s.gitScansPage(context.Background(), domain.Repo{ID: pageHash(2)}, a); err == nil {
		t.Fatal("cross repository cursor accepted")
	}
}
