package domain

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// GitTreeNode is an immutable directory, not a copy of the whole repository.
// Entries use basenames; 040000 entries point at other nodes. Identical
// directories across commits share their Git object ID and storage.
type GitTreeNode struct {
	OID     string              `json:"oid"`
	Entries map[string]GitEntry `json:"entries"`
}

// GitCommitTree binds a provider-verified immutable commit to its exact tree.
// Parents are explicit; a merge never borrows an arbitrary parent's file state.
type GitCommitTree struct {
	Commit  string   `json:"commit"`
	Tree    string   `json:"tree"`
	Parents []string `json:"parents"`
}

type GitTreeEvidence struct {
	Commit GitCommitTree `json:"commit"`
	Nodes  []GitTreeNode `json:"nodes"`
}

func (c GitCommitTree) Validate() error {
	if ValidateGitOID(c.Commit) != nil || ValidateGitOID(c.Tree) != nil || len(c.Commit) != len(c.Tree) || c.Parents == nil || len(c.Parents) > 16 {
		return ErrIntegrity
	}
	seen := map[string]bool{}
	for _, p := range c.Parents {
		if ValidateGitOID(p) != nil || len(p) != len(c.Commit) || p == c.Commit || seen[p] {
			return ErrIntegrity
		}
		seen[p] = true
	}
	return nil
}

// objectID implements Git's canonical tree serialization, including Git's
// directory-aware name sorting. Rehashing rejects omitted or corrupted entries.
func (n GitTreeNode) objectID(width int) (string, error) {
	if n.Entries == nil || (width != 40 && width != 64) {
		return "", ErrIntegrity
	}
	names := make([]string, 0, len(n.Entries))
	for name, e := range n.Entries {
		if ValidateGitPath(name) != nil || strings.Contains(name, "/") || ValidateGitOID(e.OID) != nil || len(e.OID) != width {
			return "", ErrIntegrity
		}
		if e.Mode != "040000" && validateGitEntry(e) != nil {
			return "", ErrIntegrity
		}
		names = append(names, name)
	}
	key := func(name string) string {
		if n.Entries[name].Mode == "040000" {
			return name + "/"
		}
		return name
	}
	sort.Slice(names, func(i, j int) bool { return key(names[i]) < key(names[j]) })
	var body bytes.Buffer
	for _, name := range names {
		e := n.Entries[name]
		mode := e.Mode
		if mode == "040000" {
			mode = "40000"
		}
		body.WriteString(mode + " " + name)
		body.WriteByte(0)
		oid, _ := hex.DecodeString(e.OID)
		body.Write(oid)
	}
	data := append([]byte(fmt.Sprintf("tree %d\x00", body.Len())), body.Bytes()...)
	if width == 40 {
		h := sha1.Sum(data)
		return hex.EncodeToString(h[:]), nil
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}
func (n GitTreeNode) Validate() error {
	if ValidateGitOID(n.OID) != nil {
		return ErrIntegrity
	}
	oid, err := n.objectID(len(n.OID))
	if err != nil || oid != n.OID {
		return ErrIntegrity
	}
	return nil
}

// BuildGitTreeEvidence validates a complete recursive provider tree and turns
// it into deduplicated directory objects. No file contents are stored.
func BuildGitTreeEvidence(c GitCommitTree, paths map[string]GitEntry) (GitTreeEvidence, error) {
	out := GitTreeEvidence{Commit: c}
	if err := c.Validate(); err != nil {
		return out, err
	}
	var err error
	out.Nodes, err = BuildGitTreeNodes(c.Tree, paths)
	if err != nil {
		return out, err
	}
	return out, out.Validate()
}

// BuildGitTreeNodes also verifies comparison-parent trees used by delta reads.
func BuildGitTreeNodes(root string, paths map[string]GitEntry) ([]GitTreeNode, error) {
	out := []GitTreeNode{}
	if ValidateGitOID(root) != nil || paths == nil || len(paths) > 100000 {
		return out, ErrIntegrity
	}
	dirs := map[string]map[string]GitEntry{"": {}}
	expected := map[string]string{}
	for path, e := range paths {
		if ValidateGitPath(path) != nil || e.OID == "" || (e.Mode != "040000" && validateGitEntry(e) != nil) || ValidateGitOID(e.OID) != nil || len(e.OID) != len(root) {
			return out, ErrIntegrity
		}
		parts := strings.Split(path, "/")
		if len(parts) > 128 {
			return out, ErrIntegrity
		}
		for i := 0; i < len(parts)-1; i++ {
			dir := strings.Join(parts[:i+1], "/")
			if dirs[dir] == nil {
				dirs[dir] = map[string]GitEntry{}
			}
		}
		if e.Mode == "040000" {
			if dirs[path] == nil {
				dirs[path] = map[string]GitEntry{}
			}
			expected[path] = e.OID
			continue
		}
		dir := strings.Join(parts[:len(parts)-1], "/")
		dirs[dir][parts[len(parts)-1]] = e
	}
	ordered := make([]string, 0, len(dirs))
	for d := range dirs {
		ordered = append(ordered, d)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return len(ordered[i]) > len(ordered[j]) || (len(ordered[i]) == len(ordered[j]) && ordered[i] < ordered[j])
	})
	seen := map[string]bool{}
	for _, dir := range ordered {
		n := GitTreeNode{Entries: dirs[dir]}
		var err error
		n.OID, err = n.objectID(len(root))
		if err != nil {
			return out, err
		}
		if want, ok := expected[dir]; ok && n.OID != want {
			return out, ErrIntegrity
		}
		if !seen[n.OID] {
			out = append(out, n)
			seen[n.OID] = true
		}
		if dir == "" {
			if n.OID != root {
				return out, ErrIntegrity
			}
			continue
		}
		parts := strings.Split(dir, "/")
		parent := strings.Join(parts[:len(parts)-1], "/")
		name := parts[len(parts)-1]
		if _, exists := dirs[parent][name]; exists {
			return out, ErrIntegrity
		}
		dirs[parent][name] = GitEntry{OID: n.OID, Mode: "040000"}
	}
	return out, nil
}
func (e GitTreeEvidence) Validate() error {
	if e.Commit.Validate() != nil || len(e.Nodes) == 0 || len(e.Nodes) > 100001 {
		return ErrIntegrity
	}
	nodes := map[string]GitTreeNode{}
	for _, n := range e.Nodes {
		if n.Validate() != nil || len(n.OID) != len(e.Commit.Tree) {
			return ErrIntegrity
		}
		if _, ok := nodes[n.OID]; ok {
			return ErrIntegrity
		}
		nodes[n.OID] = n
	}
	seen := map[string]bool{}
	todo := []string{e.Commit.Tree}
	for len(todo) > 0 {
		id := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if seen[id] {
			continue
		}
		n, ok := nodes[id]
		if !ok {
			return ErrIntegrity
		}
		seen[id] = true
		for _, entry := range n.Entries {
			if entry.Mode == "040000" {
				todo = append(todo, entry.OID)
			}
		}
	}
	if len(seen) != len(nodes) {
		return ErrIntegrity
	}
	return nil
}

// NewGitTreeNode constructs a directory using the repository object format.
func NewGitTreeNode(entries map[string]GitEntry, oidWidth int) (GitTreeNode, error) {
	n := GitTreeNode{Entries: entries}
	var err error
	n.OID, err = n.objectID(oidWidth)
	return n, err
}
