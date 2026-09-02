package cli

import (
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
)

func TestFormatFsckReportDoesNotMislabelUnreachableSnapshotsAsLost(t *testing.T) {
	rep := backendclient.FsckReport{
		Total:       4,
		Reachable:   2,
		Roots:       []string{"root"},
		Unreachable: []string{"superseded-capture"},
	}

	got := formatFsckReport(rep)
	for _, want := range []string{
		"Snapshots 4 · Reach 2 · Roots 1 · Unreachable 1 · Missing 0",
		"unreachable (unreferenced): superseded-capture",
		"unreachable snapshots are preserved",
		"No missing-parent corruption",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("fsck output missing %q:\n%s", want, got)
		}
	}
	for _, misleading := range []string{"Orphans", "orphan (", "lost"} {
		if strings.Contains(got, misleading) {
			t.Fatalf("fsck output retained misleading label %q:\n%s", misleading, got)
		}
	}
}

func TestFormatFsckReportKeepsMissingParentsExplicitlyCorrupt(t *testing.T) {
	rep := backendclient.FsckReport{Total: 2, Reachable: 1}
	rep.DanglingParents = append(rep.DanglingParents, struct {
		Snapshot string `json:"snapshot"`
		Missing  string `json:"missing"`
	}{Snapshot: "child", Missing: "absent-parent"})

	got := formatFsckReport(rep)
	if !strings.Contains(got, "corrupt (missing parent): child → absent-parent") {
		t.Fatalf("fsck output did not preserve corruption warning:\n%s", got)
	}
	if strings.Contains(got, "No missing-parent corruption") {
		t.Fatalf("fsck output contradicted missing parent:\n%s", got)
	}
}

func TestFormatFsckReportCleanRepository(t *testing.T) {
	got := formatFsckReport(backendclient.FsckReport{Total: 3, Reachable: 3})
	want := "Snapshots 3 · Reach 3 · Roots 0 · Unreachable 0 · Missing 0\n" +
		"  ✓ No issues — all snapshots are referenced and no parents are missing\n"
	if got != want {
		t.Fatalf("clean fsck output:\n%s\nwant:\n%s", got, want)
	}
}
