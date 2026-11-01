//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type invitationEmailPGFixture struct {
	ctx        context.Context
	storage    *store.PostgresStore
	peer       *store.PostgresStore
	observer   *pgx.Conn
	team       teamFixture
	invitation domain.CollaborationInvite
	queued     domain.InvitationEmail
}

func newInvitationEmailPGFixture(test *testing.T, kind string, role domain.OrganizationRole) *invitationEmailPGFixture {
	test.Helper()
	wave40IsolatedPG(test)
	_, storage, _ := collaborationPG(test)
	ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), storage), 30*time.Second)
	test.Cleanup(cancel)
	team := makeTeamFixture(test, storage)
	space := team.organization.ID
	if kind == "enterprise" {
		enterprise, err := team.identity.CreateEnterprise(ctx, team.owner, "Invitation <b>owner</b>", "email-owner-"+domain.NewID("")[:10])
		if err != nil {
			test.Fatal(err)
		}
		space = enterprise.ID
	}
	if err := team.identity.ConfigureInvitationEmail(&fakeInvitationMailer{}, "Original <from@example.test>", "https://original.example.test"); err != nil {
		test.Fatal(err)
	}
	invitation, err := team.identity.CreateCollaborationInvitation(ctx, team.owner.ID, kind, space, team.outsider.Email, role, 7)
	if err != nil {
		test.Fatal(err)
	}
	queued, err := storage.GetInvitationEmail(ctx, invitation.ID)
	if err != nil || queued.State != "queued" || queued.Attempts != 0 {
		test.Fatal("fixture did not enqueue a real invitation email", queued, err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(peer.Close)
	config, err := pgxpool.ParseConfig(collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	observer, err := pgx.ConnectConfig(ctx, config.ConnConfig.Copy())
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := observer.Close(cleanup); err != nil {
			test.Error(err)
		}
	})
	return &invitationEmailPGFixture{ctx: ctx, storage: storage, peer: peer, observer: observer, team: team, invitation: invitation.CollaborationInvite, queued: queued}
}

func (fixture *invitationEmailPGFixture) observedEmail(ctx context.Context) (domain.InvitationEmail, error) {
	var raw []byte
	if err := fixture.observer.QueryRow(ctx, `SELECT record FROM invitation_emails WHERE invitation_id=$1`, fixture.invitation.ID).Scan(&raw); err != nil {
		return domain.InvitationEmail{}, err
	}
	var email domain.InvitationEmail
	if err := json.Unmarshal(raw, &email); err != nil {
		return email, err
	}
	return email, domain.ValidateInvitationEmail(email)
}

func putInvitationEmailPG(test *testing.T, ctx context.Context, storage *store.PostgresStore, email domain.InvitationEmail) {
	test.Helper()
	if err := storage.WithinIdentity(ctx, func(bound context.Context) error {
		return storage.PutInvitationEmail(bound, email)
	}); err != nil {
		test.Fatal(err)
	}
}

type invitationEmailPGObservedStore struct {
	*store.PostgresStore
	clock             time.Time
	transactions      int
	claims            int
	organizationReads int
	enterpriseReads   int
	puts              []domain.InvitationEmail
	authorityFailure  error
	afterPut          func(context.Context, domain.InvitationEmail) error
}

func (storage *invitationEmailPGObservedStore) identityScope(ctx context.Context) error {
	if !storage.InTransaction(ctx) || storage.InReadOnlyTransaction(ctx) {
		return domain.ErrIntegrity
	}
	return nil
}

func (storage *invitationEmailPGObservedStore) WithinIdentity(ctx context.Context, operation func(context.Context) error) error {
	storage.transactions++
	return storage.PostgresStore.WithinIdentity(ctx, func(bound context.Context) error {
		if err := storage.identityScope(bound); err != nil {
			return err
		}
		return operation(bound)
	})
}

func (storage *invitationEmailPGObservedStore) NextInvitationEmail(ctx context.Context) (domain.InvitationEmail, time.Time, error) {
	storage.claims++
	if err := storage.identityScope(ctx); err != nil {
		return domain.InvitationEmail{}, time.Time{}, err
	}
	email, now, err := storage.PostgresStore.NextInvitationEmail(ctx)
	if err == nil {
		storage.clock = now
	}
	return email, now, err
}

