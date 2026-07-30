package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// FileStore is a content-addressed local file store located in the .cxt/ directory of the repo root and implements the SessionStore outbound port (client-only; no SQL).
//
// It corresponds to a self-contained object DB for the .git directory (USER DECISION 2, compatibility rules). It adopts git's mental model of content-addressing: same bytes ⇒ same sha256 ⇒ automatic dedup.
//
// Disk layout (repoRoot = root of the repo working tree):
//
//	repoRoot/.cxt/objects/docs/<hex>       SessionDoc(CIR canonical bytes). Filename = sha256(canonical CIR).
//	repoRoot/.cxt/objects/snapshots/<hex>  Snapshot metadata JSON. hex = snapshot.ID(=DocHash) in hex.
//	repoRoot/.cxt/objects/memories/<hex>   MemoryDigest JSON.
//	repoRoot/.cxt/refs/heads/<branch>      branch ref → target Snapshot.ID text.
//	repoRoot/.cxt/refs/sessions/<name>     partial join remaining session pointers.
//	repoRoot/.cxt/refs/tags/<name>         tag ref → target Snapshot.ID text.
//	repoRoot/.cxt/HEAD                      symbolic ref (e.g., "ref: refs/heads/main") or direct hash.
//
// Note: docs and snapshots use the same hex (Snapshot.ID == DocHash), so they are separated into type-specific subdirectories.
// The native .claude/·AGENTS.md directory is not hijacked (only written to by MemorySink at load time).
//
// Write: Immutable objects are written using write-temp + atomic-rename. Mutable refs/HEADs are also atomic-rename.
type FileStore struct {
	// repoRoot is the root of the repo working tree where .cxt/ is located (store = repoRoot/.cxt).
	repoRoot string
}

// NewFileStore creates a FileStore.
func NewFileStore(repoRoot string) *FileStore {
	return &FileStore{repoRoot: repoRoot}
}

func (s *FileStore) storeDir() string { return filepath.Join(s.repoRoot, ".cxt") }

func hexOf(hash domain.ContentHash) string {
	return strings.TrimPrefix(string(hash), "sha256:")
}

func hashFromObjectName(name string) (domain.ContentHash, bool) {
	if strings.HasPrefix(name, ".") {
		return "", false
	}
	hash := domain.ContentHash("sha256:" + name)
	if domain.ValidateContentHash(hash) != nil {
		return "", false
	}
	return hash, true
}

func validateHashes(hashes ...domain.ContentHash) error {
	for _, h := range hashes {
		if err := domain.ValidateContentHash(h); err != nil {
			return err
		}
	}
	return nil
}

func validateSnapshotRefs(snap domain.Snapshot) error {
	if err := validateHashes(snap.ID, snap.DocHash); err != nil {
		return err
	}
	if snap.ID != snap.DocHash {
		return domain.ErrHashMismatch
	}
	if err := domain.ValidateOptionalContentHash(snap.MemoryHash); err != nil {
		return err
	}
	if err := domain.ValidateOptionalContentHash(snap.ClaudeSettings); err != nil {
		return err
	}
	if err := domain.ValidateOptionalContentHash(snap.AgentsSettings); err != nil {
		return err
	}
	if err := domain.ValidateOptionalContentHash(snap.CodexSettings); err != nil {
		return err
	}
	for _, p := range snap.Parents {
		if err := domain.ValidateContentHash(p); err != nil {
			return err
		}
	}
	for _, p := range snap.GraftParents {
		if err := domain.ValidateContentHash(p); err != nil {
			return err
		}
	}
	if snap.GraftSeq > domain.MaxGraftSeq {
		return fmt.Errorf("%w: graft sequence exceeds persistent range", domain.ErrHashMismatch)
	}
	return nil
}

