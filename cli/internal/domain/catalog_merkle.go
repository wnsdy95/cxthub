package domain

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

const (
	CatalogMerkleVersion = 1
	CatalogMerkleScope   = "sync-metadata-merkle-v1"
	// Hash preimages are the UTF-8 domain separator (including NUL) followed
	// by canonicalJSON. Key payloads are [kind, normalized-key]; node payloads
	// contain every node field and complete normalized entry values.
	catalogMerkleKeyDomain  = CatalogMerkleScope + ":key\x00"
	catalogMerkleNodeDomain = CatalogMerkleScope + ":node\x00"
	catalogMerkleHex        = "0123456789abcdef"
)

type CatalogMerkleChild struct {
	Prefix string      `json:"prefix"`
	Hash   ContentHash `json:"hash"`
	Count  int64       `json:"count"`
}

type CatalogMerkleNode struct {
	Version  int                  `json:"version"`
	Scope    string               `json:"scope"`
	RepoID   ContentHash          `json:"repo_id"`
	Prefix   string               `json:"prefix"`
	Count    int64                `json:"count"`
	Children []CatalogMerkleChild `json:"children"`
	Entries  []CatalogEntry       `json:"entries"`
}

type CatalogMerkleRequest struct {
	Version    int                `json:"version"`
	RootHash   ContentHash        `json:"root_hash,omitempty"`
	Checkpoint *CatalogCheckpoint `json:"checkpoint,omitempty"`
	Prefix     string             `json:"prefix,omitempty"`
	Offset     int                `json:"offset"`
	Limit      int                `json:"limit"`
}

// CatalogMerklePage binds one node (or part of a leaf) to an immutable root.
// Checkpoint is a source boundary, never an acknowledgement of a partial image.
type CatalogMerklePage struct {
	Version    int                  `json:"version"`
	Scope      string               `json:"scope"`
	RepoID     ContentHash          `json:"repo_id"`
	Checkpoint CatalogCheckpoint    `json:"checkpoint"`
	RootHash   ContentHash          `json:"root_hash"`
	Prefix     string               `json:"prefix"`
	NodeHash   ContentHash          `json:"node_hash"`
	Count      int64                `json:"count"`
	Children   []CatalogMerkleChild `json:"children"`
	Entries    []CatalogEntry       `json:"entries"`
	Offset     int                  `json:"offset"`
	NextOffset *int                 `json:"next_offset,omitempty"`
}

// MarshalJSON keeps both collection fields nonnull, even for empty literals.
func (n CatalogMerkleNode) MarshalJSON() ([]byte, error) {
	type wire CatalogMerkleNode
	if n.Children == nil {
		n.Children = []CatalogMerkleChild{}
	}
	if n.Entries == nil {
		n.Entries = []CatalogEntry{}
	}
	return json.Marshal(wire(n))
}

func (p CatalogMerklePage) MarshalJSON() ([]byte, error) {
	type wire CatalogMerklePage
	if p.Children == nil {
		p.Children = []CatalogMerkleChild{}
	}
	if p.Entries == nil {
		p.Entries = []CatalogEntry{}
	}
	return json.Marshal(wire(p))
}

func catalogMerklePrefixValid(prefix string) bool {
	if len(prefix) > 2 {
		return false
	}
	for _, c := range prefix {
		if !strings.ContainsRune(catalogMerkleHex, c) {
			return false
		}
	}
	return true
}

func catalogMerkleCheckpointValid(repo ContentHash, cp CatalogCheckpoint) bool {
	return ValidateContentHash(repo) == nil && cp.Version == CatalogVersion && string(cp.RepoID) == string(repo) && catalogEpochValid(cp.Epoch) && cp.Sequence >= 0
}

func (r CatalogMerkleRequest) Validate() error {
	if r.Version != CatalogMerkleVersion || !catalogMerklePrefixValid(r.Prefix) || r.Offset < 0 || r.Limit < 0 || r.Limit > MaxCatalogLimit {
		return catalogMerkleInvalid("request shape")
	}
	if (r.RootHash == "") != (r.Checkpoint == nil) {
		return catalogMerkleInvalid("root hash and checkpoint must appear together")
	}
	if r.RootHash == "" {
		if r.Prefix != "" || r.Offset != 0 {
			return catalogMerkleInvalid("latest request must select root")
		}
	} else if ValidateContentHash(r.RootHash) != nil || !catalogMerkleCheckpointValid(ContentHash(r.Checkpoint.RepoID), *r.Checkpoint) {
		return catalogMerkleInvalid("frozen root identity")
	}
	if len(r.Prefix) < 2 && r.Offset != 0 {
		return catalogMerkleInvalid("branch offset")
	}
	return nil
}

