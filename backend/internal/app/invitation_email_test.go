package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type fakeInvitationMailer struct {
	mu       sync.Mutex
	messages []domain.EmailMessage
	keys     []string
	err      error
}

func (m *fakeInvitationMailer) Send(_ context.Context, key string, message domain.EmailMessage) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, message)
	m.keys = append(m.keys, key)
	return "provider-id", m.err
}

type emailTestStore interface {
	invitationTestStore
	outbound.InvitationEmailStore
}

func TestInvitationEmailLifecycle(t *testing.T) {
	runInvitationEmailLifecycle(t, store.NewFSStore(t.TempDir()))
}
func runInvitationEmailLifecycle(t *testing.T, st emailTestStore) {
	ctx := systemTestContext()
	for _, kind := range []string{"organization", "enterprise"} {
		t.Run(kind, func(t *testing.T) {
			f := makeTeamFixture(t, st)
			s := f.identity
			space := f.organization.ID
			if kind == "enterprise" {
				e, err := s.CreateEnterprise(ctx, f.owner, "Example <b>Company</b>", "email-"+domain.NewID("")[:10])
				if err != nil {
					t.Fatal(err)
				}
				space = e.ID
			}
			m := &fakeInvitationMailer{err: &outbound.EmailDeliveryError{Reason: "transport_failed", Retryable: true}}
			if err := s.ConfigureInvitationEmail(m, "CXTHub <from@example.test>", "https://example.test"); err != nil {
				t.Fatal(err)
			}
			i, err := s.CreateCollaborationInvitation(ctx, f.owner.ID, kind, space, f.outsider.Email, domain.OrganizationMember, 7)
			if err != nil {
				t.Fatal(err)
			}
			if !i.EmailEnabled || i.EmailStatus != "queued" {
				t.Fatal(i)
			}
			if _, member := s.OrganizationRoleOf(ctx, space, f.outsider.ID); member {
				t.Fatal("creation implicitly accepted")
			}
			worked, err := s.ProcessInvitationEmail(ctx)
			if !worked || err != nil {
				t.Fatal(worked, err)
			}
			job, err := st.GetInvitationEmail(ctx, i.ID)
			if err != nil || job.State != "retrying" || job.Attempts != 1 {
				t.Fatal(job, err)
			}
			if !strings.Contains(job.Message.Text, "https://example.test/invite/"+i.ID) || job.Message.To[0] != f.outsider.Email {
				t.Fatal("wrong recipient/link")
			}
			if kind == "enterprise" && (strings.Contains(job.Message.HTML, "<b>Company</b>") || !strings.Contains(job.Message.HTML, "&lt;b&gt;Company&lt;/b&gt;")) {
				t.Fatal("unescaped name")
			}
			// Simulate restart, a due retry and changed configuration: frozen payload/key survive.
			job.NextAttempt = time.Now().Add(-time.Second)
			if err := st.PutInvitationEmail(ctx, job); err != nil {
				t.Fatal(err)
			}
			restart := NewIdentityService(nil, st)
			m.err = nil
			if err := restart.ConfigureInvitationEmail(m, "New <new@example.test>", "https://new.example.test"); err != nil {
				t.Fatal(err)
			}
			if _, err := restart.ProcessInvitationEmail(ctx); err != nil {
				t.Fatal(err)
			}
			job, _ = st.GetInvitationEmail(ctx, i.ID)
			if job.State != "accepted" || job.ProviderID != "provider-id" || len(m.keys) != 2 || m.keys[0] != m.keys[1] || !reflect.DeepEqual(m.messages[0], m.messages[1]) {
				t.Fatal("retry contract", job)
			}
			if worked, err := restart.ProcessInvitationEmail(ctx); worked || err != nil {
				t.Fatal("sent twice", worked, err)
			}
			renewed, err := restart.ActOnCollaborationInvitation(ctx, f.owner.ID, i.ID, "resend")
			if err != nil {
				t.Fatal(err)
			}
			if renewed.ID == i.ID || renewed.EmailStatus != "queued" {
				t.Fatal("resend must create fresh invitation")
			}
			if _, err := restart.ActOnCollaborationInvitation(ctx, f.owner.ID, renewed.ID, "revoke"); err != nil {
				t.Fatal(err)
			}
			if _, err := restart.ProcessInvitationEmail(ctx); err != nil {
				t.Fatal(err)
			}
			job, _ = st.GetInvitationEmail(ctx, renewed.ID)
			if job.State != "cancelled" || len(m.keys) != 2 {
				t.Fatal("revoked email sent", job)
			}
		})
	}
}
func TestInvitationEmailDisabledAndRetryWindow(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	f := makeTeamFixture(t, st)
	s := f.identity
	ctx := systemTestContext()
	i, err := s.CreateCollaborationInvitation(ctx, f.owner.ID, "organization", f.organization.ID, f.outsider.Email, domain.OrganizationMember, 7)
	if err != nil {
		t.Fatal(err)
	}
	if i.EmailEnabled || i.EmailStatus != "not_sent" {
		t.Fatal(i)
	}
	m := &fakeInvitationMailer{}
	if err := s.ConfigureInvitationEmail(m, "from@example.test", "https://example.test"); err != nil {
		t.Fatal(err)
	}
	if worked, err := s.ProcessInvitationEmail(ctx); worked || err != nil {
		t.Fatal("enabling email must not send old invites")
	}
	i, err = s.ActOnCollaborationInvitation(ctx, f.owner.ID, i.ID, "resend")
	if err != nil {
		t.Fatal(err)
	}
	j, _ := st.GetInvitationEmail(ctx, i.ID)
	j.State = "sending"
	j.Attempts = 1
	j.FirstAttempt = time.Now().Add(-24 * time.Hour)
	j.NextAttempt = time.Now().Add(-time.Minute)
	if err := st.PutInvitationEmail(ctx, j); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessInvitationEmail(ctx); err != nil {
		t.Fatal(err)
	}
	j, _ = st.GetInvitationEmail(ctx, i.ID)
	if j.State != "attention" || j.Reason != "retry_window_expired" || len(m.keys) != 0 {
		t.Fatal("unsafe retry", j)
	}
	if _, err := s.ActOnCollaborationInvitation(ctx, f.outsider.ID, i.ID, "accept"); err != nil {
		t.Fatal("email failure must not prevent inbox acceptance", err)
	}
}
func TestInvitationEmailConfiguration(t *testing.T) {
	s := NewIdentityService(nil, store.NewFSStore(t.TempDir()))
	for _, base := range []string{"http://public.example.test", "https://user:pass@example.test", "https://example.test?override=yes", "https://example.test/path"} {
		if s.ConfigureInvitationEmail(&fakeInvitationMailer{}, "from@example.test", base) == nil {
			t.Fatal("unsafe URL accepted", base)
		}
	}
}
func TestInvitationEmailConcurrentClaims(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	runInvitationEmailConcurrentClaims(t, st, st)
}
func runInvitationEmailConcurrentClaims(t *testing.T, st, peer emailTestStore) {
	f := makeTeamFixture(t, st)
	ctx := systemTestContext()
	m := &fakeInvitationMailer{}
	services := []*IdentityService{f.identity, NewIdentityService(nil, peer)}
	for _, s := range services {
		if err := s.ConfigureInvitationEmail(m, "from@example.test", "https://example.test"); err != nil {
			t.Fatal(err)
		}
	}
	i, err := services[0].CreateCollaborationInvitation(ctx, f.owner.ID, "organization", f.organization.ID, f.outsider.Email, domain.OrganizationMember, 7)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if _, err := services[n%2].ProcessInvitationEmail(ctx); err != nil && !errors.Is(err, domain.ErrConflict) {
				t.Error(err)
			}
		}(n)
	}
	wg.Wait()
	j, _ := st.GetInvitationEmail(ctx, i.ID)
	if len(m.keys) != 1 || j.State != "accepted" {
		t.Fatal("claim overlap", len(m.keys), j)
	}
}

