package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestActivitySerializesEmptyCollectionsAsArrays(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	svc := NewService(st, st, nil, nil, st)

	commitWorkspace := domain.Workspace{
		ID:            "ws_commit",
		Name:          "Commit only",
		Slug:          "commit-only",
		OwnerUsername: "alice",
	}
	createdWorkspace := domain.Workspace{
		ID:            "ws_created",
		Name:          "Created only",
		Slug:          "created-only",
		OwnerUsername: "alice",
		CreatedAt:     time.Date(2026, time.July, 3, 0, 0, 0, 0, time.UTC),
	}
	repoID := domain.HashContent([]byte("activity-repo"))
	if _, err := st.PutRepo(ctx, domain.Repo{
		ID:          repoID,
		WorkspaceID: commitWorkspace.ID,
	}); err != nil {
		t.Fatal(err)
	}
	snapshotID := domain.HashContent([]byte("activity-snapshot"))
	if err := st.PutSnapshot(ctx, domain.Snapshot{
		ID:        snapshotID,
		RepoID:    repoID,
		DocHash:   snapshotID,
		CreatedAt: time.Date(2026, time.August, 4, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	months, err := svc.Activity(ctx, []domain.Workspace{commitWorkspace, createdWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	if len(months) != 2 {
		t.Fatalf("months = %d, want 2", len(months))
	}
	byMonth := make(map[string]domain.ActivityMonth, len(months))
	for _, month := range months {
		byMonth[month.Month] = month
	}
	if got := byMonth["2026-08"].Created; got == nil || len(got) != 0 {
		t.Fatalf("commit-only created = %#v, want non-nil empty slice", got)
	}
	if got := byMonth["2026-07"].CommitRepos; got == nil || len(got) != 0 {
		t.Fatalf("created-only commit_repos = %#v, want non-nil empty slice", got)
	}

	encoded, err := json.Marshal(months)
	if err != nil {
		t.Fatal(err)
	}
	jsonText := string(encoded)
	for _, forbidden := range []string{`"created":null`, `"commit_repos":null`} {
		if strings.Contains(jsonText, forbidden) {
			t.Fatalf("activity response contains %s: %s", forbidden, jsonText)
		}
	}
}