// CatalogMerkleBucket hashes only the typed logical key. It also accepts keys
// from deletions so incremental builders can locate the bucket to rewrite.
func CatalogMerkleBucket(entry CatalogEntry) (string, error) {
	key, err := catalogMerkleNormalizedKey(entry)
	if err != nil {
		return "", err
	}
	raw, err := canonicalJSON([2]string{entry.Kind, key})
	if err != nil {
		return "", err
	}
	hash := HashContent(append([]byte(catalogMerkleKeyDomain), raw...))
	return string(hash)[len("sha256:") : len("sha256:")+2], nil
}

func catalogMerkleNormalizedKey(e CatalogEntry) (string, error) {
	switch e.Kind {
	case "snapshot", "protocol":
		if ValidateContentHash(ContentHash(e.Key)) != nil {
			return "", catalogMerkleInvalid("entry key")
		}
	case "history":
		if len(e.Key) != 32 || strings.Trim(e.Key, catalogMerkleHex) != "" {
			return "", catalogMerkleInvalid("history key")
		}
	case "ref":
		key, err := catalogMerkleRefKey(e.Key)
		if err != nil {
			return "", err
		}
		raw, err := canonicalJSON(key)
		return string(raw), err
	default:
		return "", catalogMerkleInvalid("unknown entry kind")
	}
	return e.Key, nil
}

// NewCatalogMerkleLeaf owns a copy of its input and orders entries by kind and
// normalized key. It preserves original keys, raw values, and sequences in the
// returned node; normalization affects only identity, ordering, and hashing.
func NewCatalogMerkleLeaf(repo ContentHash, prefix string, entries []CatalogEntry) (CatalogMerkleNode, error) {
	n := CatalogMerkleNode{Version: CatalogMerkleVersion, Scope: CatalogMerkleScope, RepoID: repo, Prefix: prefix, Count: int64(len(entries)), Children: []CatalogMerkleChild{}, Entries: make([]CatalogEntry, len(entries))}
	if len(prefix) != 2 {
		return CatalogMerkleNode{}, catalogMerkleInvalid("leaf prefix")
	}
	keys := make(map[string]string, len(entries))
	for i, e := range entries {
		key, err := catalogMerkleNormalizedKey(e)
		if err != nil {
			return CatalogMerkleNode{}, err
		}
		keys[e.Kind+"\x00"+e.Key] = key
		e.Value = append(json.RawMessage(nil), e.Value...)
		n.Entries[i] = e
	}
	sort.Slice(n.Entries, func(i, j int) bool {
		a, b := n.Entries[i], n.Entries[j]
		return a.Kind < b.Kind || (a.Kind == b.Kind && keys[a.Kind+"\x00"+a.Key] < keys[b.Kind+"\x00"+b.Key])
	})
	if _, err := catalogMerkleCanonicalNode(n); err != nil {
		return CatalogMerkleNode{}, err
	}
	return n, nil
}

// NewCatalogMerkleBranch requires all sixteen children in prefix order. Missing
// buckets are represented by their empty leaf/branch hashes, never omitted.
func NewCatalogMerkleBranch(repo ContentHash, prefix string, children []CatalogMerkleChild) (CatalogMerkleNode, error) {
	n := CatalogMerkleNode{Version: CatalogMerkleVersion, Scope: CatalogMerkleScope, RepoID: repo, Prefix: prefix, Children: append([]CatalogMerkleChild{}, children...), Entries: []CatalogEntry{}}
	if len(prefix) > 1 {
		return CatalogMerkleNode{}, catalogMerkleInvalid("branch prefix")
	}
	for _, child := range children {
		if child.Count < 0 || child.Count > math.MaxInt64-n.Count {
			return CatalogMerkleNode{}, catalogMerkleInvalid("child count overflow")
		}
		n.Count += child.Count
	}
	if _, err := catalogMerkleCanonicalNode(n); err != nil {
		return CatalogMerkleNode{}, err
	}
	return n, nil
}

