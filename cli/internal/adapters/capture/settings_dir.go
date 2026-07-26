package capture

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// ReadSettingsDir reads the SettingsBundle from the repoRoot/.{kind}(claude|agents) folder.
// It records the agent settings state at the commit point content-addressed.
// Determinism: path sorting. Safety: hidden subdirectories (.git etc.) and truncation at 2MB (bundle, false).
func ReadSettingsDir(repoRoot, kind string) (domain.SettingsBundle, bool) {
	if !domain.ValidSettingsKind(kind) {
		return domain.SettingsBundle{}, false
	}
	root := filepath.Join(repoRoot, "."+kind)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return domain.SettingsBundle{}, false
	}
	var files []domain.SettingsFile
	total := 0
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, werr error) error {
		if werr != nil || fi.IsDir() {
			if fi != nil && fi.IsDir() && strings.HasPrefix(fi.Name(), ".") && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil || strings.HasPrefix(fi.Name(), ".") {
			return nil
		}
		data, rerr2 := providerfs.ReadRepoFile(repoRoot, filepath.Join("."+kind, rel))
		if rerr2 != nil {
			return nil
		}
		total += len(data)
		files = append(files, domain.SettingsFile{
			Path:       filepath.ToSlash(rel),
			ContentB64: base64.StdEncoding.EncodeToString(data),
		})
		return nil
	})
	if len(files) == 0 || total > 2<<20 {
		return domain.SettingsBundle{}, false
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return domain.SettingsBundle{Kind: kind, Files: files}, true
}

// WriteSettingsDir reflects the validated bundle exactly into repoRoot/.{expectedKind}.
// It writes the entire thing to a temporary directory on the same filesystem and then replaces it,
// so write/verification failures do not erase the existing settings. The logical backup (content-addressed object) before replacement is the caller's responsibility.
func WriteSettingsDir(repoRoot, expectedKind string, expectedHash domain.ContentHash, bundle domain.SettingsBundle) (int, error) {
	if err := domain.ValidateSettingsBundle(expectedKind, expectedHash, bundle); err != nil {
		return 0, err
	}
	absRepo, err := filepath.Abs(repoRoot)
	if err != nil {
		return 0, err
	}
	root := filepath.Join(absRepo, "."+expectedKind)
	tmp, err := os.MkdirTemp(absRepo, ".cxt-settings-"+expectedKind+"-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tmp)

	for _, f := range bundle.Files {
		clean := filepath.Clean(filepath.FromSlash(f.Path))
		data, derr := base64.StdEncoding.DecodeString(f.ContentB64)
		if derr != nil {
			return 0, derr
		}
		target := filepath.Join(tmp, clean)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return 0, err
		}
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return 0, err
		}
		if _, err := file.Write(data); err != nil {
			_ = file.Close()
			return 0, err
		}
		if err := file.Close(); err != nil {
			return 0, err
		}
	}

	old := tmp + "-old"
	hadOld := false
	if _, err := os.Lstat(root); err == nil {
		if err := os.Rename(root, old); err != nil {
			return 0, err
		}
		hadOld = true
	} else if !os.IsNotExist(err) {
		return 0, err
	}
	if err := os.Rename(tmp, root); err != nil {
		if hadOld {
			_ = os.Rename(old, root)
		}
		return 0, err
	}
	if hadOld {
		_ = os.RemoveAll(old)
	}
	return len(bundle.Files), nil
}
