package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestPushCatalogRetainsWorkingSelectionsAndExistingOrdering(t *testing.T) {
	ctx := context.Background()
	st := NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte(t.Name())))
	id := domain.HashContent([]byte("snapshot"))
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	position := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: repo, Kind: "position", Branch: "main", BranchID: "main", Source: id, Target: id, GitAfter: strings.Repeat("1", 40), WorktreeID: strings.Repeat("b", 32), CreatedAt: time.Now().UTC()}
	// A selection can be journaled only in position.json. Preserve the same
	// history input and ordering before the application orders publications.
	publish := position
	publish.ID = strings.Repeat("c", 32)
	publish.Kind = "publish"
	publish.CreatedAt = position.CreatedAt.Add(-time.Hour)
	if err := st.PutHistoryEvent(ctx, publish); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(domain.WorkingPosition{RepoID: repo, WorktreeID: position.WorktreeID, Selection: &position})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(st.storeDir(), "worktrees", position.WorktreeID, "position.json"), raw); err != nil {
		t.Fatal(err)
	}
	want, err := st.ListHistoryEvents(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	man, got, err := st.ReadPushCatalog(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog lost selections/order: %+v want %+v", got, want)
	}
	if len(man.SnapshotIndex) != 1 || man.SnapshotIndex[0] != id {
		t.Fatal("catalog lost dependencies")
	}
}
