// Package store provides server-side metadata/blob storage adapters.
//
// The default adapter is FSStore (file-based, content-addressed), operating end-to-end without external dependencies (stdlib).
// It enables REST server push/pull operations (demonstrable without a Postgres server).
// The production target PostgreSQL adapter is gated under //go:build postgres in a separate file (pgx), and store.Open(factory) selects the appropriate implementation based on the build tag.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Store is the union of storage capabilities the server requires (factory return type).
type Store interface {
	outbound.MetadataStore
	outbound.BlobStore
	outbound.WorkspaceStore
	// ApplyMigrations applies schema migrations idempotently (FS is no-op).
	ApplyMigrations(ctx context.Context, dir string) (int, error)
}

// FSStore is a file system-based content-addressed server store (MetadataStore + BlobStore).
//
// Multi-tenant layout (repoHex = repoID in hex):
//
//	dataDir/repos/<repoHex>/repo.json
//	dataDir/repos/<repoHex>/objects/docs/<docHex>          (SessionDoc.CIR JSON; key = client-provided doc.Hash)
//	dataDir/repos/<repoHex>/objects/chunks/<chunkHex>      (compressed transcript component)
//	dataDir/repos/<repoHex>/objects/memories/<hex>         (MemoryDigest JSON)
//	dataDir/repos/<repoHex>/objects/memory_chunks/<hex>    (compressed memory component)
//	dataDir/repos/<repoHex>/snapshots/<idHex>              (Snapshot metadata JSON; key = client-provided snap.ID)
//	dataDir/repos/<repoHex>/refs/heads/<name> · refs/sessions/<name> · refs/tags/<name> · HEAD
//	dataDir/repos/<repoHex>/memmeta/<snapHex>.json         (MemoryDigest metadata)
//	dataDir/repos/<repoHex>/pending/<sessionID>.json       (uncommitted capture pointer)
//
// Content-addressing uses client-provided hash as key (server is consumer of body integrity, data model Q3).
type FSStore struct {
	dataDir     string
	recoveryErr error
}

// NewFSStore creates an FSStore rooted at dataDir.
func NewFSStore(dataDir string) *FSStore {
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	s := &FSStore{dataDir: dataDir}
	s.recoveryErr = s.recoverJoinJournals()
	return s
}

// OpenFSStore returns the store only after recovering an incomplete join journal. The production start path must always use this checked constructor, while NewFSStore is for testing.
func OpenFSStore(dataDir string) (*FSStore, error) {
	s := NewFSStore(dataDir)
	if s.recoveryErr != nil {
		return nil, s.recoveryErr
	}
	return s, nil
}

var _ Store = (*FSStore)(nil)

// --- Path/IO Helpers ---

func hexOf(h domain.ContentHash) string { return strings.TrimPrefix(string(h), "sha256:") }

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

func validateHash(h domain.ContentHash) error {
	return domain.ValidateContentHash(h)
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
	if err := validateHashes(snap.RepoID, snap.ID, snap.DocHash); err != nil {
		return err
	}
	if snap.ID != snap.DocHash {
		return domain.ErrIntegrity
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
		if err := validateHash(p); err != nil {
			return err
		}
	}
	for _, p := range snap.GraftParents {
		if err := validateHash(p); err != nil {
			return err
		}
	}
	if snap.GraftSeq > domain.MaxGraftSeq {
		return fmt.Errorf("%w: graft sequence exceeds persistent range", domain.ErrIntegrity)
	}
	return nil
}

func validateSettingsKind(kind string) error {
	if !domain.ValidSettingsKind(kind) {
		return domain.ErrValidation
	}
	return nil
}

func (s *FSStore) repoDir(repoID domain.ContentHash) string {
	return filepath.Join(s.dataDir, "repos", hexOf(repoID))
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// Renaming itself ensures durable directory entries. Using the same helper for all metadata writes provides the same guarantee. Renaming the directory entry ensures that the boundary persists after a power failure.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// removeFileDurable syncs the deleted directory entry to the filesystem. Joining the journal after a power outage or during a prepared rollback can lead to discrepancies in the recovery state and audit log. Therefore, plain Remove should not be used at the transaction boundary.
func removeFileDurable(path string) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// --- Repo ---

// GetRepo reads the repo.json. If it doesn't exist, it returns domain.ErrNotFound.
func (s *FSStore) GetRepo(_ context.Context, id domain.ContentHash) (domain.Repo, error) {
	if err := validateHash(id); err != nil {
		return domain.Repo{}, err
	}
	data, err := os.ReadFile(filepath.Join(s.repoDir(id), "repo.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return domain.Repo{}, domain.ErrNotFound
		}
		return domain.Repo{}, err
	}
	var r domain.Repo
	if err := json.Unmarshal(data, &r); err != nil {
		return domain.Repo{}, err
	}
	if err := validateHash(r.ID); err != nil || r.ID != id {
		return domain.Repo{}, domain.ErrIntegrity
	}
	if r.DefaultBranch != "" {
		if err := domain.ValidateBranchName(r.DefaultBranch); err != nil {
			return domain.Repo{}, err
		}
	}
	r.LocalPath = ""
	r.RemoteURL = domain.SanitizeRemoteURL(r.RemoteURL)
	r.GitRemoteURL = domain.SanitizeRemoteURL(r.GitRemoteURL)
	return r, nil
}

// PutRepo is idempotent: if it already exists, it returns the existing record (sync protocol). Exception: for unowned (workspace_id="") records, it fills in a delayed binding if a new binding arrives.
func (s *FSStore) PutRepo(ctx context.Context, repo domain.Repo) (domain.Repo, error) {
	if err := validateHash(repo.ID); err != nil {
		return domain.Repo{}, err
	}
	if repo.DefaultBranch != "" {
		if err := domain.ValidateBranchName(repo.DefaultBranch); err != nil {
			return domain.Repo{}, err
		}
	}
	repo.LocalPath = ""
	repo.RemoteURL = domain.SanitizeRemoteURL(repo.RemoteURL)
	repo.GitRemoteURL = domain.SanitizeRemoteURL(repo.GitRemoteURL)
	if existing, err := s.GetRepo(ctx, repo.ID); err == nil {
		changed := false
		if existing.WorkspaceID == "" && repo.WorkspaceID != "" {
			existing.WorkspaceID = repo.WorkspaceID
			changed = true
		}
		// default_branch/git_remote_url updates make the latest pushed value the source of truth for local Git metadata.
		if repo.DefaultBranch != "" && repo.DefaultBranch != existing.DefaultBranch {
			existing.DefaultBranch = repo.DefaultBranch
			changed = true
		}
		if repo.GitRemoteURL != "" && repo.GitRemoteURL != existing.GitRemoteURL {
			existing.GitRemoteURL = repo.GitRemoteURL
			changed = true
		}
		if changed {
			data, _ := json.Marshal(existing)
			if werr := writeAtomic(filepath.Join(s.repoDir(existing.ID), "repo.json"), data); werr != nil {
				return domain.Repo{}, werr
			}
		}
		return existing, nil
	}
	data, _ := json.Marshal(repo)
	if err := writeAtomic(filepath.Join(s.repoDir(repo.ID), "repo.json"), data); err != nil {
		return domain.Repo{}, err
	}
	return repo, nil
}

// UpdateRepoAbout updates the About(description/website/topics).
func (s *FSStore) UpdateRepoAbout(ctx context.Context, id domain.ContentHash, description, website string, topics []string) error {
	if err := validateHash(id); err != nil {
		return err
	}
	r, err := s.GetRepo(ctx, id)
	if err != nil {
		return err
	}
	r.Description, r.Website, r.Topics = description, website, topics
	data, _ := json.Marshal(r)
	return writeAtomic(filepath.Join(s.repoDir(id), "repo.json"), data)
}

// UpdateRepoConfig updates the default branch and protected branch settings (nil = no change).
func (s *FSStore) UpdateRepoConfig(ctx context.Context, id domain.ContentHash, defaultBranch *string, protectDefault *bool) error {
	if err := validateHash(id); err != nil {
		return err
	}
	r, err := s.GetRepo(ctx, id)
	if err != nil {
		return err
	}
	if defaultBranch != nil && *defaultBranch != "" {
		r.DefaultBranch = *defaultBranch
	}
	if protectDefault != nil {
		r.ProtectDefault = *protectDefault
	}
	data, _ := json.Marshal(r)
	return writeAtomic(filepath.Join(s.repoDir(id), "repo.json"), data)
}

// PutSettingsBundle stores the team default settings bundle (.claude/.agents/.codex).
func (s *FSStore) PutSettingsBundle(_ context.Context, repoID domain.ContentHash, bundle domain.SettingsBundle) error {
	if err := validateHash(repoID); err != nil {
		return err
	}
	if err := domain.ValidateSettingsBundle(bundle.Kind, "", bundle); err != nil {
		return err
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.repoDir(repoID), "settings", bundle.Kind+".json"), data)
}

