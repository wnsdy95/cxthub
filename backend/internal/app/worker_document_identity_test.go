package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type workerRoundTrip func(*http.Request) (*http.Response, error)

func (f workerRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type workerNotificationStore struct {
	*store.FSStore
	beforeGuard func()
	seen        context.Context
}

func (s *workerNotificationStore) ClaimNotification(ctx context.Context, now time.Time, lease time.Duration) (outbound.NotificationDelivery, error) {
	s.seen = ctx
	return s.FSStore.ClaimNotification(ctx, now, lease)
}
func (s *workerNotificationStore) ValidateNotificationDelivery(ctx context.Context, d outbound.NotificationDelivery, now time.Time) error {
	if s.beforeGuard != nil {
		s.beforeGuard()
	}
	return s.FSStore.ValidateNotificationDelivery(ctx, d, now)
}

func TestWorkerDeclarationPreservesActorAndReplacesSpoof(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	s := NewService(st, st, nil, nil, st)
	want := s.DocumentIdentitiesSupported()
	wantErr := error(domain.ErrDocumentIdentityUpgradeRequired)
	if hasDocumentIdentity(want, domain.DocumentIdentityRootV1) {
		wantErr = nil
	}
	for _, actor := range []string{"", "user", "system"} {
		ctx := context.Background()
		if actor == "user" {
			ctx = inbound.WithRepositoryActor(ctx, "synthetic")
		}
		if actor == "system" {
			ctx = inbound.WithSystemActor(ctx)
		}
		user, system := inbound.RepositoryActor(ctx)
		root := []domain.DocumentIdentity{"future-unsupported"}
		ctx = inbound.WithDocumentIdentities(ctx, root)
		ctx = outbound.WithDocumentIdentityCompatibility(ctx, root, root)
		ctx = s.workerDocumentIdentityContext(ctx)
		if u, a := inbound.RepositoryActor(ctx); u != user || a != system {
			t.Fatal("actor changed")
		}
		if !reflect.DeepEqual(inbound.DocumentIdentities(ctx), want) {
			t.Fatal("spoofed peer survived")
		}
		if e := outbound.CheckDocumentIdentityCompatibility(ctx, domain.DocumentIdentityRootV1); !errors.Is(e, wantErr) {
			t.Fatal(e)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if e := outbound.CheckDocumentIdentityCompatibility(canceled, domain.DocumentIdentityRootV1); !errors.Is(e, context.Canceled) {
			t.Fatal("cancellation lost", e)
		}
		cleanup := context.WithoutCancel(canceled)
		if e := outbound.CheckDocumentIdentityCompatibility(cleanup, domain.DocumentIdentityRootV1); !errors.Is(e, wantErr) {
			t.Fatal("cleanup lost declaration", e)
		}
	}
}

func TestWorkerNotificationPolicyBeforeEgressAndAfterSend(t *testing.T) {
	for _, when := range []string{"before-claim", "before-egress", "after-send", "legacy", "config-revoked"} {
		t.Run(when, func(t *testing.T) {
			svc, st, record := notificationFixture(t)
			supported := hasDocumentIdentity(svc.DocumentIdentitiesSupported(), domain.DocumentIdentityRootV1)
			if svc.RootPublicationEnabled() {
				t.Fatal("fixture enabled new root admission")
			}
			ctx := context.Background()
			repo := domain.HashContent([]byte(t.Name()))
			if _, e := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID}); e != nil {
				t.Fatal(e)
			}
			if e := enqueueRepositoryNotification(ctx, st, record, "synthetic", "queued metadata"); e != nil {
				t.Fatal(e)
			}
			upgrade := func() {
				t.Helper()
				if e := st.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); e != nil {
					t.Fatal(e)
				}
			}
			wrapped := &workerNotificationStore{FSStore: st}
			svc.meta = wrapped
			if when == "before-claim" {
				upgrade()
			}
			if when == "before-egress" {
				wrapped.beforeGuard = upgrade
			}
			if when == "config-revoked" {
				wrapped.beforeGuard = func() {
					record.WebhookURL = ""
					if err := st.CreateRepository(ctx, record); err != nil {
						t.Fatal(err)
					}
				}
			}
			sends := 0
			old := safeWebhookClient()
			webhookClient = &http.Client{Transport: workerRoundTrip(func(r *http.Request) (*http.Response, error) {
				sends++
				if when == "after-send" {
					upgrade()
				}
				return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			})}
			defer func() { webhookClient = old }()
			roots := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
			ctx = inbound.WithDocumentIdentities(ctx, roots)
			ctx = outbound.WithDocumentIdentityCompatibility(ctx, roots, roots)
			worked, e := svc.ProcessNotification(ctx)
			if when == "config-revoked" {
				if !worked || !errors.Is(e, domain.ErrForbidden) {
					t.Fatal(worked, e)
				}
			} else if when == "before-claim" && !supported {
				if worked || e != nil {
					t.Fatal(worked, e)
				}
			} else if when == "legacy" || supported {
				if !worked || e != nil {
					t.Fatal(worked, e)
				}
			} else if !worked || !errors.Is(e, domain.ErrDocumentIdentityUpgradeRequired) {
				t.Fatal(worked, e)
			}
			if _, system := inbound.RepositoryActor(wrapped.seen); system {
				t.Fatal("notification gained system actor")
			}
			expected := 0
			if when != "config-revoked" && (supported || when == "after-send" || when == "legacy") {
				expected = 1
			}
			if sends != expected {
				t.Fatal("unexpected egress", sends)
			}
			jobs, e := st.ListNotifications(ctx, record.ID)
			if e != nil || len(jobs) != 1 {
				t.Fatal(jobs, e)
			}
			if when != "config-revoked" && (when == "legacy" || supported) {
				if jobs[0].State != "delivered" {
					t.Fatal(jobs)
				}
			} else if jobs[0].State == "delivered" {
				t.Fatal("receipt crossed opt-in")
			}
			if when == "before-claim" && !supported && jobs[0].Attempts != 0 {
				t.Fatal("unsupported claim spent attempt")
			}
		})
	}
}

