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

// FileStore keeps the repository's local files in the .cxt/ directory of the
// repo root (client-only; no SQL):
//
//	repoRoot/.cxt/objects/settingsobjs/<hex>  a settings bundle, named by its content hash
//	repoRoot/.cxt/locks/<namespace>/<key>.lock a lock shared by local cxt processes
//
// Files are written with write-temp and atomic rename.
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
