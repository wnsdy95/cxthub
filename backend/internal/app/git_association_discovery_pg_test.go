//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type associationCatalogPG struct {
	*store.PostgresStore
	limit          int
	rejectSave     bool
	resetRead      bool
	originOverride string
	duringRead     func(context.Context)
}

func (storage *associationCatalogPG) GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error) {
	repo, err := storage.PostgresStore.GetRepo(ctx, id)
	if err == nil && storage.originOverride != "" {
		repo.GitRemoteURL = storage.originOverride
	}
	return repo, err
}

func (storage *associationCatalogPG) CatalogChanges(ctx context.Context, repo domain.ContentHash, request domain.CatalogRequest) (domain.CatalogPage, error) {
	if storage.duringRead != nil {
		storage.duringRead(ctx)
	}
	if storage.resetRead {
		return domain.CatalogPage{}, domain.ErrCatalogResetRequired
	}
	if storage.limit != 0 {
		request.Limit = storage.limit
	}
	return storage.PostgresStore.CatalogChanges(ctx, repo, request)
}

func (storage *associationCatalogPG) SaveGitAssociationDiscovery(ctx context.Context, next domain.GitAssociationDiscovery, expected int64, publications []domain.HistoryEvent) error {
	if err := storage.PostgresStore.SaveGitAssociationDiscovery(ctx, next, expected, publications); err != nil {
		return err
	}
	if storage.rejectSave {
		return domain.ErrIntegrity
	}
	return nil
}

func associationDiscoveryPGFixture(t *testing.T, publicationCount int) (*GitScans, *GitHubConnections, *associationCatalogPG, domain.Repo, []domain.HistoryEvent, *scanReader) {
	t.Helper()
	worker, original, repo := providerObservationPGFixture(t)
	ctx := systemTestContext()
	capture := collaborationSnapshot(t, original, repo.ID, "discovery capture")
	ordinary := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo.ID), BranchID: "feature", Branch: "feature", Kind: "position", Source: capture, Target: capture, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(100, 0).UTC()}
	if err := worker.core.RecordHistory(ctx, ordinary); err != nil {
		t.Fatal(err)
	}
	publications := []domain.HistoryEvent{}
	for index := 0; index < publicationCount; index++ {
		publication := prPublication(ordinary)
		publication.ID = fmt.Sprintf("%032x", index+20)
		if err := worker.core.RecordHistory(ctx, publication); err != nil {
			t.Fatal(err)
		}
		publications = append(publications, publication)
	}
	storage := &associationCatalogPG{PostgresStore: original}
	worker.core.meta = storage
	reader := &scanReader{headPages: map[int][]outbound.GitHead{1: {{Ref: "refs/heads/main", Commit: strings.Repeat("b", 40)}}}}
	scans, err := NewGitScans(worker.core, reader)
	if err != nil {
		t.Fatal(err)
	}
	scans.SetSourceAuthorizer(worker)
	return scans, worker, storage, repo, publications, reader
}

func TestPGGitAssociationDiscoveryResumesAndFindsLatePublication(t *testing.T) {
	scans, worker, storage, repo, publications, reader := associationDiscoveryPGFixture(t, 3)
	ctx := systemTestContext()
	storage.limit = 2
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := storage.GetGitAssociationDiscovery(ctx, repo.ID, repo.GitRemoteURL)
	if err != nil || first.Cursor == "" || first.Checkpoint != nil {
		t.Fatalf("partial baseline advertised complete: %+v %v", first, err)
	}
	late := publications[0]
	late.ID = strings.Repeat("9", 32)
	if err := worker.core.RecordHistory(ctx, late); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewGitScans(worker.core, reader)
	if err != nil {
		t.Fatal(err)
	}
	restarted.SetSourceAuthorizer(worker)
	var completed domain.GitAssociationDiscovery
	for attempt := 0; attempt < 20; attempt++ {
		if err := restarted.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		completed, err = storage.GetGitAssociationDiscovery(ctx, repo.ID, repo.GitRemoteURL)
		if err != nil {
			t.Fatal(err)
		}
		if completed.Checkpoint != nil {
			break
		}
	}
	if completed.Checkpoint == nil || completed.Checkpoint.Sequence != first.Through || completed.Cursor != "" {
		t.Fatalf("baseline changed during paging: %+v -> %+v", first, completed)
	}
	rows, err := storage.ListGitAssociationPublications(ctx, repo.ID, repo.GitRemoteURL, "", 101)
	if err != nil || !reflect.DeepEqual(rows, publications) {
		t.Fatalf("partial baseline consumed later data or lost originals: %+v %v", rows, err)
	}
	for attempt := 0; attempt < 20; attempt++ {
		if _, err := restarted.discoverGitAssociations(ctx, repo); err != nil {
			t.Fatal(err)
		}
		current, err := storage.GetGitAssociationDiscovery(ctx, repo.ID, repo.GitRemoteURL)
		if err != nil {
			t.Fatal(err)
		}
		if current.Cursor == "" && current.Checkpoint.Sequence > completed.Checkpoint.Sequence {
			break
		}
	}
	rows, err = storage.ListGitAssociationPublications(ctx, repo.ID, repo.GitRemoteURL, "", 101)
	if err != nil || !reflect.DeepEqual(rows, append(publications, late)) {
		t.Fatal("late publication missed by delta", rows, err)
	}
	before, err := storage.GetGitAssociationDiscovery(ctx, repo.ID, repo.GitRemoteURL)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if progress, err := restarted.discoverGitAssociations(ctx, repo); err != nil || progress {
		t.Fatal("unchanged catalog caused writes", progress, err)
	}
	after, err := storage.GetGitAssociationDiscovery(ctx, repo.ID, repo.GitRemoteURL)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("idle cursor changed", before, after, err)
	}
	nextRevision, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil || revision != nextRevision || len(reader.headReads) != 1 {
		t.Fatal("local catalog discovery invalidated queries or crawled provider", revision, nextRevision, reader.headReads, err)
	}
}

