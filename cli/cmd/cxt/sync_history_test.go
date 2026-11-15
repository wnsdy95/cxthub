package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/storage"
)

// The production entry point answers sync commands before building any
// container, so pausing must be exercised through run, not RunHistoricalUploadsPause.
func TestSyncHistoryPauseThroughProductionDispatch(t *testing.T) {
	root := bindRepairFixture(t, "https://example.test/team/repository")
	if err := os.MkdirAll(filepath.Join(root, ".cxt"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st := storage.NewFileStore(root)
	if err := run([]string{"cxt", "sync", "status", "--json"}); err != nil {
		t.Fatal(err)
	}
	controls := filepath.Join(root, ".cxt", "historical-uploads")
	if _, err := os.Stat(controls); !os.IsNotExist(err) {
		t.Fatal("status wrote upload control", err)
	}
	if err := run([]string{"cxt", "sync", "pause-history"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(controls)
	if err != nil || len(entries) != 1 {
		t.Fatal("pause wrote no control", entries, err)
	}
	repo := "sha256:" + entries[0].Name()[:len(entries[0].Name())-len(".json")]
	control, err := st.HistoricalUploadControl(ctx, repo)
	if err != nil || !control.Paused() {
		t.Fatal("pause did not pause", control, err)
	}
	first := control.PausedAt
	if err := run([]string{"cxt", "sync", "pause-history"}); err != nil {
		t.Fatal(err)
	}
	if control, err = st.HistoricalUploadControl(ctx, repo); err != nil || !control.PausedAt.Equal(first) {
		t.Fatal("repeated pause moved the pause time", control, first, err)
	}
	if err := run([]string{"cxt", "sync", "resume-history"}); err != nil {
		t.Fatal(err)
	}
	if control, err = st.HistoricalUploadControl(ctx, repo); err != nil || control.Paused() {
		t.Fatal("resume left uploads paused", control, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".cxt", "historical-backfill")); !os.IsNotExist(err) {
		t.Fatal("pause or resume created queue records", err)
	}
}
