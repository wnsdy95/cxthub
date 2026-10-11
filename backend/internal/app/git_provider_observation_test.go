package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type providerObservationHeadReader struct {
	*scanReader
	duringRead func(context.Context)
}

func (reader *providerObservationHeadReader) ListGitHeads(ctx context.Context, origin string, page int) ([]outbound.GitHead, bool, error) {
	if reader.duringRead != nil {
		reader.duringRead(ctx)
	}
	return reader.scanReader.ListGitHeads(ctx, origin, page)
}

type providerObservationStore interface {
	outbound.GitHubStore
	outbound.GitScanStore
	outbound.GitHeadScanStore
	outbound.GitHeadReceiptStore
	outbound.PRHistoryStore
}

func providerObservationExpected(repo domain.Repo, source, delivery string) domain.GitRefObservation {
	return domain.GitRefObservation{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL, Source: source, Delivery: delivery, Ref: "refs/heads/main", After: strings.Repeat("a", 40), Provider: &domain.GitProviderObservation{InstallationID: 789, RepositoryID: 91}}.WithID()
}

func runProviderObservationReconcile(t *testing.T, worker *GitHubConnections, storage providerObservationStore, repo domain.Repo, revoke bool) {
	t.Helper()
	ctx := context.Background()
	reader := &providerObservationHeadReader{scanReader: &scanReader{headPages: map[int][]outbound.GitHead{1: {{Ref: "refs/heads/main", Commit: strings.Repeat("a", 40)}}}}}
	reader.duringRead = func(work context.Context) {
		if work.Value(afterCommitKey{}) != nil || outbound.GitRepository(work) != repo.ID {
			t.Fatal("provider read occurred in publication transaction or without repository scope")
		}
		if revoke {
			if err := worker.id.withIdentity(work, func(tx context.Context) error {
				connections, err := storage.ListGitHubConnections(tx)
				if err != nil {
					return err
				}
				connection := connections[0]
				connection.Enabled = false
				connection.Generation++
				return storage.PutGitHubConnection(tx, connection)
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	scans, err := NewGitScans(worker.core, reader)
	if err != nil {
		t.Fatal(err)
	}
	scans.SetSourceAuthorizer(worker)
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(reader.headReads) != 1 {
		t.Fatalf("provider read count: %v", reader.headReads)
	}
	expected := providerObservationExpected(repo, "reconciliation", "")
	actual, err := storage.GetPRRefObservation(ctx, repo.ID, repo.GitRemoteURL, expected.ID)
	jobs, jobsErr := storage.ListGitScans(ctx, repo.ID, "", 10)
	checkpoint, checkpointErr := storage.GetGitHeadScan(ctx, repo.ID, repo.GitRemoteURL)
	receipts, receiptsErr := storage.ListGitHeadReceipts(ctx, repo.ID, repo.GitRemoteURL, 0, 10)
	if jobsErr != nil || checkpointErr != nil || receiptsErr != nil {
		t.Fatal(jobsErr, checkpointErr, receiptsErr)
	}
	if revoke {
		if !errors.Is(err, domain.ErrNotFound) || len(jobs) != 0 || len(receipts) != 0 || checkpoint.State != "retrying" || checkpoint.Page != 1 {
			t.Fatalf("revoked read published or lost retry: %+v %v %+v %+v %+v", actual, err, jobs, checkpoint, receipts)
		}
	} else if err != nil || !reflect.DeepEqual(actual, expected) || len(jobs) != 1 || checkpoint.State != "completed" {
		t.Fatalf("qualified read missing: %+v %v %+v %+v", actual, err, jobs, checkpoint)
	} else if len(receipts) != 1 || receipts[0].Validate() != nil || receipts[0].Version != checkpoint.Version || receipts[0].Page != 1 || receipts[0].Provider != *expected.Provider || !reflect.DeepEqual(receipts[0].Heads, []domain.GitHeadReceiptHead{{Ref: expected.Ref, Commit: expected.After, ObservationID: expected.ID}}) {
		t.Fatalf("qualified read occurrence missing or relabeled: %+v", receipts)
	}
}

func TestGitProviderObservationReconcile(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(fmt.Sprint(revoke), func(t *testing.T) {
			worker, storage, repo := githubPRObservationFixture(t)
			runProviderObservationReconcile(t, worker, storage, repo, revoke)
		})
	}
}

func TestGitProviderObservationLegacySourcesRemainUnqualified(t *testing.T) {
	for _, connectedAuthorizer := range []bool{false, true} {
		t.Run(fmt.Sprint(connectedAuthorizer), func(t *testing.T) {
			worker, storage, fixture, _ := githubFixture(t)
			worker.core = NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
			ctx := context.Background()
			repo := domain.Repo{ID: hh(t.Name()), RepositoryID: fixture.repository.ID, GitRemoteURL: "https://github.com/acme/api.git"}
			if _, err := storage.PutRepo(ctx, repo); err != nil {
				t.Fatal(err)
			}
			identity, fence, err := worker.AuthorizeGitObservation(ctx, repo.ID)
			if err != nil || identity != nil || fence == nil || fence(ctx) != nil {
				t.Fatalf("operator-token source incorrectly qualified: %+v %v", identity, err)
			}
			reader := &scanReader{headPages: map[int][]outbound.GitHead{1: {{Ref: "refs/heads/main", Commit: strings.Repeat("a", 40)}}}}
			scans, err := NewGitScans(worker.core, reader)
			if err != nil {
				t.Fatal(err)
			}
			if connectedAuthorizer {
				scans.SetSourceAuthorizer(worker)
			}
			if err := scans.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			receipts, err := storage.ListGitHeadReceipts(ctx, repo.ID, repo.GitRemoteURL, 0, 10)
			if err != nil || len(receipts) != 0 {
				t.Fatalf("legacy provider reads produced qualified receipts: %+v %v", receipts, err)
			}
			if _, err := scans.ObservePush(systemTestContext(), repo.GitRemoteURL, "refs/heads/main", "", strings.Repeat("a", 40), false, "legacy-push"); err != nil {
				t.Fatal(err)
			}
			for _, source := range []string{"github-push", "reconciliation"} {
				expected := providerObservationExpected(repo, source, "")
				if source == "github-push" {
					expected.Delivery = "legacy-push"
				}
				expected.Provider = nil
				expected = expected.WithID()
				actual, err := storage.GetPRRefObservation(ctx, repo.ID, repo.GitRemoteURL, expected.ID)
				if err != nil || !reflect.DeepEqual(actual, expected) {
					t.Fatalf("legacy observation changed: %+v %v", actual, err)
				}
			}
		})
	}
}

func TestGitProviderObservationPushBindingAndLegacyReplay(t *testing.T) {
	worker, storage, repo := githubPRObservationFixture(t)
	ctx := context.Background()
	delivery := "qualified-push"
	expected := providerObservationExpected(repo, "github-push", githubHash(delivery))
	legacy := expected
	legacy.Provider = nil
	legacy = legacy.WithID()
	if err := storage.RecordGitRefObservation(ctx, legacy, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	body := []byte(fmt.Sprintf(`{"installation":{"id":789},"repository":{"id":91},"ref":"refs/heads/main","before":"%s","after":"%s","provider":{"installation_id":1,"repository_id":2}}`, strings.Repeat("0", 40), expected.After))
	if err := worker.Receive(ctx, delivery, "push", body); err != nil {
		t.Fatal(err)
	}
	if err := worker.deliver(ctx, githubHash(delivery)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []domain.GitRefObservation{legacy, expected} {
		actual, err := storage.GetPRRefObservation(ctx, repo.ID, repo.GitRemoteURL, want.ID)
		if err != nil || !reflect.DeepEqual(actual, want) {
			t.Fatalf("immutable observation changed: %+v %v", actual, err)
		}
	}
	if err := worker.applyDelivery(ctx, domain.GitHubDelivery{ID: expected.Delivery, Kind: "push", Body: body}); err != nil {
		t.Fatal(err)
	}
	altered := []byte(strings.ReplaceAll(string(body), expected.After, strings.Repeat("b", 40)))
	if err := worker.applyDelivery(ctx, domain.GitHubDelivery{ID: expected.Delivery, Kind: "push", Body: altered}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("altered qualified delivery accepted: %v", err)
	}
	for _, replacement := range []struct{ old, new string }{{`"id":789`, `"id":790`}, {`"id":91`, `"id":92`}} {
		mismatch := []byte(strings.Replace(string(body), replacement.old, replacement.new, 1))
		err := worker.applyDelivery(ctx, domain.GitHubDelivery{ID: "mismatch", Kind: "push", Body: mismatch})
		if err != nil {
			t.Fatal(err)
		}
		unexpected := expected
		unexpected.Delivery = "mismatch"
		if _, err := storage.GetPRRefObservation(ctx, repo.ID, repo.GitRemoteURL, unexpected.WithID().ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("foreign provider identity accepted: %v", err)
		}
	}
}
