package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const publicationGitOrigin = "https://git.example.test/acme/publication.git"

type publicationLocalGit struct {
	directory string
}

func (reader publicationLocalGit) git(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", reader.directory}, args...)...)
	command.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_GRAFT_FILE="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=")
	output, err := command.Output()
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		diagnostic := strings.TrimSpace(string(exitError.Stderr))
		if len(diagnostic) > 2048 {
			diagnostic = diagnostic[:2048] + " [truncated]"
		}
		return nil, fmt.Errorf("fixture git %v: %w: %s", args, err, diagnostic)
	}
	return output, err
}

func (reader publicationLocalGit) ListGitHeads(ctx context.Context, origin string, page int) ([]outbound.GitHead, bool, error) {
	if origin != publicationGitOrigin || page < 1 {
		return nil, false, domain.ErrValidation
	}
	heads := []outbound.GitHead{}
	if page > 1 {
		return heads, false, nil
	}
	raw, err := reader.git(ctx, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/")
	if err != nil {
		return nil, false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || domain.ValidateGitOID(fields[1]) != nil {
			return nil, false, domain.ErrIntegrity
		}
		heads = append(heads, outbound.GitHead{Ref: fields[0], Commit: fields[1]})
	}
	return heads, false, nil
}
