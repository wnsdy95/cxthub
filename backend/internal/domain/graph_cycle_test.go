package domain

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestAcyclicAncestryMatchesTransitiveOracle(t *testing.T) {
	ids := []ContentHash{"a", "b", "c"}
	// Exhaust every directed graph of three nodes. Natural, overlay, and
	// duplicate mixed representations must have the same cycle semantics.
	for mask := 0; mask < 1<<9; mask++ {
		var reach [3][3]bool
		for i := range ids {
			for j := range ids {
				reach[i][j] = mask&(1<<(i*3+j)) != 0
			}
		}
		for k := range ids {
			for i := range ids {
				for j := range ids {
					reach[i][j] = reach[i][j] || (reach[i][k] && reach[k][j])
				}
			}
		}
		cycle := reach[0][0] || reach[1][1] || reach[2][2]
		for mode := 0; mode < 3; mode++ {
			snaps := make(map[ContentHash]Snapshot)
			for i, id := range ids {
				s := Snapshot{ID: id, Parents: []ContentHash{"missing"}}
				for j, parent := range ids {
					if mask&(1<<(i*3+j)) == 0 {
						continue
					}
					if mode != 1 {
						s.Parents = append(s.Parents, parent, parent)
					}
					if mode != 0 {
						s.GraftParents = append(s.GraftParents, parent, parent)
					}
				}
				snaps[id] = s
			}
			// An unrelated root cannot hide a cycle in another component.
			snaps["unrelated"] = Snapshot{ID: "unrelated"}
			err := ValidateAcyclicAncestry(context.Background(), snaps)
			if errors.Is(err, ErrIntegrity) != cycle || (!cycle && err != nil) {
				t.Fatalf("mask=%d mode=%d cycle=%v err=%v", mask, mode, cycle, err)
			}
		}
	}
}

func TestAcyclicAncestryDeepChainAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ValidateAcyclicAncestry(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := ValidateAcyclicAncestry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	snaps := make(map[ContentHash]Snapshot, 20000)
	for i := 0; i < 20000; i++ {
		id := ContentHash(fmt.Sprint(i))
		snaps[id] = Snapshot{ID: id, Parents: []ContentHash{ContentHash(fmt.Sprint(i - 1))}}
	}
	if err := ValidateAcyclicAncestry(context.Background(), snaps); err != nil {
		t.Fatal(err)
	}
	snaps["0"] = Snapshot{ID: "0", GraftParents: []ContentHash{"19999"}}
	if err := ValidateAcyclicAncestry(context.Background(), snaps); !errors.Is(err, ErrIntegrity) {
		t.Fatal("long cycle admitted", err)
	}
}
