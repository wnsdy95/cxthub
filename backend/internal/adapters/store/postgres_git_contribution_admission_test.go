//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func gitAdmissionPGBaseline(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, origin string, now time.Time) domain.GitContributionAdmission {
	t.Helper()
	claim, err := storage.ClaimGitHeadScan(ctx, repo, origin, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	provider := &domain.GitProviderObservation{InstallationID: 789, RepositoryID: 91}
	source := domain.GitRefObservation{RepoID: repo, GitOrigin: origin, Source: "reconciliation", Ref: "refs/heads/feature", After: strings.Repeat("f", 40), Provider: provider}.WithID()
	destination := domain.GitRefObservation{RepoID: repo, GitOrigin: origin, Source: "reconciliation", Ref: "refs/heads/main", After: strings.Repeat("b", 40), Provider: provider}.WithID()
	if err := storage.FinishGitHeadScan(ctx, claim, []domain.GitRefObservation{destination, source}, false, now); err != nil {
		t.Fatal(err)
	}
	receipt, err := storage.GetGitHeadReceipt(ctx, repo, origin, claim.Version)
	if err != nil {
		t.Fatal(err)
	}
	candidate := domain.GitDestinationCandidate{RepoID: repo, GitOrigin: origin, InstallationID: provider.InstallationID, ProviderRepositoryID: provider.RepositoryID,
		ObservationID: destination.ID, ReadReceiptID: receipt.ID, ReadVersion: receipt.Version, DestinationBranchID: "main-identity", DestinationBranch: "main", DestinationHead: destination.After}.WithID()
	admission := domain.GitContributionAdmission{Destination: candidate, SourceBranchID: "feature-identity", SourceBranch: "feature",
		SourceAnchorID: strings.Repeat("4", 32), DestinationAnchorID: strings.Repeat("5", 32), PublicationID: strings.Repeat("3", 32),
		Capture: domain.HashContent([]byte("original capture")), OriginalHead: strings.Repeat("a", 40), SourceHead: source.After, SourceObservationID: source.ID,
		OrdinaryObservationIDs: []string{strings.Repeat("1", 32)}, CarryObservationIDs: []string{strings.Repeat("2", 32)}, AfterReadVersion: claim.Version}.WithID()
	if admission.Validate() != nil || !admission.MatchesBaseline(receipt) {
		t.Fatal("invalid admission fixture")
	}
	return admission
}

func gitAdmissionPGFixture(t *testing.T, storage *PostgresStore) (context.Context, domain.GitContributionAdmission, time.Time) {
	t.Helper()
	ctx, repo, now := headReceiptFixture(t, storage)
	return ctx, gitAdmissionPGBaseline(t, ctx, storage, repo.ID, repo.GitRemoteURL, now), now
}

