package graphwire

import (
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"reflect"
	"testing"
)

func TestIndexedStatePreservesAllIdentifierSets(t *testing.T) {
	g := domain.GraphState{Version: 1, SnapshotIDs: []domain.ContentHash{"b", "a"}, GraphIDs: []domain.ContentHash{"a"},
		BranchSnapshots: map[string][]domain.ContentHash{"z": {"missing-z", "a"}, "a": {"missing-a"}},
		Hold:            []domain.GraphHoldCluster{{IDs: []domain.ContentHash{"b", "a"}}},
		Previous:        []domain.GraphProgressGroup{{Key: "rewind", Before: "b", After: "a", SnapshotIDs: []domain.ContentHash{"b"}, CollapsibleIDs: []domain.ContentHash{"b"}}},
	}
	source := reflect.ValueOf(&g).Elem()
	hashSlice := reflect.TypeOf([]domain.ContentHash{})
	for i := 0; i < source.NumField(); i++ {
		if source.Field(i).Type() == hashSlice && source.Field(i).IsNil() {
			source.Field(i).Set(reflect.ValueOf([]domain.ContentHash{"b", "a"}))
		}
	}
	first, err := json.Marshal(Encode(g))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		next, _ := json.Marshal(Encode(g))
		if string(next) != string(first) {
			t.Fatal("map iteration changed encoding")
		}
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(first, &raw); err != nil {
		t.Fatal(err)
	}
	var dict []domain.ContentHash
	if err = json.Unmarshal(raw["dictionary"], &dict); err != nil {
		t.Fatal(err)
	}
	decode := func(data json.RawMessage) []domain.ContentHash {
		t.Helper()
		var indices []uint32
		if e := json.Unmarshal(data, &indices); e != nil {
			t.Fatalf("identifier set wasn't encoded: %s: %v", data, e)
		}
		ids := []domain.ContentHash{}
		for _, i := range indices {
			if int(i) >= len(dict) {
				t.Fatal("invalid dictionary index")
			}
			ids = append(ids, dict[i])
		}
		return ids
	}
	for i := 0; i < source.NumField(); i++ {
		if source.Field(i).Type() == hashSlice {
			key := source.Type().Field(i).Tag.Get("json")
			if !reflect.DeepEqual(decode(raw[key]), source.Field(i).Interface()) {
				t.Fatalf("lost %s", key)
			}
		}
	}
	var branches map[string]json.RawMessage
	_ = json.Unmarshal(raw["branch_snapshots"], &branches)
	for key, ids := range g.BranchSnapshots {
		if !reflect.DeepEqual(decode(branches[key]), ids) {
			t.Fatalf("lost branch %s", key)
		}
	}
	var previous []map[string]json.RawMessage
	_ = json.Unmarshal(raw["previous"], &previous)
	if !reflect.DeepEqual(decode(previous[0]["snapshot_ids"]), g.Previous[0].SnapshotIDs) || string(previous[0]["before"]) != `"b"` {
		t.Fatal("progress metadata changed")
	}
	var hold []map[string]json.RawMessage
	_ = json.Unmarshal(raw["hold"], &hold)
	if !reflect.DeepEqual(decode(hold[0]["ids"]), g.Hold[0].IDs) {
		t.Fatal("lost hold ids")
	}
	// The adapter must neither rewrite nor share mutations into the domain model.
	encoded := Encode(g)
	encoded.SnapshotIDs[0] = 99
	if g.SnapshotIDs[0] != "b" {
		t.Fatal("encoder mutated domain identifiers")
	}
}