func TestWorkerNotificationExactClaimGuard(t *testing.T) {
	_, st, record := notificationFixture(t)
	ctx := context.Background()
	if e := enqueueRepositoryNotification(ctx, st, record, "synthetic", "queued text"); e != nil {
		t.Fatal(e)
	}
	d, e := st.ClaimNotification(ctx, time.Now().UTC(), time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"text", "destination", "version", "expired"} {
		bad := d
		now := time.Now().UTC()
		switch mode {
		case "text":
			bad.Job.Text = "different"
		case "destination":
			bad.Destination += "/other"
		case "version":
			bad.Job.Version++
		case "expired":
			now = d.Job.LeaseUntil.Add(time.Second)
		}
		if e := st.ValidateNotificationDelivery(ctx, bad, now); !errors.Is(e, domain.ErrRefConflict) {
			t.Fatal(mode, e)
		}
	}
	if e := st.ValidateNotificationDelivery(ctx, d, time.Now().UTC()); e != nil {
		t.Fatal(e)
	}
	record.WebhookURL += "/changed"
	if e := st.CreateRepository(ctx, record); e != nil {
		t.Fatal(e)
	}
	if e := st.ValidateNotificationDelivery(ctx, d, time.Now().UTC()); !errors.Is(e, domain.ErrForbidden) {
		t.Fatal(e)
	}
}

