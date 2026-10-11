//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func gitAdmissionMatchPGFixture(t *testing.T, storage *PostgresStore, more bool) (context.Context, domain.GitAdmissionMatch, domain.GitHeadReceipt, time.Time) {
	t.Helper()
	ctx, repo, now := headReceiptFixture(t, storage)
	claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.FinishGitHeadScan(ctx, claim, []domain.GitRefObservation{headReceiptObservation(repo, "a")}, more, now); err != nil {
		t.Fatal(err)
	}
	receipt, err := storage.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, claim.Version)
	if err != nil {
		t.Fatal(err)
	}
	next := domain.GitAdmissionMatch{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL, Provider: receipt.Provider, Page: receipt.Page, Version: 1,
		ThroughPublicationID: strings.Repeat("3", 32), AfterPublicationID: strings.Repeat("1", 32), AfterDestination: "main"}
	return ctx, next, receipt, now
}

func saveGitAdmissionMatchPG(ctx context.Context, storage *PostgresStore, next domain.GitAdmissionMatch, expected int64, receipt domain.GitHeadReceipt) error {
	return storage.WithinRepository(ctx, next.RepoID, func(bound context.Context) error {
		return storage.SaveGitAdmissionMatch(bound, next, expected, receipt)
	})
}

func TestPGGitAdmissionMatchCASAndRevision(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, more := range []bool{false, true} {
		t.Run(fmt.Sprint(more), func(t *testing.T) {
			ctx, next, receipt, _ := gitAdmissionMatchPGFixture(t, storage, more)
			if _, err := storage.GetGitAdmissionMatch(ctx, next.RepoID, next.GitOrigin, next.Provider, next.Page); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("missing match invented", err)
			}
			before, err := storage.RepositoryRevision(ctx, next.RepoID)
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range []int64{-1, 1, math.MaxInt64} {
				if err := saveGitAdmissionMatchPG(ctx, storage, next, expected, receipt); !errors.Is(err, domain.ErrConflict) {
					t.Fatal("invalid expected generation accepted", expected, err)
				}
			}
			missing := next
			missing.Version = 2
			if err := saveGitAdmissionMatchPG(ctx, storage, missing, 1, receipt); !errors.Is(err, domain.ErrConflict) {
				t.Fatal("missing match accepted nonzero expected version", err)
			}
			if err := saveGitAdmissionMatchPG(ctx, storage, next, 0, receipt); err != nil {
				t.Fatal(err)
			}
			for _, destination := range []string{"main", "different"} {
				replay := next
				replay.AfterDestination = destination
				if err := saveGitAdmissionMatchPG(ctx, storage, replay, 0, receipt); !errors.Is(err, domain.ErrConflict) {
					t.Fatal("lost response bypassed caller CAS", err)
				}
			}
			next.Version++
			next.AfterPublicationID, next.AfterDestination = next.ThroughPublicationID, ""
			if err := saveGitAdmissionMatchPG(ctx, storage, next, 1, receipt); err != nil {
				t.Fatal(err)
			}
			if got, err := peer.GetGitAdmissionMatch(ctx, next.RepoID, next.GitOrigin, next.Provider, next.Page); err != nil || got != next {
				t.Fatal("committed match changed", got, err)
			}
			next.Version++
			next.ThroughPublicationID, next.AfterPublicationID = "", ""
			if err := saveGitAdmissionMatchPG(ctx, storage, next, 2, receipt); err != nil {
				t.Fatal("empty range could not reset", err)
			}
			if got, err := peer.GetGitAdmissionMatch(ctx, next.RepoID, next.GitOrigin, next.Provider, next.Page); err != nil || got != next {
				t.Fatal("empty range reset lost", got, err)
			}
			if after, err := peer.RepositoryRevision(ctx, next.RepoID); err != nil || after != before {
				t.Fatal("match bookkeeping changed public revisions", before, after, err)
			}
		})
	}
}

