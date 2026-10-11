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

type contributionWorkerPGStore struct {
	*gitContributionApplyPGStore
	clock        time.Time
	rejectFinish bool
}

func (storage *contributionWorkerPGStore) ClaimGitContributionWork(ctx context.Context, repo domain.ContentHash, _ time.Time, lease time.Duration) (domain.GitContributionWork, error) {
	storage.clock = storage.clock.Add(10 * time.Minute)
	return storage.PostgresStore.ClaimGitContributionWork(ctx, repo, storage.clock, lease)
}

func (storage *contributionWorkerPGStore) FinishGitContributionWork(ctx context.Context, work domain.GitContributionWork) error {
	if err := storage.PostgresStore.FinishGitContributionWork(ctx, work); err != nil {
		return err
	}
	if storage.rejectFinish {
		return domain.ErrIntegrity
	}
	return nil
}

func automaticContributionFixture(t *testing.T) (*gitContributionApplyPGFixture, *contributionWorkerPGStore) {
	t.Helper()
	fixture := newGitContributionApplyPGFixture(t, "empty")
	storage := &contributionWorkerPGStore{gitContributionApplyPGStore: fixture.storage, clock: time.Now().UTC()}
	fixture.scans.core.meta = storage
	return fixture, storage
}

func TestPGGitContributionWorkerAutomaticallyCompletesAndReplays(t *testing.T) {
	fixture, _ := automaticContributionFixture(t)
	fixture.observeIntegration(t, "main", true)
	before := fixture.state(t, systemTestContext())
	prepared, _ := fixture.prepare(t)
	if err := fixture.scans.ProcessContributions(systemTestContext(), 1); err != nil {
		t.Fatal(err)
	}
	after := fixture.state(t, systemTestContext())
	assertGitContributionAppliedOnce(t, before, after, prepared)
	if err := fixture.scans.ProcessContributions(systemTestContext(), 1); err != nil {
		t.Fatal(err)
	}
	if replay := fixture.state(t, systemTestContext()); !reflect.DeepEqual(after, replay) {
		t.Fatal("completed work applied twice")
	}
}

func TestPGGitContributionWorkerRevisitsDelayedEvidenceAfterReset(t *testing.T) {
	fixture, _ := automaticContributionFixture(t)
	fixture.observeIntegration(t, "main", false)
	if err := fixture.scans.ProcessContributions(systemTestContext(), 1); err != nil {
		t.Fatal(err)
	}
	if got := fixture.state(t, systemTestContext()); len(got.proofs) != 0 {
		t.Fatal("missing code proof was accepted")
	}
	fixture.reader.headPages[1] = []outbound.GitHead{{Ref: "refs/heads/main", Commit: strings.Repeat("b", 40)}}
	if err := fixture.scans.Reconcile(systemTestContext()); err != nil {
		t.Fatal(err)
	}
	fixture.indexIntegration()
	before := fixture.state(t, systemTestContext())
	prepared, _ := fixture.prepare(t)
	for range 4 {
		if err := fixture.scans.ProcessContributions(systemTestContext(), 1); err != nil {
			t.Fatal(err)
		}
	}
	assertGitContributionAppliedOnce(t, before, fixture.state(t, systemTestContext()), prepared)
}

func TestPGGitContributionWorkerCompletionAndEffectRollbackTogether(t *testing.T) {
	fixture, storage := automaticContributionFixture(t)
	fixture.observeIntegration(t, "main", true)
	before := fixture.state(t, systemTestContext())
	prepared, _ := fixture.prepare(t)
	storage.rejectFinish = true
	if err := fixture.scans.ProcessContributions(systemTestContext(), 1); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("completion failure not propagated", err)
	}
	if after := fixture.state(t, systemTestContext()); !reflect.DeepEqual(before, after) {
		t.Fatal("effect survived failed completion transaction")
	}
	storage.rejectFinish = false
	if err := fixture.scans.ProcessContributions(systemTestContext(), 1); err != nil {
		t.Fatal(err)
	}
	assertGitContributionAppliedOnce(t, before, fixture.state(t, systemTestContext()), prepared)
}
