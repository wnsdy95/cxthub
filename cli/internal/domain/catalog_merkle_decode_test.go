package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCatalogMerkleDecodeStrictEnvelope(t *testing.T) {
	repo, entries := catalogMerkleFixture(t)
	root, _ := catalogMerkleMustBuild(t, repo, entries)
	page := catalogMerklePage(t, root, root)
	raw := string(catalogMerkleRaw(t, page))
	decoded, err := DecodeCatalogMerklePage([]byte(raw))
	if err != nil || !reflect.DeepEqual(decoded, page) {
		t.Fatalf("valid root: %v", err)
	}
	for _, tc := range []struct{ name, wire string }{
		{"null", `null`}, {"array", `[]`}, {"string", `"page"`},
		{"trailing_object", raw + `{}`}, {"trailing_null", raw + `null`}, {"trailing_garbage", raw + `!`},
		{"unknown", `{"future":true,` + raw[1:]},
		{"duplicate", `{"version":1,` + raw[1:]},
		{"escaped_duplicate", `{"\u0076ersion":1,` + raw[1:]},
		{"case_alias", strings.Replace(raw, `"version":`, `"Version":`, 1)},
		{"null_version", strings.Replace(raw, `"version":1`, `"version":null`, 1)},
		{"fractional_version", strings.Replace(raw, `"version":1`, `"version":1.0`, 1)},
		{"string_version", strings.Replace(raw, `"version":1`, `"version":"1"`, 1)},
		{"overflow_count", strings.Replace(raw, `"count":4`, `"count":9223372036854775808`, 1)},
		{"negative_count", strings.Replace(raw, `"count":4`, `"count":-1`, 1)},
		{"object_children", strings.Replace(raw, `"children":[`, `"children":{`, 1)},
		{"null_child", strings.Replace(raw, `"children":[{`, `"children":[null,{`, 1)},
		{"null_next", `{"next_offset":null,` + raw[1:]},
		{"zero_next", `{"next_offset":0,` + raw[1:]},
		{"string_next", `{"next_offset":"1",` + raw[1:]},
		{"fractional_offset", strings.Replace(raw, `"offset":0`, `"offset":0.0`, 1)},
		{"checkpoint_duplicate", strings.Replace(raw, `"epoch":`, `"sequence":0,"epoch":`, 1)},
		{"checkpoint_case", strings.Replace(raw, `"epoch":`, `"Epoch":`, 1)},
		{"child_duplicate", strings.Replace(raw, `"prefix":"0"`, `"prefix":"0","prefix":"0"`, 1)},
		{"child_case", strings.Replace(raw, `"prefix":"0"`, `"Prefix":"0"`, 1)},
		{"child_unknown", strings.Replace(raw, `"prefix":"0"`, `"future":true,"prefix":"0"`, 1)},
		{"child_null_hash", strings.Replace(raw, `"hash":"`+string(root.Children[0].Hash)+`"`, `"hash":null`, 1)},
		{"bad_utf8", "\xff" + raw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wire == raw {
				t.Fatal("ineffective wire mutation")
			}
			got, err := DecodeCatalogMerklePage([]byte(tc.wire))
			if !errors.Is(err, ErrHashMismatch) || got.Version != 0 || got.Entries != nil {
				t.Fatalf("got=%+v error=%v", got, err)
			}
		})
	}
	// Reflect must discover every required page field and recurse into both new
	// child descriptors and the existing checkpoint type.
	for _, path := range []string{"page", "checkpoint", "child"} {
		for _, key := range catalogMerkleRequiredFields(path) {
			for _, null := range []bool{false, true} {
				t.Run(path+"/"+key+map[bool]string{false: "/missing", true: "/null"}[null], func(t *testing.T) {
					var top map[string]json.RawMessage
					if err := json.Unmarshal([]byte(raw), &top); err != nil {
						t.Fatal(err)
					}
					target := top
					var children []map[string]json.RawMessage
					switch path {
					case "checkpoint":
						target = map[string]json.RawMessage{}
						if err := json.Unmarshal(top[path], &target); err != nil {
							t.Fatal(err)
						}
					case "child":
						if err := json.Unmarshal(top["children"], &children); err != nil {
							t.Fatal(err)
						}
						target = children[0]
					}
					if null {
						target[key] = json.RawMessage(`null`)
					} else {
						delete(target, key)
					}
					switch path {
					case "checkpoint":
						top[path] = catalogMerkleRaw(t, target)
					case "child":
						top["children"] = catalogMerkleRaw(t, children)
					}
					if _, err := DecodeCatalogMerklePage(catalogMerkleRaw(t, top)); err == nil {
						t.Fatal("missing/null required field accepted")
					}
				})
			}
		}
	}
}

func catalogMerkleRequiredFields(path string) []string {
	switch path {
	case "checkpoint":
		return []string{"version", "repo_id", "epoch", "sequence"}
	case "child":
		return []string{"prefix", "hash", "count"}
	default:
		return []string{"version", "scope", "repo_id", "checkpoint", "root_hash", "prefix", "node_hash", "count", "children", "entries", "offset"}
	}
}

