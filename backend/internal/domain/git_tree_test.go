package domain

import (
	"bytes"
	"encoding/hex"
	"os/exec"
	"strings"
	"testing"
)

func TestGitTreesMatchGitObjectSerialization(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command("git", "init", "-q", "--object-format="+format, dir)
			if b, e := cmd.CombinedOutput(); e != nil {
				t.Fatalf("git init: %s %v", b, e)
			}
			width := 40
			if format == "sha256" {
				width = 64
			}
			oid := strings.Repeat("a", width)
			bin, _ := hex.DecodeString(oid)
			// A directory "a" sorts AFTER a file "a.c" in a Git tree.
			body := append([]byte("100755 a.c\x00"), bin...)
			body = append(body, []byte("40000 a\x00")...)
			body = append(body, bin...)
			cmd = exec.Command("git", "-C", dir, "hash-object", "-t", "tree", "--stdin")
			cmd.Stdin = bytes.NewReader(body)
			raw, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			n, err := NewGitTreeNode(map[string]GitEntry{"a": {OID: oid, Mode: "040000"}, "a.c": {OID: oid, Mode: "100755"}}, width)
			if err != nil || n.OID != strings.TrimSpace(string(raw)) {
				t.Fatalf("Git disagreement: %s vs %s (%v)", n.OID, raw, err)
			}
			if n.Validate() != nil {
				t.Fatal("valid tree rejected")
			}
			n.Entries["a.c"] = GitEntry{OID: oid, Mode: "100644"}
			if n.Validate() == nil {
				t.Fatal("mode corruption accepted")
			}
		})
	}
}
func TestGitTreeEvidenceDeduplicatesAndRejectsIncompleteTrees(t *testing.T) {
	entry := GitEntry{OID: strings.Repeat("a", 40), Mode: "120000"}
	child, _ := NewGitTreeNode(map[string]GitEntry{"link": entry}, 40)
	empty, _ := NewGitTreeNode(map[string]GitEntry{}, 40)
	root, _ := NewGitTreeNode(map[string]GitEntry{"a": {OID: child.OID, Mode: "040000"}, "b": {OID: child.OID, Mode: "040000"}, "empty": {OID: empty.OID, Mode: "040000"}}, 40)
	c := GitCommitTree{Commit: strings.Repeat("c", 40), Tree: root.OID, Parents: []string{}}
	paths := map[string]GitEntry{"a/link": entry, "b/link": entry, "empty": {OID: empty.OID, Mode: "040000"}}
	e, err := BuildGitTreeEvidence(c, paths)
	if err != nil || len(e.Nodes) != 3 {
		t.Fatalf("dedup: %d %v", len(e.Nodes), err)
	}
	delete(paths, "b/link")
	if _, err = BuildGitTreeEvidence(c, paths); err == nil {
		t.Fatal("omitted path accepted")
	}
	if (GitTreeEvidence{Commit: c, Nodes: []GitTreeNode{root}}).Validate() == nil {
		t.Fatal("incomplete closure accepted")
	}
	paths["a"] = entry
	if _, err = BuildGitTreeEvidence(c, paths); err == nil {
		t.Fatal("file/directory collision accepted")
	}
}