func opaquePointerName(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte{byte(len(part) >> 24), byte(len(part) >> 16), byte(len(part) >> 8), byte(len(part))})
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// pendingPath writes a full SHA-256 filename that exposes or conflicts with the original.
func (s *FSStore) pendingPath(repoID domain.ContentHash, sessionID string) string {
	return filepath.Join(s.repoDir(repoID), "pending", opaquePointerName(sessionID)+".json")
}

func (s *FSStore) legacyPendingPath(repoID domain.ContentHash, sessionID string) string {
	return filepath.Join(s.repoDir(repoID), "pending", fsSafeName(sessionID)+".json")
}

// PutPending updates the session's pending context pointer.
func (s *FSStore) PutPending(ctx context.Context, repoID domain.ContentHash, p domain.Pending) error {
	_, err := s.ReplacePending(ctx, repoID, p)
	return err
}

func (s *FSStore) ReplacePending(_ context.Context, repoID domain.ContentHash, p domain.Pending) (domain.ContentHash, error) {
	if err := validateHashes(repoID, p.Target); err != nil {
		return "", err
	}
	if p.RepoID != repoID || p.SessionID == "" || len(p.SessionID) > 128 {
		return "", domain.ErrIntegrity
	}
	lock := s.pendingLock(repoID, p.SessionID)
	lock.Lock()
	defer lock.Unlock()
	previous := domain.ContentHash("")
	if current, found, err := s.pendingForSession(repoID, p.SessionID); err != nil {
		return "", err
	} else if found {
		previous = current.Target
		if current.Dismissed {
			p.Dismissed = true
		}
	}
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	if err := writeAtomic(s.pendingPath(repoID, p.SessionID), data); err != nil {
		return "", err
	}
	_ = os.Remove(s.legacyPendingPath(repoID, p.SessionID))
	return previous, nil
}

// ListPendings returns the entire list of pending pointers in the repo.
func (s *FSStore) ListPendings(_ context.Context, repoID domain.ContentHash) ([]domain.Pending, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.repoDir(repoID), "pending")
	entries, err := os.ReadDir(dir)
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
		data, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		var p domain.Pending
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		if p.SessionID == "" || len(p.SessionID) > 128 {
			return nil, domain.ErrIntegrity
		}
		if err := validateHashes(p.RepoID, p.Target); err != nil || p.RepoID != repoID {
			return nil, domain.ErrIntegrity
		}
		isOpaque := e.Name() == opaquePointerName(p.SessionID)+".json"
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

// DeletePending removes the session's pending pointer (no error if it doesn't exist — idempotent).
func (s *FSStore) DeletePending(_ context.Context, repoID domain.ContentHash, sessionID string) error {
	if err := validateHash(repoID); err != nil {
		return err
	}
	if sessionID == "" || len(sessionID) > 128 {
		return domain.ErrValidation
	}
	lock := s.pendingLock(repoID, sessionID)
	lock.Lock()
	defer lock.Unlock()
	return s.deletePendingFiles(repoID, sessionID)
}

func (s *FSStore) deletePendingFiles(repoID domain.ContentHash, sessionID string) error {
	for _, path := range []string{s.pendingPath(repoID, sessionID), s.legacyPendingPath(repoID, sessionID)} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// CompareAndDeletePending removes only the expected capture. The same
// per-pointer lock is used by PutPending and unconditional deletion, so a
// concurrent replacement cannot be unlinked after this comparison.
func (s *FSStore) CompareAndDeletePending(_ context.Context, repoID domain.ContentHash, sessionID string, expected domain.ContentHash) (domain.PendingDeleteResult, error) {
	if err := validateHashes(repoID, expected); err != nil {
		return domain.PendingDeleteKept, err
	}
	if sessionID == "" || len(sessionID) > 128 {
		return domain.PendingDeleteKept, domain.ErrValidation
	}
	lock := s.pendingLock(repoID, sessionID)
	lock.Lock()
	defer lock.Unlock()
	current, found, err := s.pendingForSession(repoID, sessionID)
	if err != nil {
		return domain.PendingDeleteKept, err
	}
	if !found {
		return domain.PendingDeleteAbsent, nil
	}
	if current.Target != expected {
		return domain.PendingDeleteKept, nil
	}
	if err := s.deletePendingFiles(repoID, sessionID); err != nil {
		return domain.PendingDeleteKept, err
	}
	return domain.PendingDeleteDeleted, nil
}

func (s *FSStore) pendingForSession(repoID domain.ContentHash, sessionID string) (domain.Pending, bool, error) {
	for _, path := range []string{s.pendingPath(repoID, sessionID), s.legacyPendingPath(repoID, sessionID)} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return domain.Pending{}, false, err
		}
		var p domain.Pending
		if err := json.Unmarshal(data, &p); err != nil {
			return domain.Pending{}, false, err
		}
		if p.RepoID != repoID || p.SessionID != sessionID {
			return domain.Pending{}, false, domain.ErrIntegrity
		}
		if err := validateHash(p.Target); err != nil {
			return domain.Pending{}, false, err
		}
		return p, true, nil
	}
	return domain.Pending{}, false, nil
}

func (s *FSStore) SetPendingDismissed(_ context.Context, repoID domain.ContentHash, sessionID string, dismissed bool) (bool, error) {
	if err := validateHash(repoID); err != nil {
		return false, err
	}
	if sessionID == "" || len(sessionID) > 128 {
		return false, domain.ErrValidation
	}
	lock := s.pendingLock(repoID, sessionID)
	lock.Lock()
	defer lock.Unlock()
	p, found, err := s.pendingForSession(repoID, sessionID)
	if err != nil || !found {
		return false, err
	}
	if p.Dismissed == dismissed {
		return true, nil
	}
	p.Dismissed = dismissed
	data, err := json.Marshal(p)
	if err != nil {
		return false, err
	}
	if err := writeAtomic(s.pendingPath(repoID, sessionID), data); err != nil {
		return false, err
	}
	_ = os.Remove(s.legacyPendingPath(repoID, sessionID))
	return true, nil
}

// fsSafeName converts user input to a file-safe string (path traversal defense).
//
// While safe replacement alone can cause different inputs to collapse into the same filename (e.g., "feature/foo", "feature.foo", "feature_foo" all become "feature_foo"), appending a short hash of the original value as a suffix avoids collisions — for example, "user__branch" ensures that different branches can share a file without overwriting each other's pointers (issue: residual pointers from old branches persist after push resolution, contaminating web On Hold/continuing conversations).
// Prefix (readability) + hash (uniqueness).
func fsSafeName(v string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, v)
	sum := sha256.Sum256([]byte(v))
	return safe + "-" + hex.EncodeToString(sum[:])[:8]
}

// unsyncPath is the push pending pointer file path for (user, branch).
func (s *FSStore) unsyncPath(repoID domain.ContentHash, user, branch string) string {
	return filepath.Join(s.repoDir(repoID), "unsync", opaquePointerName(user, branch)+".json")
}

func (s *FSStore) legacyUnsyncPath(repoID domain.ContentHash, user, branch string) string {
	return filepath.Join(s.repoDir(repoID), "unsync", fsSafeName(user)+"__"+fsSafeName(branch)+".json")
}

// PutUnsync upserts a push pending pointer.
func (s *FSStore) PutUnsync(_ context.Context, repoID domain.ContentHash, u domain.Unsync) error {
	if err := validateHashes(repoID, u.Target); err != nil {
		return err
	}
	if err := domain.ValidateBranchName(u.Branch); err != nil {
		return err
	}
	if u.RepoID != repoID || u.User == "" || len(u.User) > 128 {
		return domain.ErrIntegrity
	}
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	if err := writeAtomic(s.unsyncPath(repoID, u.User, u.Branch), data); err != nil {
		return err
	}
	_ = os.Remove(s.legacyUnsyncPath(repoID, u.User, u.Branch))
	return nil
}

// ListUnsyncs returns the entire list of push pending pointers in the repo.
func (s *FSStore) ListUnsyncs(_ context.Context, repoID domain.ContentHash) ([]domain.Unsync, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.repoDir(repoID), "unsync")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]domain.Unsync)
	fromOpaque := make(map[string]bool)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		var u domain.Unsync
		if err := json.Unmarshal(data, &u); err != nil {
			return nil, err
		}
		if err := validateHashes(u.RepoID, u.Target); err != nil || u.RepoID != repoID || u.User == "" || len(u.User) > 128 {
			return nil, domain.ErrIntegrity
		}
		if err := domain.ValidateBranchName(u.Branch); err != nil {
			return nil, err
		}
		key := u.User + "\x00" + u.Branch
		isOpaque := e.Name() == opaquePointerName(u.User, u.Branch)+".json"
		if !fromOpaque[key] || isOpaque {
			byKey[key] = u
			fromOpaque[key] = isOpaque
		}
	}
	out := make([]domain.Unsync, 0, len(byKey))
	for _, u := range byKey {
		out = append(out, u)
	}
	return out, nil
}