func (storage *invitationEmailPGObservedStore) GetInvitationEmail(ctx context.Context, invitation string) (domain.InvitationEmail, error) {
	if err := storage.identityScope(ctx); err != nil {
		return domain.InvitationEmail{}, err
	}
	return storage.PostgresStore.GetInvitationEmail(ctx, invitation)
}

func (storage *invitationEmailPGObservedStore) PutInvitationEmail(ctx context.Context, email domain.InvitationEmail) error {
	if err := storage.identityScope(ctx); err != nil {
		return err
	}
	if err := storage.PostgresStore.PutInvitationEmail(ctx, email); err != nil {
		return err
	}
	storage.puts = append(storage.puts, email)
	if storage.afterPut != nil {
		return storage.afterPut(ctx, email)
	}
	return nil
}

func (storage *invitationEmailPGObservedStore) GetCollaborationInvite(ctx context.Context, invitation string) (domain.CollaborationInvite, error) {
	if err := storage.identityScope(ctx); err != nil {
		return domain.CollaborationInvite{}, err
	}
	return storage.PostgresStore.GetCollaborationInvite(ctx, invitation)
}

func (storage *invitationEmailPGObservedStore) GetOrganizationMembership(ctx context.Context, organization, actor string) (domain.OrganizationMembership, error) {
	storage.organizationReads++
	if err := storage.identityScope(ctx); err != nil {
		return domain.OrganizationMembership{}, err
	}
	if storage.authorityFailure != nil {
		return domain.OrganizationMembership{}, storage.authorityFailure
	}
	return storage.PostgresStore.GetOrganizationMembership(ctx, organization, actor)
}

func (storage *invitationEmailPGObservedStore) ListEnterpriseMembers(ctx context.Context, enterprise string) ([]domain.EnterpriseMembership, error) {
	storage.enterpriseReads++
	if err := storage.identityScope(ctx); err != nil {
		return nil, err
	}
	if storage.authorityFailure != nil {
		return nil, storage.authorityFailure
	}
	return storage.PostgresStore.ListEnterpriseMembers(ctx, enterprise)
}

func newInvitationEmailPGOwner(test *testing.T, storage *invitationEmailPGObservedStore, sender outbound.InvitationMailer) *InvitationEmailWorker {
	test.Helper()
	owner, err := NewInvitationEmailWorker(InvitationEmailWorkerConfig{
		Profile: InvitationEmailComplete, Queue: storage, Invitations: storage,
		Organizations: storage, Enterprises: storage, Transactions: storage, Sender: sender,
	})
	if err != nil {
		test.Fatal("complete public invitation email owner", err)
	}
	return owner
}

func TestPGInvitationEmailWorkerIdentityConfigurationUsesCurrentPorts(test *testing.T) {
	fixture := newInvitationEmailPGFixture(test, "organization", domain.OrganizationMember)
	config := fixture.team.identity.InvitationEmailWorkerConfig(InvitationEmailComplete)
	if config.Queue != fixture.storage || config.Invitations != fixture.storage || config.Organizations != fixture.storage || config.Enterprises != fixture.storage || config.Transactions != fixture.storage || config.Sender != fixture.team.identity.invitationEmail.sender {
		test.Fatal("API configuration replaced current invitation dependencies")
	}
	sender := &fakeInvitationMailer{}
	config.Sender = sender
	owner, err := NewInvitationEmailWorker(config)
	if err != nil {
		test.Fatal("actual identity configuration cannot assemble the complete owner", err)
	}
	if worked, err := owner.ProcessInvitationEmail(fixture.ctx); !worked || err != nil {
		test.Fatal("configured public owner did not process the actual queue", worked, err)
	}
	current, err := fixture.observedEmail(fixture.ctx)
	if err != nil || current.State != "accepted" || current.Attempts != 1 || len(sender.keys) != 1 || sender.keys[0] != "collaboration-invite/"+fixture.invitation.ID || !reflect.DeepEqual(sender.messages[0], fixture.queued.Message) {
		test.Fatal("configured public owner lost the persisted invitation", current, err)
	}
}

