package domain

import (
	"strings"
	"testing"
)

func TestGitPublicationWitnessQueryUsesEntireEvidenceKey(t *testing.T) {
	_, _, publication, history := participationFixture()
	query := GitPublicationWitnessQuery{Publication: publication, GitHead: publication.GitAfter}
	if query.Validate() != nil || !query.Matches(history[0]) {
		t.Fatal("original evidence did not match")
	}
	for _, field := range []string{"repo", "branch-id", "branch", "local", "worktree", "target", "head", "kind", "invalid"} {
		t.Run(field, func(t *testing.T) {
			event := history[0]
			switch field {
			case "repo":
				event.RepoID = string(HashContent([]byte("foreign")))
			case "branch-id":
				event.BranchID = "foreign"
			case "branch":
				event.Branch = "foreign"
			case "local":
				event.LocalBranch = "foreign"
			case "worktree":
				event.WorktreeID = strings.Repeat("f", 32)
			case "target":
				event.Target = HashContent([]byte("foreign"))
			case "head":
				event.GitAfter = strings.Repeat("e", 40)
			case "kind":
				event.Kind = "publish"
			case "invalid":
				event.ID = "invalid"
			}
			if query.Matches(event) {
				t.Fatal("inexact evidence matched")
			}
		})
	}
	carried := history[0]
	carried.GitAfter, query.GitHead = strings.Repeat("e", 40), strings.Repeat("e", 40)
	if !query.Matches(carried) || query.Matches(history[0]) || query.Publication.GitAfter != publication.GitAfter {
		t.Fatal("carry changed original provenance")
	}
}

func TestGitPublicationWitnessQueryFrozenIDs(t *testing.T) {
	_, _, publication, _ := participationFixture()
	for _, ids := range [][]string{nil, {strings.Repeat("6", 32)}, {strings.Repeat("6", 32), strings.Repeat("7", 32)}} {
		if (GitPublicationWitnessQuery{Publication: publication, GitHead: publication.GitAfter, IDs: ids}).Validate() != nil {
			t.Fatal("valid exact IDs rejected", ids)
		}
	}
	for _, ids := range [][]string{{"invalid"}, {publication.ID}, {strings.Repeat("6", 32), strings.Repeat("6", 32)}, {strings.Repeat("7", 32), strings.Repeat("6", 32)}, make([]string, MaxGitAdmissionWitnesses+1)} {
		if (GitPublicationWitnessQuery{Publication: publication, GitHead: publication.GitAfter, IDs: ids}).Validate() == nil {
			t.Fatal("invalid frozen set accepted", ids)
		}
	}
}