// DeleteUnsync removes a push pending pointer (no error if it doesn't exist — idempotent).
// In addition to deleting derivative paths (current + legacy), it also scans directories to delete files with names in the (user, branch) format — pointers recorded before the filename scheme change are not reachable via derivative paths, leaving them as zombie entries in ListUnsyncs that cannot be deleted (bug: residual pointers from old branches persist after push resolution, contaminating web On Hold/continuing conversations).
func (s *FSStore) DeleteUnsync(_ context.Context, repoID domain.ContentHash, user, branch string) error {
	if err := validateHash(repoID); err != nil {
		return err
	}
	if err := domain.ValidateBranchName(branch); err != nil {
		return err
	}
	if user == "" || len(user) > 128 {
		return domain.ErrValidation
	}
	for _, path := range []string{s.unsyncPath(repoID, user, branch), s.legacyUnsyncPath(repoID, user, branch)} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	dir := filepath.Join(s.repoDir(repoID), "unsync")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		var u domain.Unsync
		if json.Unmarshal(data, &u) != nil {
			continue
		}
		if u.User == user && u.Branch == branch {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// UpdateSnapshotMessage is a hook label → commit message promotion exclusive metadata update (service calls this after unidirectional rule validation — here it handles existence and atomic replacement only).
func (s *FSStore) UpdateSnapshotMessage(ctx context.Context, repoID, id domain.ContentHash, message string) error {
	if err := validateHashes(repoID, id); err != nil {
		return err
	}
	mu := s.snapshotLock(repoID, id)
	mu.Lock()
	defer mu.Unlock()
	snap, err := s.GetSnapshot(ctx, repoID, id)
	if err != nil {
		return err
	}
	// Storage layer CAS: finalizes unidirectional rules within a lock — pre-validation in the service layer results in check-then-act for concurrent promotion, leading to last-write-wins.
	if !strings.HasPrefix(snap.Message, domain.HookMessagePrefix) {
		if snap.Message == message {
			return nil // already promoted (concurrent retry) — idempotent
		}
		return fmt.Errorf("%w: snapshot message already promoted", domain.ErrConflict)
	}
	snap.Message = message
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return writeAtomic(s.snapshotPath(repoID, id), data)
}

// DeleteSnapshot removes snapshot metadata (hook capture leaf GC exclusive — idempotent).
func (s *FSStore) DeleteSnapshot(_ context.Context, repoID, id domain.ContentHash) error {
	if err := validateHashes(repoID, id); err != nil {
		return err
	}
	err := os.Remove(s.snapshotPath(repoID, id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// DeleteDoc removes the doc body (hook capture leaf GC exclusive — idempotent).
func (s *FSStore) DeleteDoc(_ context.Context, repoID, hash domain.ContentHash) error {
	if err := validateHashes(repoID, hash); err != nil {
		return err
	}
	err := os.Remove(s.docPath(repoID, hash))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// GetSettingsBundle retrieves the settings bundle. Returns ErrNotFound if not found.
func (s *FSStore) GetSettingsBundle(_ context.Context, repoID domain.ContentHash, kind string) (domain.SettingsBundle, error) {
	if err := validateHash(repoID); err != nil {
		return domain.SettingsBundle{}, err
	}
	if err := validateSettingsKind(kind); err != nil {
		return domain.SettingsBundle{}, err
	}
	data, err := os.ReadFile(filepath.Join(s.repoDir(repoID), "settings", kind+".json"))
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
	if err := domain.ValidateSettingsBundle(kind, "", b); err != nil {
		return domain.SettingsBundle{}, err
	}
	return b, nil
}

// PutSecretsEnvelope stores a secret envelope (opaque bytes — E2E).
func (s *FSStore) PutSecretsEnvelope(_ context.Context, repoID domain.ContentHash, raw []byte) error {
	if err := validateHash(repoID); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.repoDir(repoID), "secrets.enc.json"), raw)
}

// GetSecretsEnvelope retrieves a secret envelope.
func (s *FSStore) GetSecretsEnvelope(_ context.Context, repoID domain.ContentHash) ([]byte, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.repoDir(repoID), "secrets.enc.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	return data, nil
}

// PutSettingsObject stores the commit attachment settings object (content-addressed, idempotent).
func (s *FSStore) PutSettingsObject(_ context.Context, repoID domain.ContentHash, hash domain.ContentHash, bundle domain.SettingsBundle) error {
	if err := validateHashes(repoID, hash); err != nil {
		return err
	}
	if err := domain.ValidateSettingsBundle(bundle.Kind, hash, bundle); err != nil {
		return err
	}
	p := filepath.Join(s.repoDir(repoID), "objects", "settingsobjs", hexOf(hash))
	if exists(p) {
		return nil
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		return err
	}
	return writeAtomic(p, data)
}

// GetSettingsObject retrieves the settings object.
func (s *FSStore) GetSettingsObject(_ context.Context, repoID domain.ContentHash, hash domain.ContentHash) (domain.SettingsBundle, error) {
	if err := validateHashes(repoID, hash); err != nil {
		return domain.SettingsBundle{}, err
	}
	data, err := os.ReadFile(filepath.Join(s.repoDir(repoID), "objects", "settingsobjs", hexOf(hash)))
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

// ListRepos returns all repos (v1: simplified team visibility, no filtering applied).
func (s *FSStore) ListRepos(_ context.Context, _ string) ([]domain.Repo, error) {
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "repos"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []domain.Repo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id, ok := hashFromObjectName(e.Name())
		if !ok {
			continue
		}
		r, err := s.GetRepo(context.Background(), id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// --- Snapshot Metadata ---

func (s *FSStore) snapshotPath(repoID, id domain.ContentHash) string {
	return filepath.Join(s.repoDir(repoID), "snapshots", hexOf(id))
}

func (s *FSStore) GetSnapshot(_ context.Context, repoID, id domain.ContentHash) (domain.Snapshot, error) {
	if err := validateHashes(repoID, id); err != nil {
		return domain.Snapshot{}, err
	}
	data, err := os.ReadFile(s.snapshotPath(repoID, id))
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
	if snap.RepoID != repoID || snap.ID != id {
		return domain.Snapshot{}, domain.ErrIntegrity
	}
	return snap, nil
}

// requireSnapshots ensures that the preexistence of snapshots that graft/ref point to is verified within the repository atomic boundary. Relying solely on the pre-query in the service layer can lead to dangling edges/refs through port calls or stale reads.
func (s *FSStore) requireSnapshots(ctx context.Context, repoID domain.ContentHash, ids ...domain.ContentHash) error {
	seen := make(map[domain.ContentHash]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, err := s.GetSnapshot(ctx, repoID, id); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return fmt.Errorf("%w: snapshot %s", domain.ErrNotFound, id)
			}
			return err
		}
	}
	return nil
}

// PutSnapshot is an idempotent content-addressed storage (re-saving the same ID is a no-op).
// Exception — stash → commit promotion: content-hash deduplication allows the same session content to arrive as a "(stash)" label first. After arriving as a commit (non-stash) label, it promotes branch/message (hash-external derivative metadata — ID/body immutable, CLI same rules).
func (s *FSStore) PutSnapshot(ctx context.Context, snap domain.Snapshot) error {
	// The projection calculated in the API list response is not part of the stored object.
	snap.Branches = nil
	if err := validateSnapshotRefs(snap); err != nil {
		return err
	}
	for _, hash := range []domain.ContentHash{snap.ClaudeSettings, snap.AgentsSettings, snap.CodexSettings} {
		if hash == "" {
			continue
		}
		if _, err := s.GetSettingsObject(ctx, snap.RepoID, hash); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return fmt.Errorf("%w: missing settings object %s", domain.ErrIntegrity, hash)
			}
			return err
		}
	}
	// Existing snapshot's stash→commit promotion is a read-modify-write operation. Without using the same snapshot lock as join/add/message/memory, a late PutSnapshot could overwrite the old graft register with the entire old file.
	mu := s.snapshotLock(snap.RepoID, snap.ID)
	mu.Lock()
	defer mu.Unlock()
	p := s.snapshotPath(snap.RepoID, snap.ID)
	if exists(p) {
		existing, err := s.GetSnapshot(context.Background(), snap.RepoID, snap.ID)
		if err != nil {
			return err
		}
		if snap.Branch == domain.StashBranchLabel {
			return nil
		}
		if existing.Branch != domain.StashBranchLabel {
			return nil
		}
		existing.Branch = snap.Branch
		existing.Message = snap.Message
		b, merr := json.Marshal(existing)
		if merr != nil {
			return merr
		}
		return writeAtomic(p, b)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return writeAtomic(p, data)
}

// AddGraftParents is an internal append-only operation. Actual version and cycle validation and writes are performed within the atomic boundary of addGraftParents' repo.
func (s *FSStore) AddGraftParents(ctx context.Context, repoID, id domain.ContentHash, add []domain.ContentHash) error {
	return s.addGraftParents(ctx, repoID, id, add, nil)
}

// AddGraftParentsCAS is a compare-and-add for delayed client events. If the requested edge already exists and the current seq is expected, the event is acknowledged (+1). If expected+1, it is treated as an idempotent success due to a response loss retry for the same event. Any earlier expected value is a conflict. This range limit prevents the replay of old events superseded by join and ensures that legacy seq=0 edges converge exactly with the client's optimistic seq.
func (s *FSStore) AddGraftParentsCAS(ctx context.Context, repoID, id domain.ContentHash, add []domain.ContentHash, expected uint64) error {
	return s.addGraftParents(ctx, repoID, id, add, &expected)
}

func (s *FSStore) addGraftParents(ctx context.Context, repoID, id domain.ContentHash, add []domain.ContentHash, expected *uint64) error {
	if err := validateHashes(repoID, id); err != nil {
		return err
	}
	if err := validateHashes(add...); err != nil {
		return err
	}
	repoMu := s.refLock(repoID, domain.RefBranch, "")
	repoMu.Lock()
	defer repoMu.Unlock()
	mu := s.snapshotLock(repoID, id)
	mu.Lock()
	defer mu.Unlock()
	snap, err := s.GetSnapshot(ctx, repoID, id)
	if err != nil {
		return err
	}
	if err := s.requireSnapshots(ctx, repoID, add...); err != nil {
		return err
	}
	seen := map[domain.ContentHash]bool{}
	for _, p := range snap.Parents {
		seen[p] = true
	}
	for _, g := range snap.GraftParents {
		seen[g] = true
	}
	changed := false
	for _, a := range add {
		if a == "" || a == id || seen[a] {
			continue
		}
		seen[a] = true
		snap.GraftParents = append(snap.GraftParents, a)
		changed = true
	}
	if !changed {
		if expected == nil {
			return nil // Duplicate calls to internal append do not advance the register.
		}
		switch {
		case snap.GraftSeq == *expected:
			if snap.GraftSeq == domain.MaxGraftSeq {
				return fmt.Errorf("%w: graft sequence exhausted", domain.ErrConflict)
			}
			snap.GraftSeq++ // Even legacy edges are precisely acknowledged by this CAS event.
			data, err := json.Marshal(snap)
			if err != nil {
				return err
			}
			return writeAtomic(s.snapshotPath(repoID, id), data)
		case *expected < domain.MaxGraftSeq && snap.GraftSeq == *expected+1:
			return nil // Retry response loss or queue.
		default:
			return domain.ErrConflict
		}
	}
	if expected != nil && snap.GraftSeq != *expected {
		return domain.ErrConflict
	}
	if snap.GraftSeq == domain.MaxGraftSeq {
		return fmt.Errorf("%w: graft sequence exhausted", domain.ErrConflict)
	}
	snap.Grafted = true
	snap.GraftSeq++
	all, err := s.ListSnapshots(ctx, repoID, "")
	if err != nil {
		return err
	}
	for i := range all {
		if all[i].ID == id {
			all[i] = snap
			break
		}
	}
	if snapshotGraphHasCycle(all) {
		return fmt.Errorf("%w: graft would create a reachability cycle", domain.ErrConflict)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return writeAtomic(s.snapshotPath(repoID, id), data)
}

// snapshotGraphHasCycle checks the final graph combining natural parents and graft overlays. It is called before mutation storage to enforce the DAG invariant independently of service layer stale reads.
func snapshotGraphHasCycle(snaps []domain.Snapshot) bool {
	byID := make(map[domain.ContentHash]domain.Snapshot, len(snaps))
	for _, snap := range snaps {
		byID[snap.ID] = snap
	}
	state := make(map[domain.ContentHash]uint8, len(snaps)) // 1=visiting, 2=done
	var visit func(domain.ContentHash) bool
	visit = func(id domain.ContentHash) bool {
		switch state[id] {
		case 1:
			return true
		case 2:
			return false
		}
		snap, ok := byID[id]
		if !ok {
			return false
		}
		state[id] = 1
		for _, parent := range snap.ReachabilityParents() {
			if visit(parent) {
				return true
			}
		}
		state[id] = 2
		return false
	}
	for id := range byID {
		if visit(id) {
			return true
		}
	}
	return false
}

// SetGraftParents replaces the entire graft register (LWW — join supersede exclusive). Unlike AddGraftParents, it can express edge removal. seq advances to current+1.
func (s *FSStore) SetGraftParents(ctx context.Context, repoID, id domain.ContentHash, parents []domain.ContentHash) error {
	if err := validateHashes(repoID, id); err != nil {
		return err
	}
	if err := validateHashes(parents...); err != nil {
		return err
	}
	repoMu := s.refLock(repoID, domain.RefBranch, "")
	repoMu.Lock()
	defer repoMu.Unlock()
	mu := s.snapshotLock(repoID, id)
	mu.Lock()
	defer mu.Unlock()
	snap, err := s.GetSnapshot(ctx, repoID, id)
	if err != nil {
		return err
	}
	if err := s.requireSnapshots(ctx, repoID, parents...); err != nil {
		return err
	}
	seen := map[domain.ContentHash]bool{id: true}
	out := make([]domain.ContentHash, 0, len(parents))
	for _, p := range snap.Parents {
		seen[p] = true // Duplicate removal of natural parents (reachability same)
	}
	for _, p := range parents {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	snap.GraftParents = out
	snap.Grafted = len(out) > 0
	if snap.GraftSeq == domain.MaxGraftSeq {
		return fmt.Errorf("%w: graft sequence exhausted", domain.ErrConflict)
	}
	snap.GraftSeq++
	all, err := s.ListSnapshots(ctx, repoID, "")
	if err != nil {
		return err
	}
	for i := range all {
		if all[i].ID == id {
			all[i] = snap
			break
		}
	}
	if snapshotGraphHasCycle(all) {
		return fmt.Errorf("%w: graft replacement would create a reachability cycle", domain.ErrConflict)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return writeAtomic(s.snapshotPath(repoID, id), data)
}

// ApplyJoin applies the validated prepared/committed durable journal under repo/ref lock and snapshot lock. A prepared crash rolls back, while a committed crash rolls forward.
func (s *FSStore) ApplyJoin(ctx context.Context, m outbound.JoinMutation) error {
	if err := validateHashes(m.RepoID, m.Source, m.ExpectedHead, m.NewHead); err != nil {
		return err
	}
	if len(m.Segment) == 0 || m.Segment[0] != m.Source {
		return fmt.Errorf("%w: join segment must start at source", domain.ErrValidation)
	}
	segmentSeen := map[domain.ContentHash]bool{}
	for _, id := range m.Segment {
		if err := validateHash(id); err != nil {
			return err
		}
		if segmentSeen[id] {
			return fmt.Errorf("%w: duplicate join segment snapshot", domain.ErrIntegrity)
		}
		segmentSeen[id] = true
	}
	if !segmentSeen[m.NewHead] {
		return fmt.Errorf("%w: join head is outside segment", domain.ErrValidation)
	}
	if err := domain.ValidateBranchName(m.Branch); err != nil {
		return err
	}
	if (m.ForkName == "") != (m.ForkTip == "") {
		return fmt.Errorf("%w: join fork name and tip must be provided together", domain.ErrValidation)
	}
	if m.ForkName != "" {
		if err := domain.ValidateBranchName(m.ForkName); err != nil {
			return err
		}
		if err := validateHash(m.ForkTip); err != nil {
			return err
		}
		if !segmentSeen[m.ForkTip] {
			return fmt.Errorf("%w: join session tip is outside segment", domain.ErrValidation)
		}
	}
	if len(m.Grafts) == 0 {
		return fmt.Errorf("%w: join requires at least one graft patch", domain.ErrValidation)
	}
	ids := make([]domain.ContentHash, 0, len(m.Grafts))
	required := []domain.ContentHash{m.Source, m.ExpectedHead, m.NewHead, m.ForkTip}
	required = append(required, m.Segment...)
	seenID := map[domain.ContentHash]bool{}
	for _, p := range m.Grafts {
		if err := validateHash(p.SnapshotID); err != nil {
			return err
		}
		if err := validateHashes(p.Parents...); err != nil {
			return err
		}
		if seenID[p.SnapshotID] {
			return domain.ErrIntegrity
		}
		seenID[p.SnapshotID] = true
		ids = append(ids, p.SnapshotID)
		required = append(required, p.SnapshotID)
		required = append(required, p.Parents...)
	}
	if err := validateJoinMutationPlan(m); err != nil {
		return err
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	refMu := s.refLock(m.RepoID, domain.RefBranch, m.Branch)
	refMu.Lock()
	defer refMu.Unlock()
	journalPath := s.joinJournalPath(m.RepoID)
	if _, err := os.Stat(journalPath); err == nil {
		// The committed/prepared journal from the previous operation is the source of truth for startup recovery. Overwriting it prevents recovery of ref·graft·reflog mid-states, so it fails closed.
		return fmt.Errorf("%w: unfinished join journal requires store recovery", domain.ErrConflict)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	locks := make([]*sync.Mutex, 0, len(ids))
	for _, id := range ids {
		mu := s.snapshotLock(m.RepoID, id)
		mu.Lock()
		locks = append(locks, mu)
	}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}()
	cur, err := s.GetRef(ctx, m.RepoID, domain.RefBranch, m.Branch)
	if err != nil || cur.Target != m.ExpectedHead {
		return domain.ErrRefConflict
	}
	if err := s.requireSnapshots(ctx, m.RepoID, required...); err != nil {
		return err
	}
	if err := s.ensureJoinGraphScope(ctx, m); err != nil {
		return err
	}
	journal := fsJoinJournal{
		Version: fsJoinJournalVersion, Phase: fsJoinPrepared, RepoID: m.RepoID,
		Branch: m.Branch, ExpectedHead: m.ExpectedHead, NewHead: m.NewHead,
		ForkName: m.ForkName, ForkTip: m.ForkTip, CreatedAt: time.Now().UTC(),
	}
	for _, patch := range m.Grafts {
		snap, err := s.GetSnapshot(ctx, m.RepoID, patch.SnapshotID)
		if err != nil {
			return err
		}
		if snap.GraftSeq != patch.ExpectedSeq {
			return domain.ErrConflict
		}
		if snap.GraftSeq == domain.MaxGraftSeq {
			return fmt.Errorf("%w: graft sequence exhausted", domain.ErrConflict)
		}
		before, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		snap.GraftParents = dedupHashParents(snap.ID, snap.Parents, patch.Parents)
		snap.Grafted = len(snap.GraftParents) > 0
		snap.GraftSeq++
		after, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		journal.Snapshots = append(journal.Snapshots, fsJoinSnapshot{ID: patch.SnapshotID, Before: before, After: after})
	}
	all, err := s.ListSnapshots(ctx, m.RepoID, "")
	if err != nil {
		return err
	}
	if err := validateJoinTransitionOrder(all, journal.Snapshots, m.RepoID); err != nil {
		return err
	}
	if m.ForkName != "" {
		if existing, err := s.GetRef(ctx, m.RepoID, domain.RefSession, m.ForkName); err == nil {
			if existing.Target != m.ForkTip {
				return domain.ErrRefConflict
			}
			journal.ForkExisted = true
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
	}
	if err := s.writeJoinJournal(journalPath, journal); err != nil {
		return err
	}
	if err := s.applyJoinProjection(journal, true); err != nil {
		return s.failPreparedJoin(journalPath, journal, err)
	}
	journal.Phase = fsJoinCommitted
	if err := s.writeJoinJournal(journalPath, journal); err != nil {
		return s.failPreparedJoin(journalPath, journal, err)
	}
	// After the committed marker, no rollbacks are performed. On reflog/remove failure, the journal is left to allow startup recovery to complete the same after-state and audit log.
	return s.finishCommittedJoin(journalPath, journal)
}

func (s *FSStore) ensureJoinGraphScope(ctx context.Context, m outbound.JoinMutation) error {
	if len(m.Grafts) == 0 {
		return nil
	}
	snaps, err := s.ListSnapshots(ctx, m.RepoID, "")
	if err != nil {
		return err
	}
	byID := make(map[domain.ContentHash]domain.Snapshot, len(snaps))
	for _, snap := range snaps {
		byID[snap.ID] = snap
	}
	refs, err := s.ListRefs(ctx, m.RepoID)
	if err != nil {
		return err
	}
	attached := make(map[domain.ContentHash]bool, len(snaps))
	sessionPrefix := domain.SessionRefPrefix(m.Branch)
	blocked := append([]domain.ContentHash{}, m.Segment...)
	for _, patch := range m.Grafts {
		blocked = append(blocked, patch.SnapshotID)
	}
	for _, ref := range refs {
		if ref.Target != "" && ((ref.Kind == domain.RefBranch && ref.Name == m.Branch) ||
			(ref.Kind == domain.RefSession && strings.HasPrefix(ref.Name, sessionPrefix))) {
			reach := snapshotReachableFrom(byID, ref.Target)
			for id := range reach {
				attached[id] = true
			}
		}
		otherScope := (ref.Kind == domain.RefBranch && ref.Name != m.Branch) ||
			(ref.Kind == domain.RefSession && !strings.HasPrefix(ref.Name, sessionPrefix))
		if !otherScope || ref.Target == "" {
			continue
		}
		reach := snapshotReachableFrom(byID, ref.Target)
		for _, id := range blocked {
			if reach[id] {
				return fmt.Errorf("%w: snapshot %s is reachable from branch %q", domain.ErrConflict, id, ref.Name)
			}
		}
	}
	return validateJoinSegmentTopology(m, byID, attached)
}

func snapshotReachableFrom(byID map[domain.ContentHash]domain.Snapshot, head domain.ContentHash) map[domain.ContentHash]bool {
	out := map[domain.ContentHash]bool{}
	stack := []domain.ContentHash{head}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == "" || out[id] {
			continue
		}
		out[id] = true
		if snap, ok := byID[id]; ok {
			stack = append(stack, snap.ReachabilityParents()...)
		}
	}
	return out
}

type fsJoinSnapshot struct {
	ID     domain.ContentHash `json:"id"`
	Before json.RawMessage    `json:"before"`
	After  json.RawMessage    `json:"after"`
}

type fsJoinPhase string

const (
	fsJoinJournalVersion = 1
	fsJoinPrepared       = fsJoinPhase("prepared")
	fsJoinCommitted      = fsJoinPhase("committed")
)

type fsJoinJournal struct {
	Version      int                `json:"version"`
	Phase        fsJoinPhase        `json:"phase"`
	RepoID       domain.ContentHash `json:"repo_id"`
	Branch       string             `json:"branch"`
	ExpectedHead domain.ContentHash `json:"expected_head"`
	NewHead      domain.ContentHash `json:"new_head"`
	ForkName     string             `json:"fork_name,omitempty"`
	ForkTip      domain.ContentHash `json:"fork_tip,omitempty"`
	ForkExisted  bool               `json:"fork_existed,omitempty"`
	CreatedAt    time.Time          `json:"created_at"`
	Snapshots    []fsJoinSnapshot   `json:"snapshots"`
}

func (s *FSStore) joinJournalPath(repoID domain.ContentHash) string {
	return filepath.Join(s.repoDir(repoID), ".join-txn.json")
}

func (s *FSStore) writeJoinJournal(path string, journal fsJoinJournal) error {
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

func validateJoinSnapshot(raw json.RawMessage, id, repoID domain.ContentHash) (domain.Snapshot, error) {
	var snap domain.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return domain.Snapshot{}, err
	}
	if snap.ID != id || snap.RepoID != repoID {
		return domain.Snapshot{}, domain.ErrIntegrity
	}
	if err := validateSnapshotRefs(snap); err != nil {
		return domain.Snapshot{}, err
	}
	return snap, nil
}

func validateFSJoinJournal(j fsJoinJournal, repoDirName string) error {
	if j.Version != fsJoinJournalVersion || (j.Phase != fsJoinPrepared && j.Phase != fsJoinCommitted) {
		return fmt.Errorf("%w: unsupported join journal version/phase", domain.ErrIntegrity)
	}
	if validateHashes(j.RepoID, j.ExpectedHead, j.NewHead) != nil || hexOf(j.RepoID) != repoDirName || domain.ValidateBranchName(j.Branch) != nil || j.CreatedAt.IsZero() {
		return fmt.Errorf("%w: invalid join journal header", domain.ErrIntegrity)
	}
	if (j.ForkName == "") != (j.ForkTip == "") || (j.ForkExisted && j.ForkName == "") {
		return fmt.Errorf("%w: incomplete join fork journal", domain.ErrIntegrity)
	}
	if j.ForkName != "" && (domain.ValidateBranchName(j.ForkName) != nil || validateHash(j.ForkTip) != nil) {
		return fmt.Errorf("%w: invalid join fork journal", domain.ErrIntegrity)
	}
	seen := map[domain.ContentHash]bool{}
	if len(j.Snapshots) == 0 {
		return fmt.Errorf("%w: empty join journal", domain.ErrIntegrity)
	}
	for _, entry := range j.Snapshots {
		if validateHash(entry.ID) != nil || seen[entry.ID] {
			return fmt.Errorf("%w: invalid/duplicate join snapshot", domain.ErrIntegrity)
		}
		seen[entry.ID] = true
		before, err := validateJoinSnapshot(entry.Before, entry.ID, j.RepoID)
		if err != nil {
			return fmt.Errorf("join journal before %s: %w", entry.ID, err)
		}
		after, err := validateJoinSnapshot(entry.After, entry.ID, j.RepoID)
		if err != nil || before.GraftSeq == domain.MaxGraftSeq || after.GraftSeq != before.GraftSeq+1 {
			return fmt.Errorf("%w: invalid join snapshot transition %s", domain.ErrIntegrity, entry.ID)
		}
		// A join journal may modify only the graft register. ID does not protect out-of-hash fields, so without
		// this comparison a corrupt journal could overwrite unrelated metadata such as branch or message while
		// appearing to perform a valid recovery.
		expected := before
		expected.GraftParents = after.GraftParents
		expected.Grafted = after.Grafted
		expected.GraftSeq = after.GraftSeq
		if !reflect.DeepEqual(expected, after) || !reflect.DeepEqual(after.GraftParents, dedupHashParents(after.ID, after.Parents, after.GraftParents)) {
			return fmt.Errorf("%w: join journal changes non-graft metadata %s", domain.ErrIntegrity, entry.ID)
		}
	}
	return nil
}

// validateJoinTransitionOrder checks if all prefixes of forward and rollback orders, applied by file rename, form a DAG. Even if the final before/after is normal, writing add before removal can cause a temporary cycle on disk. Therefore, it validates the patch order service has defined across the repository boundary.
func validateJoinTransitionOrder(all []domain.Snapshot, entries []fsJoinSnapshot, repoID domain.ContentHash) error {
	candidate := append([]domain.Snapshot(nil), all...)
	position := make(map[domain.ContentHash]int, len(candidate))
	before := make(map[domain.ContentHash]domain.Snapshot, len(entries))
	after := make(map[domain.ContentHash]domain.Snapshot, len(entries))
	for i, snap := range candidate {
		position[snap.ID] = i
	}
	for _, entry := range entries {
		pos, ok := position[entry.ID]
		if !ok {
			return fmt.Errorf("%w: join transition snapshot missing %s", domain.ErrIntegrity, entry.ID)
		}
		var err error
		before[entry.ID], err = validateJoinSnapshot(entry.Before, entry.ID, repoID)
		if err != nil {
			return err
		}
		after[entry.ID], err = validateJoinSnapshot(entry.After, entry.ID, repoID)
		if err != nil {
			return err
		}
		candidate[pos] = before[entry.ID]
	}
	if snapshotGraphHasCycle(candidate) {
		return fmt.Errorf("%w: join before-state contains a reachability cycle", domain.ErrConflict)
	}
	for _, entry := range entries {
		candidate[position[entry.ID]] = after[entry.ID]
		if snapshotGraphHasCycle(candidate) {
			return fmt.Errorf("%w: join patch order creates an intermediate reachability cycle at %s", domain.ErrConflict, entry.ID)
		}
	}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		candidate[position[entry.ID]] = before[entry.ID]
		if snapshotGraphHasCycle(candidate) {
			return fmt.Errorf("%w: join rollback order creates an intermediate reachability cycle at %s", domain.ErrConflict, entry.ID)
		}
	}
	return nil
}

func (s *FSStore) validateJoinJournalGraphs(ctx context.Context, j fsJoinJournal) error {
	all, err := s.ListSnapshots(ctx, j.RepoID, "")
	if err != nil {
		return err
	}
	byID := make(map[domain.ContentHash]bool, len(all))
	for _, snap := range all {
		byID[snap.ID] = true
	}
	for _, id := range []domain.ContentHash{j.ExpectedHead, j.NewHead, j.ForkTip} {
		if id != "" && !byID[id] {
			return fmt.Errorf("%w: join journal ref target missing %s", domain.ErrIntegrity, id)
		}
	}
	for _, entry := range j.Snapshots {
		if !byID[entry.ID] {
			return fmt.Errorf("%w: join journal snapshot missing %s", domain.ErrIntegrity, entry.ID)
		}
		after, _ := validateJoinSnapshot(entry.After, entry.ID, j.RepoID)
		for _, parent := range after.GraftParents {
			if !byID[parent] {
				return fmt.Errorf("%w: join journal graft parent missing %s", domain.ErrIntegrity, parent)
			}
		}
	}
	if err := validateJoinTransitionOrder(all, j.Snapshots, j.RepoID); err != nil {
		return fmt.Errorf("%w: join journal transition order: %v", domain.ErrIntegrity, err)
	}
	return nil
}

// validateJoinJournalProjection checks if the disk state at the crash point matches one of the before/after states recorded in the journal. Due to atomic rename, the normal intermediate state must be one of the two. Any other value indicates separate corruption or external change, so recovery is not overwritten and it fails closed.
func (s *FSStore) validateJoinJournalProjection(ctx context.Context, j fsJoinJournal) error {
	branch, err := s.GetRef(ctx, j.RepoID, domain.RefBranch, j.Branch)
	if err != nil || (branch.Target != j.ExpectedHead && branch.Target != j.NewHead) {
		return fmt.Errorf("%w: join journal branch projection disagrees", domain.ErrIntegrity)
	}
	for _, entry := range j.Snapshots {
		current, err := s.GetSnapshot(ctx, j.RepoID, entry.ID)
		if err != nil {
			return fmt.Errorf("%w: join journal snapshot projection missing %s", domain.ErrIntegrity, entry.ID)
		}
		before, _ := validateJoinSnapshot(entry.Before, entry.ID, j.RepoID)
		after, _ := validateJoinSnapshot(entry.After, entry.ID, j.RepoID)
		if !reflect.DeepEqual(current, before) && !reflect.DeepEqual(current, after) {
			return fmt.Errorf("%w: join journal snapshot projection disagrees %s", domain.ErrIntegrity, entry.ID)
		}
	}
	if j.ForkName == "" {
		return nil
	}
	fork, err := s.GetRef(ctx, j.RepoID, domain.RefSession, j.ForkName)
	if errors.Is(err, domain.ErrNotFound) && !j.ForkExisted {
		return nil
	}
	if err != nil || fork.Target != j.ForkTip {
		return fmt.Errorf("%w: join journal session projection disagrees", domain.ErrIntegrity)
	}
	return nil
}

// applyJoinProjection accepts only fully validated journals. Forwards are applied in the order service has defined (supersede→new edge), and rollbacks in reverse order to avoid cycles in any intermediate state.
func (s *FSStore) applyJoinProjection(j fsJoinJournal, forward bool) error {
	if forward {
		for _, entry := range j.Snapshots {
			if err := writeAtomic(s.snapshotPath(j.RepoID, entry.ID), entry.After); err != nil {
				return err
			}
		}
		if j.ForkName != "" {
			if err := writeAtomic(s.refFile(j.RepoID, domain.RefSession, j.ForkName), []byte(string(j.ForkTip)+"\n")); err != nil {
				return err
			}
		}
		return writeAtomic(s.refFile(j.RepoID, domain.RefBranch, j.Branch), []byte(string(j.NewHead)+"\n"))
	}
	for i := len(j.Snapshots) - 1; i >= 0; i-- {
		entry := j.Snapshots[i]
		if err := writeAtomic(s.snapshotPath(j.RepoID, entry.ID), entry.Before); err != nil {
			return err
		}
	}
	if j.ForkName != "" {
		path := s.refFile(j.RepoID, domain.RefSession, j.ForkName)
		if j.ForkExisted {
			if err := writeAtomic(path, []byte(string(j.ForkTip)+"\n")); err != nil {
				return err
			}
		} else if err := removeFileDurable(path); err != nil {
			return err
		}
	}
	return writeAtomic(s.refFile(j.RepoID, domain.RefBranch, j.Branch), []byte(string(j.ExpectedHead)+"\n"))
}

func (s *FSStore) failPreparedJoin(path string, j fsJoinJournal, cause error) error {
	j.Phase = fsJoinPrepared
	if err := s.applyJoinProjection(j, false); err != nil {
		return fmt.Errorf("join apply failed (%v); rollback failed (journal retained): %w", cause, err)
	}
	if err := removeFileDurable(path); err != nil {
		return fmt.Errorf("join apply failed (%v); rollback journal cleanup failed: %w", cause, err)
	}
	return cause
}

func (s *FSStore) ensureJoinReflog(repoID domain.ContentHash, entry domain.RefLogEntry) error {
	logs, err := s.ReadReflog(context.Background(), repoID)
	if err != nil {
		return err
	}
	for _, existing := range logs {
		if existing.Kind == entry.Kind && existing.Name == entry.Name && existing.Old == entry.Old && existing.New == entry.New && existing.CreatedAt.Equal(entry.CreatedAt) {
			return nil
		}
	}
	return s.appendReflogStrict(repoID, entry)
}

func (s *FSStore) finishCommittedJoin(path string, j fsJoinJournal) error {
	if err := s.applyJoinProjection(j, true); err != nil {
		return err
	}
	if j.ForkName != "" && !j.ForkExisted {
		if err := s.ensureJoinReflog(j.RepoID, domain.RefLogEntry{Kind: domain.RefSession, Name: j.ForkName, New: j.ForkTip, CreatedAt: j.CreatedAt}); err != nil {
			return err
		}
	}
	if err := s.ensureJoinReflog(j.RepoID, domain.RefLogEntry{Kind: domain.RefBranch, Name: j.Branch, Old: j.ExpectedHead, New: j.NewHead, CreatedAt: j.CreatedAt}); err != nil {
		return err
	}
	return removeFileDurable(path)
}

// recoverJoinJournals first fully validates all journals before writing them. Prepared journals roll back, while committed journals roll forward. If any recovery fails, the checked constructor rejects server startup, and the journal remains for the next retry.
func (s *FSStore) recoverJoinJournals() error {
	reposDir := filepath.Join(s.dataDir, "repos")
	entries, err := os.ReadDir(reposDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	type pendingJournal struct {
		path string
		j    fsJoinJournal
	}
	var pending []pendingJournal
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(reposDir, entry.Name(), ".join-txn.json")
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		var j fsJoinJournal
		if err := json.Unmarshal(data, &j); err != nil {
			return fmt.Errorf("decode join journal %s: %w", path, err)
		}
		if err := validateFSJoinJournal(j, entry.Name()); err != nil {
			return fmt.Errorf("validate join journal %s: %w", path, err)
		}
		if err := s.validateJoinJournalGraphs(context.Background(), j); err != nil {
			return fmt.Errorf("validate join journal graph %s: %w", path, err)
		}
		if err := s.validateJoinJournalProjection(context.Background(), j); err != nil {
			return fmt.Errorf("validate join journal projection %s: %w", path, err)
		}
		pending = append(pending, pendingJournal{path: path, j: j})
	}
	for _, item := range pending {
		mu := s.refLock(item.j.RepoID, domain.RefBranch, item.j.Branch)
		mu.Lock()
		var err error
		if item.j.Phase == fsJoinPrepared {
			err = s.applyJoinProjection(item.j, false)
			if err == nil {
				err = removeFileDurable(item.path)
			}
		} else {
			err = s.finishCommittedJoin(item.path, item.j)
		}
		mu.Unlock()
		if err != nil {
			return fmt.Errorf("recover join journal %s: %w", item.path, err)
		}
	}
	return nil
}

func dedupHashParents(id domain.ContentHash, natural, graft []domain.ContentHash) []domain.ContentHash {
	seen := map[domain.ContentHash]bool{id: true}
	for _, p := range natural {
		seen[p] = true
	}
	out := make([]domain.ContentHash, 0, len(graft))
	for _, p := range graft {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// CompareAndSwapSnapshotMemory advances only the derived memory pointer. The
// same per-snapshot lock used by graft/message metadata makes the comparison
// and write one atomic boundary.
func (s *FSStore) CompareAndSwapSnapshotMemory(ctx context.Context, repoID, id, expected, next domain.ContentHash) error {
	if err := validateHashes(repoID, id, next); err != nil {
		return err
	}
	if err := domain.ValidateOptionalContentHash(expected); err != nil {
		return err
	}
	if _, err := s.GetMemory(ctx, repoID, next); err != nil {
		return err
	}
	mu := s.snapshotLock(repoID, id)
	mu.Lock()
	defer mu.Unlock()
	snap, err := s.GetSnapshot(ctx, repoID, id)
	if err != nil {
		return err
	}
	if snap.MemoryHash == next {
		return nil
	}
	if snap.MemoryHash != expected {
		return domain.ErrConflict
	}
	snap.MemoryHash = next
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return writeAtomic(s.snapshotPath(repoID, id), data)
}

func (s *FSStore) ListSnapshots(_ context.Context, repoID domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.repoDir(repoID), "snapshots")
	entries, err := os.ReadDir(dir)
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
		snap, err := s.GetSnapshot(context.Background(), repoID, id)
		if err != nil {
			return nil, err
		}
		if branch == "" || snap.Branch == branch {
			out = append(out, snap)
		}
	}
	// git log meaning: Latest commit first (ReadDir is file name=hash order, unrelated to time).
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// HasSnapshots returns the snapshots the server has for the given IDs (negotiation).
func regularObjectExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%w: object path is not a regular file", domain.ErrIntegrity)
	}
	return true, nil
}

func (s *FSStore) HasSnapshots(ctx context.Context, repoID domain.ContentHash, ids []domain.ContentHash) ([]domain.ContentHash, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	var have []domain.ContentHash
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := validateHash(id); err != nil {
			return nil, err
		}
		exists, err := regularObjectExists(s.snapshotPath(repoID, id))
		if err != nil {
			return nil, err
		}
		if exists {
			have = append(have, id)
		}
	}
	return have, nil
}

// --- Ref / manifest ---

func (s *FSStore) refFile(repoID domain.ContentHash, kind domain.RefKind, name string) string {
	switch kind {
	case domain.RefHead:
		return filepath.Join(s.repoDir(repoID), "HEAD")
	case domain.RefSession:
		return filepath.Join(s.repoDir(repoID), "refs", "sessions", filepath.FromSlash(name))
	case domain.RefTag:
		return filepath.Join(s.repoDir(repoID), "refs", "tags", filepath.FromSlash(name))
	default:
		return filepath.Join(s.repoDir(repoID), "refs", "heads", filepath.FromSlash(name))
	}
}

func (s *FSStore) GetRef(_ context.Context, repoID domain.ContentHash, kind domain.RefKind, name string) (domain.Ref, error) {
	if err := validateHash(repoID); err != nil {
		return domain.Ref{}, err
	}
	if err := domain.ValidateRefName(kind, name); err != nil {
		return domain.Ref{}, err
	}
	data, err := os.ReadFile(s.refFile(repoID, kind, name))
	if err != nil {
		if os.IsNotExist(err) {
			return domain.Ref{}, domain.ErrNotFound
		}
		return domain.Ref{}, err
	}
	content := strings.TrimSpace(string(data))
	ref := domain.Ref{Kind: kind, Name: name, RepoID: repoID}
	if kind == domain.RefHead {
		if sym, ok := strings.CutPrefix(content, "ref: refs/heads/"); ok {
			ref.Symbolic = sym
			if err := domain.ValidateRef(ref); err != nil {
				return domain.Ref{}, err
			}
			return ref, nil
		}
	}
	ref.Target = domain.ContentHash(content)
	if err := domain.ValidateRef(ref); err != nil {
		return domain.Ref{}, err
	}
	return ref, nil
}

func (s *FSStore) ListRefs(ctx context.Context, repoID domain.ContentHash) ([]domain.Ref, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	var out []domain.Ref
	if head, err := s.GetRef(ctx, repoID, domain.RefHead, domain.HeadRefName); err == nil {
		out = append(out, head)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	for _, kc := range []struct {
		sub  string
		kind domain.RefKind
	}{{"heads", domain.RefBranch}, {"sessions", domain.RefSession}, {"tags", domain.RefTag}} {
		root := filepath.Join(s.repoDir(repoID), "refs", kc.sub)
		err := filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
			if werr != nil {
				if os.IsNotExist(werr) {
					return nil
				}
				return werr
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

var fsRefLocks sync.Map

func (s *FSStore) refLock(repoID domain.ContentHash, _ domain.RefKind, _ string) *sync.Mutex {
	key := s.dataDir + "\x00" + string(repoID)
	lock, _ := fsRefLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// fsSnapshotLocks is a mutex for (dataDir, repoID, snapshotID) units. All paths that modify snapshot files (UpdateSnapshotMessage/AddGraftParents/CompareAndSwapSnapshotMemory) must share it — locking one path leaves a lost-update race with other meta updates (review P1). Separate from refLock because refLock is specific to ref movement, and reusing it for snapshot meta updates blurs the lock's meaning.
var fsSnapshotLocks sync.Map

func (s *FSStore) snapshotLock(repoID, id domain.ContentHash) *sync.Mutex {
	key := s.dataDir + "\x00" + string(repoID) + "\x00" + string(id)
	lock, _ := fsSnapshotLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

var fsPendingLocks sync.Map

func (s *FSStore) pendingLock(repoID domain.ContentHash, sessionID string) *sync.Mutex {
	key := s.dataDir + "\x00" + string(repoID) + "\x00" + sessionID
	lock, _ := fsPendingLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// CompareAndSwapRef moves to next only if expected matches current target (optimistic locking). expected=="" means "must not exist" (new creation). HEAD (symbolic) is always recorded. Lock is shared among FSStore instances in the same dataDir within the same process. Multiple cxtd processes writing to the same dataDir concurrently are not supported and use the PostgreSQL adapter.
func (s *FSStore) CompareAndSwapRef(ctx context.Context, repoID domain.ContentHash, next domain.Ref, expected domain.ContentHash) error {
	next.RepoID = repoID
	if err := domain.ValidateRef(next); err != nil {
		return err
	}
	if err := domain.ValidateOptionalContentHash(expected); err != nil {
		return err
	}
	lock := s.refLock(repoID, next.Kind, next.Name)
	lock.Lock()
	defer lock.Unlock()
	if next.Kind == domain.RefHead {
		content := string(next.Target)
		if next.Symbolic != "" {
			content = "ref: refs/heads/" + strings.TrimPrefix(next.Symbolic, "refs/heads/")
		}
		return writeAtomic(s.refFile(repoID, domain.RefHead, domain.HeadRefName), []byte(content+"\n"))
	}
	cur, err := s.GetRef(ctx, repoID, next.Kind, next.Name)
	curTarget := domain.ContentHash("")
	if err == nil {
		curTarget = cur.Target
	}
	if curTarget != expected {
		return domain.ErrRefConflict
	}
	if err := writeAtomic(s.refFile(repoID, next.Kind, next.Name), []byte(string(next.Target)+"\n")); err != nil {
		return err
	}
	// reflog: appends-only log on ref movement success (safety net for recovered tips). Best-effort.
	s.appendReflog(repoID, domain.RefLogEntry{Kind: next.Kind, Name: next.Name, Old: curTarget, New: next.Target, CreatedAt: time.Now().UTC()})
	return nil
}

func (s *FSStore) reflogPath(repoID domain.ContentHash) string {
	return filepath.Join(s.repoDir(repoID), "reflog.jsonl")
}

func (s *FSStore) appendReflogStrict(repoID domain.ContentHash, e domain.RefLogEntry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.reflogPath(repoID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// reflog file is created for the first time in this append, fsync alone is not durable. Synchronize parent directory before deleting the join journal.
	d, err := os.Open(filepath.Dir(s.reflogPath(repoID)))
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// appendReflog is a best-effort audit log for general ref movements. Join includes reflog in completion conditions, so use appendReflogStrict/ensureJoinReflog directly.
func (s *FSStore) appendReflog(repoID domain.ContentHash, e domain.RefLogEntry) {
	_ = s.appendReflogStrict(repoID, e)
}

// ReadReflog returns the latest reflog entries of the repo (empty slice if none).
func (s *FSStore) ReadReflog(_ context.Context, repoID domain.ContentHash) ([]domain.RefLogEntry, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.reflogPath(repoID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []domain.RefLogEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e domain.RefLogEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			out = append(out, e)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 { // latest first
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (s *FSStore) GetManifest(ctx context.Context, repoID domain.ContentHash) (domain.Manifest, error) {
	if err := validateHash(repoID); err != nil {
		return domain.Manifest{}, err
	}
	refs, err := s.ListRefs(ctx, repoID)
	if err != nil {
		return domain.Manifest{}, err
	}
	var index []domain.ContentHash
	memoryAttachments := map[domain.ContentHash]domain.ContentHash{}
	snapshotStates := map[domain.ContentHash]domain.ContentHash{}
	dir := filepath.Join(s.repoDir(repoID), "snapshots")
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			id, ok := hashFromObjectName(e.Name())
			if !ok {
				continue
			}
			snap, err := s.GetSnapshot(ctx, repoID, id)
			if err != nil {
				return domain.Manifest{}, err
			}
			index = append(index, id)
			if snap.MemoryHash != "" {
				memoryAttachments[id] = snap.MemoryHash
			}
			state, err := domain.SnapshotStateHash(snap)
			if err != nil {
				return domain.Manifest{}, err
			}
			snapshotStates[id] = state
		}
	} else if !os.IsNotExist(err) {
		return domain.Manifest{}, err
	}
	return domain.Manifest{
		RepoID:            repoID,
		Refs:              refs,
		SnapshotIndex:     index,
		MemoryAttachments: memoryAttachments,
		SnapshotStates:    snapshotStates,
		Version:           len(index),
		UpdatedAt:         time.Now().UTC(),
	}, nil
}

// --- Memory Meta ---

func (s *FSStore) memMetaPath(repoID, snapshotID domain.ContentHash) string {
	return filepath.Join(s.repoDir(repoID), "memmeta", hexOf(snapshotID)+".json")
}

func (s *FSStore) GetMemoryMeta(_ context.Context, repoID, snapshotID domain.ContentHash) (domain.MemoryDigest, error) {
	if err := validateHashes(repoID, snapshotID); err != nil {
		return domain.MemoryDigest{}, err
	}
	data, err := os.ReadFile(s.memMetaPath(repoID, snapshotID))
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
	if d.SnapshotID != snapshotID {
		return domain.MemoryDigest{}, domain.ErrIntegrity
	}
	return d, nil
}

func (s *FSStore) PutMemoryMeta(ctx context.Context, repoID domain.ContentHash, digest domain.MemoryDigest) error {
	if err := validateHashes(repoID, digest.SnapshotID); err != nil {
		return err
	}
	// Target snapshot preexistence required (sync protocol).
	if !exists(s.snapshotPath(repoID, digest.SnapshotID)) {
		return domain.ErrNotFound
	}
	data, _ := json.Marshal(digest)
	return writeAtomic(s.memMetaPath(repoID, digest.SnapshotID), data)
}

// --- BlobStore ---

func (s *FSStore) docPath(repoID, hash domain.ContentHash) string {
	return filepath.Join(s.repoDir(repoID), "objects", "docs", hexOf(hash))
}
func (s *FSStore) memPath(repoID, hash domain.ContentHash) string {
	return filepath.Join(s.repoDir(repoID), "objects", "memories", hexOf(hash))
}

// PutDoc stores the SessionDoc.CIR body under the client-provided doc.Hash key (content-hash dedup).
func (s *FSStore) PutDoc(_ context.Context, repoID domain.ContentHash, doc domain.SessionDoc) (bool, error) {
	if err := validateHash(repoID); err != nil {
		return false, err
	}
	if err := domain.ValidateSessionDocHash(doc); err != nil {
		return false, err
	}
	p := s.docPath(repoID, doc.Hash)
	if exists(p) {
		if _, err := s.GetDoc(context.Background(), repoID, doc.Hash); err != nil {
			return false, err
		}
		return false, nil
	}
	data, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		return false, err
	}
	// Chunk CAS basic (doc_chunks.go) — append-only session prefixes are deduped across pushes.
	// Inapplicable chunking falls back to whole. Integrity hash remains whole canonical.
	chunked, _, err := s.putDocChunked(repoID, doc.Hash, data)
	if err != nil {
		return false, err
	}
	if !chunked {
		if err := writeAtomic(p, docCompress(data)); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (s *FSStore) GetDoc(_ context.Context, repoID, hash domain.ContentHash) (domain.SessionDoc, error) {
	if err := validateHashes(repoID, hash); err != nil {
		return domain.SessionDoc{}, err
	}
	data, err := os.ReadFile(s.docPath(repoID, hash))
	if err != nil {
		if os.IsNotExist(err) {
			return domain.SessionDoc{}, domain.ErrNotFound
		}
		return domain.SessionDoc{}, err
	}
	if data, err = docDecompress(data); err != nil {
		return domain.SessionDoc{}, err
	}
	// For chunked (manifest) types, reassembly — reassembly bytes are compared to the integrity hash,
	// equivalent to RecalculateSessionDocHash (byte equality is stronger).
	if cb, isManifest, cerr := s.getDocChunked(repoID, hash, data); isManifest {
		if cerr != nil {
			return domain.SessionDoc{}, cerr
		}
		var cir domain.CIRDocument
		if err := json.Unmarshal(cb, &cir); err != nil {
			return domain.SessionDoc{}, err
		}
		return domain.SessionDoc{Hash: hash, CIR: cir}, nil
	}
	var cir domain.CIRDocument
	if err := json.Unmarshal(data, &cir); err != nil {
		return domain.SessionDoc{}, err
	}
	doc := domain.SessionDoc{Hash: hash, CIR: cir}
	if err := domain.ValidateSessionDocHash(doc); err != nil {
		return domain.SessionDoc{}, err
	}
	return doc, nil
}

func (s *FSStore) HasDocs(ctx context.Context, repoID domain.ContentHash, hashes []domain.ContentHash) ([]domain.ContentHash, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	var have []domain.ContentHash
	for _, h := range hashes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := validateHash(h); err != nil {
			return nil, err
		}
		exists, err := regularObjectExists(s.docPath(repoID, h))
		if err != nil {
			return nil, err
		}
		if exists {
			have = append(have, h)
		}
	}
	return have, nil
}

func (s *FSStore) PutMemory(_ context.Context, repoID domain.ContentHash, digest domain.MemoryDigest) (domain.ContentHash, error) {
	if err := validateHash(repoID); err != nil {
		return "", err
	}
	if err := validateMemoryDigestRefs(digest); err != nil {
		return "", err
	}
	data, err := json.Marshal(digest)
	if err != nil {
		return "", err
	}
	h, err := domain.MemoryDigestHash(digest)
	if err != nil {
		return "", err
	}
	p := s.memPath(repoID, h)
	if exists(p) {
		if _, err := s.GetMemory(context.Background(), repoID, h); err != nil {
			return "", err
		}
	} else {
		chunked, _, err := s.putMemoryChunked(repoID, h, digest)
		if err != nil {
			return "", err
		}
		if !chunked {
			if err := writeAtomic(p, data); err != nil {
				return "", err
			}
		}
	}
	return h, nil
}

func (s *FSStore) GetMemory(_ context.Context, repoID, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if err := validateHashes(repoID, hash); err != nil {
		return domain.MemoryDigest{}, err
	}
	data, err := os.ReadFile(s.memPath(repoID, hash))
	if err != nil {
		if os.IsNotExist(err) {
			return domain.MemoryDigest{}, domain.ErrNotFound
		}
		return domain.MemoryDigest{}, err
	}
	if chunked, isManifest, chunkErr := s.getMemoryChunked(repoID, hash, data); isManifest {
		if chunkErr != nil {
			return domain.MemoryDigest{}, chunkErr
		}
		return chunked, nil
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
		return domain.MemoryDigest{}, domain.ErrIntegrity
	}
	if err := validateMemoryDigestRefs(d); err != nil {
		return domain.MemoryDigest{}, err
	}
	return d, nil
}
