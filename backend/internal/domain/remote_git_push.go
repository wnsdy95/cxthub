package domain

import (
	"fmt"
	"time"
)

// RemoteBranchPush is one time the server saw a branch of the remote with
// another newest commit than at its read before: somebody pushed it in
// between. SeenAt is when the server read it, so it is at or after the push.
//
// The first read of a repository records no push: every branch is new to the
// server then, and nothing says when it was pushed.
type RemoteBranchPush struct {
	Branch string    `json:"branch"`
	SHA    string    `json:"sha"`
	SeenAt time.Time `json:"seen_at"`
}

// Validate checks a push before it is stored.
func (p RemoteBranchPush) Validate() error {
	if (RemoteBranch{Name: p.Branch, SHA: p.SHA}).Validate() != nil || p.SeenAt.IsZero() {
		return fmt.Errorf("%w: remote branch push", ErrValidation)
	}
	return nil
}

// RemoteBranchPushes compares the branches a read found with the ones the
// read before it stored. A branch that is new, or whose newest commit is
// another one, was pushed. A branch that is gone was not.
func RemoteBranchPushes(before, after []RemoteBranch, seenAt time.Time) []RemoteBranchPush {
	had := make(map[string]string, len(before))
	for _, branch := range before {
		had[branch.Name] = branch.SHA
	}
	pushes := []RemoteBranchPush{}
	for _, branch := range after {
		if had[branch.Name] != branch.SHA {
			pushes = append(pushes, RemoteBranchPush{Branch: branch.Name, SHA: branch.SHA, SeenAt: seenAt.UTC()})
		}
	}
	return pushes
}
