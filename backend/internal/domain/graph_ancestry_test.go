package domain

import (
	"math/rand"
	"reflect"
	"testing"
)

func TestGraphAncestryBitsetsMatchNaturalAndGraftTraversal(t *testing.T) {
	random := rand.New(rand.NewSource(42))
	snapshots := []Snapshot{}
	for i := 0; i < 513; i++ {
		s := graphFixture(string(rune(i + 1000)))
		if i > 0 {
			s.Parents = []ContentHash{snapshots[random.Intn(i)].ID}
		}
		if i > 1 && i%3 == 0 {
			s.GraftParents = []ContentHash{snapshots[random.Intn(i)].ID}
		}
		if i%67 == 0 {
			s.GraftParents = append(s.GraftParents, HashContent([]byte("missing")))
		}
		snapshots = append(snapshots, s)
	}
	index, err := newGraphStateIndex(snapshots)
	if err != nil {
		t.Fatal(err)
	}
	for _, cached := range []bool{true, false} {
		if !cached {
			index.closure = map[ContentHash]graphClosure{}
			index.closureWeight = 8 << 20
		}
		for _, s := range snapshots {
			want, complete := contextEvidenceClosure(index.byID, s.ID, false)
			got := index.from(s.ID)
			if !reflect.DeepEqual(got.ids.list(), graphIDs(want)) || got.complete != complete {
				t.Fatalf("closure changed for %s cached=%v", s.ID, cached)
			}
			for _, candidate := range snapshots {
				if got.ids.has(candidate.ID) != want[candidate.ID] {
					t.Fatal("membership disagrees")
				}
			}
			if got.ids.has("absent") {
				t.Fatal("unknown object was included")
			}
		}
		if !cached && len(index.closure) != 0 {
			t.Fatal("cache exceeded its byte budget")
		}
	}
	if got := index.from("missing"); got.complete || len(got.ids.list()) != 0 {
		t.Fatal("unknown root is not a complete closure")
	}
}
