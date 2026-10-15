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
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func githubPRObservationFixture(t *testing.T) (*GitHubConnections, *store.FSStore, domain.Repo) {
	t.Helper()
	worker, storage, fixture, remote := githubFixture(t)
	ctx := context.Background()
	worker.core = NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	remote.repos = []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}
	state := startGitHub(t, worker, fixture.owner.ID, fixture.organization.NamespaceID)
	if _, err := worker.Complete(ctx, fixture.owner.ID, state, "code"); err != nil {
		t.Fatal(err)
	}
	if err := worker.reconcile(ctx, fixture.organization.NamespaceID); err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: hh(t.Name()), RepositoryID: fixture.repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
	if _, err := storage.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	connection, err := storage.GetGitHubConnection(ctx, fixture.organization.NamespaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Edit(ctx, fixture.owner.ID, fixture.organization.NamespaceID, connection.Generation, "bind", domain.GitHubBinding{RepositoryID: fixture.repository.ID, ContextRepoID: repo.ID, ExternalID: 91}, domain.GitHubTeamMapping{}); err != nil {
		t.Fatal(err)
	}
	return worker, storage, repo
}

type githubPRObservationStore struct {
	*store.FSStore
	observations []domain.GitRefObservation
	err          error
}

func TestGitHubPREnqueueDoesNotInvalidateGraph(t *testing.T) {
	worker, storage, repo := githubPRObservationFixture(t)
	ctx := context.Background()
	connections, err := storage.ListGitHubConnections(ctx)
	if err != nil || len(connections) != 1 || len(connections[0].Bindings) != 1 {
		t.Fatalf("connection fixture: %+v %v", connections, err)
	}
	connection := connections[0]
	before, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequestMerge{Number: 42, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	for range 3 {
		if err := worker.enqueuePR(ctx, connection, connection.Bindings[0], pr); err != nil {
			t.Fatal(err)
		}
	}
	after, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil || after != before {
		t.Fatalf("reconciliation invalidated graph without publication: before=%+v after=%+v err=%v", before, after, err)
	}
	job, err := storage.GetPRJob(ctx, repo.ID, domain.PRPromotionID(repo.ID, pr.Number))
	if err != nil || job.PR != pr || job.State != "waiting" {
		t.Fatalf("durable PR acceptance: %+v %v", job, err)
	}
	scans, err := storage.ListGitScans(ctx, repo.ID, "", 100)
	if err != nil || len(scans) != 2 {
		t.Fatalf("durable evidence scans: %+v %v", scans, err)
	}
	connection.Generation++
	if err := worker.enqueuePR(ctx, connection, connection.Bindings[0], pr); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("stale binding accepted: %v", err)
	}
}

func (storage *githubPRObservationStore) RecordGitRefObservation(ctx context.Context, observation domain.GitRefObservation, now time.Time) error {
	if storage.err != nil {
		return storage.err
	}
	if err := storage.FSStore.RecordGitRefObservation(ctx, observation, now); err != nil {
		return err
	}
	storage.observations = append(storage.observations, observation)
	return nil
}

type githubNoScanStore struct {
	outbound.MetadataStore
}