func TestWorkerGitHubClaimsAndGrantPublicationPolicy(t *testing.T) {
	for _, when := range []string{"before-reconcile", "during-provider", "before-delivery"} {
		t.Run(when, func(t *testing.T) {
			g, st, f, remote := githubFixture(t)
			ctx := context.Background()
			ns := f.organization.NamespaceID
			state := startGitHub(t, g, f.owner.ID, ns)
			if _, e := g.Complete(ctx, f.owner.ID, state, "code"); e != nil {
				t.Fatal(e)
			}
			if e := g.reconcile(ctx, ns); e != nil {
				t.Fatal(e)
			}
			if e := f.identity.SetTeamRepository(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.repository.ID, domain.RolePuller); e != nil {
				t.Fatal(e)
			}
			repo := domain.HashContent([]byte(t.Name()))
			if _, e := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: f.repository.ID}); e != nil {
				t.Fatal(e)
			}
			c, e := st.GetGitHubConnection(ctx, ns)
			if e != nil {
				t.Fatal(e)
			}
			c.NextSync = time.Time{}
			c.Mappings = []domain.GitHubTeamMapping{{TeamID: f.team.ID, ExternalID: 99, SyncMembers: true}}
			c.Bindings = []domain.GitHubBinding{{ContextRepoID: repo, RepositoryID: f.repository.ID, ExternalID: 111}}
			if e = st.PutGitHubConnection(ctx, c); e != nil {
				t.Fatal(e)
			}
			upgrade := func() {
				t.Helper()
				if e := st.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); e != nil {
					t.Fatal(e)
				}
			}
			calls := 0
			remote.onMembers = func() {
				calls++
				if when == "during-provider" {
					upgrade()
				}
			}
			if when == "before-delivery" {
				body := []byte(`{"installation":{"id":789},"repository":{"id":111},"ref":"refs/heads/main"}`)
				if e := g.Receive(ctx, "synthetic", "push", body); e != nil {
					t.Fatal(e)
				}
				upgrade()
				if e := g.deliver(ctx, githubHash("synthetic")); !errors.Is(e, domain.ErrDocumentIdentityUpgradeRequired) {
					t.Fatal(e)
				}
				j, e := st.GetGitHubDelivery(ctx, githubHash("synthetic"))
				if e != nil || j.Attempts != 0 || j.Lease != "" || j.Done {
					t.Fatal(j, e)
				}
				return
			}
			if when == "before-reconcile" {
				upgrade()
			}
			if e := g.reconcile(ctx, ns); !errors.Is(e, domain.ErrDocumentIdentityUpgradeRequired) {
				t.Fatal(e)
			}
			current, e := st.GetGitHubConnection(ctx, ns)
			if e != nil {
				t.Fatal(e)
			}
			if when == "before-reconcile" && (!current.NextSync.IsZero() || calls != 0) {
				t.Fatal("unsupported claim changed connection", current, calls)
			}
			if when == "during-provider" && current.CheckedAt.After(c.CheckedAt) {
				t.Fatal("published after opt-in")
			}
		})
	}
}

type workerEntryStore struct {
	*store.FSStore
	seen []context.Context
}

