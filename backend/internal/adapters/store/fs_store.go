// Package store provides server-side metadata/blob storage adapters.
//
// The default adapter is FSStore (file-based, content-addressed), operating end-to-end without external dependencies (stdlib).
// It enables REST server push/pull operations (demonstrable without a Postgres server).
// The production target PostgreSQL adapter is gated under //go:build postgres in a separate file (pgx), and store.Open(factory) selects the appropriate implementation based on the build tag.
package store

import (
	"context"
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
	outbound.GitHubStore
	outbound.MetadataStore
	outbound.BlobStore
	outbound.VerifiedDocStore
	outbound.RepositoryStore
	outbound.OrganizationStore
	outbound.OAuthStore
	outbound.RuntimeStore
	VerifyMigrations(ctx context.Context, dir string) error
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
	docProofs   docProofCache
	dataDir     string
	recoveryErr error

	joinProjectionWriter func(path string, data []byte) error
}

// NewFSStore creates an FSStore rooted at dataDir.
func NewFSStore(dataDir string) *FSStore {
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	s := &FSStore{dataDir: dataDir}
	s.recoveryErr = s.recoverJoinJournals()
	if s.recoveryErr == nil {
		s.recoveryErr = s.recoverHistoryJournals()
	}
	if s.recoveryErr == nil {
		s.recoveryErr = s.migrateOwnership()
	}
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
	if err := snap.DocIdentity.Validate(); err != nil {
		return err
	}
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
	return writeAtomicMode(path, data, 0644)
}