func TestGitHubPRHeadObservationActionsAndDuplicates(t *testing.T) {
	for _, action := range []string{"synchronize", "opened", "reopened", "closed"} {
		t.Run(action, func(t *testing.T) {
			worker, storage, repo := githubPRObservationFixture(t)
			recorded := &githubPRObservationStore{FSStore: storage}
			worker.core.meta = recorded
			ctx := context.Background()
			before, after := strings.Repeat("a", 40), strings.Repeat("b", 40)
			body := githubPRObservationBody(action, before, after)
			for attempt := 0; attempt < 2; attempt++ {
				if err := worker.Receive(ctx, action, "pull_request", body); err != nil {
					t.Fatal(err)
				}
				if err := worker.deliver(ctx, githubHash(action)); err != nil {
					t.Fatal(err)
				}
			}
			if len(recorded.observations) != 1 {
				t.Fatalf("expected one durable observation: %+v", recorded.observations)
			}
			observation := recorded.observations[0]
			expected := domain.GitPullRequestObservation{InstallationID: 789, RepositoryID: 91, ID: 701, Number: 7, Action: action, HeadRepositoryID: 91, BaseRepositoryID: 91, BaseRef: "main"}
			if action == "closed" {
				expected.MergeSHA = strings.Repeat("c", 40)
			}
			if observation.Validate() != nil || observation.RepoID != repo.ID || observation.GitOrigin != repo.GitRemoteURL || observation.Delivery != githubHash(action) || observation.Ref != "refs/heads/feature" || observation.After != after || !reflect.DeepEqual(observation.PullRequest, &expected) {
				t.Fatalf("incorrect normalized evidence: %+v", observation)
			}
			if action != "synchronize" {
				before = ""
			}
			if observation.Before != before {
				t.Fatalf("incorrect before: %q", observation.Before)
			}
			job, err := storage.GetPRJob(ctx, repo.ID, domain.PRPromotionID(repo.ID, 7))
			if action == "closed" {
				if err != nil || job.PR.HeadSHA != after {
					t.Fatalf("merged PR not queued: %+v %v", job, err)
				}
			} else if !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("unmerged event queued promotion: %v", err)
			}
			altered := []byte(strings.Replace(string(body), `"id":701`, `"id":702`, 1))
			if err := worker.Receive(ctx, action, "pull_request", altered); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("altered delivery accepted: %v", err)
			}
			if err := worker.applyDelivery(ctx, domain.GitHubDelivery{ID: githubHash(action), Kind: "pull_request", Body: body}); err != nil {
				t.Fatalf("durable observation replay failed: %v", err)
			}
			if err := worker.applyDelivery(ctx, domain.GitHubDelivery{ID: githubHash(action), Kind: "pull_request", Body: altered}); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("altered observation accepted: %v", err)
			}
		})
	}
}

func TestGitHubPRHeadObservationsOutOfOrder(t *testing.T) {
	worker, storage, repo := githubPRObservationFixture(t)
	recorded := &githubPRObservationStore{FSStore: storage}
	worker.core.meta = recorded
	ctx := context.Background()
	original, intermediate, final := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("d", 40)
	for index, event := range []struct{ action, before, after string }{
		{"closed", "", final},
		{"synchronize", intermediate, final},
		{"opened", "", original},
		{"synchronize", original, intermediate},
	} {
		delivery := fmt.Sprintf("ordered-%d", index)
		if err := worker.Receive(ctx, delivery, "pull_request", githubPRObservationBody(event.action, event.before, event.after)); err != nil {
			t.Fatal(err)
		}
		if err := worker.deliver(ctx, githubHash(delivery)); err != nil {
			t.Fatal(err)
		}
	}
	if len(recorded.observations) != 4 {
		t.Fatalf("lost out-of-order evidence: %+v", recorded.observations)
	}
	job, err := storage.GetPRJob(ctx, repo.ID, domain.PRPromotionID(repo.ID, 7))
	if err != nil || job.PR.HeadSHA != final {
		t.Fatalf("older head replaced merged anchor: %+v %v", job, err)
	}
	jobs, err := storage.ListGitScans(ctx, repo.ID, "", 10)
	if err != nil || len(jobs) != 4 {
		t.Fatalf("incorrect endpoint scans: %+v %v", jobs, err)
	}
	history, err := storage.ListHistoryEvents(ctx, repo.ID)
	if err != nil || len(history) != 0 {
		t.Fatalf("invented publication: %+v %v", history, err)
	}
	refs, err := storage.ListRefs(ctx, repo.ID)
	if err != nil || len(refs) != 0 {
		t.Fatalf("context moved: %+v %v", refs, err)
	}
}