func TestPGInvitationEmailWorkerDirectClaimAndCompletionRollback(test *testing.T) {
	for _, failureState := range []string{"sending", "accepted"} {
		test.Run(failureState, func(test *testing.T) {
			fixture := newInvitationEmailPGFixture(test, "organization", domain.OrganizationMember)
			storage := &invitationEmailPGObservedStore{PostgresStore: fixture.storage}
			failure := errors.New("injected invitation email write rollback")
			injected, sent := false, 0
			storage.afterPut = func(bound context.Context, email domain.InvitationEmail) error {
				current, err := fixture.storage.GetInvitationEmail(bound, email.InvitationID)
				if err != nil || !reflect.DeepEqual(current, email) {
					return errors.Join(domain.ErrIntegrity, err)
				}
				outside, err := fixture.observedEmail(fixture.ctx)
				if err != nil {
					return err
				}
				if email.State == "sending" {
					if !reflect.DeepEqual(outside, fixture.queued) || email.Attempts != 1 || !email.FirstAttempt.Equal(storage.clock) || !email.NextAttempt.Equal(storage.clock.Add(2*time.Minute)) {
						return domain.ErrIntegrity
					}
				} else if outside.State != "sending" || outside.Attempts != 1 || outside.ProviderID != "" {
					return domain.ErrIntegrity
				}
				if email.State == failureState {
					injected = true
					return failure
				}
				return nil
			}
			sender := invitationMailFunc(func(sendCtx context.Context, key string, message domain.EmailMessage) (string, error) {
				sent++
				deadline, bounded := sendCtx.Deadline()
				if fixture.storage.InTransaction(sendCtx) || !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 10*time.Second || key != "collaboration-invite/"+fixture.invitation.ID || !reflect.DeepEqual(message, fixture.queued.Message) {
					return "", domain.ErrIntegrity
				}
				if err := fixture.peer.WithinIdentity(sendCtx, func(bound context.Context) error {
					current, err := fixture.peer.GetInvitationEmail(bound, fixture.invitation.ID)
					if err != nil || current.State != "sending" || current.Attempts != 1 {
						return errors.Join(domain.ErrIntegrity, err)
					}
					return nil
				}); err != nil {
					return "", err
				}
				return "direct-owner-provider", nil
			})
			owner := newInvitationEmailPGOwner(test, storage, sender)
			worked, err := owner.ProcessInvitationEmail(fixture.ctx)
			if !errors.Is(err, failure) || !injected || worked != (failureState == "accepted") {
				test.Fatal("direct owner did not reach the real rollback boundary", worked, injected, err)
			}
			current, err := fixture.observedEmail(fixture.ctx)
			if err != nil {
				test.Fatal(err)
			}
			if failureState == "sending" {
				if !reflect.DeepEqual(current, fixture.queued) || sent != 0 || storage.transactions != 1 {
					test.Fatal("claim write escaped rollback or sent before commit", current, sent, storage.transactions)
				}
			} else {
				if current.State != "sending" || current.Attempts != 1 || current.ProviderID != "" || sent != 1 || storage.transactions != 2 {
					test.Fatal("failed completion lost the committed claim or leaked provider acceptance", current, sent, storage.transactions)
				}
				current.NextAttempt = time.Now().Add(-time.Second)
				putInvitationEmailPG(test, fixture.ctx, fixture.peer, current)
			}
			recoveredSender := &fakeInvitationMailer{}
			restarted := newInvitationEmailPGOwner(test, &invitationEmailPGObservedStore{PostgresStore: fixture.peer}, recoveredSender)
			if worked, err := restarted.ProcessInvitationEmail(fixture.ctx); !worked || err != nil {
				test.Fatal("independent owner could not recover rolled-back work", worked, err)
			}
			recovered, err := fixture.observedEmail(fixture.ctx)
			if err != nil || recovered.State != "accepted" || recovered.Attempts != sent+1 || recovered.ProviderID != "provider-id" || len(recoveredSender.keys) != 1 || recoveredSender.keys[0] != "collaboration-invite/"+fixture.invitation.ID || !reflect.DeepEqual(recoveredSender.messages[0], fixture.queued.Message) {
				test.Fatal("recovery changed the stored message or idempotency identity", recovered, err)
			}
		})
	}
}