// catalogMerkleCanonicalNode validates before normalizing. Hashing never fixes
// a malformed received node's order, count, bucket placement, or duplicate keys.
func catalogMerkleCanonicalNode(n CatalogMerkleNode) (CatalogMerkleNode, error) {
	if n.Version != CatalogMerkleVersion || n.Scope != CatalogMerkleScope || ValidateContentHash(n.RepoID) != nil || !catalogMerklePrefixValid(n.Prefix) || n.Count < 0 || n.Children == nil || n.Entries == nil {
		return CatalogMerkleNode{}, catalogMerkleInvalid("node identity or shape")
	}
	if len(n.Prefix) < 2 {
		if len(n.Children) != 16 || len(n.Entries) != 0 {
			return CatalogMerkleNode{}, catalogMerkleInvalid("branch shape")
		}
		var count int64
		for i, child := range n.Children {
			if child.Prefix != n.Prefix+string(catalogMerkleHex[i]) || ValidateContentHash(child.Hash) != nil || child.Count < 0 || child.Count > math.MaxInt64-count {
				return CatalogMerkleNode{}, catalogMerkleInvalid("child identity, order, or count")
			}
			count += child.Count
		}
		if count != n.Count {
			return CatalogMerkleNode{}, catalogMerkleInvalid("branch count")
		}
		return n, nil
	}
	if len(n.Children) != 0 || n.Count != int64(len(n.Entries)) {
		return CatalogMerkleNode{}, catalogMerkleInvalid("leaf shape or count")
	}
	entries, err := catalogMerkleCanonicalEntries(n.RepoID, n.Prefix, n.Entries)
	if err != nil {
		return CatalogMerkleNode{}, err
	}
	n.Entries = entries
	return n, nil
}

func catalogMerkleCanonicalEntries(repo ContentHash, prefix string, entries []CatalogEntry) ([]CatalogEntry, error) {
	out := make([]CatalogEntry, len(entries))
	var previousKind, previousKey string
	for i, e := range entries {
		if e.Deleted || catalogMerkleValidateEntry(repo, e) != nil {
			return nil, catalogMerkleInvalid("invalid live entry")
		}
		key, err := catalogMerkleNormalizedKey(e)
		if err != nil {
			return nil, err
		}
		bucket, err := CatalogMerkleBucket(e)
		if err != nil || bucket != prefix {
			return nil, catalogMerkleInvalid("entry bucket")
		}
		if i > 0 && !(previousKind < e.Kind || (previousKind == e.Kind && previousKey < key)) {
			return nil, catalogMerkleInvalid("entry order or duplicate semantic key")
		}
		previousKind, previousKey = e.Kind, key
		value, err := canonicalJSON(e.Value)
		if err != nil {
			return nil, catalogMerkleInvalid("entry JSON")
		}
		e.Key, e.Value = key, value
		out[i] = e
	}
	return out, nil
}

// CatalogMerkleHash includes scope, repository, prefix, counts, child descriptors,
// original sequence, and every canonical value field. Checkpoints bind roots
// separately, so epoch rotation alone does not alter the metadata tree.
func CatalogMerkleHash(node CatalogMerkleNode) (ContentHash, error) {
	normalized, err := catalogMerkleCanonicalNode(node)
	if err != nil {
		return "", err
	}
	raw, err := canonicalJSON(normalized)
	if err != nil {
		return "", err
	}
	return HashContent(append([]byte(catalogMerkleNodeDomain), raw...)), nil
}