func TestGitHubPRHeadObservationFailuresRetainBody(t *testing.T) {
	for name, change := range map[string]func(*GitHubConnections, *store.FSStore, []byte) []byte{
		"storage unavailable": func(worker *GitHubConnections, storage *store.FSStore, body []byte) []byte {
			worker.core.meta = &githubPRObservationStore{FSStore: storage, err: errors.New("unavailable")}
			return body
		},
		"missing capability": func(worker *GitHubConnections, storage *store.FSStore, body []byte) []byte {
			worker.core.meta = githubNoScanStore{MetadataStore: storage}
			return body
		},
		"missing PR identity": func(_ *GitHubConnections, _ *store.FSStore, body []byte) []byte {
			return []byte(strings.Replace(string(body), `"id":701`, `"id":0`, 1))
		},
		"missing head identity": func(_ *GitHubConnections, _ *store.FSStore, body []byte) []byte {
			return []byte(strings.Replace(string(body), `"repo":{"id":91}`, `"repo":{"id":0}`, 1))
		},
		"foreign base": func(_ *GitHubConnections, _ *store.FSStore, body []byte) []byte {
			return []byte(strings.Replace(string(body), `"base":{"ref":"main","repo":{"id":91}}`, `"base":{"ref":"main","repo":{"id":92}}`, 1))
		},
		"wrong installation": func(_ *GitHubConnections, _ *store.FSStore, body []byte) []byte {
			return []byte(strings.Replace(string(body), `"id":789`, `"id":790`, 1))
		},
		"unbound repository": func(_ *GitHubConnections, _ *store.FSStore, body []byte) []byte {
			return []byte(strings.ReplaceAll(string(body), `"id":91`, `"id":92`))
		},
		"short before": func(_ *GitHubConnections, _ *store.FSStore, body []byte) []byte {
			return []byte(strings.Replace(string(body), strings.Repeat("a", 40), "abcdef0", 1))
		},
		"missing after": func(_ *GitHubConnections, _ *store.FSStore, body []byte) []byte {
			return []byte(strings.Replace(string(body), `"after":"`+strings.Repeat("b", 40)+`"`, `"after":""`, 1))
		},
		"head disagrees with after": func(_ *GitHubConnections, _ *store.FSStore, body []byte) []byte {
			return []byte(strings.Replace(string(body), `"after":"`+strings.Repeat("b", 40)+`"`, `"after":"`+strings.Repeat("d", 40)+`"`, 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			worker, storage, repo := githubPRObservationFixture(t)
			ctx := context.Background()
			body := change(worker, storage, githubPRObservationBody("synchronize", strings.Repeat("a", 40), strings.Repeat("b", 40)))
			if err := worker.Receive(ctx, name, "pull_request", body); err != nil {
				t.Fatal(err)
			}
			if err := worker.deliver(ctx, githubHash(name)); err != nil {
				t.Fatal(err)
			}
			delivery, err := storage.GetGitHubDelivery(ctx, githubHash(name))
			if err != nil || delivery.Done || string(delivery.Body) != string(body) {
				t.Fatalf("failed evidence was acknowledged: %+v %v", delivery, err)
			}
			jobs, err := storage.ListGitScans(ctx, repo.ID, "", 10)
			if err != nil || len(jobs) != 0 {
				t.Fatalf("failed evidence created work: %+v %v", jobs, err)
			}
		})
	}
}

func githubPRObservationBody(action, before, after string) []byte {
	return []byte(fmt.Sprintf(`{"installation":{"id":789},"repository":{"id":91},"action":%q,"number":7,"before":%q,"after":%q,"pull_request":{"id":701,"merged":%t,"merge_commit_sha":%q,"head":{"ref":"feature","sha":%q,"repo":{"id":91}},"base":{"ref":"main","repo":{"id":91}}}}`, action, before, after, action == "closed", strings.Repeat("c", 40), after))
}

func TestGitHubPRHeadObservationRechecksBinding(t *testing.T) {
	for name, change := range map[string]func(*domain.GitHubConnection){
		"disabled":              func(connection *domain.GitHubConnection) { connection.Enabled = false },
		"suspended":             func(connection *domain.GitHubConnection) { connection.Installation.Suspended = true },
		"expired authorization": func(connection *domain.GitHubConnection) { connection.CheckedAt = time.Now().Add(-time.Hour) },
		"origin rebound": func(connection *domain.GitHubConnection) {
			connection.Bindings[0].GitOrigin = "https://github.com/acme/other.git"
		},
	} {
		t.Run(name, func(t *testing.T) {
			worker, storage, repo := githubPRObservationFixture(t)
			ctx := context.Background()
			connections, err := storage.ListGitHubConnections(ctx)
			if err != nil || len(connections) != 1 {
				t.Fatalf("connection fixture: %+v %v", connections, err)
			}
			body := githubPRObservationBody("synchronize", strings.Repeat("a", 40), strings.Repeat("b", 40))
			if err := worker.Receive(ctx, name, "pull_request", body); err != nil {
				t.Fatal(err)
			}
			change(&connections[0])
			if err := storage.PutGitHubConnection(ctx, connections[0]); err != nil {
				t.Fatal(err)
			}
			if err := worker.deliver(ctx, githubHash(name)); err != nil {
				t.Fatal(err)
			}
			delivery, err := storage.GetGitHubDelivery(ctx, githubHash(name))
			if err != nil || delivery.Done || string(delivery.Body) != string(body) {
				t.Fatalf("unauthorized evidence acknowledged: %+v %v", delivery, err)
			}
			jobs, err := storage.ListGitScans(ctx, repo.ID, "", 10)
			if err != nil || len(jobs) != 0 {
				t.Fatalf("unauthorized evidence created scans: %+v %v", jobs, err)
			}
		})
	}
}

func TestGitHubPRHeadObservationIgnoresNonHeadActions(t *testing.T) {
	worker, storage, repo := githubPRObservationFixture(t)
	ctx := context.Background()
	for _, action := range []string{"force-pushed", "edited", "closed"} {
		body := githubPRObservationBody(action, strings.Repeat("a", 40), strings.Repeat("b", 40))
		body = []byte(strings.Replace(string(body), `"merged":true`, `"merged":false`, 1))
		if err := worker.Receive(ctx, action, "pull_request", body); err != nil {
			t.Fatal(err)
		}
		if err := worker.deliver(ctx, githubHash(action)); err != nil {
			t.Fatal(err)
		}
		jobs, err := storage.ListGitScans(ctx, repo.ID, "", 10)
		if err != nil || len(jobs) != 0 {
			t.Fatalf("unsupported action created head evidence: %+v %v", jobs, err)
		}
	}
}

func TestGitHubPRHeadObservationIgnoresValidForks(t *testing.T) {
	for _, action := range []string{"opened", "synchronize", "closed"} {
		t.Run(action, func(t *testing.T) {
			worker, storage, repo := githubPRObservationFixture(t)
			recorded := &githubPRObservationStore{FSStore: storage}
			worker.core.meta = recorded
			ctx := context.Background()
			body := githubPRObservationBody(action, strings.Repeat("a", 40), strings.Repeat("b", 40))
			body = []byte(strings.Replace(string(body), `"repo":{"id":91}`, `"repo":{"id":92}`, 1))
			for attempt := 0; attempt < 2; attempt++ {
				if err := worker.Receive(ctx, action, "pull_request", body); err != nil {
					t.Fatal(err)
				}
				if err := worker.deliver(ctx, githubHash(action)); err != nil {
					t.Fatal(err)
				}
			}
			delivery, err := storage.GetGitHubDelivery(ctx, githubHash(action))
			if err != nil || !delivery.Done || len(delivery.Body) != 0 || delivery.Attempts != 1 {
				t.Fatalf("valid unsupported fork remains retryable: done=%v attempts=%d retained=%d err=%v", delivery.Done, delivery.Attempts, len(delivery.Body), err)
			}
			if len(recorded.observations) != 0 {
				t.Fatal("fork published same-repository evidence")
			}
			jobs, err := storage.ListGitScans(ctx, repo.ID, "", 10)
			if err != nil || len(jobs) != 0 {
				t.Fatalf("fork queued scans: %+v %v", jobs, err)
			}
			if _, err := storage.GetPRJob(ctx, repo.ID, domain.PRPromotionID(repo.ID, 7)); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("fork queued promotion: %v", err)
			}
		})
	}
}