func TestPGInvitationEmailWorkerRechecksCurrentIssuerRole(test *testing.T) {
	for _, kind := range []string{"organization", "enterprise"} {
		for _, target := range []domain.OrganizationRole{domain.OrganizationMember, domain.OrganizationAdmin} {
			test.Run(kind+"/"+string(target), func(test *testing.T) {
				fixture := newInvitationEmailPGFixture(test, kind, target)
				storage := &invitationEmailPGObservedStore{PostgresStore: fixture.storage}
				sender := &fakeInvitationMailer{}
				owner := newInvitationEmailPGOwner(test, storage, sender)
				next := domain.OrganizationMember
				if target == domain.OrganizationAdmin {
					next = domain.OrganizationAdmin
				}
				commands := NewIdentityService(nil, fixture.peer)
				if kind == "organization" {
					if err := commands.UpdateOrganizationMember(fixture.ctx, fixture.team.owner.ID, fixture.invitation.SpaceID, fixture.team.member.ID, domain.OrganizationOwner); err != nil {
						test.Fatal(err)
					}
					if err := commands.UpdateOrganizationMember(fixture.ctx, fixture.team.member.ID, fixture.invitation.SpaceID, fixture.team.owner.ID, next); err != nil {
						test.Fatal(err)
					}
				} else {
					if err := commands.UpdateEnterpriseMember(fixture.ctx, fixture.team.owner.ID, fixture.invitation.SpaceID, fixture.team.member.ID, domain.EnterpriseOwner); err != nil {
						test.Fatal(err)
					}
					if err := commands.UpdateEnterpriseMember(fixture.ctx, fixture.team.member.ID, fixture.invitation.SpaceID, fixture.team.owner.ID, domain.EnterpriseRole(next)); err != nil {
						test.Fatal(err)
					}
				}
				if worked, err := owner.ProcessInvitationEmail(fixture.ctx); !worked || err != nil {
					test.Fatal(worked, err)
				}
				current, err := fixture.observedEmail(fixture.ctx)
				if err != nil || current.State != "cancelled" || current.Reason != "issuer_not_authorized" || current.Attempts != 0 || len(sender.keys) != 0 || storage.organizationReads+storage.enterpriseReads != 1 || storage.transactions != 1 {
					test.Fatal("owner used stale authority or ignored owner-only target role", current, err)
				}
			})
		}
	}
}

func TestPGInvitationEmailWorkerAuthorityOutageRollsBackWithoutCancellation(test *testing.T) {
	for _, kind := range []string{"organization", "enterprise"} {
		test.Run(kind, func(test *testing.T) {
			fixture := newInvitationEmailPGFixture(test, kind, domain.OrganizationMember)
			failure := errors.New("invitation issuer membership unavailable")
			storage := &invitationEmailPGObservedStore{PostgresStore: fixture.storage, authorityFailure: failure}
			sender := &fakeInvitationMailer{}
			owner := newInvitationEmailPGOwner(test, storage, sender)
			if worked, err := owner.ProcessInvitationEmail(fixture.ctx); worked || !errors.Is(err, failure) {
				test.Fatal("membership outage was not returned", worked, err)
			}
			current, err := fixture.observedEmail(fixture.ctx)
			if err != nil || !reflect.DeepEqual(current, fixture.queued) || storage.claims != 1 || storage.transactions != 1 || len(storage.puts) != 0 || storage.organizationReads+storage.enterpriseReads != 1 || len(sender.keys) != 0 {
				test.Fatal("temporary membership outage consumed or cancelled durable work", current, err)
			}
			storage.authorityFailure = nil
			if worked, err := owner.ProcessInvitationEmail(fixture.ctx); !worked || err != nil {
				test.Fatal("owner did not recover after membership returned", worked, err)
			}
			current, err = fixture.observedEmail(fixture.ctx)
			if err != nil || current.State != "accepted" || current.Attempts != 1 || len(sender.keys) != 1 || storage.transactions != 3 {
				test.Fatal("outage recovery changed claim ownership", current, err)
			}
		})
	}
}

