package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// unavailableScanReader fails tree reads with a configured provider error and
// otherwise serves the synthetic scanReader history.
type unavailableScanReader struct {
	*scanReader
	failure   error
	treeReads int
}

func (reader *unavailableScanReader) ReadCommitTree(ctx context.Context, origin, sha string) (domain.GitTreeEvidence, error) {
	reader.treeReads++
	if reader.failure != nil {
		return domain.GitTreeEvidence{}, reader.failure
	}
	return reader.scanReader.ReadCommitTree(ctx, origin, sha)
}

var errScanCommitUnavailable = fmt.Errorf("%w: %w", outbound.ErrGitCommitUnavailable, outbound.GitProviderStatusError{Status: 404})

func unavailableScanFixture(test *testing.T, job func(domain.ContentHash, string, string) domain.GitScanJob) (*GitScans, *store.FSStore, *unavailableScanReader, domain.GitScanJob) {
	test.Helper()
	core, storage := newFsckSvc(test)
	repo := domain.Repo{ID: hh(test.Name()), GitRemoteURL: "https://github.com/example/unavailable"}
	if _, err := storage.PutRepo(systemTestContext(), repo); err != nil {
		test.Fatal(err)
	}
	commit := strings.Repeat("b", 40)
	reader := &unavailableScanReader{scanReader: &scanReader{deltas: map[string]domain.GitCommitDelta{commit: {Commit: commit, Parents: []string{}, Changes: []domain.GitPathChange{}, Complete: true}}}}
	scans, err := NewGitScans(core, reader)
	if err != nil {
		test.Fatal(err)
	}
	queued := job(repo.ID, repo.GitRemoteURL, commit)
	if err = storage.EnqueueGitScan(systemTestContext(), queued); err != nil {
		test.Fatal(err)
	}
	return scans, storage, reader, queued
}

// A scan row as production held it on 2026-11-17: a locally rewritten commit
// that never reached GitHub, retried about 5,400 times since 2026-09-19.
func productionUnavailableScan(repo domain.ContentHash, origin, commit string) domain.GitScanJob {
	now := time.Now().UTC()
	job := domain.NewGitScan(repo, origin, commit, now.Add(-59*24*time.Hour))
	job.State = "retrying"
	job.Reason = "temporary_provider_or_storage_failure"
	job.Attempts = 5400
	job.Version = 5400
	job.UpdatedAt = now.Add(-10 * time.Minute)
	job.NextAttempt = now.Add(-time.Minute)
	return job
}