func TestPGGitContributionAdmissionTransactionBoundary(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, admission, _ := gitAdmissionPGFixture(t, storage)
	repo, origin := admission.Destination.RepoID, admission.Destination.GitOrigin
	assertRejected := func(bound context.Context, writer *PostgresStore) {
		t.Helper()
		if _, err := writer.FenceGitHeadRead(bound, repo, origin); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("incompatible transaction fenced a read", err)
		}
		if err := writer.PutGitContributionAdmission(bound, admission); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("incompatible transaction wrote admission", err)
		}
	}
	assertRejected(ctx, storage)
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		assertRejected(bound, storage)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	foreignRepo := domain.HashContent([]byte(t.Name() + " foreign"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: foreignRepo}); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, foreignRepo, func(bound context.Context) error {
		assertRejected(bound, storage)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var escaped context.Context
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		escaped = bound
		assertRejected(bound, peer)
		if version, err := storage.FenceGitHeadRead(bound, repo, origin); err != nil || version != admission.AfterReadVersion {
			t.Fatal("owner could not fence read", version, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertRejected(escaped, storage)
	if rows, err := storage.ListGitContributionAdmissions(ctx, repo, "", "", 10); err != nil || rows == nil || len(rows) != 0 {
		t.Fatal("rejected transaction leaked admission", rows, err)
	}
}

func TestPGGitContributionAdmissionRollbackReplayAndRevision(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, canceled := range []bool{false, true} {
		name := "rollback"
		if canceled {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			ctx, admission, _ := gitAdmissionPGFixture(t, storage)
			repo := admission.Destination.RepoID
			before, err := storage.RepositoryRevision(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			abort := errors.New("abort admission")
			work, cancel := context.WithCancel(ctx)
			defer cancel()
			err = storage.WithinRepository(work, repo, func(bound context.Context) error {
				if err := storage.PutGitContributionAdmission(bound, admission); err != nil {
					return err
				}
				if got, err := storage.GetGitContributionAdmission(bound, repo, admission.ID); err != nil || !reflect.DeepEqual(got, admission) {
					t.Fatal("transaction cannot read own admission", got, err)
				}
				if rows, err := storage.ListGitContributionAdmissions(bound, repo, admission.Destination.GitOrigin, "", 10); err != nil || !reflect.DeepEqual(rows, []domain.GitContributionAdmission{admission}) {
					t.Fatal("transaction list escaped handle", rows, err)
				}
				if _, err := peer.GetGitContributionAdmission(ctx, repo, admission.ID); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("uncommitted admission visible", err)
				}
				if err := storage.AdvanceEvidenceRevision(bound, repo); err != nil {
					return err
				}
				if canceled {
					cancel()
					return work.Err()
				}
				return abort
			})
			want := abort
			if canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatal("outer abort lost", err)
			}
			if _, err := peer.GetGitContributionAdmission(ctx, repo, admission.ID); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("admission escaped rollback", err)
			}
			if after, err := peer.RepositoryRevision(ctx, repo); err != nil || after != before {
				t.Fatal("revision escaped rollback", after, err)
			}
			for range 2 {
				if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
					return storage.PutGitContributionAdmission(bound, admission)
				}); err != nil {
					t.Fatal("replay failed", err)
				}
			}
			if got, err := peer.GetGitContributionAdmission(ctx, repo, admission.ID); err != nil || !reflect.DeepEqual(got, admission) {
				t.Fatal("committed admission changed", got, err)
			}
			if rows, err := peer.ListGitContributionAdmissions(ctx, repo, "", "", 10); err != nil || len(rows) != 1 {
				t.Fatal("replay duplicated admission", rows, err)
			}
			if after, err := peer.RepositoryRevision(ctx, repo); err != nil || after != before {
				t.Fatal("adapter advanced revision", after, err)
			}
		})
	}
}

func TestPGGitContributionAdmissionWatermarkIncludesRunningClaim(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, admission, now := gitAdmissionPGFixture(t, storage)
	repo, origin := admission.Destination.RepoID, admission.Destination.GitOrigin
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		return storage.PutGitContributionAdmission(bound, admission)
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := storage.ClaimGitHeadScan(ctx, repo, origin, now.Add(6*time.Minute), time.Minute)
	if err != nil || claim.Version <= admission.AfterReadVersion || claim.State != "running" {
		t.Fatal(claim, err)
	}
	err = storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		version, err := storage.FenceGitHeadRead(bound, repo, origin)
		if err != nil || version != claim.Version {
			t.Fatal("running claim omitted from watermark", version, err)
		}
		if err := storage.PutGitContributionAdmission(bound, admission); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("idempotence bypassed stale watermark", err)
		}
		future := admission
		future.AfterReadVersion = version + 1
		future = future.WithID()
		if err := storage.PutGitContributionAdmission(bound, future); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("future watermark accepted", err)
		}
		current := admission
		current.PublicationID = strings.Repeat("6", 32)
		current.AfterReadVersion = version
		return storage.PutGitContributionAdmission(bound, current.WithID())
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := storage.ListGitContributionAdmissions(ctx, repo, origin, "", 10); err != nil || len(rows) != 2 {
		t.Fatal("incorrect fenced admission count", rows, err)
	}
}