func TestPGInvitationEmailWorkerDoesNotReviveInactiveInvitations(test *testing.T) {
	for _, action := range []string{"revoke", "expire", "accept"} {
		test.Run(action, func(test *testing.T) {
			fixture := newInvitationEmailPGFixture(test, "organization", domain.OrganizationMember)
			sender := &fakeInvitationMailer{}
			owner := newInvitationEmailPGOwner(test, &invitationEmailPGObservedStore{PostgresStore: fixture.storage}, sender)
			commands := NewIdentityService(nil, fixture.peer)
			if action == "expire" {
				if err := fixture.peer.WithinIdentity(fixture.ctx, func(bound context.Context) error {
					invitation, err := fixture.peer.GetCollaborationInvite(bound, fixture.invitation.ID)
					if err != nil {
						return err
					}
					invitation.CreatedAt = time.Now().Add(-8 * 24 * time.Hour)
					invitation.ExpiresAt = time.Now().Add(-time.Hour)
					return fixture.peer.PutCollaborationInvite(bound, invitation)
				}); err != nil {
					test.Fatal(err)
				}
			} else {
				actor := fixture.team.owner.ID
				if action == "accept" {
					actor = fixture.team.outsider.ID
				}
				if _, err := commands.ActOnCollaborationInvitation(fixture.ctx, actor, fixture.invitation.ID, action); err != nil {
					test.Fatal(err)
				}
			}
			if worked, err := owner.ProcessInvitationEmail(fixture.ctx); !worked || err != nil {
				test.Fatal(worked, err)
			}
			current, err := fixture.observedEmail(fixture.ctx)
			if err != nil || current.State != "cancelled" || current.Reason != "invitation_inactive" || current.Attempts != 0 || len(sender.keys) != 0 {
				test.Fatal("inactive invitation was sent", current, err)
			}
			if action != "accept" {
				if _, err := commands.ActOnCollaborationInvitation(fixture.ctx, fixture.team.outsider.ID, fixture.invitation.ID, "accept"); !errors.Is(err, domain.ErrConflict) {
					test.Fatal("cancelled mail revived invitation acceptance", err)
				}
			}
		})
	}
}

func TestPGInvitationEmailWorkerRetryRestartKeepsStoredPayloadAndKey(test *testing.T) {
	fixture := newInvitationEmailPGFixture(test, "enterprise", domain.OrganizationMember)
	storage := &invitationEmailPGObservedStore{PostgresStore: fixture.storage}
	sender := &fakeInvitationMailer{err: &outbound.EmailDeliveryError{Reason: "rate_limited", Retryable: true, RetryAfter: 5 * time.Minute}}
	owner := newInvitationEmailPGOwner(test, storage, sender)
	if worked, err := owner.ProcessInvitationEmail(fixture.ctx); !worked || err != nil {
		test.Fatal(worked, err)
	}
	retrying, err := fixture.observedEmail(fixture.ctx)
	if err != nil || retrying.State != "retrying" || retrying.Reason != "rate_limited" || retrying.Attempts != 1 || !retrying.FirstAttempt.Equal(storage.clock) || !retrying.NextAttempt.Equal(storage.clock.Add(5*time.Minute+10*time.Second)) || retrying.ProviderID != "" {
		test.Fatal("owner changed storage-clock retry-after or replica request allowance", retrying, err)
	}
	if worked, err := owner.ProcessInvitationEmail(fixture.ctx); worked || err != nil || len(sender.keys) != 1 {
		test.Fatal("owner sent a retry before its durable deadline", worked, err)
	}
	commands := NewIdentityService(nil, fixture.peer)
	if err := commands.ConfigureInvitationEmail(&fakeInvitationMailer{}, "Changed <new@example.test>", "https://new.example.test"); err != nil {
		test.Fatal(err)
	}
	retrying.NextAttempt = time.Now().Add(-time.Second)
	putInvitationEmailPG(test, fixture.ctx, fixture.peer, retrying)
	restartedSender := &fakeInvitationMailer{}
	restarted := newInvitationEmailPGOwner(test, &invitationEmailPGObservedStore{PostgresStore: fixture.peer}, restartedSender)
	if worked, err := restarted.ProcessInvitationEmail(fixture.ctx); !worked || err != nil {
		test.Fatal(worked, err)
	}
	accepted, err := fixture.observedEmail(fixture.ctx)
	if err != nil || accepted.State != "accepted" || accepted.ProviderID != "provider-id" || accepted.Attempts != 2 || !accepted.FirstAttempt.Equal(retrying.FirstAttempt) || len(restartedSender.keys) != 1 || restartedSender.keys[0] != sender.keys[0] || !reflect.DeepEqual(restartedSender.messages[0], sender.messages[0]) || !reflect.DeepEqual(accepted.Message, fixture.queued.Message) {
		test.Fatal("restarted public owner changed frozen message or idempotency key", accepted, err)
	}
	if worked, err := restarted.ProcessInvitationEmail(fixture.ctx); worked || err != nil || len(restartedSender.keys) != 1 {
		test.Fatal("provider acceptance was sent again", worked, err)
	}
}