func TestPGGitAssociationDiscoveryAtomicHandoffAndPolicy(t *testing.T) {
	for _, scenario := range []string{"rollback", "revoked", "origin-changed", "legacy-authority"} {
		t.Run(scenario, func(t *testing.T) {
			scans, worker, storage, repo, _, _ := associationDiscoveryPGFixture(t, 2)
			ctx := systemTestContext()
			want := domain.ErrIntegrity
			switch scenario {
			case "rollback":
				storage.rejectSave = true
			case "legacy-authority":
				scans.SetSourceAuthorizer(nil)
				want = domain.ErrForbidden
			case "revoked":
				storage.duringRead = func(work context.Context) {
					if err := worker.id.withIdentity(work, func(write context.Context) error {
						connections, err := storage.ListGitHubConnections(write)
						if err != nil {
							return err
						}
						connection := connections[0]
						connection.Enabled, connection.Generation = false, connection.Generation+1
						return storage.PutGitHubConnection(write, connection)
					}); err != nil {
						t.Fatal(err)
					}
				}
				want = domain.ErrForbidden
			case "origin-changed":
				storage.duringRead = func(work context.Context) {
					storage.originOverride = repo.GitRemoteURL + "-changed"
				}
				want = domain.ErrConflict
			}
			if progress, err := scans.discoverGitAssociations(ctx, repo); !errors.Is(err, want) || progress {
				t.Fatal("failed handoff advanced", progress, err, want)
			}
			if _, err := storage.GetGitAssociationDiscovery(ctx, repo.ID, repo.GitRemoteURL); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("cursor escaped failed handoff", err)
			}
			rows, err := storage.ListGitAssociationPublications(ctx, repo.ID, repo.GitRemoteURL, "", 101)
			if err != nil || len(rows) != 0 {
				t.Fatal("publications escaped failed handoff", rows, err)
			}
		})
	}
}

func TestPGGitAssociationDiscoveryResetPreservesWork(t *testing.T) {
	scans, _, storage, repo, publications, _ := associationDiscoveryPGFixture(t, 2)
	ctx := systemTestContext()
	if progress, err := scans.discoverGitAssociations(ctx, repo); err != nil || !progress {
		t.Fatal(progress, err)
	}
	storage.resetRead = true
	if progress, err := scans.discoverGitAssociations(ctx, repo); err != nil || !progress {
		t.Fatal("expired checkpoint did not reset", progress, err)
	}
	reset, err := storage.GetGitAssociationDiscovery(ctx, repo.ID, repo.GitRemoteURL)
	if err != nil || reset.Checkpoint != nil || reset.Cursor != "" {
		t.Fatal("reset retained expired cursor", reset, err)
	}
	if progress, err := scans.discoverGitAssociations(ctx, repo); !errors.Is(err, domain.ErrCatalogResetRequired) || progress {
		t.Fatal("unsupported fresh baseline silently completed", progress, err)
	}
	storage.resetRead = false
	if progress, err := scans.discoverGitAssociations(ctx, repo); err != nil || !progress {
		t.Fatal("fresh baseline unavailable", progress, err)
	}
	rows, err := storage.ListGitAssociationPublications(ctx, repo.ID, repo.GitRemoteURL, "", 101)
	if err != nil || !reflect.DeepEqual(rows, publications) {
		t.Fatal("reset lost or duplicated queued work", rows, err)
	}
}

func TestPGGitAssociationDiscoveryFailureDoesNotBlockHeadReconcile(t *testing.T) {
	scans, _, storage, repo, _, reader := associationDiscoveryPGFixture(t, 1)
	storage.rejectSave = true
	if err := scans.Reconcile(systemTestContext()); err != nil {
		t.Fatal(err)
	}
	receipts, err := storage.ListGitHeadReceipts(systemTestContext(), repo.ID, repo.GitRemoteURL, 0, 10)
	if err != nil || len(receipts) != 1 || len(reader.headReads) != 1 {
		t.Fatal("discovery failure blocked existing ingestion", receipts, reader.headReads, err)
	}
}