func TestPGGitAdmissionMatchConcurrentCAS(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			ctx, next, receipt, _ := gitAdmissionMatchPGFixture(t, storage, false)
			expected := int64(0)
			if existing {
				if err := saveGitAdmissionMatchPG(ctx, storage, next, 0, receipt); err != nil {
					t.Fatal(err)
				}
				expected, next.Version = 1, 2
			}
			bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			type result struct {
				next domain.GitAdmissionMatch
				err  error
			}
			ready, release := make(chan struct{}, 2), make(chan struct{})
			results := make(chan result, 2)
			for index, writer := range []*PostgresStore{storage, peer} {
				go func() {
					candidate := next
					candidate.AfterDestination = fmt.Sprintf("destination-%d", index)
					ready <- struct{}{}
					select {
					case <-release:
						results <- result{candidate, saveGitAdmissionMatchPG(bounded, writer, candidate, expected, receipt)}
					case <-bounded.Done():
						results <- result{candidate, bounded.Err()}
					}
				}()
			}
			for range 2 {
				select {
				case <-ready:
				case <-bounded.Done():
					t.Fatal(bounded.Err())
				}
			}
			close(release)
			winners := 0
			var winner domain.GitAdmissionMatch
			for range 2 {
				select {
				case got := <-results:
					if got.err == nil {
						winners++
						winner = got.next
					} else if !errors.Is(got.err, domain.ErrConflict) {
						t.Fatal("unexpected concurrent result", got.err)
					}
				case <-bounded.Done():
					t.Fatal(bounded.Err())
				}
			}
			if winners != 1 {
				t.Fatal("CAS accepted multiple writers", winners)
			}
			if got, err := peer.GetGitAdmissionMatch(ctx, next.RepoID, next.GitOrigin, next.Provider, next.Page); err != nil || got != winner {
				t.Fatal("losing writer changed match", got, winner, err)
			}
		})
	}
}

func TestPGGitAdmissionMatchAtomicFinishRollbackAndCancellation(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			ctx, repo, now := headReceiptFixture(t, storage)
			claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			observations := []domain.GitRefObservation{headReceiptObservation(repo, "a")}
			receipt, err := domain.NewGitHeadReceipt(claim, observations, false)
			if err != nil || receipt == nil {
				t.Fatal(receipt, err)
			}
			next := domain.GitAdmissionMatch{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL, Provider: receipt.Provider, Page: receipt.Page, Version: 1}
			before, err := storage.RepositoryRevision(ctx, repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			abort := errors.New("abort finished page and match")
			work, cancel := context.WithCancel(ctx)
			defer cancel()
			finishAndSave := func(bound context.Context) error {
				if err := storage.FinishGitHeadScan(bound, claim, observations, false, now); err != nil {
					return err
				}
				return storage.SaveGitAdmissionMatch(bound, next, 0, *receipt)
			}
			err = storage.WithinRepository(work, repo.ID, func(bound context.Context) error {
				if err := finishAndSave(bound); err != nil {
					return err
				}
				if got, err := storage.GetGitAdmissionMatch(bound, repo.ID, repo.GitRemoteURL, next.Provider, next.Page); err != nil || got != next {
					t.Fatal("transaction cannot read its match", got, err)
				}
				if _, err := peer.GetGitAdmissionMatch(ctx, repo.ID, repo.GitRemoteURL, next.Provider, next.Page); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("uncommitted match visible", err)
				}
				if _, err := peer.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, receipt.Version); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("uncommitted receipt visible", err)
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
				t.Fatal("outer failure lost", err)
			}
			if _, err := peer.GetGitAdmissionMatch(ctx, repo.ID, repo.GitRemoteURL, next.Provider, next.Page); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("match escaped rollback", err)
			}
			if _, err := peer.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, receipt.Version); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("receipt escaped rollback", err)
			}
			if got, err := peer.GetGitHeadScan(ctx, repo.ID, repo.GitRemoteURL); err != nil || !reflect.DeepEqual(got, claim) {
				t.Fatal("finished scan escaped rollback", got, err)
			}
			if err := storage.WithinRepository(ctx, repo.ID, finishAndSave); err != nil {
				t.Fatal("rolled-back finish could not retry", err)
			}
			updated := next
			updated.Version++
			if err := storage.WithinRepository(ctx, repo.ID, func(bound context.Context) error {
				if err := storage.SaveGitAdmissionMatch(bound, updated, next.Version, *receipt); err != nil {
					return err
				}
				return abort
			}); !errors.Is(err, abort) {
				t.Fatal(err)
			}
			if got, err := peer.GetGitAdmissionMatch(ctx, repo.ID, repo.GitRemoteURL, next.Provider, next.Page); err != nil || got != next {
				t.Fatal("match update escaped rollback", got, err)
			}
			if after, err := peer.RepositoryRevision(ctx, repo.ID); err != nil || after != before {
				t.Fatal("match changed public revisions", before, after, err)
			}
		})
	}
}