func TestPGInvitationEmailWorkerRetryBoundsAndAttention(test *testing.T) {
	fixture := newInvitationEmailPGFixture(test, "organization", domain.OrganizationMember)
	for _, scenario := range []struct {
		name     string
		attempts int
		age      time.Duration
		failure  error
		state    string
		reason   string
		delay    time.Duration
		sends    int
	}{
		{name: "transport", failure: errors.New("unclassified transport failure"), state: "retrying", reason: "transport_failed", delay: time.Minute, sends: 1},
		{name: "capped_backoff", attempts: 8, age: time.Hour, failure: &outbound.EmailDeliveryError{Reason: "transport_failed", Retryable: true}, state: "retrying", reason: "transport_failed", delay: time.Hour, sends: 1},
		{name: "last_send", attempts: 11, age: time.Hour, failure: &outbound.EmailDeliveryError{Reason: "transport_failed", Retryable: true}, state: "attention", reason: "transport_failed", sends: 1},
		{name: "window_crossing", attempts: 1, age: 23*time.Hour - 30*time.Second, failure: &outbound.EmailDeliveryError{Reason: "transport_failed", Retryable: true}, state: "attention", reason: "retry_window_expired", delay: 2 * time.Minute, sends: 1},
		{name: "permanent", failure: &outbound.EmailDeliveryError{Reason: "invalid_api_key"}, state: "attention", reason: "invalid_api_key", sends: 1},
		{name: "exhausted", attempts: 12, age: time.Hour, state: "attention", reason: "attempts_exhausted"},
		{name: "expired_window", attempts: 1, age: 24 * time.Hour, state: "attention", reason: "retry_window_expired"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			email := fixture.queued
			email.NextAttempt = time.Now().Add(-time.Second)
			if scenario.attempts > 0 {
				email.State = "retrying"
				email.Attempts = scenario.attempts
				email.FirstAttempt = time.Now().Add(-scenario.age)
			}
			putInvitationEmailPG(test, fixture.ctx, fixture.storage, email)
			storage := &invitationEmailPGObservedStore{PostgresStore: fixture.storage}
			sender := &fakeInvitationMailer{err: scenario.failure}
			owner := newInvitationEmailPGOwner(test, storage, sender)
			if worked, err := owner.ProcessInvitationEmail(fixture.ctx); !worked || err != nil {
				test.Fatal(worked, err)
			}
			current, err := fixture.observedEmail(fixture.ctx)
			if err != nil || current.State != scenario.state || current.Reason != scenario.reason || current.Attempts != scenario.attempts+scenario.sends || current.ProviderID != "" || len(sender.keys) != scenario.sends {
				test.Fatal("public owner changed durable retry bounds", current, err)
			}
			if scenario.delay != 0 && !current.NextAttempt.Equal(storage.clock.Add(scenario.delay+10*time.Second)) {
				test.Fatal("retry scheduling lost the storage clock or bounded send allowance", current.NextAttempt, storage.clock, scenario.delay)
			}
			if worked, err := owner.ProcessInvitationEmail(fixture.ctx); worked || err != nil || len(sender.keys) != scenario.sends {
				test.Fatal("terminal or not-yet-due job sent again", worked, err)
			}
		})
	}
}

