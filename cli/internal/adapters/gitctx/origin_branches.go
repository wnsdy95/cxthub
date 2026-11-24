package gitctx

import "context"

// OriginBranches lists origin's branches as this clone holds them: every
// remote-tracking branch of origin with the commit it is at, in the order of
// their names. It reads Git's own records and asks no remote. The list is
// different after this computer pushed or fetched.
func OriginBranches(ctx context.Context, cwd string) (string, error) {
	out, err := gitRead(ctx, cwd, nil, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/origin")
	if err != nil {
		return "", err
	}
	return string(out), nil
}