func writeAtomicMode(path string, data []byte, mode os.FileMode) error {
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
	if err := tmp.Chmod(mode); err != nil {
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
	protocol, err := s.contextProtocol(id)
	if err != nil {
		return domain.Repo{}, err
	}
	r.ContextProtocol = protocol
	r.LocalPath = ""
	r.RemoteURL = domain.SanitizeRemoteURL(r.RemoteURL)
	r.GitRemoteURL = domain.SanitizeRemoteURL(r.GitRemoteURL)
	return r, nil
}

// PutRepo is idempotent: if it already exists, it returns the existing record (sync protocol). Exception: for unowned (repository_id="") records, it fills in a delayed binding if a new binding arrives.
func (s *FSStore) PutRepo(ctx context.Context, repo domain.Repo) (domain.Repo, error) {
	if repo.RequiredDocIdentity != domain.DocumentIdentityLegacy {
		return domain.Repo{}, domain.ErrValidation
	}
	if repo.RepositoryID != "" {
		if err := domain.ValidateRepositoryID(repo.RepositoryID); err != nil {
			return domain.Repo{}, err
		}
		binding := s.repositoryBindingLock(repo.RepositoryID)
		binding.Lock()
		defer binding.Unlock()
		if bound, err := s.GetBoundRepo(ctx, repo.RepositoryID); err == nil && bound.ID != repo.ID {
			return domain.Repo{}, domain.ErrConflict
		} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return domain.Repo{}, err
		}
	}
	lock := s.refLock(repo.ID, domain.RefBranch, "")
	lock.Lock()
	defer lock.Unlock()
	if err := validateHash(repo.ID); err != nil {
		return domain.Repo{}, err
	}
	if repo.DefaultBranch != "" {
		if err := domain.ValidateBranchName(repo.DefaultBranch); err != nil {
			return domain.Repo{}, err
		}
	}
	repo.ContextProtocol = 0
	repo.LocalPath = ""
	repo.RemoteURL = domain.SanitizeRemoteURL(repo.RemoteURL)
	repo.GitRemoteURL = domain.SanitizeRemoteURL(repo.GitRemoteURL)
	if existing, err := s.GetRepo(ctx, repo.ID); err == nil {
		if existing.RepositoryID != "" && repo.RepositoryID != "" && existing.RepositoryID != repo.RepositoryID {
			return domain.Repo{}, domain.ErrConflict
		}
		changed := false
		if existing.RepositoryID == "" && repo.RepositoryID != "" {
			existing.RepositoryID = repo.RepositoryID
			changed = true
		}
		// Registration may fill an unknown origin, but cannot replace established
		// identity or reset an owner-configured default branch.
		if repo.DefaultBranch != "" && existing.DefaultBranch == "" {
			existing.DefaultBranch = repo.DefaultBranch
			changed = true
		}
		if repo.GitRemoteURL != "" && existing.GitRemoteURL == "" {
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
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.Repo{}, err
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
	lock := s.refLock(id, domain.RefBranch, "")
	lock.Lock()
	defer lock.Unlock()
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
	lock := s.refLock(id, domain.RefBranch, "")
	lock.Lock()
	defer lock.Unlock()
	if err := s.recoverHistoryEvent(ctx, id); err != nil {
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

// DeleteDoc removes the doc body (hook capture leaf GC exclusive — idempotent).
func (s *FSStore) DeleteDoc(_ context.Context, repoID, hash domain.ContentHash) error {
	if err := validateHashes(repoID, hash); err != nil {
		return err
	}
	legacyIndex := filepath.Join(s.repoDir(repoID), "read-index-v1", hexOf(hash))
	for _, path := range []string{s.docPath(repoID, hash), s.readIndexPath(repoID, hash), s.readIndexPath(repoID, hash) + ".search", s.readIndexPath(repoID, hash) + ".filter", legacyIndex, legacyIndex + ".search", legacyIndex + ".filter"} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.RemoveAll(filepath.Dir(s.rangeIndexPath(repoID, hash)))
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
	lock := s.refLock(repoID, domain.RefBranch, "")
	lock.Lock()
	defer lock.Unlock()
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
	BranchID     string             `json:"branch_id,omitempty"`
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
	if domain.ValidateRef(domain.Ref{RepoID: j.RepoID, Kind: domain.RefBranch, Name: j.Branch, Target: j.NewHead, BranchID: j.BranchID}) != nil {
		return domain.ErrIntegrity
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
	if err != nil || branch.BranchID != j.BranchID || (branch.Target != j.ExpectedHead && branch.Target != j.NewHead) {
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
	writeProjection := s.joinProjectionWriter
	if writeProjection == nil {
		writeProjection = writeAtomic
	}
	if forward {
		for _, entry := range j.Snapshots {
			if err := writeProjection(s.snapshotPath(j.RepoID, entry.ID), entry.After); err != nil {
				return err
			}
		}
		if j.ForkName != "" {
			if err := writeProjection(s.refFile(j.RepoID, domain.RefSession, j.ForkName), []byte(string(j.ForkTip)+"\n")); err != nil {
				return err
			}
		}
		return writeProjection(s.refFile(j.RepoID, domain.RefBranch, j.Branch), encodeRef(domain.Ref{RepoID: j.RepoID, Kind: domain.RefBranch, Name: j.Branch, BranchID: j.BranchID, Target: j.NewHead}))
	}
	for i := len(j.Snapshots) - 1; i >= 0; i-- {
		entry := j.Snapshots[i]
		if err := writeProjection(s.snapshotPath(j.RepoID, entry.ID), entry.Before); err != nil {
			return err
		}
	}
	if j.ForkName != "" {
		path := s.refFile(j.RepoID, domain.RefSession, j.ForkName)
		if j.ForkExisted {
			if err := writeProjection(path, []byte(string(j.ForkTip)+"\n")); err != nil {
				return err
			}
		} else if err := removeFileDurable(path); err != nil {
			return err
		}
	}
	return writeProjection(s.refFile(j.RepoID, domain.RefBranch, j.Branch), encodeRef(domain.Ref{RepoID: j.RepoID, Kind: domain.RefBranch, Name: j.Branch, BranchID: j.BranchID, Target: j.ExpectedHead}))
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
	return s.completeCommittedJoin(path, j)
}

func (storage *FSStore) completeCommittedJoin(path string, journal fsJoinJournal) error {
	if journal.ForkName != "" && !journal.ForkExisted {
		if err := storage.ensureJoinReflog(journal.RepoID, domain.RefLogEntry{Kind: domain.RefSession, Name: journal.ForkName, New: journal.ForkTip, CreatedAt: journal.CreatedAt}); err != nil {
			return err
		}
	}
	if err := storage.ensureJoinReflog(journal.RepoID, domain.RefLogEntry{Kind: domain.RefBranch, Name: journal.Branch, Old: journal.ExpectedHead, New: journal.NewHead, CreatedAt: journal.CreatedAt}); err != nil {
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

func (s *FSStore) getRefRaw(_ context.Context, repoID domain.ContentHash, kind domain.RefKind, name string) (domain.Ref, error) {
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
	if kind == domain.RefBranch && strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		var ref domain.Ref
		if err := json.Unmarshal(data, &ref); err != nil {
			return ref, err
		}
		if ref.RepoID != repoID || ref.Kind != kind || ref.Name != name {
			return ref, domain.ErrIntegrity
		}
		if err := domain.ValidateRef(ref); err != nil {
			return ref, err
		}
		return ref, nil
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

func (s *FSStore) GetRef(ctx context.Context, repoID domain.ContentHash, kind domain.RefKind, name string) (domain.Ref, error) {
	ref, err := s.getRefRaw(ctx, repoID, kind, name)
	if err != nil || (kind != domain.RefBranch && kind != domain.RefHead) {
		return ref, err
	}
	protocol, err := s.contextProtocol(repoID)
	if err != nil {
		return domain.Ref{}, err
	}
	if protocol == 1 {
		return ref, nil
	}
	refs, err := s.listRefsRaw(ctx, repoID)
	if err != nil {
		return domain.Ref{}, err
	}
	projected, err := domain.ProjectBranchLifecycleRefs(refs)
	if err != nil {
		return domain.Ref{}, err
	}
	for _, candidate := range projected {
		if candidate.Kind == kind && candidate.Name == name {
			return candidate, nil
		}
	}
	return domain.Ref{}, domain.ErrNotFound
}

func (s *FSStore) listRefsRaw(ctx context.Context, repoID domain.ContentHash) ([]domain.Ref, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	var out []domain.Ref
	if head, err := s.getRefRaw(ctx, repoID, domain.RefHead, domain.HeadRefName); err == nil {
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
			ref, rerr := s.getRefRaw(ctx, repoID, kc.kind, filepath.ToSlash(rel))
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

func (s *FSStore) detachHeadFromBranchRaw(ctx context.Context, repoID domain.ContentHash, branch string, target domain.ContentHash) error {
	head, err := s.getRefRaw(ctx, repoID, domain.RefHead, domain.HeadRefName)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimPrefix(head.Symbolic, "refs/heads/") != branch {
		return nil
	}
	return writeAtomic(s.refFile(repoID, domain.RefHead, domain.HeadRefName), []byte(string(target)+"\n"))
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
func (s *FSStore) PutDoc(ctx context.Context, repoID domain.ContentHash, doc domain.SessionDoc) (bool, error) {
	if err := validateHash(repoID); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	verified, err := domain.VerifySessionDoc(doc)
	if err != nil {
		return false, err
	}
	return s.PutVerifiedDoc(ctx, repoID, verified)
}

func (s *FSStore) PutVerifiedDoc(ctx context.Context, repoID domain.ContentHash, doc domain.VerifiedSessionDoc) (bool, error) {
	if doc.DocumentRef().Identity != domain.DocumentIdentityLegacy {
		return false, domain.ErrUnsupportedDocumentIdentity
	}
	if err := validateHash(repoID); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !doc.Valid() {
		return false, domain.ErrIntegrity
	}
	s.docProofs.put(docProofKey{repo: repoID, expected: doc.Hash(), representation: doc.Hash()}, doc.Reference())
	p := s.docPath(repoID, doc.Hash())
	if exists(p) {
		if _, err := s.GetDoc(ctx, repoID, doc.Hash()); err != nil {
			return false, err
		}
		return false, nil
	}
	// Chunk CAS basic (doc_chunks.go) — append-only session prefixes are deduped across pushes.
	// Inapplicable chunking falls back to whole. Integrity hash remains whole canonical.
	if plan, chunked := doc.ChunkPlan(); chunked {
		if _, err := s.putDocChunkPlan(repoID, doc.Hash(), plan); err != nil {
			return false, err
		}
	} else {
		if err := writeAtomic(p, docCompress(doc.Bytes())); err != nil {
			return false, err
		}
	}
	if err := s.putVerifiedReadIndex(repoID, doc); err != nil {
		return false, err
	}
	return true, nil
}

func (s *FSStore) GetDoc(ctx context.Context, repoID, hash domain.ContentHash) (domain.SessionDoc, error) {
	data, chunked, err := s.readDocBytes(ctx, repoID, hash)
	if err != nil {
		return domain.SessionDoc{}, err
	}
	var cir domain.CIRDocument
	if err := json.Unmarshal(data, &cir); err != nil {
		return domain.SessionDoc{}, err
	}
	doc := domain.SessionDoc{Hash: hash, CIR: cir}
	if !chunked {
		if err := domain.ValidateSessionDocHash(doc); err != nil {
			return domain.SessionDoc{}, err
		}
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
