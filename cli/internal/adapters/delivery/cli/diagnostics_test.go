package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func treeBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = fmt.Sprintf("%x", sha256.Sum256(raw))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDoctorReportsRetainedUploadsWithoutMutation(t *testing.T) {
	cwd, _, st, repo, id := historyFixture(t)
	ctx := context.Background()
	snap, err := st.GetSnapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.StageBackfills(ctx, repo, []domain.Snapshot{snap}); err != nil {
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
	next.NextAttempt = time.Now().UTC().Add(time.Minute)
	if err := st.UpdateBackfill(ctx, jobs[0], &next); err != nil {
		t.Fatal(err)
	}
	before := treeBytes(t, cwd)
	var out bytes.Buffer
	if err := RunDiagnostics(ctx, cwd, []string{"doctor", "--json"}, &out); err == nil {
		t.Fatal("failed upload not reported")
	}
	var report struct {
		Backfills []domain.SnapshotBackfill `json:"historical_uploads"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Backfills) != 1 || report.Backfills[0] != next {
		t.Fatal(report)
	}
	if !reflect.DeepEqual(before, treeBytes(t, cwd)) {
		t.Fatal("doctor mutated durable queue")
	}
}

func TestDiagnosticsSurvivesDamagedReplicaWithoutReplayOrWrites(t *testing.T) {
	cwd, c, _, _, source := historyFixture(t)
	code := gitOut(cwd, "rev-parse", "HEAD")
	if err := runBirthVote(t, cwd, c, "prepared", strings.Repeat("0", 40)+" "+code+" refs/heads/queued"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cwd, ".cxt", "objects", "docs", strings.TrimPrefix(string(source), "sha256:"))
	if err := os.WriteFile(path, []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	before := treeBytes(t, cwd)
	var out bytes.Buffer
	if err := RunDiagnostics(context.Background(), cwd, []string{"doctor", "--json"}, &out); err == nil {
		t.Fatal("damaged replica reported healthy")
	}
	if !strings.Contains(out.String(), "needs-git-evidence") || !strings.Contains(out.String(), "document") {
		t.Fatalf("missing diagnostics: %s", out.String())
	}
	out.Reset()
	if err := RunDiagnostics(context.Background(), cwd, []string{"branch", "operations", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not-checked") {
		t.Fatal("local phase confused with server acknowledgement")
	}
	if after := treeBytes(t, cwd); !reflect.DeepEqual(before, after) {
		t.Fatal("read-only diagnostics changed files or replayed an operation")
	}
	if err := os.Rename(filepath.Join(cwd, ".cxt"), filepath.Join(cwd, "saved-replica")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := RunDiagnostics(context.Background(), cwd, []string{"branch", "operations", "--json"}, &out); err != nil || !strings.Contains(out.String(), "queued") {
		t.Fatalf("Git journal unavailable after replica loss: %s %v", out.String(), err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".cxt")); !os.IsNotExist(err) {
		t.Fatal("diagnostics recreated .cxt")
	}
}

func TestDiagnosticArgumentsRejectAccidentalMutations(t *testing.T) {
	for _, args := range [][]string{{"cxt", "doctor", "--json"}, {"cxt", "branch", "operations", "--json"}, {"cxt", "branch", "replay"}} {
		if _, err := PreflightArgs(args); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"cxt", "doctor", "repair"}, {"cxt", "branch", "replay", "--json"}, {"cxt", "branch", "operations", "--provider", "claude"}, {"cxt", "branch", "archive", "main", "--json"}} {
		if _, err := PreflightArgs(args); err == nil {
			t.Fatalf("unexpected arguments accepted: %v", args)
		}
	}
}
