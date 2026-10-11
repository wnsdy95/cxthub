package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGitSupplementPublicationRequiresExactOriginal(t *testing.T) {
	repo, capture := hh("supplement repo"), hh("supplement capture")
	ordinary := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "source", Branch: "feature", Kind: "position", Source: capture, Target: capture, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(100, 0).UTC()}
	original := prPublication(ordinary)
	record := (domain.GitContribution{RepoID: repo, GitOrigin: "https://github.com/example/repo", ProviderRepositoryID: 91, DestinationBranchID: "main", SourceBranchID: ordinary.BranchID, Capture: capture, OriginalHead: ordinary.GitAfter, IntegrationSHA: strings.Repeat("b", 40)}).WithID()
	for _, scenario := range []struct {
		name   string
		change func(*domain.HistoryEvent, *domain.HistoryEvent)
		want   error
	}{
		{"original", func(_ *domain.HistoryEvent, _ *domain.HistoryEvent) {}, nil},
		{"other branch", func(pub, proof *domain.HistoryEvent) { pub.BranchID = "another"; proof.BranchID = pub.BranchID }, domain.ErrConflict},
		{"other capture", func(pub, proof *domain.HistoryEvent) {
			pub.Target = hh("unrelated capture")
			pub.Source = pub.Target
			proof.Target = pub.Target
			proof.Source = pub.Target
		}, domain.ErrConflict},
		{"other code", func(pub, proof *domain.HistoryEvent) {
			pub.GitAfter = strings.Repeat("c", 40)
			proof.GitAfter = pub.GitAfter
		}, domain.ErrConflict},
		{"unpinned", func(pub, _ *domain.HistoryEvent) { pub.MemoryPinned = false }, domain.ErrConflict},
		{"unproven worktree", func(pub, _ *domain.HistoryEvent) { pub.WorktreeID = strings.Repeat("f", 32) }, domain.ErrNotFound},
		{"same incarnation renamed", func(pub, proof *domain.HistoryEvent) { pub.Branch = "renamed"; proof.Branch = pub.Branch }, nil},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			publication, proof := original, ordinary
			scenario.change(&publication, &proof)
			index, err := domain.NewGitPublicationIndex(context.Background(), repo, []domain.HistoryEvent{proof, publication})
			if err != nil {
				t.Fatal(err)
			}
			got, err := gitSupplementPublication(index, record, publication.ID)
			if !errors.Is(err, scenario.want) {
				t.Fatalf("publication acceptance: %+v %v", got, err)
			}
			if err == nil && got.ID != publication.ID {
				t.Fatal("publication was replaced")
			}
		})
	}
}