func TestCatalogMerkleDecodeStrictEntriesAndRawPreservation(t *testing.T) {
	repo, entries := catalogMerkleFixture(t)
	root, nodes := catalogMerkleMustBuild(t, repo, entries)
	var page CatalogMerklePage
	for _, node := range nodes {
		if node.Count == 1 && len(node.Prefix) == 2 && node.Entries[0].Kind == "snapshot" {
			page = catalogMerklePage(t, root, node)
			break
		}
	}
	raw := string(catalogMerkleRaw(t, page))
	if _, err := DecodeCatalogMerklePage([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, wire string }{
		{"missing_value", strings.Replace(raw, `"value":`, `"future":`, 1)},
		{"duplicate_value", strings.Replace(raw, `"value":`, `"value":{},"value":`, 1)},
		{"null_deleted", strings.Replace(raw, `"kind":"snapshot"`, `"deleted":null,"kind":"snapshot"`, 1)},
		{"tombstone", strings.Replace(raw, `"kind":"snapshot"`, `"deleted":true,"kind":"snapshot"`, 1)},
		{"missing_sequence", strings.Replace(raw, `"sequence":9007199254740997,"kind"`, `"kind"`, 1)},
		{"duplicate_sequence", strings.Replace(raw, `"kind":"snapshot"`, `"sequence":9007199254740997,"kind":"snapshot"`, 1)},
		{"entry_case", strings.Replace(raw, `"kind":"snapshot"`, `"Kind":"snapshot"`, 1)},
		{"value_unknown", strings.Replace(raw, `"branch":"main"`, `"future":1,"branch":"main"`, 1)},
		{"value_duplicate", strings.Replace(raw, `"branch":"main"`, `"branch":"main","branch":"main"`, 1)},
		{"value_case", strings.Replace(raw, `"branch":"main"`, `"Branch":"main"`, 1)},
		{"value_null_scalar", strings.Replace(raw, `"branch":"main"`, `"branch":null`, 1)},
		{"value_null_object", strings.Replace(raw, `"author":{`, `"author":null,"duplicate_author":{`, 1)},
		{"nested_duplicate", strings.Replace(raw, `"name":"Élodie"`, `"name":"Élodie","name":"Élodie"`, 1)},
		{"nested_unknown", strings.Replace(raw, `"name":"Élodie"`, `"future":true,"name":"Élodie"`, 1)},
		{"nested_case", strings.Replace(raw, `"name":"Élodie"`, `"Name":"Élodie"`, 1)},
		{"array_null_element", strings.Replace(raw, `"models":["z","a"]`, `"models":[null,"a"]`, 1)},
		{"count_mismatch", strings.Replace(raw, `"count":1`, `"count":2`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wire == raw {
				t.Fatal("ineffective mutation")
			}
			if got, err := DecodeCatalogMerklePage([]byte(tc.wire)); !errors.Is(err, ErrHashMismatch) || got.Entries != nil {
				t.Fatalf("invalid page accepted: %v", err)
			}
		})
	}
	// Value is optional on CatalogEntry, but required for a live Merkle leaf.
	for _, value := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`[]`)} {
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &object); err != nil {
			t.Fatal(err)
		}
		var wireEntries []map[string]json.RawMessage
		if err := json.Unmarshal(object["entries"], &wireEntries); err != nil {
			t.Fatal(err)
		}
		if value == nil {
			delete(wireEntries[0], "value")
		} else {
			wireEntries[0]["value"] = value
		}
		object["entries"] = catalogMerkleRaw(t, wireEntries)
		if _, err := DecodeCatalogMerklePage(catalogMerkleRaw(t, object)); err == nil {
			t.Fatal("missing/malformed live value accepted")
		}
	}
	// Decode retains original JSON whitespace and number lexemes in Value.
	value := page.Entries[0].Value
	pretty := bytes.ReplaceAll(value, []byte(`,`), []byte(", \n "))
	marshaledValue := catalogMerkleRaw(t, value)
	wire := bytes.Replace([]byte(raw), marshaledValue, pretty, 1)
	decoded, err := DecodeCatalogMerklePage(wire)
	if err != nil || !bytes.Equal(decoded.Entries[0].Value, pretty) {
		t.Fatalf("raw metadata changed: %v", err)
	}
}

func TestCatalogMerkleDecodePartialLeaves(t *testing.T) {
	repo, bucket, entries := catalogMerkleCollisions(t, 3)
	leaf, err := NewCatalogMerkleLeaf(repo, bucket, entries)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := catalogMerkleMustBuild(t, repo, entries)
	full := catalogMerklePage(t, root, leaf)
	first := full
	first.Entries = full.Entries[:2]
	next := 2
	first.NextOffset = &next
	last := full
	last.Entries = full.Entries[2:]
	last.Offset = 2
	for _, page := range []CatalogMerklePage{first, last} {
		got, err := DecodeCatalogMerklePage(catalogMerkleRaw(t, page))
		if err != nil || !reflect.DeepEqual(got, page) {
			t.Fatalf("partial page: %v", err)
		}
	}
	first.NodeHash = "bad"
	if _, err := DecodeCatalogMerklePage(catalogMerkleRaw(t, first)); err == nil {
		t.Fatal("invalid partial node hash shape")
	}
}
