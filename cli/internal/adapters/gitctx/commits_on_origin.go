package gitctx

import (
	"context"
	"strings"
)

// CommitsOnOrigin says which of the given commits a remote-tracking branch of
// origin holds in this clone. It reads Git's own records and asks no remote:
// it is what this computer knows was pushed or fetched. A commit that is not
// a full commit number, or that Git does not have, is not among the answer.
func CommitsOnOrigin(ctx context.Context, cwd string, commits []string) map[string]bool {
	held := map[string]bool{}
	branches, _ := followedBranches(ctx, cwd)
	for _, sha := range commits {
		if !fullCommitNumber(sha) || held[sha] {
			continue
		}
		if ctx.Err() != nil {
			return held
		}
		out, err := gitRead(ctx, cwd, nil, "for-each-ref", "--count=1", "--contains", sha, "--format=%(refname)", branches)
		if err == nil && strings.TrimSpace(string(out)) != "" {
			held[sha] = true
		}
	}
	return held
}
