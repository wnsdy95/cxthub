package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type headClaimIsolationStore struct {
	*store.FSStore
	repos       []domain.Repo
	claims      []domain.ContentHash
	failures    map[domain.ContentHash]error
	beforeClaim func(domain.ContentHash)
}

func (storage *headClaimIsolationStore) ListRepos(context.Context, string) ([]domain.Repo, error) {
	return append([]domain.Repo{}, storage.repos...), nil
}

func (storage *headClaimIsolationStore) ClaimGitHeadScan(ctx context.Context, repo domain.ContentHash, origin string, now time.Time, lease time.Duration) (domain.GitHeadScan, error) {
	storage.claims = append(storage.claims, repo)
	if storage.beforeClaim != nil {
		storage.beforeClaim(repo)
	}
	if err := storage.failures[repo]; err != nil {
		return domain.GitHeadScan{}, err
	}
	return storage.FSStore.ClaimGitHeadScan(ctx, repo, origin, now, lease)
}

func gitScansIsolationFixture(t *testing.T, names ...string) (*GitScans, *headClaimIsolationStore, *scanReader) {
	t.Helper()
	core, original := newFsckSvc(t)
	storage := &headClaimIsolationStore{FSStore: original, failures: map[domain.ContentHash]error{}}
	for _, name := range names {
		repo := domain.Repo{ID: hh(t.Name() + "/" + name), GitRemoteURL: "https://github.com/example/" + name}
		if _, err := original.PutRepo(systemTestContext(), repo); err != nil {
			t.Fatal(err)
		}
		storage.repos = append(storage.repos, repo)
	}
	core.meta = storage
	reader := &scanReader{headPages: map[int][]outbound.GitHead{1: {{Ref: "refs/heads/main", Commit: strings.Repeat("a", 40)}}}}
	scans, err := NewGitScans(core, reader)
	if err != nil {
		t.Fatal(err)
	}
	return scans, storage, reader
}

func TestGitHeadReconciliationIsolatesClaimFailures(t *testing.T) {
	for _, multiple := range []bool{false, true} {
		name := "bad-first-good-second"
		if multiple {
			name = "multiple-claim-errors"
		}
		t.Run(name, func(t *testing.T) {
			names := []string{"bad-first", "good-second"}
			if multiple {
				names = append(names, "bad-third")
			}
			scans, storage, reader := gitScansIsolationFixture(t, names...)
			firstFailure := errors.New("first repository claim failed")
			storage.failures[storage.repos[0].ID] = firstFailure
			var lastFailure error
			if multiple {
				lastFailure = errors.New("last repository claim failed")
				storage.failures[storage.repos[2].ID] = lastFailure
			}
			ctx := systemTestContext()
			err := scans.Reconcile(ctx)
			if !errors.Is(err, firstFailure) || (multiple && !errors.Is(err, lastFailure)) {
				t.Fatal("claim failures were not returned together", err)
			}
			expectedClaims := []domain.ContentHash{}
			for _, repo := range storage.repos {
				expectedClaims = append(expectedClaims, repo.ID)
			}
			if !reflect.DeepEqual(storage.claims, expectedClaims) || !reflect.DeepEqual(reader.headReads, []int{1}) {
				t.Fatal("failed claim starved a later repository", storage.claims, reader.headReads)
			}
			good := storage.repos[1]
			checkpoint, err := storage.GetGitHeadScan(ctx, good.ID, good.GitRemoteURL)
			if err != nil || checkpoint.State != "completed" {
				t.Fatal("healthy repository did not complete head reconciliation", checkpoint, err)
			}
			observation := domain.GitRefObservation{RepoID: good.ID, GitOrigin: good.GitRemoteURL, Source: "reconciliation", Ref: "refs/heads/main", After: strings.Repeat("a", 40)}.WithID()
			stored, err := storage.GetPRRefObservation(ctx, good.ID, good.GitRemoteURL, observation.ID)
			if err != nil || !reflect.DeepEqual(stored, observation) {
				t.Fatal("healthy repository did not retain its head observation", stored, err)
			}
			jobs, err := storage.ListGitScans(ctx, good.ID, "", 10)
			if err != nil || len(jobs) != 1 || jobs[0].Commit != observation.After {
				t.Fatal("healthy repository did not enqueue observed head", jobs, err)
			}
			for _, repo := range storage.repos {
				if storage.failures[repo.ID] == nil {
					continue
				}
				if _, err := storage.GetGitHeadScan(ctx, repo.ID, repo.GitRemoteURL); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("failed claim created a checkpoint", repo.ID, err)
				}
			}
		})
	}
}

func TestGitHeadReconciliationClaimSkipsRemainNonfatal(t *testing.T) {
	for _, skipped := range []error{domain.ErrNotFound, domain.ErrDocumentIdentityUpgradeRequired} {
		t.Run(skipped.Error(), func(t *testing.T) {
			scans, storage, reader := gitScansIsolationFixture(t, "skipped-first", "good-second")
			storage.failures[storage.repos[0].ID] = skipped
			if err := scans.Reconcile(systemTestContext()); err != nil {
				t.Fatal("normal claim skip became a reconciler failure", err)
			}
			if !reflect.DeepEqual(storage.claims, []domain.ContentHash{storage.repos[0].ID, storage.repos[1].ID}) || !reflect.DeepEqual(reader.headReads, []int{1}) {
				t.Fatal("skipped claim prevented later reconciliation", storage.claims, reader.headReads)
			}
		})
	}
}

func TestGitHeadReconciliationClaimCancellation(t *testing.T) {
	claimFailure := errors.New("repository claim failed during cancellation")
	for _, scenario := range []struct {
		name           string
		laterRepo      bool
		beforeRun      bool
		claimError     error
		wantClaimError bool
	}{
		{name: "already-canceled", beforeRun: true},
		{name: "stops-before-later-repository", laterRepo: true, claimError: claimFailure, wantClaimError: true},
		{name: "final-claim-error", claimError: claimFailure, wantClaimError: true},
		{name: "final-not-found", claimError: domain.ErrNotFound},
		{name: "final-policy-skip", claimError: domain.ErrDocumentIdentityUpgradeRequired},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			names := []string{"canceled-first"}
			if scenario.laterRepo {
				names = append(names, "good-second")
			}
			scans, storage, reader := gitScansIsolationFixture(t, names...)
			ctx, cancel := context.WithCancel(systemTestContext())
			defer cancel()
			storage.failures[storage.repos[0].ID] = scenario.claimError
			storage.beforeClaim = func(domain.ContentHash) { cancel() }
			if scenario.beforeRun {
				cancel()
			}
			err := scans.Reconcile(ctx)
			if !errors.Is(err, context.Canceled) || (scenario.wantClaimError && !errors.Is(err, claimFailure)) {
				t.Fatal("cancellation or accumulated claim failure was lost", err)
			}
			expectedClaims := 1
			if scenario.beforeRun {
				expectedClaims = 0
			}
			if len(storage.claims) != expectedClaims || len(reader.headReads) != 0 {
				t.Fatal("reconciliation continued after cancellation", storage.claims, reader.headReads)
			}
		})
	}
}
