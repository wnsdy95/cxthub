package providerfs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// EnsureRealDir creates path and rejects a symlink or non-directory at the
// resulting leaf. Callers use this for user-owned state outside a repository.
func EnsureRealDir(path string, perm os.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("unsafe output directory %q: real directory required", path)
	}
	return nil
}

// EnsurePrivateDir additionally fixes the leaf directory permissions. It is
// intended for ~/.cxt credential state, not shared repository directories.
func EnsurePrivateDir(path string) error {
	if err := EnsureRealDir(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

// OpenRegularFile refuses symlink and special-file inputs before opening
// user-owned state. This keeps credential, provider-session and managed-hook
// reads symmetric with WriteRegularFileAtomic.
func OpenRegularFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("unsafe input target %q: regular file required", path)
	}
	dir := filepath.Dir(path)
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return nil, fmt.Errorf("unsafe input directory %q: real directory required", dir)
	}
	return os.Open(path)
}

func ReadRegularFile(path string) ([]byte, error) {
	f, err := OpenRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// OpenRepoFile opens a repository-relative regular file while refusing every
// symlink below the (possibly symlinked) repository root.
func OpenRepoFile(repoRoot, relative string) (*os.File, error) {
	clean, err := cleanRepoRelative(relative)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return nil, err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, fmt.Errorf("unsafe repository root %q: real directory required", repoRoot)
	}

	parts := strings.Split(clean, string(filepath.Separator))
	current := root
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("unsafe repository-relative path %q", relative)
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe repository path %q: symlink not allowed", current)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, fmt.Errorf("unsafe repository path %q: directory required", current)
		}
		if i == len(parts)-1 && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("unsafe repository file %q: regular file required", current)
		}
	}
	return os.Open(current)
}

func ReadRepoFile(repoRoot, relative string) ([]byte, error) {
	f, err := OpenRepoFile(repoRoot, relative)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func cleanRepoRelative(relative string) (string, error) {
	clean := filepath.Clean(relative)
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe repository-relative path %q", relative)
	}
	return clean, nil
}

// EnsureRepoDir creates a directory below repoRoot without following any
// repository-controlled symlink. repoRoot itself may be reached through a
// symlink (a common workspace setup), so it is resolved before the walk.
func EnsureRepoDir(repoRoot, relative string, perm os.FileMode) (string, error) {
	root, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("unsafe repository root %q: real directory required", repoRoot)
	}

	clean := filepath.Clean(relative)
	if clean == "." {
		return root, nil
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe repository-relative directory %q", relative)
	}

	current := root
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("unsafe repository-relative directory %q", relative)
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err := os.Mkdir(current, perm); err != nil && !os.IsExist(err) {
				return "", err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("unsafe repository directory %q: real directory required", current)
		}
	}
	return current, nil
}

// PrepareRepoFile resolves a repository-relative file path, creates its real
// parent directories, and rejects an existing symlink or special-file target.
func PrepareRepoFile(repoRoot, relative string, dirPerm os.FileMode) (string, error) {
	clean, err := cleanRepoRelative(relative)
	if err != nil {
		return "", err
	}
	dir, err := EnsureRepoDir(repoRoot, filepath.Dir(clean), dirPerm)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, filepath.Base(clean))
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", fmt.Errorf("unsafe repository file %q: regular file required", path)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return path, nil
}

// WriteRepoFileAtomic writes a repository-relative file without following
// symlinks in either the target or any directory below repoRoot.
func WriteRepoFileAtomic(repoRoot, relative string, data []byte, perm os.FileMode) error {
	path, err := PrepareRepoFile(repoRoot, relative, 0o755)
	if err != nil {
		return err
	}
	return WriteRegularFileAtomic(path, data, perm)
}

// RemoveRepoFile removes a repository-relative regular file without following
// repository-controlled symlinks. Missing files are treated as success.
func RemoveRepoFile(repoRoot, relative string) error {
	path, err := PrepareRepoFile(repoRoot, relative, 0o755)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// WriteRegularFileAtomic refuses symlink/device targets and replaces a regular
// file through a same-directory temp file, so the final write never follows a
// repository-controlled symlink.
func WriteRegularFileAtomic(path string, data []byte, perm os.FileMode) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("unsafe output target %q: regular file required", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	dir := filepath.Dir(path)
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return fmt.Errorf("unsafe output directory %q: real directory required", dir)
	}
	tmp, err := os.CreateTemp(dir, ".cxt-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(perm); err != nil {
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