type invitationEmailPGProcessResult struct {
	worked bool
	err    error
}

func TestPGInvitationEmailWorkerStaleAttemptAndStateFinishFences(test *testing.T) {
	for _, fence := range []string{"attempt", "state"} {
		test.Run(fence, func(test *testing.T) {
			fixture := newInvitationEmailPGFixture(test, "organization", domain.OrganizationMember)
			ctx, cancel := context.WithCancel(fixture.ctx)
			entered, release := make(chan struct{}), make(chan struct{})
			slow := invitationMailFunc(func(sendCtx context.Context, key string, message domain.EmailMessage) (string, error) {
				if fixture.storage.InTransaction(sendCtx) || key != "collaboration-invite/"+fixture.invitation.ID || !reflect.DeepEqual(message, fixture.queued.Message) {
					return "", domain.ErrIntegrity
				}
				close(entered)
				select {
				case <-release:
					return "stale-provider-response", nil
				case <-sendCtx.Done():
					return "", sendCtx.Err()
				}
			})
			owner := newInvitationEmailPGOwner(test, &invitationEmailPGObservedStore{PostgresStore: fixture.storage}, slow)
			finished, joined := make(chan invitationEmailPGProcessResult, 1), make(chan struct{})
			go func() {
				defer close(joined)
				worked, err := owner.ProcessInvitationEmail(ctx)
				finished <- invitationEmailPGProcessResult{worked: worked, err: err}
			}()
			defer func() { cancel(); <-joined }()
			select {
			case <-entered:
			case result := <-finished:
				test.Fatal("owner did not start the delayed fake provider", result)
			case <-ctx.Done():
				test.Fatal(ctx.Err())
			}
			claimed, err := fixture.observedEmail(ctx)
			if err != nil || claimed.State != "sending" || claimed.Attempts != 1 {
				test.Fatal("claim was not visible on an independent connection before send", claimed, err)
			}
			peerSender := &fakeInvitationMailer{}
			peer := newInvitationEmailPGOwner(test, &invitationEmailPGObservedStore{PostgresStore: fixture.peer}, peerSender)
			if worked, err := peer.ProcessInvitationEmail(ctx); worked || err != nil {
				test.Fatal("second owner claimed an unexpired lease", worked, err)
			}
			claimed.NextAttempt = time.Now().Add(-time.Second)
			putInvitationEmailPG(test, ctx, fixture.peer, claimed)
			if fence == "state" {
				commands := NewIdentityService(nil, fixture.peer)
				if _, err := commands.ActOnCollaborationInvitation(ctx, fixture.team.owner.ID, fixture.invitation.ID, "revoke"); err != nil {
					test.Fatal("in-flight send retained the identity lock", err)
				}
			}
			if worked, err := peer.ProcessInvitationEmail(ctx); !worked || err != nil {
				test.Fatal("independent owner could not reclaim expired work", worked, err)
			}
			newer, err := fixture.observedEmail(ctx)
			if err != nil {
				test.Fatal(err)
			}
			if fence == "attempt" {
				if newer.State != "accepted" || newer.Attempts != 2 || newer.ProviderID != "provider-id" || len(peerSender.keys) != 1 || peerSender.keys[0] != "collaboration-invite/"+fixture.invitation.ID || !reflect.DeepEqual(peerSender.messages[0], fixture.queued.Message) {
					test.Fatal("reclaimed attempt changed stored provider identity", newer)
				}
			} else if newer.State != "cancelled" || newer.Attempts != 1 || newer.Reason != "invitation_inactive" || len(peerSender.keys) != 0 {
				test.Fatal("reclaimed revoked invitation was sent", newer)
			}
			close(release)
			select {
			case result := <-finished:
				if !result.worked || !errors.Is(result.err, domain.ErrConflict) {
					test.Fatal("stale attempt or state finish was not fenced", result)
				}
			case <-ctx.Done():
				test.Fatal(ctx.Err())
			}
			current, err := fixture.observedEmail(ctx)
			if err != nil || !reflect.DeepEqual(current, newer) {
				test.Fatal("stale provider response overwrote the independent owner's state", current, err)
			}
			if fence == "state" {
				commands := NewIdentityService(nil, fixture.peer)
				if _, err := commands.ActOnCollaborationInvitation(ctx, fixture.team.outsider.ID, fixture.invitation.ID, "accept"); !errors.Is(err, domain.ErrConflict) {
					test.Fatal("late provider response revived a revoked invitation", err)
				}
			}
		})
	}
}

