package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestSyncStatusReadsQueuesWithoutAuditingObjectsOrMutating(t *testing.T) {
	cwd, _, st, repo, id := historyFixture(t)
	ctx := context.Background()
	snap, err := st.GetSnapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.StageBackfills(ctx, repo, []domain.Snapshot{snap}); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListBackfills(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	next := jobs[0]
	next.Version++
	next.Attempts++
	next.Reason = "unavailable"
	next.NextAttempt = time.Now().UTC().Add(time.Hour)
	if err = st.UpdateBackfill(ctx, jobs[0], &next); err != nil {
		t.Fatal(err)
	}
	// The cheap command must not decode this document, and must not claim
	// integrity or server acknowledgement when only queue metadata was read.
	if err = os.WriteFile(filepath.Join(cwd, ".cxt", "objects", "docs", strings.TrimPrefix(string(id), "sha256:")), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	before := treeBytes(t, cwd)
	var out bytes.Buffer
	if err = RunSyncStatus(ctx, cwd, true, &out); err != nil {
		t.Fatal(err)
	}
	var report syncStatus
	if err = json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.ServerChecked || report.IntegrityChecked || report.Ready != 0 || report.RetryWaiting != 1 || len(report.Historical) != 1 || report.Historical[0] != next {
		t.Fatalf("incorrect status: %+v", report)
	}
	if !reflect.DeepEqual(before, treeBytes(t, cwd)) {
		t.Fatal("status mutated the replica")
	}
	if err = os.Rename(filepath.Join(cwd, ".cxt"), filepath.Join(cwd, "preserved-replica")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err = RunSyncStatus(ctx, cwd, true, &out); err == nil {
		t.Fatal("missing replica reported empty/healthy")
	}
	if _, err = os.Stat(filepath.Join(cwd, ".cxt")); !os.IsNotExist(err) {
		t.Fatal("status recreated replica")
	}
}

func TestSyncStatusReportsCorruptQueueWithoutRepair(t *testing.T) {
	cwd, _, st, repo, id := historyFixture(t)
	ctx := context.Background()
	snap, _ := st.GetSnapshot(ctx, id)
	if err := st.StageBackfills(ctx, repo, []domain.Snapshot{snap}); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(cwd, ".cxt", "historical-backfill", "*", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	if err = os.WriteFile(files[0], []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	before := treeBytes(t, cwd)
	var out bytes.Buffer
	if err = RunSyncStatus(ctx, cwd, true, &out); err == nil {
		t.Fatal("corrupt queue reported as empty")
	}
	var report syncStatus
	if err = json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Issues) == 0 || report.Historical == nil {
		t.Fatal("missing explicit failure", report)
	}
	if !reflect.DeepEqual(before, treeBytes(t, cwd)) {
		t.Fatal("status repaired corrupt queue")
	}
}

func TestSyncStatusArguments(t *testing.T) {
	for _, args := range [][]string{{"cxt", "sync", "status"}, {"cxt", "sync", "status", "--json"}, {"cxt", "sync", "--json", "status"}} {
		if _, err := PreflightArgs(args); err != nil {
			t.Fatal(args, err)
		}
	}
	for _, args := range [][]string{{"cxt", "sync"}, {"cxt", "sync", "repair"}, {"cxt", "sync", "status", "--force"}, {"cxt", "sync", "status", "push"}} {
		if _, err := PreflightArgs(args); err == nil {
			t.Fatal("unexpected status arguments accepted", args)
		}
	}
}
