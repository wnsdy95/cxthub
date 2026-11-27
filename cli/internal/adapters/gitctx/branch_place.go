package gitctx

import (
	"context"
	"strconv"
	"strings"
)

// BranchPlace reads from Git the branch a folder is on and that branch's
// newest commits. It writes nothing.
type BranchPlace struct{}

// NewBranchPlace creates the adapter.
func NewBranchPlace() *BranchPlace { return &BranchPlace{} }

// BranchPlace returns the branch cwd is on and the newest commits of that
// branch, the newest first and at most limit. A folder that is on no branch,
// or in no repository, has neither.
func (*BranchPlace) BranchPlace(ctx context.Context, cwd string, limit int) (string, []string, error) {
	out, err := gitRead(ctx, cwd, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", nil, ctx.Err()
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" || limit < 1 {
		return branch, nil, nil
	}
	// A branch without a commit has nothing to list.
	listed, err := gitRead(ctx, cwd, nil, "rev-list", "--max-count="+strconv.Itoa(limit), "HEAD")
	if err != nil {
		return branch, nil, ctx.Err()
	}
	var newest []string
	for _, line := range strings.Split(string(listed), "\n") {
		if sha := strings.TrimSpace(line); fullCommitNumber(sha) {
			newest = append(newest, sha)
		}
	}
	return branch, newest, nil
}

// fullCommitNumber reports whether sha is a whole SHA-1 or SHA-256 commit
// number in lower case.
func fullCommitNumber(sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	for _, digit := range sha {
		if (digit < '0' || digit > '9') && (digit < 'a' || digit > 'f') {
			return false
		}
	}
	return true
}