func TestPGInvitationEmailWorkerSharesRevocationIdentityLock(test *testing.T) {
	fixture := newInvitationEmailPGFixture(test, "organization", domain.OrganizationMember)
	ctx, cancel := context.WithCancel(fixture.ctx)
	held, release := make(chan struct{}), make(chan struct{})
	revoked, mutationJoined := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(mutationJoined)
		revoked <- fixture.peer.WithinIdentity(ctx, func(bound context.Context) error {
			commands := NewIdentityService(nil, fixture.peer)
			if _, err := commands.ActOnCollaborationInvitation(bound, fixture.team.owner.ID, fixture.invitation.ID, "revoke"); err != nil {
				return err
			}
			close(held)
			select {
			case <-release:
				return nil
			case <-bound.Done():
				return bound.Err()
			}
		})
	}()
	defer func() { cancel(); <-mutationJoined }()
	select {
	case <-held:
	case err := <-revoked:
		test.Fatal("revocation did not stage under the identity lock", err)
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	sender := &fakeInvitationMailer{}
	owner := newInvitationEmailPGOwner(test, &invitationEmailPGObservedStore{PostgresStore: fixture.storage}, sender)
	finished, workerJoined := make(chan invitationEmailPGProcessResult, 1), make(chan struct{})
	go func() {
		defer close(workerJoined)
		worked, err := owner.ProcessInvitationEmail(ctx)
		finished <- invitationEmailPGProcessResult{worked: worked, err: err}
	}()
	defer func() { cancel(); <-workerJoined }()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := fixture.observer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database()))`).Scan(&waiting); err != nil {
			test.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case result := <-finished:
			test.Fatal("worker bypassed the live revocation identity lock", result)
		case <-ticker.C:
		case <-ctx.Done():
			test.Fatal("worker never waited on the existing PostgreSQL identity lock", ctx.Err())
		}
	}
	close(release)
	select {
	case err := <-revoked:
		if err != nil {
			test.Fatal(err)
		}
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	select {
	case result := <-finished:
		if !result.worked || result.err != nil {
			test.Fatal("worker did not process the committed revocation", result)
		}
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	current, err := fixture.observedEmail(ctx)
	if err != nil || current.State != "cancelled" || current.Reason != "invitation_inactive" || current.Attempts != 0 || len(sender.keys) != 0 {
		test.Fatal("serialized revocation still allowed an email claim", current, err)
	}
}

func TestPGInvitationEmailWorkerFinishUsesCallerCancellation(test *testing.T) {
	fixture := newInvitationEmailPGFixture(test, "organization", domain.OrganizationMember)
	ctx, cancel := context.WithCancel(fixture.ctx)
	defer cancel()
	sent := 0
	sender := invitationMailFunc(func(sendCtx context.Context, _ string, _ domain.EmailMessage) (string, error) {
		if fixture.storage.InTransaction(sendCtx) {
			return "", domain.ErrIntegrity
		}
		sent++
		cancel()
		return "provider-accepted-before-cancel", nil
	})
	owner := newInvitationEmailPGOwner(test, &invitationEmailPGObservedStore{PostgresStore: fixture.storage}, sender)
	if worked, err := owner.ProcessInvitationEmail(ctx); !worked || !errors.Is(err, context.Canceled) {
		test.Fatal("finish detached from the cancelled caller", worked, err)
	}
	current, err := fixture.observedEmail(fixture.ctx)
	if err != nil || current.State != "sending" || current.Attempts != 1 || current.ProviderID != "" || sent != 1 {
		test.Fatal("cancelled finish committed acceptance or erased the durable claim", current, err)
	}
}
