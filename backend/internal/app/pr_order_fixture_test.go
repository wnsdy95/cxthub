package app

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func recordPRMergeChain(t *testing.T, service *Service, origin string, commits ...string) {
	t.Helper()
	reader := &scanReader{deltas: map[string]domain.GitCommitDelta{}}
	for index, commit := range commits {
		delta := domain.GitCommitDelta{Commit: commit, Parents: []string{}, Complete: true, Changes: []domain.GitPathChange{}}
		if index > 0 {
			delta.Parent = commits[index-1]
			delta.Parents = []string{delta.Parent}
		}
		reader.deltas[commit] = delta
	}
	scans, err := NewGitScans(service, reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scans.ObservePush(systemTestContext(), origin, "refs/heads/main", "", commits[len(commits)-1], false, t.Name()); err != nil {
		t.Fatal(err)
	}
	if err := scans.Process(systemTestContext(), 100); err != nil {
		t.Fatal(err)
	}
}