func (s *FileStore) objectPath(kind string, hash domain.ContentHash) string {
	return filepath.Join(s.storeDir(), "objects", kind, hexOf(hash))
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func validateCxtDir(dir string) error {
	current := filepath.Clean(dir)
	var chain []string
	foundRoot := false
	for {
		chain = append(chain, current)
		if filepath.Base(current) == ".cxt" {
			foundRoot = true
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	if !foundRoot {
		return domain.ErrHashMismatch
	}
	for i := len(chain) - 1; i >= 0; i-- {
		info, err := os.Lstat(chain[i])
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return domain.ErrHashMismatch
		}
	}
	return nil
}

func validateCxtWritePath(path string) error {
	if err := validateCxtDir(filepath.Dir(path)); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return domain.ErrHashMismatch
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func readCxtFile(path string) ([]byte, error) {
	if err := validateCxtWritePath(path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func readCxtDir(path string) ([]os.DirEntry, error) {
	if err := validateCxtDir(path); err != nil {
		return nil, err
	}
	return os.ReadDir(path)
}

func removeCxtFile(path string) error {
	if err := validateCxtWritePath(path); err != nil {
		return err
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// writeAtomic creates the parent directory and writes the file atomically using write-temp + rename.
func writeAtomic(path string, data []byte) error {
	if err := validateCxtWritePath(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := validateCxtWritePath(path); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// PutDoc stores a SessionDoc by the content hash of its canonical CIR bytes.
// The returned hash is sha256(canonical bytes), so the top-level content hash provides integrity.
// Storage uses the chunk CAS in doc_chunks.go by default: append-only session prefixes are
// deduplicated across captures so only deltas are written. Documents that cannot be chunked
// (for example, documents with no events) fall back to whole-blob storage. Existing objects
// are a no-op (idempotent deduplication).
func (s *FileStore) PutDoc(_ context.Context, doc domain.SessionDoc) (domain.ContentHash, error) {
	cb, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		return "", err
	}
	h := domain.HashContent(cb)
	if doc.Hash != "" && doc.Hash != h {
		return "", domain.ErrHashMismatch
	}
	p := s.objectPath("docs", h)
	if fileExists(p) {
		if _, err := s.GetDoc(context.Background(), h); err != nil {
			return "", err
		}
		return h, nil
	}
	chunked, err := s.putDocChunked(h, cb)
	if err != nil {
		return "", err
	}
	if !chunked {
		if err := writeAtomic(p, docCompress(cb)); err != nil {
			return "", err
		}
	}
	return h, nil
}

// GetDoc retrieves a SessionDoc by ContentHash and returns domain.ErrNotFound when absent.
// Chunk-manifest documents are reassembled from chunks and checked against their canonical
// content hash. Legacy whole blobs are read from their original path.
func (s *FileStore) GetDoc(_ context.Context, hash domain.ContentHash) (domain.SessionDoc, error) {
	if err := domain.ValidateContentHash(hash); err != nil {
		return domain.SessionDoc{}, err
	}
	data, err := readCxtFile(s.objectPath("docs", hash))
	if err != nil {
		if os.IsNotExist(err) {
			return domain.SessionDoc{}, domain.ErrNotFound
		}
		return domain.SessionDoc{}, err
	}
	data, err = docDecompress(data)
	if err != nil {
		return domain.SessionDoc{}, domain.ErrInvalidCIR
	}
	if cb, isManifest, cerr := s.getDocChunked(hash, data); isManifest {
		if cerr != nil {
			return domain.SessionDoc{}, cerr
		}
		var cir domain.CIRDocument
		if err := json.Unmarshal(cb, &cir); err != nil {
			return domain.SessionDoc{}, domain.ErrInvalidCIR
		}
		// No additional recalculation is needed since the reassembled bytes have already been compared to the hash.
		return domain.SessionDoc{Hash: hash, CIR: cir}, nil
	}
	var cir domain.CIRDocument
	if err := json.Unmarshal(data, &cir); err != nil {
		return domain.SessionDoc{}, domain.ErrInvalidCIR
	}
	doc := domain.SessionDoc{Hash: hash, CIR: cir}
	if err := domain.ValidateSessionDocHash(doc); err != nil {
		return domain.SessionDoc{}, err
	}
	return doc, nil
}

// HasDoc determines the existence of a doc (body not loaded — for pull delta negotiation).
func (s *FileStore) HasDoc(_ context.Context, hash domain.ContentHash) (bool, error) {
	if err := domain.ValidateContentHash(hash); err != nil {
		return false, err
	}
	return fileExists(s.objectPath("docs", hash)), nil
}

// PutSnapshot stores Snapshot metadata. No-op if it already exists (immutable).
// dedupGraft removes duplicate self/parent grafts during adoption (reachability same — notation cleanup).
func dedupGraft(id domain.ContentHash, parents, graft []domain.ContentHash) []domain.ContentHash {
	seen := map[domain.ContentHash]bool{id: true}
	for _, p := range parents {
		seen[p] = true
	}
	out := make([]domain.ContentHash, 0, len(graft))
	for _, g := range graft {
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	return out
}

func (s *FileStore) PutSnapshot(_ context.Context, snap domain.Snapshot) error {
	if err := validateSnapshotRefs(snap); err != nil {
		return err
	}
	p := s.objectPath("snapshots", snap.ID)
	if fileExists(p) {
		// Immutable object: idempotent. Exception — only derived fields not in ID/content are updated:
		//   - MemoryHash attachment (derived pointer, compatibility rules): attaches if new value.
		//   - GraftParents overlay adoption: adds reachability edge during server diverged append merge (Parents original immutable). Not receiving results in local reachability walk divergence from server, leading to incorrect ff determination (replica convergence).
		existing, err := s.GetSnapshot(context.Background(), snap.ID)
		if err != nil {
			return err
		}
		changed := false
		if snap.MemoryHash != "" && existing.MemoryHash != snap.MemoryHash {
			// The derived pointer is replaceable after re-memorization; the body and ID remain immutable.
			existing.MemoryHash = snap.MemoryHash
			changed = true
		}
		if strings.HasPrefix(existing.Message, domain.HookMessagePrefix) &&
			snap.Message != "" && !strings.HasPrefix(snap.Message, domain.HookMessagePrefix) {
			// Message promotion (hook→commit, unidirectional): hook capture leaf becomes commit snapshot. If message hash dedup captures hook, commit message ([git sha] link included) is lost — hook label is a residual of progress, not a definitive label, so commit message overwrites it (stash-dedup message trap — label is safe outside hash-derived metadata). Reverse direction (commit→hook) is impossible.
			existing.Message = snap.Message
			changed = true
		}
		// Graft (set, seq) register merge: a higher seq replaces the entire set, allowing join reordering to supersede or remove edges. Legacy seq=0 values merge additively, and lower seqs are ignored. Different sets at the same positive seq are concurrent local/server projection conflicts. Pull must adopt the incoming server value so locally unioned edges removed by join stay removed.
		switch {
		case snap.GraftSeq > existing.GraftSeq:
			existing.GraftParents = dedupGraft(existing.ID, existing.Parents, snap.GraftParents)
			existing.GraftSeq = snap.GraftSeq
			existing.Grafted = len(existing.GraftParents) > 0 || snap.Grafted
			changed = true
		case snap.GraftSeq == 0 && existing.GraftSeq == 0 && len(snap.GraftParents) > 0:
			seen := map[domain.ContentHash]bool{existing.ID: true}
			for _, g := range existing.GraftParents {
				seen[g] = true
			}
			for _, pp := range existing.Parents {
				seen[pp] = true
			}
			for _, g := range snap.GraftParents {
				if g != "" && !seen[g] {
					seen[g] = true
					existing.GraftParents = append(existing.GraftParents, g)
					changed = true
				}
			}
			if snap.Grafted && !existing.Grafted {
				existing.Grafted = true
				changed = true
			}
		case snap.GraftSeq > 0 && snap.GraftSeq == existing.GraftSeq:
			incoming := dedupGraft(existing.ID, existing.Parents, snap.GraftParents)
			incomingGrafted := len(incoming) > 0 || snap.Grafted
			if !sameHashList(existing.GraftParents, incoming) || existing.Grafted != incomingGrafted {
				existing.GraftParents = incoming
				existing.Grafted = incomingGrafted
				changed = true
			}
		}
		// Stash → commit promotion: content-hash dedup captures same session content in stash and commit sides, one object assumes both roles. If initial "(stash)" label remains, push/web stash exclusion filter omits commit ancestry center, creating server parent loss. If non-stash storage occurs, label is promoted (branch/message are hash-derived metadata — ID immutable).
		if existing.Branch == domain.StashBranchLabel && snap.Branch != domain.StashBranchLabel {
			existing.Branch = snap.Branch
			existing.Message = snap.Message
			changed = true
		}
		if changed {
			b, _ := json.Marshal(existing)
			return writeAtomic(p, b)
		}
		return nil
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return writeAtomic(p, data)
}

func sameHashList(left, right []domain.ContentHash) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// ReconcileGraftState forces adoption of a server truth graft register after an
// authoritative GET. Local GraftSeq can be ahead of server due to a pending add,
// so this cannot be recovered with general PutSnapshot max-seq merge. Caller
// passes a validated GET response only.
func (s *FileStore) ReconcileGraftState(_ context.Context, authoritative domain.Snapshot) error {
	if err := validateSnapshotRefs(authoritative); err != nil {
		return err
	}
	existing, err := s.GetSnapshot(context.Background(), authoritative.ID)
	if err != nil {
		return err
	}
	if existing.RepoID != authoritative.RepoID || !sameHashList(existing.Parents, authoritative.Parents) {
		return domain.ErrHashMismatch
	}
	existing.GraftParents = dedupGraft(existing.ID, existing.Parents, authoritative.GraftParents)
	existing.Grafted = len(existing.GraftParents) > 0 || authoritative.Grafted
	existing.GraftSeq = authoritative.GraftSeq
	b, err := json.Marshal(existing)
	if err != nil {
		return err
	}
	return writeAtomic(s.objectPath("snapshots", existing.ID), b)
}

// GetSnapshot retrieves Snapshot metadata by ID. Returns domain.ErrNotFound if not found.
func (s *FileStore) GetSnapshot(_ context.Context, id domain.ContentHash) (domain.Snapshot, error) {
	if err := domain.ValidateContentHash(id); err != nil {
		return domain.Snapshot{}, err
	}
	data, err := readCxtFile(s.objectPath("snapshots", id))
	if err != nil {
		if os.IsNotExist(err) {
			return domain.Snapshot{}, domain.ErrNotFound
		}
		return domain.Snapshot{}, err
	}
	var snap domain.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return domain.Snapshot{}, err
	}
	if err := validateSnapshotRefs(snap); err != nil {
		return domain.Snapshot{}, err
	}
	if snap.ID != id {
		return domain.Snapshot{}, domain.ErrHashMismatch
	}
	return snap, nil
}

// ListSnapshots returns the snapshot list of repo/branch. If branch=="" it returns the entire repo.
func (s *FileStore) ListSnapshots(_ context.Context, _ string, branch string) ([]domain.Snapshot, error) {
	dir := filepath.Join(s.storeDir(), "objects", "snapshots")
	entries, err := readCxtDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []domain.Snapshot
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := hashFromObjectName(e.Name())
		if !ok {
			continue
		}
		snap, err := s.GetSnapshot(context.Background(), id)
		if err != nil {
			return nil, err
		}
		if branch == "" || snap.Branch == branch {
			out = append(out, snap)
		}
	}
	// git log meaning: latest commit first.
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// PutRef upserts a mutable pointer (HEAD/branch/session/tag).
func (s *FileStore) PutRef(_ context.Context, ref domain.Ref) error {
	if err := domain.ValidateRef(ref); err != nil {
		return err
	}
	switch ref.Kind {
	case domain.RefHEAD:
		content := string(ref.Target)
		if ref.Symbolic != "" {
			content = "ref: refs/heads/" + strings.TrimPrefix(ref.Symbolic, "refs/heads/")
		}
		return writeAtomic(filepath.Join(s.storeDir(), "HEAD"), []byte(content+"\n"))
	case domain.RefBranch:
		return writeAtomic(s.refPath("heads", ref.Name), []byte(string(ref.Target)+"\n"))
	case domain.RefSession:
		return writeAtomic(s.refPath("sessions", ref.Name), []byte(string(ref.Target)+"\n"))
	case domain.RefTag:
		return writeAtomic(s.refPath("tags", ref.Name), []byte(string(ref.Target)+"\n"))
	default:
		return domain.ErrNotFound
	}
}

func (s *FileStore) refPath(kind, name string) string {
	return filepath.Join(s.storeDir(), "refs", kind, filepath.FromSlash(name))
}

// GetRef retrieves a ref by (repoID, kind, name). Returns domain.ErrNotFound if not found.
func (s *FileStore) GetRef(_ context.Context, repoID string, kind domain.RefKind, name string) (domain.Ref, error) {
	if err := domain.ValidateRefName(kind, name); err != nil {
		return domain.Ref{}, err
	}
	switch kind {
	case domain.RefHEAD:
		data, err := readCxtFile(filepath.Join(s.storeDir(), "HEAD"))
		if err != nil {
			if os.IsNotExist(err) {
				return domain.Ref{}, domain.ErrNotFound
			}
			return domain.Ref{}, err
		}
		content := strings.TrimSpace(string(data))
		ref := domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repoID}
		if sym, ok := strings.CutPrefix(content, "ref: refs/heads/"); ok {
			ref.Symbolic = sym
		} else {
			ref.Target = domain.ContentHash(content)
		}
		if err := domain.ValidateRef(ref); err != nil {
			return domain.Ref{}, err
		}
		return ref, nil
	case domain.RefBranch, domain.RefSession, domain.RefTag:
		sub := "heads"
		if kind == domain.RefSession {
			sub = "sessions"
		} else if kind == domain.RefTag {
			sub = "tags"
		}
		data, err := readCxtFile(s.refPath(sub, name))
		if err != nil {
			if os.IsNotExist(err) {
				return domain.Ref{}, domain.ErrNotFound
			}
			return domain.Ref{}, err
		}
		ref := domain.Ref{Kind: kind, Name: name, RepoID: repoID, Target: domain.ContentHash(strings.TrimSpace(string(data)))}
		if err := domain.ValidateRef(ref); err != nil {
			return domain.Ref{}, err
		}
		return ref, nil
	default:
		return domain.Ref{}, domain.ErrNotFound
	}
}

// ListRefs lists all refs (HEAD + branches + session lines + tags) of a repo.
func (s *FileStore) ListRefs(ctx context.Context, repoID string) ([]domain.Ref, error) {
	var out []domain.Ref
	if head, err := s.GetRef(ctx, repoID, domain.RefHEAD, "HEAD"); err == nil {
		out = append(out, head)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	for _, kc := range []struct {
		sub  string
		kind domain.RefKind
	}{{"heads", domain.RefBranch}, {"sessions", domain.RefSession}, {"tags", domain.RefTag}} {
		root := filepath.Join(s.storeDir(), "refs", kc.sub)
		if err := validateCxtDir(root); err != nil {
			return nil, err
		}
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if info.IsDir() || strings.HasPrefix(filepath.Base(path), ".") {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			ref, rerr := s.GetRef(ctx, repoID, kc.kind, filepath.ToSlash(rel))
			if rerr != nil {
				return rerr
			}
			out = append(out, ref)
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return out, nil
}

// Manifest returns the catalog (snapshot index + ref list) of a repo.
func (s *FileStore) Manifest(ctx context.Context, repoID string) (domain.Manifest, error) {
	refs, err := s.ListRefs(ctx, repoID)
	if err != nil {
		return domain.Manifest{}, err
	}
	var index []domain.ContentHash
	dir := filepath.Join(s.storeDir(), "objects", "snapshots")
	if entries, err := readCxtDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			id, ok := hashFromObjectName(e.Name())
			if !ok {
				continue
			}
			if _, err := s.GetSnapshot(ctx, id); err != nil {
				return domain.Manifest{}, err
			}
			index = append(index, id)
		}
	} else if !os.IsNotExist(err) {
		return domain.Manifest{}, err
	}
	return domain.Manifest{
		RepoID:        repoID,
		Refs:          refs,
		SnapshotIndex: index,
		Version:       0,
		UpdatedAt:     time.Now().UTC(),
	}, nil
}

// PutMemory stores a MemoryDigest as a content-addressed blob. No-op if it already exists.
func (s *FileStore) PutMemory(_ context.Context, digest domain.MemoryDigest) (domain.ContentHash, error) {
	if err := domain.ValidateOptionalContentHash(digest.SnapshotID); err != nil {
		return "", err
	}
	data, err := json.Marshal(digest)
	if err != nil {
		return "", err
	}
	h := domain.HashContent(data)
	p := s.objectPath("memories", h)
	if fileExists(p) {
		if _, err := s.GetMemory(context.Background(), h); err != nil {
			return "", err
		}
	} else {
		if err := writeAtomic(p, data); err != nil {
			return "", err
		}
	}
	return h, nil
}

// GetMemory retrieves a MemoryDigest by ContentHash. Returns domain.ErrNotFound if not found.
func (s *FileStore) GetMemory(_ context.Context, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if err := domain.ValidateContentHash(hash); err != nil {
		return domain.MemoryDigest{}, err
	}
	data, err := readCxtFile(s.objectPath("memories", hash))
	if err != nil {
		if os.IsNotExist(err) {
			return domain.MemoryDigest{}, domain.ErrNotFound
		}
		return domain.MemoryDigest{}, err
	}
	var d domain.MemoryDigest
	if err := json.Unmarshal(data, &d); err != nil {
		return domain.MemoryDigest{}, err
	}
	got, err := domain.MemoryDigestHash(d)
	if err != nil {
		return domain.MemoryDigest{}, err
	}
	if got != hash {
		return domain.MemoryDigest{}, domain.ErrHashMismatch
	}
	if err := domain.ValidateOptionalContentHash(d.SnapshotID); err != nil {
		return domain.MemoryDigest{}, err
	}
	return d, nil
}

// Ensure FileStore implements outbound.SessionStore.
var _ outbound.SessionStore = (*FileStore)(nil)

// --- stash stack (git stash handling, .cxt/stash.json — newest at front) ---

func (s *FileStore) stashPath() string { return filepath.Join(s.storeDir(), "stash.json") }

func (s *FileStore) readStash() ([]domain.StashEntry, error) {
	data, err := readCxtFile(s.stashPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []domain.StashEntry
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *FileStore) writeStash(entries []domain.StashEntry) error {
	data, _ := json.MarshalIndent(entries, "", "  ")
	return writeAtomic(s.stashPath(), data)
}

// StashPush pushes an item to the front of the stack (stash@{0}).
func (s *FileStore) StashPush(_ context.Context, _ string, e domain.StashEntry) error {
	entries, err := s.readStash()
	if err != nil {
		return err
	}
	return s.writeStash(append([]domain.StashEntry{e}, entries...))
}

// StashPop removes and returns the latest item. Returns ErrNotFound if empty.
func (s *FileStore) StashPop(_ context.Context, _ string) (domain.StashEntry, error) {
	entries, err := s.readStash()
	if err != nil {
		return domain.StashEntry{}, err
	}
	if len(entries) == 0 {
		return domain.StashEntry{}, domain.ErrNotFound
	}
	top := entries[0]
	if err := s.writeStash(entries[1:]); err != nil {
		return domain.StashEntry{}, err
	}
	return top, nil
}

// StashList returns the entire stack in newest order.
func (s *FileStore) StashList(_ context.Context, _ string) ([]domain.StashEntry, error) {
	return s.readStash()
}

// --- in-progress context pointer (.cxt/pending/<sessionID>.json — session-specific upsert) ---

func (s *FileStore) pendingPath(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(s.storeDir(), "pending", hex.EncodeToString(sum[:])+".json")
}

func (s *FileStore) legacyPendingPath(sessionID string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, sessionID)
	return filepath.Join(s.storeDir(), "pending", safe+".json")
}

// PutPending overwrites the session's pending pointer (hook capture "sliding" = this upsert).
func (s *FileStore) PutPending(_ context.Context, p domain.Pending) error {
	if p.SessionID == "" || len(p.SessionID) > 128 {
		return errors.New("pending: session_id is required as the pointer key")
	}
	if err := domain.ValidateContentHash(p.Target); err != nil {
		return err
	}
	if p.RepoID != "" {
		if err := domain.ValidateContentHash(domain.ContentHash(p.RepoID)); err != nil {
			return err
		}
	}
	if p.Branch != "" {
		if err := domain.ValidateBranchName(p.Branch); err != nil {
			return err
		}
	}
	data, _ := json.MarshalIndent(p, "", "  ")
	if err := writeAtomic(s.pendingPath(p.SessionID), data); err != nil {
		return err
	}
	_ = removeCxtFile(s.legacyPendingPath(p.SessionID))
	return nil
}

// ListPendings returns all pendings in the repo (order not guaranteed — caller must sort).
func (s *FileStore) ListPendings(_ context.Context, _ string) ([]domain.Pending, error) {
	entries, err := readCxtDir(filepath.Join(s.storeDir(), "pending"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	bySession := make(map[string]domain.Pending)
	fromOpaque := make(map[string]bool)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, rerr := readCxtFile(filepath.Join(s.storeDir(), "pending", e.Name()))
		if rerr != nil {
			continue
		}
		var p domain.Pending
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		if p.SessionID == "" || len(p.SessionID) > 128 {
			return nil, domain.ErrHashMismatch
		}
		if err := domain.ValidateContentHash(p.Target); err != nil {
			return nil, err
		}
		if p.RepoID != "" {
			if err := domain.ValidateContentHash(domain.ContentHash(p.RepoID)); err != nil {
				return nil, err
			}
		}
		if p.Branch != "" {
			if err := domain.ValidateBranchName(p.Branch); err != nil {
				return nil, err
			}
		}
		isOpaque := e.Name() == filepath.Base(s.pendingPath(p.SessionID))
		if !fromOpaque[p.SessionID] || isOpaque {
			bySession[p.SessionID] = p
			fromOpaque[p.SessionID] = isOpaque
		}
	}
	out := make([]domain.Pending, 0, len(bySession))
	for _, p := range bySession {
		out = append(out, p)
	}
	return out, nil
}

// DeletePending removes the session's pending state (no error if not present — idempotent).
func (s *FileStore) DeletePending(_ context.Context, _ string, sessionID string) error {
	if sessionID == "" || len(sessionID) > 128 {
		return domain.ErrHashMismatch
	}
	for _, path := range []string{s.pendingPath(sessionID), s.legacyPendingPath(sessionID)} {
		if err := removeCxtFile(path); err != nil {
			return err
		}
	}
	return nil
}

// DeleteSnapshot removes the snapshot metadata object (hook capture leaf GC exclusive — idempotent).
func (s *FileStore) DeleteSnapshot(_ context.Context, id domain.ContentHash) error {
	if err := domain.ValidateContentHash(id); err != nil {
		return err
	}
	return removeCxtFile(s.objectPath("snapshots", id))
}

// DeleteDoc removes the doc body object (hook capture leaf GC exclusive — idempotent).
// For chunked docs, only the manifest is deleted — chunks are shared prefixes for subsequent capture docs,
// so deleting here is incorrect, and orphaned chunks are cleaned up by RepackDocs' mark&sweep.
func (s *FileStore) DeleteDoc(_ context.Context, hash domain.ContentHash) error {
	if err := domain.ValidateContentHash(hash); err != nil {
		return err
	}
	return removeCxtFile(s.objectPath("docs", hash))
}

// --- Configuration folder snapshot object (content-addressed) ---

// PutSettingsObject stores the SettingsBundle (kind+files only hash target) and returns the hash.
func (s *FileStore) PutSettingsObject(_ context.Context, bundle domain.SettingsBundle) (domain.ContentHash, error) {
	if err := domain.ValidateSettingsBundle(bundle.Kind, "", bundle); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(struct {
		Kind  string                `json:"kind"`
		Files []domain.SettingsFile `json:"files"`
	}{bundle.Kind, bundle.Files})
	if err != nil {
		return "", err
	}
	h, err := domain.SettingsObjectHash(bundle)
	if err != nil {
		return "", err
	}
	p := s.objectPath("settingsobjs", h)
	if fileExists(p) {
		if _, err := s.GetSettingsObject(context.Background(), h); err != nil {
			return "", err
		}
	} else {
		if err := writeAtomic(p, canonical); err != nil {
			return "", err
		}
	}
	return h, nil
}

// GetSettingsObject retrieves the settings object.
func (s *FileStore) GetSettingsObject(_ context.Context, hash domain.ContentHash) (domain.SettingsBundle, error) {
	if err := domain.ValidateContentHash(hash); err != nil {
		return domain.SettingsBundle{}, err
	}
	data, err := readCxtFile(s.objectPath("settingsobjs", hash))
	if err != nil {
		if os.IsNotExist(err) {
			return domain.SettingsBundle{}, domain.ErrNotFound
		}
		return domain.SettingsBundle{}, err
	}
	var b domain.SettingsBundle
	if err := json.Unmarshal(data, &b); err != nil {
		return domain.SettingsBundle{}, err
	}
	if err := domain.ValidateSettingsBundle(b.Kind, hash, b); err != nil {
		return domain.SettingsBundle{}, err
	}
	return b, nil
}