type invitationMailFunc func(context.Context, string, domain.EmailMessage) (string, error)

func (f invitationMailFunc) Send(ctx context.Context, key string, m domain.EmailMessage) (string, error) {
	return f(ctx, key, m)
}
func TestInvitationEmailStaleWorkerFence(t *testing.T) {
	runInvitationEmailStaleWorkerFence(t, store.NewFSStore(t.TempDir()))
}
func runInvitationEmailStaleWorkerFence(t *testing.T, st emailTestStore) {
	ctx := systemTestContext()
	f := makeTeamFixture(t, st)
	entered, release := make(chan struct{}), make(chan struct{})
	slow := invitationMailFunc(func(ctx context.Context, _ string, _ domain.EmailMessage) (string, error) {
		close(entered)
		select {
		case <-release:
			return "old-provider-response", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	if err := f.identity.ConfigureInvitationEmail(slow, "from@example.test", "https://example.test"); err != nil {
		t.Fatal(err)
	}
	i, err := f.identity.CreateCollaborationInvitation(ctx, f.owner.ID, "organization", f.organization.ID, f.outsider.Email, domain.OrganizationMember, 7)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.identity.ProcessInvitationEmail(ctx); done <- err }()
	<-entered
	// Simulate an expired lease while the first provider response is delayed.
	if err := f.identity.withIdentity(ctx, func(ctx context.Context) error {
		j, err := st.GetInvitationEmail(ctx, i.ID)
		if err != nil {
			return err
		}
		j.NextAttempt = time.Now().Add(-time.Second)
		return st.PutInvitationEmail(ctx, j)
	}); err != nil {
		t.Fatal(err)
	}
	peer := NewIdentityService(nil, st)
	m := &fakeInvitationMailer{}
	if err := peer.ConfigureInvitationEmail(m, "from@example.test", "https://example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.ProcessInvitationEmail(ctx); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale finish was not fenced", err)
	}
	j, _ := st.GetInvitationEmail(ctx, i.ID)
	if j.ProviderID != "provider-id" || j.Attempts != 2 || j.State != "accepted" {
		t.Fatal("stale response overwrote new owner", j)
	}
}
func TestInvitationEmailCancellationAndRejection(t *testing.T) {
	for _, action := range []string{"accept", "expire", "demote", "permanent", "inflight_revoke"} {
		t.Run(action, func(t *testing.T) {
			st := store.NewFSStore(t.TempDir())
			f := makeTeamFixture(t, st)
			s := f.identity
			ctx := systemTestContext()
			m := &fakeInvitationMailer{}
			if err := s.ConfigureInvitationEmail(m, "from@example.test", "https://example.test"); err != nil {
				t.Fatal(err)
			}
			i, err := s.CreateCollaborationInvitation(ctx, f.owner.ID, "organization", f.organization.ID, f.outsider.Email, domain.OrganizationMember, 7)
			if err != nil {
				t.Fatal(err)
			}
			switch action {
			case "accept":
				_, err = s.ActOnCollaborationInvitation(ctx, f.outsider.ID, i.ID, "accept")
			case "expire":
				raw := i.CollaborationInvite
				raw.CreatedAt = time.Now().Add(-8 * 24 * time.Hour)
				raw.ExpiresAt = time.Now().Add(-time.Hour)
				err = st.PutCollaborationInvite(ctx, raw)
			case "demote":
				err = s.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationOwner)
				if err == nil {
					err = s.UpdateOrganizationMember(ctx, f.member.ID, f.organization.ID, f.owner.ID, domain.OrganizationMember)
				}
			case "permanent":
				m.err = &outbound.EmailDeliveryError{Reason: "invalid_api_key"}
			case "inflight_revoke":
				mailer := invitationMailFunc(func(ctx context.Context, _ string, _ domain.EmailMessage) (string, error) {
					bounded, cancel := context.WithTimeout(ctx, time.Second)
					defer cancel()
					// This call would deadlock if network IO held the identity transaction.
					_, err := s.ActOnCollaborationInvitation(bounded, f.owner.ID, i.ID, "revoke")
					return "already-in-flight", err
				})
				err = s.ConfigureInvitationEmail(mailer, "from@example.test", "https://example.test")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ProcessInvitationEmail(ctx); err != nil {
				t.Fatal(err)
			}
			j, _ := st.GetInvitationEmail(ctx, i.ID)
			switch action {
			case "permanent":
				if j.State != "attention" || j.Reason != "invalid_api_key" {
					t.Fatal(j)
				}
			case "inflight_revoke":
				if j.State != "accepted" {
					t.Fatal(j)
				}
				if _, err := s.ActOnCollaborationInvitation(ctx, f.outsider.ID, i.ID, "accept"); !errors.Is(err, domain.ErrConflict) {
					t.Fatal("in-flight email revived revoked invite", err)
				}
			default:
				if j.State != "cancelled" || len(m.keys) != 0 {
					t.Fatal("inactive email sent", j)
				}
			}
		})
	}
}

type unavailableInvitationAuthority struct {
	*store.FSStore
	unavailable bool
}

func (s *unavailableInvitationAuthority) GetOrganizationMembership(ctx context.Context, org, user string) (domain.OrganizationMembership, error) {
	if s.unavailable {
		return domain.OrganizationMembership{}, errors.New("membership store unavailable")
	}
	return s.FSStore.GetOrganizationMembership(ctx, org, user)
}
func TestInvitationEmailAuthorityOutageDoesNotCancel(t *testing.T) {
	st := &unavailableInvitationAuthority{FSStore: store.NewFSStore(t.TempDir())}
	f := makeTeamFixture(t, st)
	ctx := systemTestContext()
	m := &fakeInvitationMailer{}
	if err := f.identity.ConfigureInvitationEmail(m, "from@example.test", "https://example.test"); err != nil {
		t.Fatal(err)
	}
	i, err := f.identity.CreateCollaborationInvitation(ctx, f.owner.ID, "organization", f.organization.ID, f.outsider.Email, domain.OrganizationMember, 7)
	if err != nil {
		t.Fatal(err)
	}
	st.unavailable = true
	if _, err := f.identity.ProcessInvitationEmail(ctx); err == nil {
		t.Fatal("storage error ignored")
	}
	j, _ := st.GetInvitationEmail(ctx, i.ID)
	if j.State != "queued" || j.Attempts != 0 || len(m.keys) != 0 {
		t.Fatal("temporary error cancelled mail", j)
	}
	st.unavailable = false
	if _, err := f.identity.ProcessInvitationEmail(ctx); err != nil {
		t.Fatal(err)
	}
	j, _ = st.GetInvitationEmail(ctx, i.ID)
	if j.State != "accepted" {
		t.Fatal(j)
	}
}
