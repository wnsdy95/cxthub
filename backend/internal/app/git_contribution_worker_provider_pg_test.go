//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type contributionProviderAuthorizer struct {
	*GitHubConnections
	provider domain.GitProviderObservation
	fenceErr error
}

func (authorizer *contributionProviderAuthorizer) AuthorizeGitObservation(ctx context.Context, repo domain.ContentHash) (*domain.GitProviderObservation, GitReadFence, error) {
	qualified, fence, err := authorizer.GitHubConnections.AuthorizeGitObservation(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	if qualified == nil || fence == nil || authorizer.provider.Validate() != nil {
		return nil, nil, domain.ErrIntegrity
	}
	identity := authorizer.provider
	return &identity, func(write context.Context) error {
		if err := fence(write); err != nil {
			return err
		}
		if authorizer.provider != identity {
			return domain.ErrConflict
		}
		return authorizer.fenceErr
	}, nil
}

type contributionProviderWorkStore struct {
	*contributionWorkerPGStore
	claims   []domain.GitContributionWork
	finished []domain.GitContributionWork
}

func (storage *contributionProviderWorkStore) ClaimGitContributionWork(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.GitContributionWork, error) {
	claim, err := storage.contributionWorkerPGStore.ClaimGitContributionWork(ctx, repo, now, lease)
	if err == nil {
		storage.claims = append(storage.claims, claim)
	}
	return claim, err
}

func (storage *contributionProviderWorkStore) FinishGitContributionWork(ctx context.Context, work domain.GitContributionWork) error {
	if err := storage.contributionWorkerPGStore.FinishGitContributionWork(ctx, work); err != nil {
		return err
	}
	storage.finished = append(storage.finished, work)
	return nil
}

func contributionProviderSweepFixture(t *testing.T, changed string) (*gitContributionApplyPGFixture, *contributionProviderWorkStore, domain.GitContextHead) {
	t.Helper()
	fixture, original := automaticContributionFixture(t)
	storage := &contributionProviderWorkStore{contributionWorkerPGStore: original}
	fixture.scans.core.meta = storage
	provider := domain.GitProviderObservation{InstallationID: fixture.admission.Destination.InstallationID, RepositoryID: fixture.admission.Destination.ProviderRepositoryID}
	if changed == "installation" {
		provider.InstallationID++
	} else {
		provider.RepositoryID++
	}
	authorizer := &contributionProviderAuthorizer{GitHubConnections: fixture.basis.worker, provider: provider}
	fixture.scans.SetSourceAuthorizer(authorizer)
	fixture.indexIntegration()
	fixture.reader.headPages[1] = []outbound.GitHead{{Ref: "refs/heads/main", Commit: strings.Repeat("d", 40)}}
	ctx := systemTestContext()
	if err := fixture.scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	fixture.scans.SetSourceAuthorizer(fixture.basis.worker)
	if err := fixture.scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	heads, err := storage.ListGitContextHeads(ctx, fixture.basis.repo.ID, fixture.basis.repo.GitRemoteURL,
		fixture.admission.Destination.DestinationBranchID, fixture.admission.AfterReadVersion, 101)
	if err != nil || len(heads) != 2 {
		t.Fatalf("real reconciler did not persist both bound occurrences: %+v %v", heads, err)
	}
	for index, head := range heads {
		receipt, err := storage.GetGitHeadReceipt(ctx, fixture.basis.repo.ID, fixture.basis.repo.GitRemoteURL, head.Destination.ReadVersion)
		if err != nil || head.Validate() != nil || !head.Destination.MatchesReceipt(receipt) {
			t.Fatalf("bound occurrence lost qualified receipt: %+v %+v %v", head, receipt, err)
		}
		want := provider
		if index == 1 {
			want = domain.GitProviderObservation{InstallationID: fixture.admission.Destination.InstallationID, RepositoryID: fixture.admission.Destination.ProviderRepositoryID}
		}
		if receipt.Provider != want || head.Destination.DestinationHead != strings.Repeat("d", 40) {
			t.Fatalf("fixture relabeled provider or code: %+v %+v", head, receipt)
		}
	}
	if heads[0].Destination.ReadVersion >= heads[1].Destination.ReadVersion {
		t.Fatal("foreign head must precede restored compatible head")
	}
	fixture.head = heads[1]
	return fixture, storage, heads[0]
}

func assertContributionProviderSkipThenApply(t *testing.T, fixture *gitContributionApplyPGFixture, storage *contributionProviderWorkStore, foreign domain.GitContextHead) {
	t.Helper()
	ctx := systemTestContext()
	before := fixture.state(t, ctx)
	prepared, _ := fixture.prepare(t)
	claimCount, finishCount := len(storage.claims), len(storage.finished)
	if err := fixture.scans.ProcessContributions(ctx, 1); err != nil {
		t.Fatalf("foreign bound head blocked restored provider: %v", err)
	}
	if len(storage.claims) != claimCount+1 || len(storage.finished) != finishCount+1 {
		t.Fatalf("limit one did not finish exactly one candidate: claims=%+v finished=%+v", storage.claims, storage.finished)
	}
	skipped := storage.finished[finishCount]
	if skipped.State != "waiting" || skipped.Reason != "provider_scope_changed" || skipped.ReadCursor != foreign.Destination.ReadVersion || skipped.ReadThrough != fixture.head.Destination.ReadVersion || !skipped.LeaseUntil.IsZero() {
		t.Fatalf("foreign head did not advance only its finite-sweep cursor: %+v", skipped)
	}
	if after := fixture.state(t, ctx); !reflect.DeepEqual(before, after) {
		t.Fatalf("foreign head changed contribution, pin, proof, revision or refs: before=%+v after=%+v", before, after)
	}
	if err := fixture.scans.ProcessContributions(ctx, 1); err != nil {
		t.Fatalf("later compatible head was starved: %v", err)
	}
	if len(storage.claims) != claimCount+2 || len(storage.finished) != finishCount+2 {
		t.Fatal("compatible head did not consume exactly one claim")
	}
	nextClaim, completed := storage.claims[claimCount+1], storage.finished[finishCount+1]
	if nextClaim.ReadCursor != skipped.ReadCursor || nextClaim.ReadThrough != skipped.ReadThrough || nextClaim.Version <= skipped.Version || completed.State != "completed" || completed.ReadCursor != fixture.head.Destination.ReadVersion {
		t.Fatalf("persisted sweep did not resume at compatible head: skipped=%+v next=%+v completed=%+v", skipped, nextClaim, completed)
	}
	after := fixture.state(t, ctx)
	assertGitContributionAppliedOnce(t, before, after, prepared)
	if len(after.proofs) != 1 || after.proofs[0].ContextHeadID != fixture.head.ID || after.proofs[0].ContextHeadID == foreign.ID {
		t.Fatal("proof borrowed the foreign provider occurrence", after.proofs)
	}
	if err := fixture.scans.ProcessContributions(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if len(storage.claims) != claimCount+2 || len(storage.finished) != finishCount+2 || !reflect.DeepEqual(fixture.state(t, ctx), after) {
		t.Fatal("completed replay repeated an effect or claimed terminal work")
	}
}

func TestPGGitContributionWorkerSkipsForeignProviderHead(t *testing.T) {
	for _, changed := range []string{"installation", "repository"} {
		t.Run(changed, func(t *testing.T) {
			fixture, storage, foreign := contributionProviderSweepFixture(t, changed)
			assertContributionProviderSkipThenApply(t, fixture, storage, foreign)
		})
	}
}

func TestPGGitContributionWorkerCurrentAuthorizationRetainsCursor(t *testing.T) {
	for _, failure := range []string{"provider-mismatch", "commit-fence"} {
		t.Run(failure, func(t *testing.T) {
			fixture, storage, foreign := contributionProviderSweepFixture(t, "installation")
			provider := domain.GitProviderObservation{InstallationID: fixture.admission.Destination.InstallationID, RepositoryID: fixture.admission.Destination.ProviderRepositoryID}
			authorizer := &contributionProviderAuthorizer{GitHubConnections: fixture.basis.worker, provider: provider}
			if failure == "provider-mismatch" {
				authorizer.provider.InstallationID++
			} else {
				authorizer.fenceErr = domain.ErrConflict
			}
			fixture.scans.SetSourceAuthorizer(authorizer)
			ctx := systemTestContext()
			before := fixture.state(t, ctx)
			started := time.Now()
			if err := fixture.scans.ProcessContributions(ctx, 1); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("current authorization conflict not enforced: %v", err)
			}
			if len(storage.claims) != 1 || len(storage.finished) != 1 {
				t.Fatalf("conflict did not settle its single claim: %+v %+v", storage.claims, storage.finished)
			}
			claim, settled := storage.claims[0], storage.finished[0]
			if settled.State != "retrying" || settled.Reason != "authorization_or_state_changed" || settled.ReadCursor != claim.ReadCursor || settled.ReadCursor != fixture.admission.AfterReadVersion || settled.ReadThrough != claim.ReadThrough || !settled.NextAttempt.After(started) {
				t.Fatalf("current authorization failure consumed a bound head: claim=%+v settled=%+v", claim, settled)
			}
			if after := fixture.state(t, ctx); !reflect.DeepEqual(after, before) {
				t.Fatal("authorization failure wrote application effects", after)
			}
			fixture.scans.SetSourceAuthorizer(fixture.basis.worker)
			assertContributionProviderSkipThenApply(t, fixture, storage, foreign)
		})
	}
}