func TestPGGitContributionAdmissionFenceBlocksClaimUntilCommit(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, admission, now := gitAdmissionPGFixture(t, storage)
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	repo, origin := admission.Destination.RepoID, admission.Destination.GitOrigin
	type result struct {
		claim domain.GitHeadScan
		err   error
	}
	finished := make(chan result, 1)
	err := storage.WithinRepository(bounded, repo, func(bound context.Context) error {
		if err := storage.PutGitContributionAdmission(bound, admission); err != nil {
			return err
		}
		go func() {
			claim, err := peer.ClaimGitHeadScan(bounded, repo, origin, now.Add(6*time.Minute), time.Minute)
			finished <- result{claim, err}
		}()
		cyclePGWaitBlocked(t, bounded, storage, storage.db(bound).(*repositoryTx).Conn().PgConn().PID())
		select {
		case got := <-finished:
			t.Fatal("claim completed before admission commit", got)
		default:
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-finished:
		if got.err != nil || got.claim.Version != admission.AfterReadVersion+1 {
			t.Fatal("claim did not resume after commit", got)
		}
	case <-bounded.Done():
		t.Fatal(bounded.Err())
	}
	if _, err := peer.GetGitContributionAdmission(ctx, repo, admission.ID); err != nil {
		t.Fatal("admission not visible after blocked claim resumed", err)
	}
}

func TestPGGitContributionAdmissionRequiresStoredMatchingBaseline(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	for _, scenario := range []string{"missing", "foreign", "receipt-id", "source-mismatch", "destination-mismatch", "invalid"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, admission, now := gitAdmissionPGFixture(t, storage)
			repo, origin := admission.Destination.RepoID, admission.Destination.GitOrigin
			want := domain.ErrIntegrity
			switch scenario {
			case "missing":
				if _, err := storage.pool.Exec(ctx, `DELETE FROM git_head_receipts WHERE repo_id=$1`, repo); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrNotFound
			case "foreign":
				foreign := domain.HashContent([]byte(t.Name() + " other repo"))
				if _, err := storage.PutRepo(ctx, domain.Repo{ID: foreign}); err != nil {
					t.Fatal(err)
				}
				other := gitAdmissionPGBaseline(t, ctx, storage, foreign, origin, now)
				admission.Destination.ReadReceiptID = other.Destination.ReadReceiptID
			case "receipt-id":
				admission.Destination.ReadReceiptID = strings.Repeat("a", 64)
			case "source-mismatch":
				admission.SourceHead = strings.Repeat("e", 40)
				provider := &domain.GitProviderObservation{InstallationID: admission.Destination.InstallationID, RepositoryID: admission.Destination.ProviderRepositoryID}
				observation := domain.GitRefObservation{RepoID: repo, GitOrigin: origin, Source: "reconciliation", Ref: "refs/heads/feature", After: admission.SourceHead, Provider: provider}.WithID()
				admission.SourceObservationID = observation.ID
			case "destination-mismatch":
				admission.Destination.DestinationHead = strings.Repeat("c", 40)
				provider := &domain.GitProviderObservation{InstallationID: admission.Destination.InstallationID, RepositoryID: admission.Destination.ProviderRepositoryID}
				observation := domain.GitRefObservation{RepoID: repo, GitOrigin: origin, Source: "reconciliation", Ref: "refs/heads/main", After: admission.Destination.DestinationHead, Provider: provider}.WithID()
				admission.Destination.ObservationID = observation.ID
			case "invalid":
				admission.Capture = "invalid"
				want = domain.ErrValidation
			}
			admission.Destination = admission.Destination.WithID()
			admission = admission.WithID()
			err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
				return storage.PutGitContributionAdmission(bound, admission)
			})
			if !errors.Is(err, want) {
				t.Fatal("unproven baseline accepted", err, want)
			}
			if rows, err := storage.ListGitContributionAdmissions(ctx, repo, "", "", 10); err != nil || len(rows) != 0 {
				t.Fatal("invalid baseline admission persisted", rows, err)
			}
		})
	}
}

func TestPGGitContributionAdmissionScopedPagination(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, admission, now := gitAdmissionPGFixture(t, storage)
	repo, origin := admission.Destination.RepoID, admission.Destination.GitOrigin
	other := gitAdmissionPGBaseline(t, ctx, storage, repo, origin+"-other", now)
	var want []string
	for index := range 4 {
		next := admission
		if index == 3 {
			next = other
		}
		next.GraphRevision = uint64(index)
		next.PublicationID = fmt.Sprintf("%032x", index+10)
		next = next.WithID()
		if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
			return storage.PutGitContributionAdmission(bound, next)
		}); err != nil {
			t.Fatal(err)
		}
		want = append(want, next.ID)
	}
	slices.Sort(want)
	var got []string
	for after := ""; ; {
		rows, err := peer.ListGitContributionAdmissions(ctx, repo, "", after, 1)
		if err != nil || rows == nil || len(rows) > 1 {
			t.Fatal(rows, err)
		}
		if len(rows) == 0 {
			break
		}
		if rows[0].ID <= after {
			t.Fatal("cursor did not advance")
		}
		got = append(got, rows[0].ID)
		after = rows[0].ID
	}
	if !slices.Equal(got, want) {
		t.Fatal("bounded cursor lost admissions", got, want)
	}
	rows, err := peer.ListGitContributionAdmissions(ctx, repo, origin, "", 101)
	if err != nil || len(rows) != 3 {
		t.Fatal("origin filter leaked rows", rows, err)
	}
	for _, row := range rows {
		if row.Destination.GitOrigin != origin {
			t.Fatal("origin scope mismatch", row)
		}
	}
	foreign := domain.HashContent([]byte(t.Name() + " foreign"))
	if rows, err := peer.ListGitContributionAdmissions(ctx, foreign, "", "", 10); err != nil || rows == nil || len(rows) != 0 {
		t.Fatal("repository scope leaked", rows, err)
	}
	if _, err := peer.GetGitContributionAdmission(ctx, foreign, want[0]); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("cross-repository get succeeded", err)
	}
}