func TestGitHubPRHeadObservationRejectsMalformedForks(t *testing.T) {
	for name, change := range map[string]func(string) string{
		"wrong base identity": func(body string) string {
			return strings.Replace(body, `"base":{"ref":"main","repo":{"id":91}}`, `"base":{"ref":"main","repo":{"id":93}}`, 1)
		},
		"missing PR identity": func(body string) string { return strings.Replace(body, `"id":701`, `"id":0`, 1) },
		"missing number":      func(body string) string { return strings.Replace(body, `"number":7`, `"number":0`, 1) },
		"conflicting number":  func(body string) string { return strings.Replace(body, `"id":701`, `"id":701,"number":8`, 1) },
		"missing before":      func(body string) string { return strings.Replace(body, strings.Repeat("a", 40), "", 1) },
	} {
		t.Run(name, func(t *testing.T) {
			worker, storage, repo := githubPRObservationFixture(t)
			ctx := context.Background()
			body := string(githubPRObservationBody("synchronize", strings.Repeat("a", 40), strings.Repeat("b", 40)))
			body = change(strings.Replace(body, `"repo":{"id":91}`, `"repo":{"id":92}`, 1))
			if err := worker.Receive(ctx, name, "pull_request", []byte(body)); err != nil {
				t.Fatal(err)
			}
			if err := worker.deliver(ctx, githubHash(name)); err != nil {
				t.Fatal(err)
			}
			delivery, err := storage.GetGitHubDelivery(ctx, githubHash(name))
			if err != nil || delivery.Done || string(delivery.Body) != body {
				t.Fatalf("malformed fork acknowledged: done=%v retained=%d err=%v", delivery.Done, len(delivery.Body), err)
			}
			jobs, err := storage.ListGitScans(ctx, repo.ID, "", 10)
			if err != nil || len(jobs) != 0 {
				t.Fatalf("malformed fork queued scans: %+v %v", jobs, err)
			}
		})
	}
}

