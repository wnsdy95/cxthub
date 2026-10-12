package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func verifyMergeAppendOrder(ctx context.Context, cwd, selectedCode, incomingAfter string) error {
	if !domain.ValidGitOID(selectedCode) || !domain.ValidGitOID(incomingAfter) {
		return fmt.Errorf("%w: merge append requires exact selected and incoming code", domain.ErrSelectionChanged)
	}
	for _, commit := range []string{selectedCode, incomingAfter} {
		kind, err := fetchGit(ctx, cwd, "cat-file", "-t", commit)
		if err != nil || kind != "commit" {
			return fmt.Errorf("%w: merge append code %s is not a verified local commit", domain.ErrSelectionChanged, commit)
		}
	}
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "merge-base", "--is-ancestor", selectedCode, incomingAfter)
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_GRAFT_FILE="+os.DevNull)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: selected server code is not a verified ancestor of the saved merge: %v", domain.ErrSelectionChanged, err)
	}
	return nil
}
