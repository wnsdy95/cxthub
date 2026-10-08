package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// These fixtures and golden hashes are byte-identical in the independent
// modules. The hashes were calculated independently with SHA-256 over sorted
// JSON (HTML and U+2028/U+2029 escaped), retaining integer precision.
const catalogMerkleFixtureJSON = `[
 {"sequence":9007199254740997,"kind":"snapshot","key":"$snapshot","value":{"id":"$snapshot","repo_id":"$repo","branch":"main","parents":[],"doc_hash":"$snapshot","provider":"codex","fidelity":"full","message":"<html>& \ubd04 ☃ \u2028 \u2029","author":{"name":"Élodie","email":"a&b@example.test","team":"\uc5f0\uad6c"},"created_at":"2026-10-07T01:02:03Z","graft_seq":9007199254740993,"compaction_count":9007199254740995,"models":["z","a"]}},
 {"sequence":3,"kind":"protocol","key":"$repo","value":{"context_protocol":1}},
 {"sequence":7,"kind":"ref","key":"[ \"br\\u0061nch\", \"main/\ubd04<&>\" ]","value":{"kind":"branch","name":"main/\ubd04<&>","repo_id":"$repo","target":"$snapshot","branch_id":"identity"}},
 {"sequence":11,"kind":"history","key":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","value":{"id":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","repo_id":"$repo","branch_id":"identity","branch":"main","kind":"birth","target":"$snapshot","memory_hash":"$memory","memory_source":"$snapshot","memory_pinned":true,"created_at":"2026-10-07T01:02:03Z"}}
]`

func catalogMerkleFixture(t testing.TB) (ContentHash, []CatalogEntry) {
	t.Helper()
	repo := ContentHash("sha256:" + strings.Repeat("a", 64))
	raw := strings.NewReplacer("$repo", string(repo), "$snapshot", "sha256:"+strings.Repeat("b", 64), "$memory", "sha256:"+strings.Repeat("d", 64)).Replace(catalogMerkleFixtureJSON)
	var entries []CatalogEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatal(err)
	}
	return repo, entries
}

