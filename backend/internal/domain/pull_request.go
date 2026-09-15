package domain

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// PullRequestMerge identifies the immutable Git revisions of a merged PR.
// Repository identity comes from the authenticated route or signed webhook.
type PullRequestMerge struct {
	Number     int    `json:"number"`
	BaseBranch string `json:"base_branch"`
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	MergeSHA   string `json:"merge_sha"`
}

func (p PullRequestMerge) Validate() error {
	if p.Number <= 0 || p.BaseBranch == p.HeadBranch {
		return fmt.Errorf("invalid pull request identity")
	}
	for _, name := range []string{p.BaseBranch, p.HeadBranch} {
		if err := ValidateBranchName(name); err != nil {
			return err
		}
	}
	for _, sha := range []string{p.HeadSHA, p.MergeSHA} {
		if (len(sha) != 40 && len(sha) != 64) || sha != strings.ToLower(sha) {
			return fmt.Errorf("PR promotion requires full Git object IDs")
		}
		if _, err := hex.DecodeString(sha); err != nil {
			return fmt.Errorf("invalid PR Git object ID")
		}
	}
	return nil
}