func TestPGGitAdmissionMatchRequiresCurrentOriginalReceipt(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	for _, scenario := range []string{"provider-installation", "provider-repository", "page", "origin", "repository", "unqualified", "forged-head", "forged-provider", "missing-receipt", "missing-scan", "running", "retrying", "wrong-finished-page", "stale-running", "stale-finished", "corrupt-receipt", "corrupt-scan-scope"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, next, receipt, now := gitAdmissionMatchPGFixture(t, storage, false)
			original := next
			want := domain.ErrIntegrity
			switch scenario {
			case "provider-installation":
				next.Provider.InstallationID++
			case "provider-repository":
				next.Provider.RepositoryID++
			case "page":
				next.Page++
			case "origin":
				next.GitOrigin += "-other"
			case "repository":
				next.RepoID = domain.HashContent([]byte("foreign match"))
			case "unqualified":
				receipt.Provider = domain.GitProviderObservation{}
				receipt = receipt.WithID()
				want = domain.ErrValidation
			case "forged-head", "forged-provider":
				if scenario == "forged-provider" {
					receipt.Provider.RepositoryID++
					next.Provider = receipt.Provider
				}
				observation := domain.GitRefObservation{RepoID: receipt.RepoID, GitOrigin: receipt.GitOrigin, Source: "reconciliation", Ref: "refs/heads/main", After: strings.Repeat("b", 40), Provider: &receipt.Provider}.WithID()
				receipt.Heads = []domain.GitHeadReceiptHead{{Ref: observation.Ref, Commit: observation.After, ObservationID: observation.ID}}
				receipt = receipt.WithID()
			case "missing-receipt", "missing-scan":
				table := "git_head_receipts"
				if scenario == "missing-scan" {
					table = "git_head_scans"
				}
				if _, err := storage.pool.Exec(ctx, "DELETE FROM "+table+" WHERE repo_id=$1", next.RepoID); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrNotFound
			case "running", "retrying", "wrong-finished-page", "corrupt-scan-scope":
				claim, err := storage.GetGitHeadScan(ctx, next.RepoID, next.GitOrigin)
				if err != nil {
					t.Fatal(err)
				}
				want = domain.ErrConflict
				switch scenario {
				case "running":
					claim.State, claim.LeaseUntil = "running", now.Add(time.Minute)
				case "retrying":
					claim.State = "retrying"
				case "wrong-finished-page":
					claim.Page++
				case "corrupt-scan-scope":
					claim.GitOrigin += "-other"
					want = domain.ErrIntegrity
				}
				raw, err := json.Marshal(claim)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := storage.pool.Exec(ctx, `UPDATE git_head_scans SET payload=$2 WHERE repo_id=$1`, next.RepoID, raw); err != nil {
					t.Fatal(err)
				}
			case "stale-running", "stale-finished":
				claim, err := storage.ClaimGitHeadScan(ctx, next.RepoID, next.GitOrigin, now.Add(6*time.Minute), time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "stale-finished" {
					repo := domain.Repo{ID: next.RepoID, GitRemoteURL: next.GitOrigin}
					if err := storage.FinishGitHeadScan(ctx, claim, []domain.GitRefObservation{headReceiptObservation(repo, "a")}, false, now.Add(6*time.Minute)); err != nil {
						t.Fatal(err)
					}
				}
				want = domain.ErrConflict
			case "corrupt-receipt":
				if _, err := storage.pool.Exec(ctx, `UPDATE git_head_receipts SET payload=jsonb_set(payload,'{page}','2') WHERE repo_id=$1`, next.RepoID); err != nil {
					t.Fatal(err)
				}
			}
			if err := storage.WithinRepository(ctx, original.RepoID, func(bound context.Context) error {
				if err := storage.SaveGitAdmissionMatch(bound, next, 0, receipt); !errors.Is(err, want) {
					t.Fatal("unqualified or noncurrent receipt accepted", err, want)
				}
				var live int
				return storage.db(bound).QueryRow(bound, `SELECT 1`).Scan(&live)
			}); err != nil {
				t.Fatal("failed save poisoned outer transaction", err)
			}
			if _, err := storage.GetGitAdmissionMatch(ctx, original.RepoID, original.GitOrigin, original.Provider, original.Page); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("invalid receipt advanced match", err)
			}
		})
	}
}