func catalogMerkleMustHash(t testing.TB, node CatalogMerkleNode) ContentHash {
	t.Helper()
	hash, err := CatalogMerkleHash(node)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func catalogMerkleMustBuild(t testing.TB, repo ContentHash, entries []CatalogEntry) (CatalogMerkleNode, map[ContentHash]CatalogMerkleNode) {
	t.Helper()
	root, nodes, err := BuildCatalogMerkle(repo, entries)
	if err != nil {
		t.Fatal(err)
	}
	return root, nodes
}

func catalogMerkleRaw(t testing.TB, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func catalogMerkleCheckpoint(t testing.TB, repo ContentHash) CatalogCheckpoint {
	t.Helper()
	var cp CatalogCheckpoint
	raw := fmt.Sprintf(`{"version":1,"repo_id":%q,"epoch":"e66213bd-003e-47d1-b965-e690036bfb08","sequence":9007199254740997}`, repo)
	if err := json.Unmarshal([]byte(raw), &cp); err != nil {
		t.Fatal(err)
	}
	return cp
}

func catalogMerklePage(t testing.TB, root, node CatalogMerkleNode) CatalogMerklePage {
	t.Helper()
	return CatalogMerklePage{Version: 1, Scope: CatalogMerkleScope, RepoID: root.RepoID, Checkpoint: catalogMerkleCheckpoint(t, root.RepoID), RootHash: catalogMerkleMustHash(t, root), Prefix: node.Prefix, NodeHash: catalogMerkleMustHash(t, node), Count: node.Count, Children: node.Children, Entries: node.Entries}
}

func TestCatalogMerkleGoldenVectors(t *testing.T) {
	repo, entries := catalogMerkleFixture(t)
	wantBuckets := []string{"12", "b0", "0e", "d7"}
	wantLeaves := []ContentHash{"sha256:f05c848de514a5015faae2a0b0773ded58075146d0914f499cead76c977b7f86", "sha256:29c05b2eaff33948b6c4a682405df563d788370e7b2f064c837da0cfc932a8ac", "sha256:b7b81cb81eb5afc8e9f91298dd5b6df370bf43f02db873dc7af4dd89741a3e81", "sha256:89648013e9d2e144ac1623880ba031bb6cc8cd9222cfbd6fe5c2d89cf12276cf"}
	for i, entry := range entries {
		bucket, err := CatalogMerkleBucket(entry)
		if err != nil {
			t.Fatal(err)
		}
		if bucket != wantBuckets[i] {
			t.Fatalf("%s bucket=%s want=%s", entry.Kind, bucket, wantBuckets[i])
		}
		node, err := NewCatalogMerkleLeaf(repo, bucket, []CatalogEntry{entry})
		if err != nil {
			t.Fatal(err)
		}
		if hash := catalogMerkleMustHash(t, node); hash != wantLeaves[i] {
			t.Fatalf("%s leaf=%s want=%s", entry.Kind, hash, wantLeaves[i])
		}
	}
	root, nodes := catalogMerkleMustBuild(t, repo, entries)
	if hash := catalogMerkleMustHash(t, root); hash != "sha256:eecda710b5fbfe942cc7387e23cf1f105301deacdda029f8e47ce18a17238f55" {
		t.Fatalf("root=%s", hash)
	}
	if root.Count != 4 || len(nodes) != 273 {
		t.Fatalf("root count=%d nodes=%d", root.Count, len(nodes))
	}
	empty, _ := catalogMerkleMustBuild(t, repo, nil)
	if hash := catalogMerkleMustHash(t, empty); hash != "sha256:992de974eefd8c47001e8b66766e12e3a4f89531a249a873f27fc44484b3c56e" {
		t.Fatalf("empty=%s", hash)
	}
	for hash, node := range nodes {
		if node.Children == nil || node.Entries == nil || catalogMerkleMustHash(t, node) != hash {
			t.Fatal("invalid content-addressed node")
		}
		for _, child := range node.Children {
			stored, exists := nodes[child.Hash]
			if !exists || stored.Prefix != child.Prefix || stored.Count != child.Count {
				t.Fatal("unbound child")
			}
		}
	}
}

func TestCatalogMerkleNormalizationAndCompleteValues(t *testing.T) {
	repo, entries := catalogMerkleFixture(t)
	root, _ := catalogMerkleMustBuild(t, repo, entries)
	want := catalogMerkleMustHash(t, root)
	before := catalogMerkleRaw(t, entries)
	rng := rand.New(rand.NewSource(452))
	for range 8 {
		shuffled := append([]CatalogEntry{}, entries...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		for i := range shuffled {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(shuffled[i].Value, &object); err != nil {
				t.Fatal(err)
			}
			keys := make([]string, 0, len(object))
			for key := range object {
				keys = append(keys, key)
			}
			sort.Sort(sort.Reverse(sort.StringSlice(keys)))
			var raw bytes.Buffer
			raw.WriteString("{\n")
			for j, key := range keys {
				if j > 0 {
					raw.WriteString(",\n")
				}
				fmt.Fprintf(&raw, "%q : %s", key, object[key])
			}
			raw.WriteString("}")
			shuffled[i].Value = raw.Bytes()
			if shuffled[i].Kind == "ref" {
				shuffled[i].Key = `["branch","main/\ubd04\u003c\u0026\u003e"]`
			}
		}
		got, _ := catalogMerkleMustBuild(t, repo, shuffled)
		if catalogMerkleMustHash(t, got) != want {
			t.Fatal("JSON spelling or insertion order changed root")
		}
	}
	if !bytes.Equal(before, catalogMerkleRaw(t, entries)) {
		t.Fatal("builder mutated inputs")
	}
	for _, tc := range []struct{ name, old, next string }{
		{"precision", "9007199254740993", "9007199254740992"},
		{"models_order", `["z","a"]`, `["a","z"]`},
		{"presentation", `<html>`, `<HTML>`},
		{"author", `Élodie`, `Elodie`},
		{"memory", `"memory_pinned":true`, `"memory_pinned":false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := append([]CatalogEntry{}, entries...)
			for i := range changed {
				changed[i].Value = bytes.ReplaceAll(changed[i].Value, []byte(tc.old), []byte(tc.next))
			}
			got, _ := catalogMerkleMustBuild(t, repo, changed)
			if catalogMerkleMustHash(t, got) == want {
				t.Fatal("complete value field not hashed")
			}
		})
	}
	changed := append([]CatalogEntry{}, entries...)
	changed[0].Sequence--
	got, _ := catalogMerkleMustBuild(t, repo, changed)
	if catalogMerkleMustHash(t, got) == want {
		t.Fatal("sequence not hashed")
	}
}

func TestCatalogMerkleDuplicateAndMalformedEntries(t *testing.T) {
	repo, entries := catalogMerkleFixture(t)
	for _, kind := range []int{0, 1, 2, 3} {
		duplicate := entries[kind]
		duplicate.Sequence++
		if kind == 2 {
			duplicate.Key = `["branch","main/\ubd04<&>"]`
		}
		root, nodes, err := BuildCatalogMerkle(repo, append(append([]CatalogEntry{}, entries...), duplicate))
		if err == nil || root.Version != 0 || nodes != nil {
			t.Fatalf("accepted duplicate %s or returned partial tree", duplicate.Kind)
		}
	}
	for _, tc := range []struct {
		name  string
		index int
		edit  func(*CatalogEntry)
	}{
		{"deleted", 0, func(e *CatalogEntry) { e.Deleted = true; e.Value = nil }},
		{"negative_sequence", 0, func(e *CatalogEntry) { e.Sequence = -1 }},
		{"unknown_kind", 0, func(e *CatalogEntry) { e.Kind = "future" }},
		{"invalid_hash", 0, func(e *CatalogEntry) { e.Key = "sha256:ABC" }},
		{"missing_value", 0, func(e *CatalogEntry) { e.Value = nil }},
		{"null_value", 0, func(e *CatalogEntry) { e.Value = []byte(`null`) }},
		{"array_value", 0, func(e *CatalogEntry) { e.Value = []byte(`[]`) }},
		{"trailing_value", 0, func(e *CatalogEntry) { e.Value = append(e.Value, []byte(`{}`)...) }},
		{"bad_utf8", 0, func(e *CatalogEntry) { e.Value = append([]byte{0xff}, e.Value...) }},
		{"mismatched_snapshot_key", 0, func(e *CatalogEntry) { e.Key = string(repo) }},
		{"unknown_value_field", 0, func(e *CatalogEntry) { e.Value = append([]byte(`{"future":1,`), e.Value[1:]...) }},
		{"duplicate_value_key", 0, func(e *CatalogEntry) { e.Value = append([]byte(`{"branch":"main",`), e.Value[1:]...) }},
		{"escaped_duplicate_value_key", 0, func(e *CatalogEntry) { e.Value = append([]byte(`{"br\u0061nch":"main",`), e.Value[1:]...) }},
		{"case_value_key", 0, func(e *CatalogEntry) { e.Value = bytes.Replace(e.Value, []byte(`"branch"`), []byte(`"Branch"`), 1) }},
		{"null_scalar", 0, func(e *CatalogEntry) {
			e.Value = bytes.Replace(e.Value, []byte(`"branch":"main"`), []byte(`"branch":null`), 1)
		}},
		{"bad_nested", 0, func(e *CatalogEntry) {
			e.Value = bytes.Replace(e.Value, []byte(`"name":"Élodie"`), []byte(`"name":"Élodie","name":"again"`), 1)
		}},
		{"protocol_key", 1, func(e *CatalogEntry) { e.Key = entries[0].Key }},
		{"protocol_value", 1, func(e *CatalogEntry) { e.Value = []byte(`{"context_protocol":2}`) }},
		{"ref_key_null", 2, func(e *CatalogEntry) { e.Key = `["branch",null]` }},
		{"ref_key_extra", 2, func(e *CatalogEntry) { e.Key = `["branch","main","extra"]` }},
		{"ref_key_shape", 2, func(e *CatalogEntry) { e.Key = `{"kind":"branch","name":"main"}` }},
		{"ref_key_mismatch", 2, func(e *CatalogEntry) { e.Key = `["branch","another"]` }},
		{"ref_key_trailing", 2, func(e *CatalogEntry) { e.Key += `null` }},
		{"ref_wrong_repo", 2, func(e *CatalogEntry) { e.Value = bytes.Replace(e.Value, []byte(repo), []byte(entries[0].Key), 1) }},
		{"history_id", 3, func(e *CatalogEntry) { e.Key = strings.Repeat("E", 32) }},
		{"history_time", 3, func(e *CatalogEntry) {
			e.Value = bytes.ReplaceAll(e.Value, []byte(`2026-10-07T01:02:03Z`), []byte(`bad`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := append([]CatalogEntry{}, entries...)
			tc.edit(&changed[tc.index])
			if root, nodes, err := BuildCatalogMerkle(repo, changed); err == nil || root.Version != 0 || nodes != nil {
				t.Fatal("accepted invalid entry or returned partial tree")
			}
		})
	}
	if _, _, err := BuildCatalogMerkle("bad", entries); err == nil {
		t.Fatal("invalid repo accepted")
	}
	for _, key := range []CatalogEntry{{Kind: "unknown", Key: "key"}, {Kind: "history", Key: strings.Repeat("g", 32)}, {Kind: "ref", Key: `["branch","../bad"]`}} {
		if _, err := CatalogMerkleBucket(key); err == nil {
			t.Fatal("invalid bucket key accepted")
		}
	}
	tombstone := entries[2]
	tombstone.Deleted = true
	tombstone.Value = nil
	if a, _ := CatalogMerkleBucket(tombstone); a == "" {
		t.Fatal("deletions must retain bucket identity")
	}
}

func TestCatalogMerkleNodeShapeAndHashCoverage(t *testing.T) {
	repo, entries := catalogMerkleFixture(t)
	root, nodes := catalogMerkleMustBuild(t, repo, entries)
	for _, tc := range []struct {
		name string
		edit func(*CatalogMerkleNode)
	}{
		{"version", func(n *CatalogMerkleNode) { n.Version++ }},
		{"scope", func(n *CatalogMerkleNode) { n.Scope = CatalogScope }},
		{"repo", func(n *CatalogMerkleNode) { n.RepoID = "bad" }},
		{"prefix", func(n *CatalogMerkleNode) { n.Prefix = "A" }},
		{"long_prefix", func(n *CatalogMerkleNode) { n.Prefix = "000" }},
		{"negative_count", func(n *CatalogMerkleNode) { n.Count = -1 }},
		{"count", func(n *CatalogMerkleNode) { n.Count++ }},
		{"nil_children", func(n *CatalogMerkleNode) { n.Children = nil }},
		{"nil_entries", func(n *CatalogMerkleNode) { n.Entries = nil }},
		{"missing_child", func(n *CatalogMerkleNode) { n.Children = n.Children[:15] }},
		{"extra_child", func(n *CatalogMerkleNode) { n.Children = append(n.Children, n.Children[0]) }},
		{"child_order", func(n *CatalogMerkleNode) { n.Children[0], n.Children[1] = n.Children[1], n.Children[0] }},
		{"child_prefix", func(n *CatalogMerkleNode) { n.Children[0].Prefix = "00" }},
		{"child_hash", func(n *CatalogMerkleNode) { n.Children[0].Hash = "bad" }},
		{"child_negative", func(n *CatalogMerkleNode) { n.Children[0].Count = -1 }},
		{"overflow", func(n *CatalogMerkleNode) { n.Children[0].Count = math.MaxInt64; n.Children[1].Count = 1 }},
		{"branch_entry", func(n *CatalogMerkleNode) { n.Entries = entries[:1] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := root
			changed.Children = append([]CatalogMerkleChild{}, root.Children...)
			tc.edit(&changed)
			if _, err := CatalogMerkleHash(changed); err == nil {
				t.Fatal("accepted malformed node")
			}
		})
	}
	for _, entry := range entries {
		bucket, _ := CatalogMerkleBucket(entry)
		node, err := NewCatalogMerkleLeaf(repo, bucket, []CatalogEntry{entry})
		if err != nil {
			t.Fatal(err)
		}
		changed := node
		changed.Prefix = "00"
		if bucket == "00" {
			changed.Prefix = "01"
		}
		if _, err := CatalogMerkleHash(changed); err == nil {
			t.Fatal("wrong leaf prefix accepted")
		}
		changed = node
		changed.Count++
		if _, err := CatalogMerkleHash(changed); err == nil {
			t.Fatal("wrong leaf count accepted")
		}
		changed = node
		changed.Children = root.Children
		if _, err := CatalogMerkleHash(changed); err == nil {
			t.Fatal("leaf children accepted")
		}
	}
	altered := root
	altered.RepoID = HashContent([]byte("another repo"))
	if catalogMerkleMustHash(t, altered) == catalogMerkleMustHash(t, root) {
		t.Fatal("repo not hashed")
	}
	altered = root
	altered.Children = append([]CatalogMerkleChild{}, root.Children...)
	altered.Children[0].Hash = HashContent([]byte("another child"))
	if catalogMerkleMustHash(t, altered) == catalogMerkleMustHash(t, root) {
		t.Fatal("child hash not hashed")
	}
	altered = root
	altered.Children = append([]CatalogMerkleChild{}, root.Children...)
	altered.Children[0].Count++
	altered.Count++
	if catalogMerkleMustHash(t, altered) == catalogMerkleMustHash(t, root) {
		t.Fatal("counts not hashed")
	}
	if _, err := NewCatalogMerkleBranch(repo, "ab", root.Children); err == nil {
		t.Fatal("leaf prefix accepted as branch")
	}
	if _, err := NewCatalogMerkleLeaf(repo, "a", nil); err == nil {
		t.Fatal("branch prefix accepted as leaf")
	}
	children := append([]CatalogMerkleChild{}, root.Children...)
	children[0].Count = math.MaxInt64
	children[1].Count = 1
	if _, err := NewCatalogMerkleBranch(repo, "", children); err == nil {
		t.Fatal("constructor count overflow")
	}
	for _, node := range nodes {
		if len(node.Prefix) == 2 && node.Count == 0 {
			for _, value := range []any{node, CatalogMerkleNode{}, CatalogMerklePage{}} {
				raw := catalogMerkleRaw(t, value)
				if !bytes.Contains(raw, []byte(`"children":[]`)) || !bytes.Contains(raw, []byte(`"entries":[]`)) {
					t.Fatal("null collection on wire")
				}
			}
			break
		}
	}
}

func TestCatalogMerkleMemoryDeleteReinsertAndOwnedInputs(t *testing.T) {
	repo, entries := catalogMerkleFixture(t)
	root, before := catalogMerkleMustBuild(t, repo, entries)
	changed := append([]CatalogEntry{}, entries...)
	changed[0].Value = append([]byte(`{"memory_hash":"sha256:`+strings.Repeat("f", 64)+`",`), changed[0].Value[1:]...)
	updated, after := catalogMerkleMustBuild(t, repo, changed)
	if catalogMerkleMustHash(t, updated) == catalogMerkleMustHash(t, root) {
		t.Fatal("memory-only edit lost")
	}
	newNodes := 0
	for hash := range after {
		if _, ok := before[hash]; !ok {
			newNodes++
		}
	}
	if newNodes != 3 {
		t.Fatalf("one-key change rewrote %d nodes, want leaf + branch + root", newNodes)
	}
	deleted, _ := catalogMerkleMustBuild(t, repo, entries[1:])
	if deleted.Count != 3 || catalogMerkleMustHash(t, deleted) == catalogMerkleMustHash(t, root) {
		t.Fatal("deletion lost")
	}
	reinserted, _ := catalogMerkleMustBuild(t, repo, entries)
	if catalogMerkleMustHash(t, reinserted) != catalogMerkleMustHash(t, root) {
		t.Fatal("same exact image failed to restore root")
	}
	changed = append([]CatalogEntry{}, entries...)
	changed[0].Sequence++
	reinserted, _ = catalogMerkleMustBuild(t, repo, changed)
	if catalogMerkleMustHash(t, reinserted) == catalogMerkleMustHash(t, root) {
		t.Fatal("new insertion sequence lost")
	}
	snapshot := entries[0]
	bucket, _ := CatalogMerkleBucket(snapshot)
	leaf, err := NewCatalogMerkleLeaf(repo, bucket, []CatalogEntry{snapshot})
	if err != nil {
		t.Fatal(err)
	}
	hash := catalogMerkleMustHash(t, leaf)
	snapshot.Value[0] = '!'
	if catalogMerkleMustHash(t, leaf) != hash {
		t.Fatal("leaf aliases caller raw memory")
	}
	children := append([]CatalogMerkleChild{}, root.Children...)
	branch, err := NewCatalogMerkleBranch(repo, "", children)
	if err != nil {
		t.Fatal(err)
	}
	children[0].Hash = "invalid"
	if catalogMerkleMustHash(t, branch) != catalogMerkleMustHash(t, root) {
		t.Fatal("branch aliases caller slice")
	}
}

func TestCatalogMerkleConcurrentDeterminism(t *testing.T) {
	repo, entries := catalogMerkleFixture(t)
	root, _ := catalogMerkleMustBuild(t, repo, entries)
	want := catalogMerkleMustHash(t, root)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			got, _, err := BuildCatalogMerkle(repo, entries)
			if err != nil {
				t.Error(err)
				return
			}
			hash, err := CatalogMerkleHash(got)
			if err != nil || hash != want {
				t.Errorf("concurrent root=%s err=%v", hash, err)
			}
		})
	}
	wg.Wait()
}

func TestCatalogMerkleMirroredImplementationAndFixtures(t *testing.T) {
	for _, name := range []string{"catalog_merkle.go", "catalog_merkle_test.go"} {
		local, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		other := "../../../cli/internal/domain/" + name
		// Resolve the peer without importing its independent internal package.
		if cwd, err := os.Getwd(); err != nil {
			t.Fatal(err)
		} else if strings.HasSuffix(cwd, "/cli/internal/domain") {
			other = "../../../backend/internal/domain/" + name
		}
		peer, err := os.ReadFile(other)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(local, peer) {
			t.Fatalf("mirrored algorithm/fixtures drift: %s", name)
		}
	}
}

func TestCatalogMerkleRequestValidation(t *testing.T) {
	repo, _ := catalogMerkleFixture(t)
	cp := catalogMerkleCheckpoint(t, repo)
	rootHash := HashContent([]byte("root"))
	for _, r := range []CatalogMerkleRequest{{Version: 1}, {Version: 1, Limit: 1000}, {Version: 1, RootHash: rootHash, Checkpoint: &cp}, {Version: 1, RootHash: rootHash, Checkpoint: &cp, Prefix: "a"}, {Version: 1, RootHash: rootHash, Checkpoint: &cp, Prefix: "ab", Offset: 7}} {
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*CatalogMerkleRequest)
	}{
		{"version", func(r *CatalogMerkleRequest) { r.Version = 2 }},
		{"root_missing", func(r *CatalogMerkleRequest) { r.RootHash = "" }},
		{"checkpoint_missing", func(r *CatalogMerkleRequest) { r.Checkpoint = nil }},
		{"root_invalid", func(r *CatalogMerkleRequest) { r.RootHash = "bad" }},
		{"checkpoint_version", func(r *CatalogMerkleRequest) { r.Checkpoint.Version++ }},
		{"checkpoint_repo", func(r *CatalogMerkleRequest) { r.Checkpoint.RepoID = "bad" }},
		{"checkpoint_epoch", func(r *CatalogMerkleRequest) { r.Checkpoint.Epoch = "bad" }},
		{"checkpoint_sequence", func(r *CatalogMerkleRequest) { r.Checkpoint.Sequence = -1 }},
		{"prefix_upper", func(r *CatalogMerkleRequest) { r.Prefix = "AB" }},
		{"prefix_long", func(r *CatalogMerkleRequest) { r.Prefix = "abc" }},
		{"prefix_slash", func(r *CatalogMerkleRequest) { r.Prefix = "/" }},
		{"offset_negative", func(r *CatalogMerkleRequest) { r.Offset = -1 }},
		{"branch_offset", func(r *CatalogMerkleRequest) { r.Prefix = "a"; r.Offset = 1 }},
		{"root_offset", func(r *CatalogMerkleRequest) { r.Prefix = ""; r.Offset = 1 }},
		{"limit_negative", func(r *CatalogMerkleRequest) { r.Limit = -1 }},
		{"limit_large", func(r *CatalogMerkleRequest) { r.Limit = 1001 }},
		{"latest_subtree", func(r *CatalogMerkleRequest) { r.RootHash = ""; r.Checkpoint = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkpoint := cp
			r := CatalogMerkleRequest{Version: 1, RootHash: rootHash, Checkpoint: &checkpoint, Prefix: "ab"}
			tc.edit(&r)
			if err := r.Validate(); err == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
}

// Find colliding, valid refs so pagination and normalized key ordering are
// exercised inside one leaf rather than across otherwise single-entry buckets.
func catalogMerkleCollisions(t testing.TB, count int) (ContentHash, string, []CatalogEntry) {
	t.Helper()
	repo, fixture := catalogMerkleFixture(t)
	buckets := map[string][]CatalogEntry{}
	for i := 0; i < 256*(count+1); i++ {
		name := fmt.Sprintf("branch-%05d", i)
		e := CatalogEntry{Kind: "ref", Key: string(catalogMerkleRaw(t, [2]string{"branch", name})), Sequence: int64(i), Value: catalogMerkleRaw(t, map[string]any{"kind": "branch", "name": name, "repo_id": repo, "target": fixture[0].Key})}
		bucket, err := CatalogMerkleBucket(e)
		if err != nil {
			t.Fatal(err)
		}
		buckets[bucket] = append(buckets[bucket], e)
		if len(buckets[bucket]) == count {
			return repo, bucket, buckets[bucket]
		}
	}
	t.Fatal("no bucket collisions")
	return "", "", nil
}

func TestCatalogMerkleLeafOrderAndPaging(t *testing.T) {
	repo, bucket, entries := catalogMerkleCollisions(t, 3)
	reversed := append([]CatalogEntry{}, entries...)
	reversed[0], reversed[2] = reversed[2], reversed[0]
	node, err := NewCatalogMerkleLeaf(repo, bucket, reversed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(node.Entries, entries) {
		t.Fatal("constructor did not order by normalized key")
	}
	malformed := node
	malformed.Entries = reversed
	if _, err := CatalogMerkleHash(malformed); err == nil {
		t.Fatal("hash repaired unordered leaf")
	}
	root, _ := catalogMerkleMustBuild(t, repo, entries)
	full := catalogMerklePage(t, root, node)
	request := CatalogMerkleRequest{Version: 1, RootHash: full.RootHash, Checkpoint: &full.Checkpoint, Prefix: bucket, Limit: 2}
	first := full
	first.Entries = full.Entries[:2]
	next := 2
	first.NextOffset = &next
	if err := first.Validate(repo, request); err != nil {
		t.Fatal(err)
	}
	request.Offset = 2
	last := full
	last.Entries = full.Entries[2:]
	last.Offset = 2
	if err := last.Validate(repo, request); err != nil {
		t.Fatal(err)
	}
	assembled := node
	assembled.Entries = append(append([]CatalogEntry{}, first.Entries...), last.Entries...)
	if catalogMerkleMustHash(t, assembled) != full.NodeHash {
		t.Fatal("page assembly changed node")
	}
	for _, tc := range []struct {
		name string
		edit func(*CatalogMerklePage)
	}{
		{"no_next", func(p *CatalogMerklePage) { p.NextOffset = nil }},
		{"no_progress", func(p *CatalogMerklePage) { p.Entries = []CatalogEntry{} }},
		{"wrong_next", func(p *CatalogMerklePage) { n := 1; p.NextOffset = &n }},
		{"huge_next", func(p *CatalogMerklePage) { n := math.MaxInt; p.NextOffset = &n }},
		{"negative_next", func(p *CatalogMerklePage) { n := -1; p.NextOffset = &n }},
		{"children", func(p *CatalogMerklePage) { p.Children = root.Children }},
		{"over_limit", func(p *CatalogMerklePage) { p.Entries = full.Entries }},
		{"count_too_small", func(p *CatalogMerklePage) { p.Count = 1 }},
		{"entry_future", func(p *CatalogMerklePage) { p.Checkpoint.Sequence = 0 }},
		{"entry_order", func(p *CatalogMerklePage) { p.Entries = []CatalogEntry{entries[1], entries[0]} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := first
			tc.edit(&bad)
			req := request
			req.Offset = 0
			if err := bad.Validate(repo, req); err == nil {
				t.Fatal("bad page accepted")
			}
		})
	}
	request.Offset = 0
	request.Limit = 3
	if err := full.Validate(repo, request); err != nil {
		t.Fatal(err)
	}
	bad := full
	bad.NodeHash = HashContent([]byte("wrong full leaf"))
	if err := bad.Validate(repo, request); err == nil {
		t.Fatal("complete leaf hash not verified")
	}
	bad = last
	bad.NextOffset = &next
	request.Offset = 2
	if err := bad.Validate(repo, request); err == nil {
		t.Fatal("final page has continuation")
	}
	bad = last
	bad.Offset = 3
	bad.Entries = []CatalogEntry{}
	request.Offset = 3
	if err := bad.Validate(repo, request); err == nil {
		t.Fatal("empty redundant final page accepted")
	}
}

func TestCatalogMerklePageBoundaries(t *testing.T) {
	repo, entries := catalogMerkleFixture(t)
	root, nodes := catalogMerkleMustBuild(t, repo, entries)
	page := catalogMerklePage(t, root, root)
	request := CatalogMerkleRequest{Version: 1}
	if err := page.Validate(repo, request); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*CatalogMerklePage)
	}{
		{"version", func(p *CatalogMerklePage) { p.Version++ }},
		{"scope", func(p *CatalogMerklePage) { p.Scope = CatalogScope }},
		{"repo", func(p *CatalogMerklePage) { p.RepoID = HashContent([]byte("wrong repo")) }},
		{"checkpoint_repo", func(p *CatalogMerklePage) { p.Checkpoint.RepoID = "bad" }},
		{"checkpoint_epoch", func(p *CatalogMerklePage) { p.Checkpoint.Epoch = "bad" }},
		{"root_hash", func(p *CatalogMerklePage) { p.RootHash = HashContent([]byte("wrong root")) }},
		{"node_hash", func(p *CatalogMerklePage) { p.NodeHash = "bad" }},
		{"prefix", func(p *CatalogMerklePage) { p.Prefix = "a" }},
		{"offset", func(p *CatalogMerklePage) { p.Offset = 1 }},
		{"count", func(p *CatalogMerklePage) { p.Count++ }},
		{"children_nil", func(p *CatalogMerklePage) { p.Children = nil }},
		{"entries_nil", func(p *CatalogMerklePage) { p.Entries = nil }},
		{"next", func(p *CatalogMerklePage) { n := 1; p.NextOffset = &n }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := page
			tc.edit(&bad)
			if err := bad.Validate(repo, request); err == nil {
				t.Fatal("bad page accepted")
			}
		})
	}
	for _, node := range nodes {
		p := catalogMerklePage(t, root, node)
		req := CatalogMerkleRequest{Version: 1, RootHash: p.RootHash, Checkpoint: &p.Checkpoint, Prefix: node.Prefix, Limit: 1000}
		if err := p.Validate(repo, req); err != nil {
			t.Fatalf("prefix=%q: %v", node.Prefix, err)
		}
		cp := p.Checkpoint
		cp.Sequence--
		req.Checkpoint = &cp
		if err := p.Validate(repo, req); err == nil {
			t.Fatal("checkpoint changed")
		}
	}
}

func BenchmarkCatalogMerkleBuild(b *testing.B) {
	repo, _, entries := catalogMerkleCollisions(b, 16)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := BuildCatalogMerkle(repo, entries); err != nil {
			b.Fatal(err)
		}
	}
}