func (s *workerEntryStore) WakePRSourceJobs(ctx context.Context, _ domain.ContentHash, _ time.Time) error {
	s.seen = append(s.seen, ctx)
	return nil
}
func (s *workerEntryStore) ClaimPRJob(ctx context.Context, _ domain.ContentHash, _ string, _ time.Time, _ time.Duration) (domain.PRPromotionJob, error) {
	s.seen = append(s.seen, ctx)
	return domain.PRPromotionJob{}, domain.ErrNotFound
}
func (s *workerEntryStore) ClaimGitChange(ctx context.Context, _ domain.ContentHash, _ string, _ time.Time, _ time.Duration) (domain.GitChangeJob, error) {
	s.seen = append(s.seen, ctx)
	return domain.GitChangeJob{}, domain.ErrNotFound
}
func (s *workerEntryStore) ClaimGitScan(ctx context.Context, _ domain.ContentHash, _ time.Time, _ time.Duration) (domain.GitScanJob, error) {
	s.seen = append(s.seen, ctx)
	return domain.GitScanJob{}, domain.ErrNotFound
}
func (s *workerEntryStore) ClaimGitHeadScan(ctx context.Context, _ domain.ContentHash, _ string, _ time.Time, _ time.Duration) (domain.GitHeadScan, error) {
	s.seen = append(s.seen, ctx)
	return domain.GitHeadScan{}, domain.ErrNotFound
}
func TestWorkerTrustedQueueEntryDeclarations(t *testing.T) {
	st := &workerEntryStore{FSStore: store.NewFSStore(t.TempDir())}
	s := &Service{meta: st, blobs: st}
	ctx := context.Background()
	root := []domain.DocumentIdentity{"future-unsupported"}
	ctx = inbound.WithDocumentIdentities(ctx, root)
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, root, root)
	if _, e := st.PutRepo(ctx, domain.Repo{ID: domain.HashContent([]byte(t.Name())), GitRemoteURL: "https://github.com/example/worker"}); e != nil {
		t.Fatal(e)
	}
	if e := s.ProcessPRPromotions(ctx, 1); e != nil {
		t.Fatal(e)
	}
	g := &GitChanges{core: s, gitChangeQuery: &gitChangeQuery{core: s, store: st}}
	if e := g.Process(ctx, 1); e != nil {
		t.Fatal(e)
	}
	scans := &GitScans{gitScanQuery: &gitScanQuery{core: s, store: st}}
	if e := scans.Process(ctx, 1); e != nil {
		t.Fatal(e)
	}
	if e := scans.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	if len(st.seen) != 5 {
		t.Fatal("entry not exercised", len(st.seen))
	}
	for _, bound := range st.seen {
		if _, system := inbound.RepositoryActor(bound); !system {
			t.Fatal("trusted worker lost existing actor")
		}
		if !reflect.DeepEqual(inbound.DocumentIdentities(bound), s.DocumentIdentitiesSupported()) {
			t.Fatal("actual declaration lost at queue entry")
		}
		wantErr := error(domain.ErrDocumentIdentityUpgradeRequired)
		if hasDocumentIdentity(s.DocumentIdentitiesSupported(), domain.DocumentIdentityRootV1) {
			wantErr = nil
		}
		if e := outbound.CheckDocumentIdentityCompatibility(bound, domain.DocumentIdentityRootV1); !errors.Is(e, wantErr) {
			t.Fatal("spoofed binary reached queue", e)
		}
	}
}

type workerGitHubStore struct {
	outbound.GitHubStore
	reads        int
	beforeFinish func()
}

func (s *workerGitHubStore) GetGitHubDelivery(ctx context.Context, id string) (domain.GitHubDelivery, error) {
	s.reads++
	if s.reads == 2 {
		s.beforeFinish()
	}
	return s.GitHubStore.GetGitHubDelivery(ctx, id)
}
func TestWorkerGitHubFinalDeliveryPolicy(t *testing.T) {
	g, st, f, _ := githubFixture(t)
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name()))
	if _, e := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: f.repository.ID}); e != nil {
		t.Fatal(e)
	}
	c := domain.GitHubConnection{NamespaceID: f.organization.NamespaceID, Enabled: true, Installation: domain.GitHubInstallation{ID: 789}, Bindings: []domain.GitHubBinding{{ContextRepoID: repo, RepositoryID: f.repository.ID, ExternalID: 111}}}
	if e := st.PutGitHubConnection(ctx, c); e != nil {
		t.Fatal(e)
	}
	body := []byte(`{"installation":{"id":789},"repository":{"id":111},"action":"opened"}`)
	if e := g.Receive(ctx, "synthetic-final", "pull_request", body); e != nil {
		t.Fatal(e)
	}
	g.store = &workerGitHubStore{GitHubStore: st, beforeFinish: func() {
		if e := st.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); e != nil {
			t.Fatal(e)
		}
	}}
	if e := g.deliver(ctx, githubHash("synthetic-final")); !errors.Is(e, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal(e)
	}
	current, e := st.GetGitHubDelivery(ctx, githubHash("synthetic-final"))
	if e != nil || current.Done || len(current.Body) == 0 || current.Attempts != 1 || current.Lease == "" {
		t.Fatal("final state crossed opt-in", current, e)
	}
}