func TestPGGitAdmissionMatchTransactionAndPolicyFences(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, next, receipt, _ := gitAdmissionMatchPGFixture(t, storage, false)
	assertRejected := func(bound context.Context, writer *PostgresStore) {
		t.Helper()
		if err := writer.SaveGitAdmissionMatch(bound, next, 0, receipt); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("incompatible transaction saved match", err)
		}
	}
	assertRejected(ctx, storage)
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		assertRejected(bound, storage)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	foreign := domain.HashContent([]byte("foreign match transaction"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: foreign}); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, foreign, func(bound context.Context) error {
		assertRejected(bound, storage)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var escaped context.Context
	if err := storage.WithinRepository(ctx, next.RepoID, func(bound context.Context) error {
		escaped = bound
		assertRejected(bound, peer)
		if _, err := peer.GetGitAdmissionMatch(bound, next.RepoID, next.GitOrigin, next.Provider, next.Page); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("foreign read escaped transaction", err)
		}
		if _, err := peer.GitAssociationPublicationUpperID(bound, next.RepoID, next.GitOrigin); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("foreign upper bound escaped transaction", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertRejected(escaped, storage)
	if _, err := storage.pool.Exec(ctx, `UPDATE repos SET git_remote_url=$2 WHERE id=$1`, next.RepoID, next.GitOrigin+"-other"); err != nil {
		t.Fatal(err)
	}
	if err := saveGitAdmissionMatchPG(ctx, storage, next, 0, receipt); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale origin accepted", err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE repos SET git_remote_url=$2 WHERE id=$1`, next.RepoID, next.GitOrigin); err != nil {
		t.Fatal(err)
	}
	if err := storage.RequireDocumentIdentity(ctx, next.RepoID, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if err := saveGitAdmissionMatchPG(ctx, storage, next, 0, receipt); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("document identity policy bypassed", err)
	}
	supported := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
	compatible := outbound.WithDocumentIdentityCompatibility(ctx, supported, supported)
	if err := saveGitAdmissionMatchPG(compatible, storage, next, 0, receipt); err != nil {
		t.Fatal("compatible worker rejected", err)
	}
}

func TestPGGitAdmissionMatchUpperIDUsesAcceptedScopedQueue(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, discovery, publications := gitDiscoveryPGFixture(t, storage)
	if upper, err := storage.GitAssociationPublicationUpperID(ctx, discovery.RepoID, discovery.GitOrigin); err != nil || upper != "" {
		t.Fatal("accepted but undiscovered history invented queue work", upper, err)
	}
	next := gitDiscoveryPGPage(discovery)
	if err := saveGitDiscoveryPG(ctx, storage, next, 0, publications[:2]); err != nil {
		t.Fatal(err)
	}
	if upper, err := peer.GitAssociationPublicationUpperID(ctx, discovery.RepoID, discovery.GitOrigin); err != nil || upper != publications[1].ID {
		t.Fatal("upper ID was not maximum queued publication", upper, err)
	}
	abort := errors.New("abort queue extension")
	updated := next
	updated.Version++
	if err := storage.WithinRepository(ctx, discovery.RepoID, func(bound context.Context) error {
		if err := storage.SaveGitAssociationDiscovery(bound, updated, next.Version, publications[2:]); err != nil {
			return err
		}
		if upper, err := storage.GitAssociationPublicationUpperID(bound, discovery.RepoID, discovery.GitOrigin); err != nil || upper != publications[2].ID {
			t.Fatal("upper ID did not use transaction snapshot", upper, err)
		}
		if upper, err := peer.GitAssociationPublicationUpperID(ctx, discovery.RepoID, discovery.GitOrigin); err != nil || upper != publications[1].ID {
			t.Fatal("uncommitted upper ID leaked", upper, err)
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	otherOrigin := discovery.GitOrigin + "-other"
	if _, err := storage.pool.Exec(ctx, `UPDATE repos SET git_remote_url=$2 WHERE id=$1`, discovery.RepoID, otherOrigin); err != nil {
		t.Fatal(err)
	}
	other := domain.GitAssociationDiscovery{RepoID: discovery.RepoID, GitOrigin: otherOrigin, Version: 1}
	if err := saveGitDiscoveryPG(ctx, storage, other, 0, publications[2:]); err != nil {
		t.Fatal(err)
	}
	for _, query := range []struct {
		repo   domain.ContentHash
		origin string
		want   string
	}{
		{discovery.RepoID, discovery.GitOrigin, publications[1].ID},
		{discovery.RepoID, otherOrigin, publications[2].ID},
		{discovery.RepoID, otherOrigin + "-missing", ""},
		{domain.HashContent([]byte("foreign queue")), discovery.GitOrigin, ""},
	} {
		if upper, err := peer.GitAssociationPublicationUpperID(ctx, query.repo, query.origin); err != nil || upper != query.want {
			t.Fatal("queue upper bound leaked scope or rollback", query, upper, err)
		}
	}
}

func TestPGGitAdmissionMatchReadScopeValidationAndCancellation(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, next, receipt, _ := gitAdmissionMatchPGFixture(t, storage, false)
	if err := saveGitAdmissionMatchPG(ctx, storage, next, 0, receipt); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"repository", "origin", "installation", "provider-repository", "page", "invalid-repo", "invalid-origin", "invalid-provider", "invalid-page"} {
		t.Run(scenario, func(t *testing.T) {
			query := next
			want := domain.ErrNotFound
			switch scenario {
			case "repository":
				query.RepoID = domain.HashContent([]byte("foreign match read"))
			case "origin":
				query.GitOrigin += "-other"
			case "installation":
				query.Provider.InstallationID++
			case "provider-repository":
				query.Provider.RepositoryID++
			case "page":
				query.Page++
			case "invalid-repo":
				query.RepoID, want = "invalid", domain.ErrValidation
			case "invalid-origin":
				query.GitOrigin, want = " bad ", domain.ErrValidation
			case "invalid-provider":
				query.Provider.InstallationID, want = 0, domain.ErrValidation
			case "invalid-page":
				query.Page, want = 0, domain.ErrValidation
			}
			if _, err := storage.GetGitAdmissionMatch(ctx, query.RepoID, query.GitOrigin, query.Provider, query.Page); !errors.Is(err, want) {
				t.Fatal("match query scope not enforced", err, want)
			}
			if want == domain.ErrValidation {
				if err := storage.SaveGitAdmissionMatch(ctx, query, 0, receipt); !errors.Is(err, domain.ErrValidation) {
					t.Fatal("invalid match accepted", err)
				}
				if scenario == "invalid-repo" || scenario == "invalid-origin" {
					if _, err := storage.GitAssociationPublicationUpperID(ctx, query.RepoID, query.GitOrigin); !errors.Is(err, domain.ErrValidation) {
						t.Fatal("invalid upper bound scope accepted", err)
					}
				}
			}
		})
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, operation := range []func() error{
		func() error { return storage.SaveGitAdmissionMatch(canceled, next, 0, receipt) },
		func() error {
			_, err := storage.GetGitAdmissionMatch(canceled, next.RepoID, next.GitOrigin, next.Provider, next.Page)
			return err
		},
		func() error {
			_, err := storage.GitAssociationPublicationUpperID(canceled, next.RepoID, next.GitOrigin)
			return err
		},
	} {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation ignored", err)
		}
	}
}

func TestPGGitAdmissionMatchStoredPayloadFences(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	for _, field := range []string{"repo", "origin", "installation", "provider-repository", "page", "version", "cursor"} {
		t.Run(field, func(t *testing.T) {
			ctx, next, receipt, _ := gitAdmissionMatchPGFixture(t, storage, false)
			if err := saveGitAdmissionMatchPG(ctx, storage, next, 0, receipt); err != nil {
				t.Fatal(err)
			}
			corrupt := next
			switch field {
			case "repo":
				corrupt.RepoID = domain.HashContent([]byte("corrupt repo"))
			case "origin":
				corrupt.GitOrigin += "-other"
			case "installation":
				corrupt.Provider.InstallationID++
			case "provider-repository":
				corrupt.Provider.RepositoryID++
			case "page":
				corrupt.Page++
			case "version":
				corrupt.Version++
			case "cursor":
				corrupt.AfterPublicationID = strings.Repeat("f", 32)
			}
			raw, err := json.Marshal(corrupt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := storage.pool.Exec(ctx, `UPDATE git_admission_match SET payload=$2 WHERE repo_id=$1`, next.RepoID, raw); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.GetGitAdmissionMatch(ctx, next.RepoID, next.GitOrigin, next.Provider, next.Page); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("column and payload mismatch accepted", err)
			}
			next.Version++
			if err := saveGitAdmissionMatchPG(ctx, storage, next, 1, receipt); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("CAS overwrote corrupt stored payload", err)
			}
		})
	}
}

func TestPGGitAdmissionMatchLocksFinishedClaimUntilCommit(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, next, receipt, now := gitAdmissionMatchPGFixture(t, storage, false)
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type result struct {
		claim domain.GitHeadScan
		err   error
	}
	finished := make(chan result, 1)
	if err := storage.WithinRepository(bounded, next.RepoID, func(bound context.Context) error {
		if err := storage.SaveGitAdmissionMatch(bound, next, 0, receipt); err != nil {
			return err
		}
		go func() {
			claim, err := peer.ClaimGitHeadScan(bounded, next.RepoID, next.GitOrigin, now.Add(6*time.Minute), time.Minute)
			finished <- result{claim, err}
		}()
		cyclePGWaitBlocked(t, bounded, storage, storage.db(bound).(*repositoryTx).Conn().PgConn().PID())
		select {
		case got := <-finished:
			t.Fatal("new claim escaped match transaction fence", got)
		default:
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-finished:
		if got.err != nil || got.claim.Version != receipt.Version+1 {
			t.Fatal("new claim did not resume after commit", got)
		}
	case <-bounded.Done():
		t.Fatal(bounded.Err())
	}
	if got, err := peer.GetGitAdmissionMatch(ctx, next.RepoID, next.GitOrigin, next.Provider, next.Page); err != nil || got != next {
		t.Fatal("committed match unavailable", got, err)
	}
}