func captureScanLog(test *testing.T) *bytes.Buffer {
	test.Helper()
	var output bytes.Buffer
	writer, flags := log.Writer(), log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	test.Cleanup(func() {
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
	return &output
}

func TestGitScanFailureClassificationStopsUnavailableCommitsAndBoundsRetries(test *testing.T) {
	transport := &url.Error{Op: "Get", URL: "https://api.github.com/repos/example/unavailable/git/commits/bbbb?access_token=secret", Err: errors.New("connection reset")}
	for _, scenario := range []struct {
		name     string
		failure  error
		age      time.Duration
		attempts int // attempts already recorded before this claim
		state    string
		reason   string
		class    string
	}{
		{"fresh commit not yet pushed keeps ordinary retries", errScanCommitUnavailable, 0, 0, "retrying", domain.GitScanCommitPending, "commit_unavailable_at_provider"},
		{"commit still unavailable after grace stops", errScanCommitUnavailable, gitCommitArrivalGrace + time.Minute, 3, "attention", domain.GitScanCommitUnavailable, "commit_unavailable_at_provider"},
		{"provider outage keeps retrying", outbound.GitProviderStatusError{Status: 503}, 59 * 24 * time.Hour, 0, "retrying", "temporary_provider_or_storage_failure", "provider_http_503"},
		{"repository hidden from credential keeps retrying", outbound.GitProviderStatusError{Status: 404}, 59 * 24 * time.Hour, 0, "retrying", "temporary_provider_or_storage_failure", "provider_http_404"},
		{"rate limit keeps retrying", outbound.ErrGitProviderRateLimited, 0, 4, "retrying", "temporary_provider_or_storage_failure", "provider_rate_limited"},
		{"transport failure keeps retrying without its URL", transport, 0, 0, "retrying", "temporary_provider_or_storage_failure", "network"},
		{"unclassified failure below limit retries", errors.New("offline"), 59 * 24 * time.Hour, gitScanRetryLimit - 2, "retrying", "temporary_provider_or_storage_failure", "unclassified"},
		{"unclassified failure at limit stops", errors.New("offline"), 59 * 24 * time.Hour, gitScanRetryLimit - 1, "attention", domain.GitScanRetryLimit, "unclassified"},
		{"evidence failure keeps its own reason past limit", domain.ErrIntegrity, 0, gitScanRetryLimit + 10, "attention", "integrity_check_failed", "integrity"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			scans, storage, reader, queued := unavailableScanFixture(test, func(repo domain.ContentHash, origin, commit string) domain.GitScanJob {
				job := domain.NewGitScan(repo, origin, commit, time.Now().UTC().Add(-scenario.age))
				job.Attempts = scenario.attempts
				return job
			})
			reader.failure = scenario.failure
			output := captureScanLog(test)
			if err := scans.Process(systemTestContext(), 1); err != nil {
				test.Fatal("Process must defer job failure, not change its error contract", err)
			}
			current, err := storage.GetGitScan(systemTestContext(), queued.RepoID, queued.ID)
			if err != nil || current.State != scenario.state || current.Reason != scenario.reason || current.Attempts != scenario.attempts+1 || current.TreeIndexed || !current.LeaseUntil.IsZero() {
				test.Fatal("scan failure classified incorrectly", current, err)
			}
			if current.NextAttempt.Sub(current.UpdatedAt) != time.Second<<min(current.Attempts, 9) {
				test.Fatal("original exponential backoff changed", current)
			}
			line := output.String()
			want := fmt.Sprintf("Git observation %s attempt %d ", queued.ID, current.Attempts)
			if !strings.Contains(line, want) || !strings.Contains(line, " class="+scenario.class+" reason="+scenario.reason) || strings.Count(line, "\n") != 1 {
				test.Fatal("deferral log lost its bounded failure class", line)
			}
			if strings.Contains(line, "api.github.com") || strings.Contains(line, "secret") || strings.Contains(line, queued.Commit) {
				test.Fatal("deferral log leaked provider details", line)
			}
		})
	}
}

func TestGitScanProductionUnavailableRowConvergesAndCanStillBeRetried(test *testing.T) {
	scans, storage, reader, queued := unavailableScanFixture(test, productionUnavailableScan)
	ctx := systemTestContext()
	reader.failure = errScanCommitUnavailable
	if err := scans.Process(ctx, 2); err != nil {
		test.Fatal(err)
	}
	current, err := storage.GetGitScan(ctx, queued.RepoID, queued.ID)
	if err != nil || current.State != "attention" || current.Reason != domain.GitScanCommitUnavailable || current.Attempts != 5401 || reader.treeReads != 1 {
		test.Fatal("existing retrying row did not stop on its next attempt", current, reader.treeReads, err)
	}
	// Stopped rows no longer take worker turns.
	if err = scans.Process(ctx, 2); err != nil || reader.treeReads != 1 {
		test.Fatal("stopped scan was claimed again", reader.treeReads, err)
	}
	// Manual retry still runs exactly one more attempt; a commit that is still
	// missing returns to attention instead of looping.
	if err = scans.RetryScan(ctx, queued.RepoID, queued.ID); err != nil {
		test.Fatal(err)
	}
	if retried, _ := storage.GetGitScan(ctx, queued.RepoID, queued.ID); retried.State != "waiting" || retried.Reason != "" {
		test.Fatal("manual retry did not reopen the scan", retried)
	}
	if err = scans.Process(ctx, 2); err != nil {
		test.Fatal(err)
	}
	current, err = storage.GetGitScan(ctx, queued.RepoID, queued.ID)
	if err != nil || current.State != "attention" || current.Reason != domain.GitScanCommitUnavailable || reader.treeReads != 2 {
		test.Fatal("manual retry of a missing commit looped or lost its reason", current, reader.treeReads, err)
	}
	// Once the commit is pushed, manual retry indexes it.
	reader.failure = nil
	if err = scans.RetryScan(ctx, queued.RepoID, queued.ID); err != nil {
		test.Fatal(err)
	}
	if err = scans.Process(ctx, 3); err != nil {
		test.Fatal(err)
	}
	current, err = storage.GetGitScan(ctx, queued.RepoID, queued.ID)
	if err != nil || current.State != "completed" || !current.TreeIndexed || !current.Indexed {
		test.Fatal("manual retry could not index a commit that later reached the provider", current, err)
	}
}

func TestGitScanPushOfStoppedCommitReopensItWithoutReplayLoop(test *testing.T) {
	scans, storage, reader, queued := unavailableScanFixture(test, productionUnavailableScan)
	ctx := systemTestContext()
	reader.failure = errScanCommitUnavailable
	if err := scans.Process(ctx, 1); err != nil {
		test.Fatal(err)
	}
	// Retained history re-uploaded by the CLI is not provider evidence.
	if err := storage.EnqueueGitScan(ctx, domain.NewGitScan(queued.RepoID, queued.GitOrigin, queued.Commit, time.Now().UTC())); err != nil {
		test.Fatal(err)
	}
	if current, _ := storage.GetGitScan(ctx, queued.RepoID, queued.ID); current.State != "attention" {
		test.Fatal("client history reopened a stopped scan", current)
	}
	// The user pushes the commit later: the push names it and reopens the scan.
	if _, err := scans.ObservePush(ctx, queued.GitOrigin, "refs/heads/feature", "", queued.Commit, false, "delivery-late-push"); err != nil {
		test.Fatal(err)
	}
	current, err := storage.GetGitScan(ctx, queued.RepoID, queued.ID)
	if err != nil || current.State != "waiting" || current.Reason != "" || current.Attempts != 5401 {
		test.Fatal("provider evidence did not reopen the stopped scan", current, err)
	}
	// A redelivered observation is not new evidence and must not loop.
	if err = scans.Process(ctx, 1); err != nil {
		test.Fatal(err)
	}
	if _, err = scans.ObservePush(ctx, queued.GitOrigin, "refs/heads/feature", "", queued.Commit, false, "delivery-late-push"); err != nil {
		test.Fatal(err)
	}
	if current, _ = storage.GetGitScan(ctx, queued.RepoID, queued.ID); current.State != "attention" || reader.treeReads != 2 {
		test.Fatal("replayed observation reopened the scan", current, reader.treeReads)
	}
	reader.failure = nil
	if _, err = scans.ObservePush(ctx, queued.GitOrigin, "refs/heads/main", "", queued.Commit, false, "delivery-merged-push"); err != nil {
		test.Fatal(err)
	}
	if err = scans.Process(ctx, 3); err != nil {
		test.Fatal(err)
	}
	if current, err = storage.GetGitScan(ctx, queued.RepoID, queued.ID); err != nil || current.State != "completed" || !current.TreeIndexed {
		test.Fatal("reopened scan did not index the pushed commit", current, err)
	}
}

func TestGitObservationFailureClassIsBounded(test *testing.T) {
	deadline := &url.Error{Op: "Get", URL: "https://api.github.com/x", Err: context.DeadlineExceeded}
	for failure, want := range map[error]string{
		errors.Join(errScanCommitUnavailable, errors.New("finish failed")): "commit_unavailable_at_provider",
		outbound.GitProviderStatusError{Status: 0}:                         "provider_http_invalid",
		outbound.GitProviderStatusError{Status: 422}:                       "provider_http_422",
		deadline:         "deadline_exceeded",
		context.Canceled: "canceled",
		fmt.Errorf("claim: %w", domain.ErrForbidden):   "forbidden",
		domain.ErrResourceOverloaded:                   "resource_overloaded",
		domain.ErrDocumentIdentityUpgradeRequired:      "document_identity_upgrade_required",
		fmt.Errorf("pg: %w", domain.ErrNotFound):       "not_found",
		errors.New("https://user:token@example.test/"): "unclassified",
	} {
		if got := gitObservationFailureClass(failure); got != want {
			test.Fatalf("class(%v)=%s want %s", failure, got, want)
		}
	}
}
