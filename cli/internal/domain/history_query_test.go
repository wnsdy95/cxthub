package domain

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func historyFixture() []Snapshot {
	r, f, m, u := HashContent([]byte("root")), HashContent([]byte("feature")), HashContent([]byte("merged")), HashContent([]byte("unreachable"))
	return []Snapshot{{ID: r, Branch: "main", CreatedAt: time.Unix(4, 0)}, {ID: f, Branch: "feature", Parents: []ContentHash{r}, CreatedAt: time.Unix(3, 0)}, {ID: m, Branch: "main", Parents: []ContentHash{r}, GraftParents: []ContentHash{f}, CreatedAt: time.Unix(2, 0)}, {ID: u, Branch: "main", CreatedAt: time.Unix(5, 0)}}
}
func TestHistoryAncestryUsesAllParentsAndIgnoresCaptureLabels(t *testing.T) {
	snaps := historyFixture()
	out, gaps, err := ReachableHistory(context.Background(), snaps, []ContentHash{snaps[2].ID})
	if err != nil || len(gaps) != 0 || len(out) != 3 {
		t.Fatal(out, gaps, err)
	}
	for i, want := range []ContentHash{snaps[2].ID, snaps[1].ID, snaps[0].ID} {
		if out[i].ID != want {
			t.Fatalf("order[%d]=%s want %s", i, out[i].ID, want)
		}
	}
	// Clock skew must not break a child's connection to an earlier-rendered parent.
	for i, j := 0, len(snaps)-1; i < j; i, j = i+1, j-1 {
		snaps[i], snaps[j] = snaps[j], snaps[i]
	}
	again, _, err := ReachableHistory(context.Background(), snaps, []ContentHash{out[0].ID})
	if err != nil || !reflect.DeepEqual(out, again) {
		t.Fatal("unstable ordering", err)
	}
}
func TestHistoryRejectsCorruptionAndReportsMissing(t *testing.T) {
	ctx := context.Background()
	snaps := historyFixture()
	missing := HashContent([]byte("missing"))
	snaps[0].Parents = []ContentHash{missing}
	out, gaps, err := ReachableHistory(ctx, snaps, []ContentHash{snaps[2].ID})
	if err != nil || len(out) != 3 || !reflect.DeepEqual(gaps, []ContentHash{missing}) {
		t.Fatal(out, gaps, err)
	}
	snaps[0].Parents = []ContentHash{snaps[2].ID}
	if _, _, err := ReachableHistory(ctx, snaps, []ContentHash{snaps[2].ID}); !errors.Is(err, ErrHashMismatch) {
		t.Fatal("cycle accepted", err)
	}
	snaps = append(historyFixture(), historyFixture()[0])
	if _, _, err := ReachableHistory(ctx, snaps, []ContentHash{snaps[2].ID}); !errors.Is(err, ErrHashMismatch) {
		t.Fatal("duplicate accepted", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := ReachableHistory(canceled, historyFixture(), []ContentHash{snaps[2].ID}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func BenchmarkHistoryAncestry(b *testing.B) {
	for _, n := range []int{100, 10000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			snaps := make([]Snapshot, n)
			for i := range snaps {
				snaps[i].ID = HashContent([]byte(fmt.Sprint(i)))
				if i > 0 {
					snaps[i].Parents = []ContentHash{snaps[i-1].ID}
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := ReachableHistory(context.Background(), snaps, []ContentHash{snaps[n-1].ID}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