func TestPGGitContributionAdmissionRejectsPolicyTamperingAndCancellation(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, admission, _ := gitAdmissionPGFixture(t, storage)
	repo, origin := admission.Destination.RepoID, admission.Destination.GitOrigin
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		return storage.PutGitContributionAdmission(bound, admission)
	}); err != nil {
		t.Fatal(err)
	}
	tampered := admission
	tampered.Capture = domain.HashContent([]byte("altered capture"))
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		return storage.PutGitContributionAdmission(bound, tampered)
	}); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("altered immutable record accepted", err)
	}
	raw, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE git_contribution_admissions SET payload=$1 WHERE repo_id=$2 AND id=$3`, raw, repo, admission.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetGitContributionAdmission(ctx, repo, admission.ID); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("tampered admission read", err)
	}
	if _, err := storage.ListGitContributionAdmissions(ctx, repo, origin, "", 10); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("tampered admission listed", err)
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		return storage.PutGitContributionAdmission(bound, admission)
	}); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("replay overwrote corrupt collision", err)
	}
	if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		return storage.PutGitContributionAdmission(bound, admission)
	}); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("policy change bypassed", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, operation := range []func() error{
		func() error { _, err := storage.FenceGitHeadRead(canceled, repo, origin); return err },
		func() error { return storage.PutGitContributionAdmission(canceled, admission) },
		func() error { _, err := storage.GetGitContributionAdmission(canceled, repo, admission.ID); return err },
		func() error {
			_, err := storage.FindGitContributionAdmission(canceled, repo, admission.Key())
			return err
		},
		func() error {
			_, err := storage.ListGitContributionAdmissions(canceled, repo, origin, "", 10)
			return err
		},
	} {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation ignored", err)
		}
	}
}

func TestPGGitContributionAdmissionSemanticKeyKeepsFirstOccurrence(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, admission, now := gitAdmissionPGFixture(t, storage)
	repo, origin := admission.Destination.RepoID, admission.Destination.GitOrigin
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		return storage.PutGitContributionAdmission(bound, admission)
	}); err != nil {
		t.Fatal(err)
	}
	newer := gitAdmissionPGBaseline(t, ctx, storage, repo, origin, now.Add(6*time.Minute))
	newer.GraphRevision, newer.EvidenceRevision = admission.GraphRevision+1, admission.EvidenceRevision+1
	newer = newer.WithID()
	if newer.Key() != admission.Key() || newer.ID == admission.ID || newer.AfterReadVersion <= admission.AfterReadVersion || newer.Destination.ReadReceiptID == admission.Destination.ReadReceiptID {
		t.Fatal("invalid refreshed occurrence fixture")
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if err := storage.PutGitContributionAdmission(bound, newer); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("semantic replay refreshed admission", err)
		}
		original, err := storage.FindGitContributionAdmission(bound, repo, newer.Key())
		if err != nil || !reflect.DeepEqual(original, admission) {
			t.Fatal("find did not preserve frozen occurrence", original, err)
		}
		return nil
	}); err != nil {
		t.Fatal("semantic collision damaged outer transaction", err)
	}
	if err := peer.WithinReadSnapshot(ctx, func(bound context.Context) error {
		original, err := peer.FindGitContributionAdmission(bound, repo, newer.Key())
		if err != nil || !reflect.DeepEqual(original, admission) {
			t.Fatal("snapshot find replaced receipt or watermark", original, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.GetGitContributionAdmission(ctx, repo, newer.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("new occurrence leaked after semantic conflict", err)
	}
	if rows, err := peer.ListGitContributionAdmissions(ctx, repo, origin, "", 10); err != nil || !reflect.DeepEqual(rows, []domain.GitContributionAdmission{admission}) {
		t.Fatal("first admission was not retained exclusively", rows, err)
	}
	if _, err := peer.FindGitContributionAdmission(ctx, domain.HashContent([]byte("foreign")), admission.Key()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("semantic key lookup crossed repositories", err)
	}
}

func TestPGGitContributionAdmissionConcurrentSemanticWriters(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, admission, _ := gitAdmissionPGFixture(t, storage)
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for index, writer := range []*PostgresStore{storage, peer} {
		go func() {
			next := admission
			next.EvidenceRevision = uint64(index)
			next = next.WithID()
			results <- writer.WithinRepository(bounded, admission.Destination.RepoID, func(bound context.Context) error {
				return writer.PutGitContributionAdmission(bound, next)
			})
		}()
	}
	accepted := 0
	for range 2 {
		select {
		case err := <-results:
			if err == nil {
				accepted++
			} else if !errors.Is(err, domain.ErrConflict) {
				t.Fatal(err)
			}
		case <-bounded.Done():
			t.Fatal(bounded.Err())
		}
	}
	if accepted != 1 {
		t.Fatal("parallel writers both changed semantic admission", accepted)
	}
	if rows, err := storage.ListGitContributionAdmissions(ctx, admission.Destination.RepoID, "", "", 10); err != nil || len(rows) != 1 || rows[0].Key() != admission.Key() {
		t.Fatal("parallel semantic admission count", rows, err)
	}
}

func TestPGGitContributionAdmissionFenceRejectsMissingOrCorruptScope(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	for _, scenario := range []string{"missing", "repository", "origin", "version", "payload"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, repo, now := headReceiptFixture(t, storage)
			want := domain.ErrNotFound
			if scenario != "missing" {
				claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				want = domain.ErrIntegrity
				switch scenario {
				case "repository":
					claim.RepoID = domain.HashContent([]byte("other repository"))
				case "origin":
					claim.GitOrigin += "-other"
				case "version":
					claim.Version = -1
				}
				raw, err := json.Marshal(claim)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "payload" {
					raw = []byte(`{"state":17}`)
				}
				if _, err := storage.pool.Exec(ctx, `UPDATE git_head_scans SET payload=$1 WHERE repo_id=$2 AND origin=$3`, raw, repo.ID, repo.GitRemoteURL); err != nil {
					t.Fatal(err)
				}
			}
			if err := storage.WithinRepository(ctx, repo.ID, func(bound context.Context) error {
				_, err := storage.FenceGitHeadRead(bound, repo.ID, repo.GitRemoteURL)
				return err
			}); !errors.Is(err, want) {
				t.Fatal("invalid checkpoint fenced", err, want)
			}
		})
	}
}

func TestPGGitContributionAdmissionValidatesQueriesAndStoredColumns(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, admission, _ := gitAdmissionPGFixture(t, storage)
	repo, origin := admission.Destination.RepoID, admission.Destination.GitOrigin
	for _, query := range []struct {
		repo          domain.ContentHash
		origin, after string
		limit         int
	}{
		{"invalid", origin, "", 1},
		{repo, " ", "", 1},
		{repo, origin, "invalid", 1},
		{repo, origin, strings.Repeat("A", 64), 1},
		{repo, origin, "", 0},
		{repo, origin, "", 102},
	} {
		if _, err := storage.ListGitContributionAdmissions(ctx, query.repo, query.origin, query.after, query.limit); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid admission query accepted", query, err)
		}
	}
	for _, lookup := range []struct {
		repo domain.ContentHash
		id   string
	}{{"invalid", admission.ID}, {repo, ""}, {repo, strings.Repeat("A", 64)}} {
		if _, err := storage.GetGitContributionAdmission(ctx, lookup.repo, lookup.id); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid get accepted", lookup, err)
		}
		if _, err := storage.FindGitContributionAdmission(ctx, lookup.repo, lookup.id); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid find accepted", lookup, err)
		}
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		return storage.PutGitContributionAdmission(bound, admission)
	}); err != nil {
		t.Fatal(err)
	}
	wrongKey := strings.Repeat("a", 64)
	if _, err := storage.pool.Exec(ctx, `UPDATE git_contribution_admissions SET admission_key=$1 WHERE repo_id=$2 AND id=$3`, wrongKey, repo, admission.ID); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []func() error{
		func() error { _, err := storage.GetGitContributionAdmission(ctx, repo, admission.ID); return err },
		func() error { _, err := storage.FindGitContributionAdmission(ctx, repo, wrongKey); return err },
		func() error { _, err := storage.ListGitContributionAdmissions(ctx, repo, origin, "", 10); return err },
	} {
		if err := operation(); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal("semantic key column corruption hidden", err)
		}
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE git_contribution_admissions SET admission_key=$1,origin=$2 WHERE repo_id=$3 AND id=$4`, admission.Key(), origin+"-wrong", repo, admission.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.FindGitContributionAdmission(ctx, repo, admission.Key()); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("origin column corruption hidden", err)
	}
}
