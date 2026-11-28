package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
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
//	repoRoot/.cxt/refs/heads/<branch>      branch ref with stable identity (JSON; legacy hash text supported).
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
	repoRoot        string
	worktreeID      string
	gitBranch       string
	gitCommit       string
	docProofKeyPath string // optional, outside the replica; configured before concurrent use
	// Pointer because worktree views copy FileStore while sharing immutable
	// event proofs. Current repository bytes are still read on every validation.
	docVerifier *domain.StoredDocumentVerifier
}

// NewFileStore creates a FileStore.
func NewFileStore(repoRoot string) *FileStore {
	return &FileStore{repoRoot: repoRoot, docVerifier: &domain.StoredDocumentVerifier{}}
}

func (s *FileStore) storeDir() string { return filepath.Join(s.repoRoot, ".cxt") }

func hexOf(hash domain.ContentHash) string {
	return strings.TrimPrefix(string(hash), "sha256:")
}

func (s *FileStore) objectPath(kind string, hash domain.ContentHash) string {
	return filepath.Join(s.storeDir(), "objects", kind, hexOf(hash))
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func validateCxtDir(dir string) error        { return providerfs.ValidateCxtDir(dir) }
func validateCxtWritePath(path string) error { return providerfs.ValidateCxtWritePath(path) }

func readCxtFile(path string) ([]byte, error) {
	if err := validateCxtWritePath(path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncCxtParents(path)
}

// Persist both replacement and newly created directories before acknowledging
// a ref/history write. Syncing only the file is insufficient after power loss.
func syncCxtParents(path string) error {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		f, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if filepath.Base(dir) == ".cxt" {
			f, err := os.Open(filepath.Dir(dir))
			if err != nil {
				return err
			}
			err = f.Sync()
			_ = f.Close()
			return err
		}
		if filepath.Dir(dir) == dir {
			return nil
		}
	}
}

const snapshotMutationLockStaleAfter = 10 * time.Minute

func (s *FileStore) withLegacyMutationLock(ctx context.Context, namespace, key string, fn func() error) error {
	locksDir := filepath.Join(s.storeDir(), "locks", namespace)
	if err := validateCxtDir(locksDir); err != nil {
		return err
	}
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		return err
	}
	if err := validateCxtDir(locksDir); err != nil {
		return err
	}
	lockPath := filepath.Join(locksDir, key+".lock")
	token := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	ownerPath := filepath.Join(lockPath, "owner")
	for {
		err := os.Mkdir(lockPath, 0o700)
		if err == nil {
			if err := writeAtomic(ownerPath, []byte(token)); err != nil {
				_ = os.Remove(lockPath)
				return err
			}
			defer func() {
				owner, readErr := readCxtFile(ownerPath)
				if readErr == nil && string(owner) == token {
					_ = os.Remove(ownerPath)
					_ = os.Remove(lockPath)
				}
			}()
			return fn()
		}
		if !os.IsExist(err) {
			return err
		}
		if info, statErr := os.Lstat(lockPath); statErr == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return domain.ErrHashMismatch
			}
			if deadMutationOwner(ownerPath) || (time.Since(info.ModTime()) > snapshotMutationLockStaleAfter && !liveMutationOwner(ownerPath)) {
				stalePath := lockPath + ".stale-" + token
				if os.Rename(lockPath, stalePath) == nil {
					_ = os.Remove(filepath.Join(stalePath, "owner"))
					_ = os.Remove(stalePath)
					continue
				}
			}
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// --- stash stack (git stash handling, .cxt/stash.json — newest at front) ---

// --- uncommitted capture pointer (.cxt/pending/<sessionID>.json — session-specific upsert) ---

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