// BuildCatalogMerkle builds all 273 nodes, including empty buckets. No partial
// result is returned on error. Modules intentionally own independent copies.
func BuildCatalogMerkle(repo ContentHash, entries []CatalogEntry) (root CatalogMerkleNode, nodes map[ContentHash]CatalogMerkleNode, err error) {
	if ValidateContentHash(repo) != nil {
		return CatalogMerkleNode{}, nil, catalogMerkleInvalid("repository")
	}
	buckets := make(map[string][]CatalogEntry, 256)
	for _, entry := range entries {
		bucket, err := CatalogMerkleBucket(entry)
		if err != nil {
			return CatalogMerkleNode{}, nil, err
		}
		buckets[bucket] = append(buckets[bucket], entry)
	}
	nodes = make(map[ContentHash]CatalogMerkleNode, 273)
	roots := make([]CatalogMerkleChild, 0, 16)
	for _, high := range catalogMerkleHex {
		prefix := string(high)
		children := make([]CatalogMerkleChild, 0, 16)
		for _, low := range catalogMerkleHex {
			bucket := prefix + string(low)
			leaf, err := NewCatalogMerkleLeaf(repo, bucket, buckets[bucket])
			if err != nil {
				return CatalogMerkleNode{}, nil, err
			}
			hash, err := CatalogMerkleHash(leaf)
			if err != nil {
				return CatalogMerkleNode{}, nil, err
			}
			nodes[hash] = leaf
			children = append(children, CatalogMerkleChild{Prefix: bucket, Hash: hash, Count: leaf.Count})
		}
		branch, err := NewCatalogMerkleBranch(repo, prefix, children)
		if err != nil {
			return CatalogMerkleNode{}, nil, err
		}
		hash, err := CatalogMerkleHash(branch)
		if err != nil {
			return CatalogMerkleNode{}, nil, err
		}
		nodes[hash] = branch
		roots = append(roots, CatalogMerkleChild{Prefix: prefix, Hash: hash, Count: branch.Count})
	}
	root, err = NewCatalogMerkleBranch(repo, "", roots)
	if err != nil {
		return CatalogMerkleNode{}, nil, err
	}
	hash, err := CatalogMerkleHash(root)
	if err != nil {
		return CatalogMerkleNode{}, nil, err
	}
	nodes[hash] = root
	return root, nodes, nil
}

// Validate binds a response to its request and checks complete node hashes.
// Partial leaf pages require callers to concatenate all entries and verify
// CatalogMerkleHash against NodeHash before reusing or installing any entries.
// The caller must also match subtree NodeHash/Count to the verified parent.
func (p CatalogMerklePage) Validate(repo ContentHash, request CatalogMerkleRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if p.Version != CatalogMerkleVersion || p.Scope != CatalogMerkleScope || p.RepoID != repo || !catalogMerkleCheckpointValid(repo, p.Checkpoint) || ValidateContentHash(p.RootHash) != nil || ValidateContentHash(p.NodeHash) != nil || p.Prefix != request.Prefix || p.Offset != request.Offset || p.Count < 0 || p.Children == nil || p.Entries == nil {
		return catalogMerkleInvalid("page identity or shape")
	}
	if request.RootHash != "" && (request.RootHash != p.RootHash || *request.Checkpoint != p.Checkpoint) {
		return catalogMerkleInvalid("page frozen root boundary")
	}
	if p.Prefix == "" && p.NodeHash != p.RootHash {
		return catalogMerkleInvalid("root node hash")
	}
	node := CatalogMerkleNode{Version: p.Version, Scope: p.Scope, RepoID: p.RepoID, Prefix: p.Prefix, Count: p.Count, Children: p.Children, Entries: p.Entries}
	if len(p.Prefix) < 2 {
		if p.NextOffset != nil {
			return catalogMerkleInvalid("branch continuation")
		}
	} else {
		limit := request.Limit
		if limit == 0 {
			limit = DefaultCatalogLimit
		}
		if len(p.Children) != 0 || len(p.Entries) > limit || int64(p.Offset) > p.Count || int64(len(p.Entries)) > p.Count-int64(p.Offset) {
			return catalogMerkleInvalid("leaf page bounds")
		}
		end := int64(p.Offset) + int64(len(p.Entries))
		if end < p.Count {
			if len(p.Entries) == 0 || p.NextOffset == nil || int64(*p.NextOffset) != end {
				return catalogMerkleInvalid("leaf continuation progress")
			}
		} else if p.NextOffset != nil || (p.Offset != 0 && len(p.Entries) == 0) {
			return catalogMerkleInvalid("final leaf continuation")
		}
		if _, err := catalogMerkleCanonicalEntries(repo, p.Prefix, p.Entries); err != nil {
			return err
		}
		for _, entry := range p.Entries {
			if entry.Sequence > p.Checkpoint.Sequence {
				return catalogMerkleInvalid("entry beyond checkpoint")
			}
		}
		if p.Offset != 0 || p.NextOffset != nil {
			return nil
		}
	}
	hash, err := CatalogMerkleHash(node)
	if err != nil {
		return err
	}
	if hash != p.NodeHash {
		return catalogMerkleInvalid("node hash mismatch")
	}
	return nil
}
