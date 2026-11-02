package domain

import (
	"context"
	"errors"
	"testing"
)

func TestRepositoryViewScopeRejectsForeignRowsWithoutBranches(test *testing.T) {
	repo := HashContent([]byte("scope repository"))
	foreign := HashContent([]byte("foreign scope repository"))
	for _, source := range []string{"ref", "snapshot", "history", "pending", "unsync", "archive"} {
		test.Run(source, func(test *testing.T) {
			for _, owner := range []ContentHash{repo, foreign} {
				var view RepositoryView
				switch source {
				case "ref":
					view.Refs = []Ref{{RepoID: owner}}
				case "snapshot":
					view.Snapshots = []Snapshot{{RepoID: owner}}
				case "history":
					view.History = []HistoryEvent{{RepoID: string(owner)}}
				case "pending":
					view.Pending = []Pending{{RepoID: owner}}
				case "unsync":
					view.Unsync = []Unsync{{RepoID: owner}}
				case "archive":
					view.ArchivedSessions = []SessionArchiveView{{SessionArchive: SessionArchive{RepoID: owner}}}
				}
				err := ValidateRepositoryViewScope(context.Background(), repo, view)
				if owner == repo && err != nil || owner != repo && !errors.Is(err, ErrIntegrity) {
					test.Fatal("repository scope validation lost a collection", source, owner, err)
				}
			}
		})
	}
}

func TestRepositoryViewScopePreservesCancellationAndHashValidation(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ValidateRepositoryViewScope(ctx, "invalid", RepositoryView{}); !errors.Is(err, context.Canceled) {
		test.Fatal("scope lookup ignored cancellation", err)
	}
	if err := ValidateRepositoryViewScope(context.Background(), "invalid", RepositoryView{}); !errors.Is(err, ErrIntegrity) {
		test.Fatal("invalid repository was accepted", err)
	}
}
