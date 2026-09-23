package graphwire

import (
	"encoding/json"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"strings"
	"testing"
)

func TestV2ReusesRepeatedTimelinesAndIndexesIndependentRoots(t *testing.T) {
	g := domain.GraphState{BranchSnapshots: map[string][]domain.ContentHash{}, BranchContexts: map[string]domain.BranchContext{}}
	for i := 0; i < 1000; i++ {
		g.SnapshotIDs = append(g.SnapshotIDs, domain.HashContent([]byte(fmt.Sprint(i))))
	}
	for i := 0; i < 40; i++ {
		b := fmt.Sprint("branch-", i)
		g.BranchSnapshots[b] = g.SnapshotIDs
		g.BranchContexts[b] = domain.BranchContext{SnapshotIDs: g.SnapshotIDs, Roots: g.SnapshotIDs[:1]}
	}
	g.BranchContexts["independent"] = domain.BranchContext{SnapshotIDs: []domain.ContentHash{"independent-tip"}, Roots: []domain.ContentHash{"independent-root"}}
	v1, _ := json.Marshal(Encode(g))
	v2, _ := json.Marshal(EncodeV2(g))
	again, _ := json.Marshal(EncodeV2(g))
	if string(v2) != string(again) {
		t.Fatal("encoding is nondeterministic")
	}
	if len(v2)*4 >= len(v1) {
		t.Fatalf("lost timeline deduplication: %d -> %d", len(v1), len(v2))
	}
	if strings.Contains(string(v2), `"snapshot_ids":["sha256:`) {
		t.Fatal("raw identifiers leaked through embedded fields")
	}
	wire := EncodeV2(g)
	for b, c := range g.BranchContexts {
		x := wire.BranchContexts[b]
		ids := x.SnapshotIDs
		if x.SnapshotIDsRef != "" {
			ids = wire.BranchSnapshots[x.SnapshotIDsRef]
		}
		if len(ids) != len(c.SnapshotIDs) {
			t.Fatal("changed timeline length")
		}
		for i, index := range ids {
			if wire.Dictionary[index] != c.SnapshotIDs[i] {
				t.Fatal("changed timeline")
			}
		}
		for i, index := range x.Roots {
			if wire.Dictionary[index] != c.Roots[i] {
				t.Fatal("changed root")
			}
		}
	}
}
