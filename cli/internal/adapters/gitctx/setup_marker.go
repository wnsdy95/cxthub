package gitctx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
)

// SetupMarker writes the mark that `cxt setup` or `cxt init` ran in a
// repository. Hooks of the agent tools are registered for every folder, so
// cxt acts only where this mark is: .cxt/HEAD, a regular file in a real .cxt
// directory at the root every worktree of the repository shares.
type SetupMarker struct{}

// NewSetupMarker returns the writer of the mark.
func NewSetupMarker() SetupMarker { return SetupMarker{} }

// MarkSetUp makes the .cxt directory and the mark when they are absent and
// returns the root that holds them. An existing mark is never rewritten.
func (SetupMarker) MarkSetUp(ctx context.Context, cwd string) (string, error) {
	roots, err := ResolveRepositoryRoots(ctx, cwd)
	if err != nil {
		return "", err
	}
	root := roots.SharedRoot
	dir := filepath.Join(root, ".cxt")
	if info, err := os.Lstat(dir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%s is not a directory; cxt keeps its files in a real .cxt directory", dir)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	} else if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	head := filepath.Join(dir, "HEAD")
	if info, err := os.Lstat(head); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s is not a regular file", head)
		}
		return root, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := providerfs.WriteRepoFileAtomic(root, ".cxt/HEAD", []byte("cxt\n"), 0o600); err != nil {
		return "", err
	}
	return root, nil
}
