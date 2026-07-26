package providerfs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLedger fixes three rules for the excluded ledger:
// Exclude immediately after materialization → Resume (entry removal) → Permanently exclude (regeneration also).
func TestLedger(t *testing.T) {
	root := t.TempDir()
	sess := filepath.Join(root, "s.jsonl")
	if err := os.WriteFile(sess, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(sess)

	// 1) Materialization record → Exclude if same size.
	if err := RecordMaterialized(root, sess); err != nil {
		t.Fatalf("record: %v", err)
	}
	if !CaptureExcluded(root, sess, info.Size()) {
		t.Fatal("Materialized but included in capture candidates (highjacking possible)")
	}

	// 2) Resume (renewal) → Recovery + entry removal.
	if !CaptureExcluded(root, sess, info.Size()) {
		t.Fatal("Failed to reconfirm growth")
	}
	if CaptureExcluded(root, sess, info.Size()+100) {
		t.Fatal("Grown session still excluded (renewal ignored)")
	}
	if CaptureExcluded(root, sess, info.Size()) { // Entry removed, now unexcluded
		t.Fatal("Entry not removed after growth")
	}

	// 3) Isolation is permanent regardless of size and persists across path recreation.
	if err := MarkSuperseded(root, sess); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if !CaptureExcluded(root, sess, info.Size()+9999) {
		t.Fatal("Isolation session captured as paused (isolation breakdown)")
	}
}
