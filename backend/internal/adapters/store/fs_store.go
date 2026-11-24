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
	"strings"
	"sync"

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
}

// NewFSStore creates an FSStore rooted at dataDir.
func NewFSStore(dataDir string) *FSStore {
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	s := &FSStore{dataDir: dataDir}
	s.recoveryErr = s.migrateOwnership()
	return s
}

// OpenFSStore returns the store only after its start-up migration succeeded. The production start path must always use this checked constructor, while NewFSStore is for testing.
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

var fsRefLocks sync.Map

func (s *FSStore) refLock(repoID domain.ContentHash, _ domain.RefKind, _ string) *sync.Mutex {
	key := s.dataDir + "\x00" + string(repoID)
	lock, _ := fsRefLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// --- Memory Meta ---

// --- BlobStore ---

func (s *FSStore) docPath(repoID, hash domain.ContentHash) string {
	return filepath.Join(s.repoDir(repoID), "objects", "docs", hexOf(hash))
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