func TestGitHubSynchronizePersistsHeadEvidenceBeforeCleanup(t *testing.T) {
	worker, storage, repo := githubPRObservationFixture(t)
	ctx := context.Background()
	before, after := strings.Repeat("a", 40), strings.Repeat("b", 40)
	body := githubPRObservationBody("synchronize", before, after)
	if err := worker.Receive(ctx, "synchronize", "pull_request", body); err != nil {
		t.Fatal(err)
	}
	restarted := NewGitHubConnections(worker.id, worker.core, storage, worker.remote)
	if err := restarted.deliver(ctx, githubHash("synchronize")); err != nil {
		t.Fatal(err)
	}
	jobs, err := storage.ListGitScans(ctx, repo.ID, "", 10)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("synchronize lost its endpoint scans: %+v %v", jobs, err)
	}
	delivery, err := storage.GetGitHubDelivery(ctx, githubHash("synchronize"))
	if err != nil || !delivery.Done || len(delivery.Body) != 0 {
		t.Fatalf("delivery was not acknowledged after evidence: %+v %v", delivery, err)
	}
	history, err := storage.ListHistoryEvents(ctx, repo.ID)
	if err != nil || len(history) != 0 {
		t.Fatalf("observation invented history: %+v %v", history, err)
	}
	refs, err := storage.ListRefs(ctx, repo.ID)
	if err != nil || len(refs) != 0 {
		t.Fatalf("observation moved context refs: %+v %v", refs, err)
	}
}
