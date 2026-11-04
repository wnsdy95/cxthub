package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave46EmailTransactionKey struct{}
type wave46EmailValueKey struct{}

type wave46EmailMemory struct {
	observations      sync.Mutex
	test              *testing.T
	mutex             sync.Mutex
	now               time.Time
	jobs              map[string]domain.InvitationEmail
	invitations       map[string]domain.CollaborationInvite
	order             []string
	organizationRole  domain.OrganizationRole
	enterpriseRole    domain.EnterpriseRole
	issuerMissing     bool
	active            bool
	local             bool
	calls             []string
	contexts          []context.Context
	nextCalls         int
	nextError         error
	invitationError   error
	organizationError error
	enterpriseError   error
	getError          error
	putError          error
	commitError       error
	onInvitation      func(context.Context)
}

func (storage *wave46EmailMemory) requireIdentity(ctx context.Context) {
	storage.test.Helper()
	if storage.local {
		if ctx.Value(identityMutationKey{}) == nil {
			storage.test.Fatal("development execution lost the shared identity mutation context")
		}
		return
	}
	if ctx.Value(wave46EmailTransactionKey{}) != storage || !storage.active {
		storage.test.Fatal("queue or authority access escaped the identity transaction")
	}
}

func (storage *wave46EmailMemory) WithinIdentity(ctx context.Context, execute func(context.Context) error) error {
	storage.contexts = append(storage.contexts, ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if ctx.Value(wave46EmailTransactionKey{}) == storage {
		return execute(ctx)
	}
	storage.mutex.Lock()
	defer storage.mutex.Unlock()
	before := make(map[string]domain.InvitationEmail, len(storage.jobs))
	for identifier, job := range storage.jobs {
		before[identifier] = job
	}
	storage.active = true
	defer func() { storage.active = false }()
	storage.calls = append(storage.calls, "begin")
	err := execute(context.WithValue(ctx, wave46EmailTransactionKey{}, storage))
	if err == nil {
		err = storage.commitError
	}
	if err != nil {
		storage.jobs = before
		storage.calls = append(storage.calls, "rollback")
		return err
	}
	storage.calls = append(storage.calls, "commit")
	return nil
}

func (storage *wave46EmailMemory) NextInvitationEmail(ctx context.Context) (domain.InvitationEmail, time.Time, error) {
	storage.requireIdentity(ctx)
	storage.calls = append(storage.calls, "next")
	storage.observations.Lock()
	storage.nextCalls++
	storage.observations.Unlock()
	if storage.nextError != nil {
		return domain.InvitationEmail{}, storage.now, storage.nextError
	}
	for _, identifier := range storage.order {
		if job := storage.jobs[identifier]; job.Ready(storage.now) {
			return job, storage.now, nil
		}
	}
	return domain.InvitationEmail{}, storage.now, domain.ErrNotFound
}

func (storage *wave46EmailMemory) GetInvitationEmail(ctx context.Context, identifier string) (domain.InvitationEmail, error) {
	storage.requireIdentity(ctx)
	storage.calls = append(storage.calls, "get")
	if storage.getError != nil {
		return domain.InvitationEmail{}, storage.getError
	}
	job, found := storage.jobs[identifier]
	if !found {
		return job, domain.ErrNotFound
	}
	return job, nil
}

func (storage *wave46EmailMemory) PutInvitationEmail(ctx context.Context, job domain.InvitationEmail) error {
	storage.requireIdentity(ctx)
	storage.calls = append(storage.calls, "put:"+job.State)
	if storage.putError != nil {
		return storage.putError
	}
	if err := domain.ValidateInvitationEmail(job); err != nil {
		return err
	}
	storage.jobs[job.InvitationID] = job
	return nil
}

func (storage *wave46EmailMemory) GetCollaborationInvite(ctx context.Context, identifier string) (domain.CollaborationInvite, error) {
	storage.requireIdentity(ctx)
	storage.calls = append(storage.calls, "invitation")
	if storage.onInvitation != nil {
		storage.onInvitation(ctx)
	}
	if storage.invitationError != nil {
		return domain.CollaborationInvite{}, storage.invitationError
	}
	invitation, found := storage.invitations[identifier]
	if !found {
		return invitation, domain.ErrNotFound
	}
	return invitation, nil
}

func (storage *wave46EmailMemory) GetOrganizationMembership(ctx context.Context, organization, actor string) (domain.OrganizationMembership, error) {
	storage.requireIdentity(ctx)
	storage.calls = append(storage.calls, "organization")
	if storage.organizationError != nil {
		return domain.OrganizationMembership{}, storage.organizationError
	}
	if storage.issuerMissing {
		return domain.OrganizationMembership{}, domain.ErrNotFound
	}
	if organization != "ent_00000000000000000000000000000046" || actor != "issuer-46" {
		storage.test.Fatal("worker changed the current invitation authority subject", organization, actor)
	}
	return domain.OrganizationMembership{OrganizationID: organization, UserID: actor, Role: storage.organizationRole}, nil
}

func (storage *wave46EmailMemory) ListEnterpriseMembers(ctx context.Context, enterprise string) ([]domain.EnterpriseMembership, error) {
	storage.requireIdentity(ctx)
	storage.calls = append(storage.calls, "enterprise")
	if storage.enterpriseError != nil {
		return nil, storage.enterpriseError
	}
	if enterprise != "ep_00000000000000000000000000000046" {
		storage.test.Fatal("worker changed the invitation enterprise", enterprise)
	}
	members := []domain.EnterpriseMembership{{EnterpriseID: enterprise, UserID: "other-owner", Role: domain.EnterpriseOwner}}
	if !storage.issuerMissing {
		members = append(members, domain.EnterpriseMembership{EnterpriseID: enterprise, UserID: "issuer-46", Role: storage.enterpriseRole})
	}
	return members, nil
}

type wave46EmailSender struct {
	storage  *wave46EmailMemory
	keys     []string
	messages []domain.EmailMessage
	err      error
	send     func(context.Context, string, domain.EmailMessage) (string, error)
}

func (sender *wave46EmailSender) Send(ctx context.Context, key string, message domain.EmailMessage) (string, error) {
	sender.storage.test.Helper()
	if sender.storage.active || ctx.Value(wave46EmailTransactionKey{}) != nil || ctx.Value(identityMutationKey{}) != nil {
		sender.storage.test.Fatal("provider request holds the identity transaction or development mutex")
	}
	deadline, bounded := ctx.Deadline()
	if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 10*time.Second {
		sender.storage.test.Fatal("provider request lost the original ten-second deadline")
	}
	sender.storage.calls = append(sender.storage.calls, "send")
	sender.storage.observations.Lock()
	sender.keys = append(sender.keys, key)
	message.To = append([]string(nil), message.To...)
	sender.messages = append(sender.messages, message)
	sender.storage.observations.Unlock()
	if sender.send != nil {
		return sender.send(ctx, key, message)
	}
	return "provider-46", sender.err
}

type wave46EmailFixture struct {
	config  InvitationEmailWorkerConfig
	storage *wave46EmailMemory
	sender  *wave46EmailSender
	id      string
}

func (fixture wave46EmailFixture) counts() (int, int) {
	fixture.storage.observations.Lock()
	defer fixture.storage.observations.Unlock()
	return len(fixture.sender.keys), fixture.storage.nextCalls
}

func newWave46EmailFixture(test *testing.T, count int) wave46EmailFixture {
	test.Helper()
	storage := &wave46EmailMemory{test: test, now: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		jobs: map[string]domain.InvitationEmail{}, invitations: map[string]domain.CollaborationInvite{},
		organizationRole: domain.OrganizationOwner, enterpriseRole: domain.EnterpriseOwner}
	for index := 0; index < count; index++ {
		identifier := fmt.Sprintf("ci_%032x", index+1)
		invitation := domain.CollaborationInvite{ID: identifier, Kind: "organization", SpaceID: "ent_00000000000000000000000000000046",
			Email: "recipient@example.test", Role: domain.OrganizationMember, Status: "pending", CreatedBy: "issuer-46",
			CreatedAt: storage.now.Add(-time.Hour), UpdatedAt: storage.now.Add(-time.Hour), ExpiresAt: storage.now.Add(7 * 24 * time.Hour)}
		job := domain.InvitationEmail{InvitationID: identifier, State: "queued", NextAttempt: storage.now,
			Message: domain.EmailMessage{From: "CXTHub <from@example.test>", To: []string{invitation.Email}, Subject: "CXTHub invitation",
				Text: "Frozen invitation https://example.test/invite/" + identifier, HTML: "<p>Frozen invitation</p>"}}
		if err := domain.ValidateCollaborationInvite(invitation); err != nil {
			test.Fatal("invalid in-memory invitation fixture", err)
		}
		if err := domain.ValidateInvitationEmail(job); err != nil {
			test.Fatal("invalid in-memory email fixture", err)
		}
		storage.order = append(storage.order, identifier)
		storage.invitations[identifier], storage.jobs[identifier] = invitation, job
	}
	sender := &wave46EmailSender{storage: storage}
	fixture := wave46EmailFixture{storage: storage, sender: sender,
		config: InvitationEmailWorkerConfig{Profile: InvitationEmailComplete, Queue: storage, Invitations: storage,
			Organizations: storage, Enterprises: storage, Transactions: storage, Sender: sender}}
	if count > 0 {
		fixture.id = storage.order[0]
	}
	return fixture
}

func wave46EmailWorker(test *testing.T, config InvitationEmailWorkerConfig) *InvitationEmailWorker {
	test.Helper()
	worker, err := NewInvitationEmailWorker(config)
	if err != nil || worker == nil {
		test.Fatal("public invitation email constructor rejected its declared ports", err)
	}
	return worker
}

func TestWave46InvitationEmailFactoryProfilesCapabilitiesAndTypedNil(test *testing.T) {
	fixture := newWave46EmailFixture(test, 1)
	for _, profile := range []InvitationEmailProfile{"", "unknown"} {
		config := fixture.config
		config.Profile = profile
		if worker, err := NewInvitationEmailWorker(config); worker != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("invalid profile accepted", profile, worker, err)
		}
	}
	missing := map[string]func(*InvitationEmailWorkerConfig){
		"queue":         func(config *InvitationEmailWorkerConfig) { config.Queue = nil },
		"invitations":   func(config *InvitationEmailWorkerConfig) { config.Invitations = nil },
		"organizations": func(config *InvitationEmailWorkerConfig) { config.Organizations = nil },
		"enterprises":   func(config *InvitationEmailWorkerConfig) { config.Enterprises = nil },
		"transactions":  func(config *InvitationEmailWorkerConfig) { config.Transactions = nil },
	}
	for name, remove := range missing {
		test.Run("complete-missing-"+name, func(test *testing.T) {
			config := fixture.config
			remove(&config)
			if worker, err := NewInvitationEmailWorker(config); worker != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
				test.Fatal("enabled Complete accepted a missing capability", worker, err)
			}
		})
	}
	var absentStorage *wave46EmailMemory
	var absentSender *wave46EmailSender
	typedNil := map[string]func(*InvitationEmailWorkerConfig){
		"queue":         func(config *InvitationEmailWorkerConfig) { config.Queue = absentStorage },
		"invitations":   func(config *InvitationEmailWorkerConfig) { config.Invitations = absentStorage },
		"organizations": func(config *InvitationEmailWorkerConfig) { config.Organizations = absentStorage },
		"enterprises":   func(config *InvitationEmailWorkerConfig) { config.Enterprises = absentStorage },
		"transactions":  func(config *InvitationEmailWorkerConfig) { config.Transactions = absentStorage },
		"sender":        func(config *InvitationEmailWorkerConfig) { config.Sender = absentSender },
	}
	for _, profile := range []InvitationEmailProfile{InvitationEmailCompatibility, InvitationEmailComplete} {
		for _, disabled := range []bool{false, true} {
			for name, replace := range typedNil {
				test.Run(fmt.Sprintf("%s-disabled-%t-nil-%s", profile, disabled, name), func(test *testing.T) {
					config := fixture.config
					config.Profile = profile
					if disabled {
						config.Sender = nil
					}
					replace(&config)
					if worker, err := NewInvitationEmailWorker(config); worker != nil || !errors.Is(err, domain.ErrValidation) {
						test.Fatal("declared typed-nil dependency accepted", worker, err)
					}
				})
			}
		}
	}
	if len(fixture.storage.calls) != 0 || len(fixture.sender.keys) != 0 {
		test.Fatal("factory validation performed storage or provider I/O")
	}
}

func TestWave46InvitationEmailFactoryDoesNotStartAndDisabledIsExplicit(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newWave46EmailFixture(test, 1)
		wave46EmailWorker(test, fixture.config)
		synctest.Wait()
		if len(fixture.storage.calls) != 0 {
			test.Fatal("public constructor started hidden execution", fixture.storage.calls)
		}
		for _, profile := range []InvitationEmailProfile{InvitationEmailCompatibility, InvitationEmailComplete} {
			for _, declared := range []bool{false, true} {
				config := InvitationEmailWorkerConfig{Profile: profile}
				if declared {
					config = fixture.config
					config.Profile, config.Sender = profile, nil
				}
				worker := wave46EmailWorker(test, config)
				if worked, err := worker.ProcessInvitationEmail(context.Background()); worked || err != nil {
					test.Fatal("disabled Process performed work", worked, err)
				}
				finished := make(chan struct{})
				go func() { worker.Run(context.Background()); close(finished) }()
				synctest.Wait()
				select {
				case <-finished:
				default:
					test.Fatal("disabled Run waited for cancellation or a timer")
				}
			}
		}
		if len(fixture.storage.calls) != 0 || len(fixture.sender.keys) != 0 {
			test.Fatal("disabled owner accessed storage or provider")
		}
	})
}

type wave46EmailConfigRepositories struct {
	outbound.RepositoryStore
	outbound.OrganizationStore
	outbound.CollaborationInviteStore
	outbound.EnterpriseStore
	outbound.InvitationEmailStore
	outbound.IdentityTransactions
}

type wave46EmailNarrowEnterpriseRepositories struct {
	outbound.RepositoryStore
	outbound.OrganizationStore
	outbound.CollaborationInviteStore
	outbound.InvitationEmailStore
	outbound.IdentityTransactions
}

func (*wave46EmailNarrowEnterpriseRepositories) ListEnterpriseMembers(context.Context, string) ([]domain.EnterpriseMembership, error) {
	return nil, errors.New("configuration must not read enterprise membership")
}

func TestWave46InvitationEmailIdentityConfigCopiesPortsWithoutNewResources(test *testing.T) {
	fixture := newWave46EmailFixture(test, 0)
	repositories := &wave46EmailConfigRepositories{}
	service := NewIdentityService(nil, repositories)
	service.invitationEmail = &invitationEmailConfig{sender: fixture.sender}
	config := service.InvitationEmailWorkerConfig(InvitationEmailComplete)
	if config.Profile != InvitationEmailComplete || config.Queue != repositories || config.Invitations != repositories ||
		config.Organizations != repositories || config.Enterprises != repositories || config.Transactions != repositories || config.Sender != fixture.sender {
		test.Fatal("identity configuration changed a current port, sender, or requested profile", config)
	}
	narrow := &wave46EmailNarrowEnterpriseRepositories{}
	legacy := NewIdentityService(nil, narrow)
	config = legacy.InvitationEmailWorkerConfig(InvitationEmailCompatibility)
	if config.Enterprises != nil || config.Sender != nil || config.Queue != narrow || config.Organizations != narrow || config.Transactions != narrow {
		test.Fatal("configuration bypassed the full legacy EnterpriseStore assertion or enabled an unconfigured sender", config)
	}
}

func TestWave46InvitationEmailCompatibilityKeepsLazyMissingReadersAndLocalScope(test *testing.T) {
	fixture := newWave46EmailFixture(test, 1)
	minimal := InvitationEmailWorkerConfig{Profile: InvitationEmailCompatibility, Sender: fixture.sender}
	if worker, err := NewInvitationEmailWorker(minimal); worker != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("enabled Compatibility accepted an absent queue", worker, err)
	}
	minimal.Queue = fixture.storage
	fixture.storage.local = true
	worker := wave46EmailWorker(test, minimal)
	if worked, err := worker.ProcessInvitationEmail(context.Background()); worked || !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("missing invitation reader did not retain its lazy forbidden result", worked, err)
	}
	if job := fixture.storage.jobs[fixture.id]; job.State != "queued" || job.Attempts != 0 {
		test.Fatal("missing invitation capability changed a queued message", job)
	}
	minimal.Invitations = fixture.storage
	worker = wave46EmailWorker(test, minimal)
	if worked, err := worker.ProcessInvitationEmail(context.Background()); !worked || err != nil {
		test.Fatal("missing authority did not retain lazy cancellation", worked, err)
	}
	if job := fixture.storage.jobs[fixture.id]; job.State != "cancelled" || job.Reason != "issuer_not_authorized" || len(fixture.sender.keys) != 0 {
		test.Fatal("missing authority sent an email", job)
	}
	fixture = newWave46EmailFixture(test, 1)
	fixture.config.Profile, fixture.config.Transactions, fixture.storage.local = InvitationEmailCompatibility, nil, true
	worker = wave46EmailWorker(test, fixture.config)
	if worked, err := worker.ProcessInvitationEmail(context.Background()); !worked || err != nil || fixture.storage.jobs[fixture.id].State != "accepted" {
		test.Fatal("development local identity scope no longer executes enabled Compatibility", worked, err)
	}
}

func TestWave46InvitationEmailPublicOwnerClaimsAndFinishesOutsideProviderIO(test *testing.T) {
	fixture := newWave46EmailFixture(test, 1)
	worker := wave46EmailWorker(test, fixture.config)
	ctx := context.WithValue(context.Background(), wave46EmailValueKey{}, "caller-46")
	original := fixture.storage.jobs[fixture.id].Message
	fixture.sender.send = func(sendCtx context.Context, key string, message domain.EmailMessage) (string, error) {
		job := fixture.storage.jobs[fixture.id]
		if job.State != "sending" || job.Attempts != 1 || !job.FirstAttempt.Equal(fixture.storage.now) ||
			!job.NextAttempt.Equal(fixture.storage.now.Add(2*time.Minute)) || job.Reason != "" {
			test.Fatal("claim changed its storage-clock lease or committed state", job)
		}
		if sendCtx.Value(wave46EmailValueKey{}) != "caller-46" || key != "collaboration-invite/"+fixture.id || !reflect.DeepEqual(message, original) {
			test.Fatal("provider request lost caller values, persisted payload, or original idempotency key")
		}
		if err := fixture.storage.WithinIdentity(ctx, func(nested context.Context) error {
			fixture.storage.requireIdentity(nested)
			return nil
		}); err != nil {
			test.Fatal("provider request prevented an independent identity command", err)
		}
		return "accepted-by-provider", nil
	}
	if worked, err := worker.ProcessInvitationEmail(ctx); !worked || err != nil {
		test.Fatal(worked, err)
	}
	job := fixture.storage.jobs[fixture.id]
	if job.State != "accepted" || job.ProviderID != "accepted-by-provider" || job.Attempts != 1 || !reflect.DeepEqual(job.Message, original) {
		test.Fatal("completion lost provider acceptance or frozen message", job)
	}
	expected := []string{"begin", "next", "invitation", "organization", "put:sending", "commit", "send", "begin", "commit", "begin", "get", "put:accepted", "commit"}
	if !reflect.DeepEqual(fixture.storage.calls, expected) {
		test.Fatal("claim/send/finish ordering changed", fixture.storage.calls)
	}
	if len(fixture.storage.contexts) != 3 || fixture.storage.contexts[0] != ctx || fixture.storage.contexts[2] != ctx {
		test.Fatal("claim or completion did not use the original caller context")
	}
	if worked, err := worker.ProcessInvitationEmail(ctx); worked || err != nil || len(fixture.sender.keys) != 1 {
		test.Fatal("provider-accepted job was sent again", worked, err)
	}
}

func TestWave46InvitationEmailCurrentAuthorityStatusAndLimits(test *testing.T) {
	for _, kind := range []string{"organization", "enterprise"} {
		for _, scenario := range []string{"owner-member", "admin-member", "owner-admin", "admin-admin", "member", "missing", "revoked", "accepted", "expired", "expires-now", "attempts", "window", "inactive-before-limits", "authority-before-limits"} {
			test.Run(kind+"/"+scenario, func(test *testing.T) {
				fixture := newWave46EmailFixture(test, 1)
				invitation, job := fixture.storage.invitations[fixture.id], fixture.storage.jobs[fixture.id]
				invitation.Kind = kind
				if kind == "enterprise" {
					invitation.SpaceID = "ep_00000000000000000000000000000046"
				}
				expectedState, expectedReason := "accepted", ""
				switch scenario {
				case "admin-member", "admin-admin":
					fixture.storage.organizationRole, fixture.storage.enterpriseRole = domain.OrganizationAdmin, domain.EnterpriseAdmin
				case "member", "authority-before-limits":
					fixture.storage.organizationRole, fixture.storage.enterpriseRole = domain.OrganizationMember, domain.EnterpriseMember
				case "missing":
					fixture.storage.issuerMissing = true
				case "revoked", "accepted", "inactive-before-limits":
					invitation.Status = "revoked"
					if scenario == "accepted" {
						invitation.Status, invitation.AcceptedBy = "accepted", "recipient-46"
					}
				case "expired":
					invitation.ExpiresAt = fixture.storage.now.Add(-time.Second)
				case "expires-now":
					invitation.ExpiresAt = fixture.storage.now
				}
				if scenario == "owner-admin" || scenario == "admin-admin" {
					invitation.Role = domain.OrganizationAdmin
				}
				switch scenario {
				case "attempts", "window", "inactive-before-limits", "authority-before-limits":
					job.State, job.Attempts, job.FirstAttempt = "retrying", 12, fixture.storage.now.Add(-23*time.Hour)
					if scenario == "attempts" {
						job.FirstAttempt = fixture.storage.now.Add(-time.Hour)
						expectedState, expectedReason = "attention", "attempts_exhausted"
					} else {
						expectedState, expectedReason = "attention", "retry_window_expired"
					}
				}
				switch scenario {
				case "admin-admin", "member", "missing", "authority-before-limits":
					expectedState, expectedReason = "cancelled", "issuer_not_authorized"
				case "revoked", "accepted", "expired", "expires-now", "inactive-before-limits":
					expectedState, expectedReason = "cancelled", "invitation_inactive"
				}
				fixture.storage.invitations[fixture.id], fixture.storage.jobs[fixture.id] = invitation, job
				worker := wave46EmailWorker(test, fixture.config)
				if worked, err := worker.ProcessInvitationEmail(context.Background()); !worked || err != nil {
					test.Fatal(worked, err)
				}
				current := fixture.storage.jobs[fixture.id]
				if current.State != expectedState || current.Reason != expectedReason {
					test.Fatal("current authority, inactivity, or limit priority changed", current)
				}
				if expectedState != "accepted" && (len(fixture.sender.keys) != 0 || current.Attempts != job.Attempts) {
					test.Fatal("inactive or exhausted invitation consumed a send attempt", current)
				}
			})
		}
	}
}

func TestWave46InvitationEmailOutagePreservesQueueAndOriginalErrorOrder(test *testing.T) {
	for _, kind := range []string{"organization", "enterprise"} {
		for _, inactive := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s-inactive-%t", kind, inactive), func(test *testing.T) {
				fixture := newWave46EmailFixture(test, 1)
				invitation := fixture.storage.invitations[fixture.id]
				invitation.Kind = kind
				outage := errors.New("membership temporarily unavailable")
				fixture.storage.organizationError = outage
				if kind == "enterprise" {
					invitation.SpaceID = "ep_00000000000000000000000000000046"
					fixture.storage.enterpriseError = outage
				}
				if inactive {
					invitation.Status = "revoked"
				}
				fixture.storage.invitations[fixture.id] = invitation
				before := fixture.storage.jobs[fixture.id]
				worker := wave46EmailWorker(test, fixture.config)
				if worked, err := worker.ProcessInvitationEmail(context.Background()); worked || !errors.Is(err, outage) {
					test.Fatal("membership outage was converted to inactivity or permanent cancellation", worked, err)
				}
				if !reflect.DeepEqual(before, fixture.storage.jobs[fixture.id]) || len(fixture.sender.keys) != 0 {
					test.Fatal("authority outage mutated a queued message")
				}
				fixture.storage.organizationError, fixture.storage.enterpriseError = nil, nil
				if worked, err := worker.ProcessInvitationEmail(context.Background()); !worked || err != nil {
					test.Fatal("recovered authority could not resume processing", worked, err)
				}
				expected := "accepted"
				if inactive {
					expected = "cancelled"
				}
				if fixture.storage.jobs[fixture.id].State != expected {
					test.Fatal("recovery revived an inactive invitation or stranded a pending one")
				}
			})
		}
	}
}

func TestWave46InvitationEmailClaimErrorsRollbackAndReturnZero(test *testing.T) {
	failure := errors.New("identity storage failed")
	for _, stage := range []string{"next", "invitation", "authority", "put", "commit"} {
		test.Run(stage, func(test *testing.T) {
			fixture := newWave46EmailFixture(test, 1)
			before := fixture.storage.jobs[fixture.id]
			switch stage {
			case "next":
				fixture.storage.nextError = failure
			case "invitation":
				fixture.storage.invitationError = failure
			case "authority":
				fixture.storage.organizationError = failure
			case "put":
				fixture.storage.putError = failure
			case "commit":
				fixture.storage.commitError = failure
			}
			worker := wave46EmailWorker(test, fixture.config)
			if job, err := worker.claimInvitationEmail(context.Background()); !errors.Is(err, failure) || !reflect.DeepEqual(job, domain.InvitationEmail{}) {
				test.Fatal("failed claim leaked an uncommitted result", job, err)
			}
			if !reflect.DeepEqual(before, fixture.storage.jobs[fixture.id]) || len(fixture.sender.keys) != 0 {
				test.Fatal("failed claim escaped rollback")
			}
		})
	}
}

func TestWave46InvitationEmailRetryScheduleUsesStorageClockAndOriginalLimits(test *testing.T) {
	for _, scenario := range []struct {
		name     string
		attempts int
		failure  error
		delay    time.Duration
		state    string
		reason   string
		window   time.Duration
	}{
		{name: "transport", failure: errors.New("unclassified transport failure"), delay: 70 * time.Second, state: "retrying", reason: "transport_failed"},
		{name: "retry-after", failure: &outbound.EmailDeliveryError{Reason: "rate_limited", Retryable: true, RetryAfter: 4 * time.Minute}, delay: 4*time.Minute + 10*time.Second, state: "retrying", reason: "rate_limited"},
		{name: "short-retry-after", failure: &outbound.EmailDeliveryError{Reason: "rate_limited", Retryable: true, RetryAfter: time.Second}, delay: 70 * time.Second, state: "retrying", reason: "rate_limited"},
		{name: "provider-longer-than-backoff-cap", failure: &outbound.EmailDeliveryError{Reason: "rate_limited", Retryable: true, RetryAfter: 2 * time.Hour}, delay: 2*time.Hour + 10*time.Second, state: "retrying", reason: "rate_limited"},
		{name: "backoff-cap", attempts: 10, failure: errors.New("transport"), delay: time.Hour + 10*time.Second, state: "retrying", reason: "transport_failed"},
		{name: "last-attempt", attempts: 11, failure: errors.New("transport"), delay: 2 * time.Minute, state: "attention", reason: "transport_failed"},
		{name: "permanent", failure: &outbound.EmailDeliveryError{Reason: "invalid_api_key"}, delay: 2 * time.Minute, state: "attention", reason: "invalid_api_key"},
		{name: "window-equality", attempts: 1, failure: errors.New("transport"), delay: 130 * time.Second, state: "attention", reason: "retry_window_expired", window: 130 * time.Second},
		{name: "inside-window", attempts: 1, failure: errors.New("transport"), delay: 130 * time.Second, state: "retrying", reason: "transport_failed", window: 131 * time.Second},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newWave46EmailFixture(test, 1)
			job := fixture.storage.jobs[fixture.id]
			if scenario.attempts > 0 {
				job.State, job.Attempts, job.FirstAttempt = "retrying", scenario.attempts, fixture.storage.now.Add(-time.Hour)
			}
			if scenario.window > 0 {
				job.FirstAttempt = fixture.storage.now.Add(-23*time.Hour + scenario.window)
			}
			fixture.storage.jobs[fixture.id], fixture.sender.err = job, scenario.failure
			worker := wave46EmailWorker(test, fixture.config)
			if worked, err := worker.ProcessInvitationEmail(context.Background()); !worked || err != nil {
				test.Fatal("persisted provider failure leaked out of processing", worked, err)
			}
			current := fixture.storage.jobs[fixture.id]
			if current.State != scenario.state || current.Reason != scenario.reason || current.ProviderID != "" ||
				current.Attempts != scenario.attempts+1 || !current.NextAttempt.Equal(fixture.storage.now.Add(scenario.delay)) {
				test.Fatal("retry classification, storage-clock allowance, backoff, or limit changed", current)
			}
		})
	}
}

func TestWave46InvitationEmailRestartRetainsFrozenMessageAndProviderKey(test *testing.T) {
	fixture := newWave46EmailFixture(test, 1)
	fixture.sender.err = &outbound.EmailDeliveryError{Reason: "rate_limited", Retryable: true}
	worker := wave46EmailWorker(test, fixture.config)
	if worked, err := worker.ProcessInvitationEmail(context.Background()); !worked || err != nil {
		test.Fatal(worked, err)
	}
	first := fixture.storage.jobs[fixture.id]
	if worked, err := worker.ProcessInvitationEmail(context.Background()); worked || err != nil || len(fixture.sender.keys) != 1 {
		test.Fatal("retry ran before its persisted due time", worked, err)
	}
	fixture.storage.now = first.NextAttempt
	invitation := fixture.storage.invitations[fixture.id]
	invitation.Email = "changed@example.test"
	fixture.storage.invitations[fixture.id] = invitation
	fixture.sender.err = nil
	restarted := wave46EmailWorker(test, fixture.config)
	if worked, err := restarted.ProcessInvitationEmail(context.Background()); !worked || err != nil {
		test.Fatal(worked, err)
	}
	current := fixture.storage.jobs[fixture.id]
	if current.State != "accepted" || current.Attempts != 2 || !current.FirstAttempt.Equal(first.FirstAttempt) ||
		len(fixture.sender.keys) != 2 || fixture.sender.keys[0] != fixture.sender.keys[1] ||
		!reflect.DeepEqual(fixture.sender.messages[0], fixture.sender.messages[1]) || !reflect.DeepEqual(current.Message, first.Message) {
		test.Fatal("restart changed persisted message identity or reopened the first-attempt window", current)
	}
}

func TestWave46InvitationEmailStaleAttemptAndStateCannotOverwriteCompletion(test *testing.T) {
	for _, scenario := range []string{"new-attempt", "changed-state"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newWave46EmailFixture(test, 1)
			worker := wave46EmailWorker(test, fixture.config)
			fixture.sender.send = func(ctx context.Context, _ string, _ domain.EmailMessage) (string, error) {
				if len(fixture.sender.keys) > 1 {
					return "new-owner", nil
				}
				if scenario == "new-attempt" {
					peer := wave46EmailWorker(test, fixture.config)
					if worked, err := peer.ProcessInvitationEmail(context.Background()); worked || err != nil {
						test.Fatal("peer reclaimed an unexpired sending lease", worked, err)
					}
					lease := fixture.storage.jobs[fixture.id].NextAttempt
					fixture.storage.now = lease.Add(-time.Nanosecond)
					if worked, err := peer.ProcessInvitationEmail(context.Background()); worked || err != nil {
						test.Fatal("peer reclaimed before the exact storage-clock lease boundary", worked, err)
					}
					fixture.storage.now = lease
					if worked, err := peer.ProcessInvitationEmail(context.Background()); !worked || err != nil {
						test.Fatal("new owner could not reclaim the expired lease", worked, err)
					}
				} else if err := fixture.storage.WithinIdentity(ctx, func(bound context.Context) error {
					current := fixture.storage.jobs[fixture.id]
					current.State, current.Reason = "cancelled", "invitation_inactive"
					return fixture.storage.PutInvitationEmail(bound, current)
				}); err != nil {
					test.Fatal(err)
				}
				return "stale-owner", nil
			}
			if worked, err := worker.ProcessInvitationEmail(context.Background()); !worked || !errors.Is(err, domain.ErrConflict) {
				test.Fatal("stale completion lost its attempt/state fence", worked, err)
			}
			current := fixture.storage.jobs[fixture.id]
			if scenario == "new-attempt" && (current.State != "accepted" || current.Attempts != 2 || current.ProviderID != "new-owner") {
				test.Fatal("stale owner overwrote newer provider acceptance", current)
			}
			if scenario == "changed-state" && (current.State != "cancelled" || current.Attempts != 1 || current.ProviderID != "") {
				test.Fatal("stale owner revived a changed state", current)
			}
		})
	}
}

func TestWave46InvitationEmailInflightRevokeDoesNotReviveInvitation(test *testing.T) {
	fixture := newWave46EmailFixture(test, 1)
	worker := wave46EmailWorker(test, fixture.config)
	fixture.sender.send = func(ctx context.Context, _ string, _ domain.EmailMessage) (string, error) {
		if err := fixture.storage.WithinIdentity(ctx, func(bound context.Context) error {
			fixture.storage.requireIdentity(bound)
			invitation := fixture.storage.invitations[fixture.id]
			invitation.Status = "revoked"
			fixture.storage.invitations[fixture.id] = invitation
			return nil
		}); err != nil {
			test.Fatal(err)
		}
		return "already-inflight", nil
	}
	if worked, err := worker.ProcessInvitationEmail(context.Background()); !worked || err != nil {
		test.Fatal(worked, err)
	}
	if fixture.storage.invitations[fixture.id].Status != "revoked" || fixture.storage.jobs[fixture.id].State != "accepted" {
		test.Fatal("provider acceptance revived the revoked invitation or changed existing in-flight semantics")
	}
}

func TestWave46InvitationEmailCompletionErrorsKeepCommittedLease(test *testing.T) {
	failure := errors.New("completion storage failed")
	for _, stage := range []string{"get", "put", "commit"} {
		test.Run(stage, func(test *testing.T) {
			fixture := newWave46EmailFixture(test, 1)
			fixture.sender.send = func(context.Context, string, domain.EmailMessage) (string, error) {
				switch stage {
				case "get":
					fixture.storage.getError = failure
				case "put":
					fixture.storage.putError = failure
				case "commit":
					fixture.storage.commitError = failure
				}
				return "accepted-uncommitted", nil
			}
			worker := wave46EmailWorker(test, fixture.config)
			if worked, err := worker.ProcessInvitationEmail(context.Background()); !worked || !errors.Is(err, failure) {
				test.Fatal("completion failure was hidden", worked, err)
			}
			if current := fixture.storage.jobs[fixture.id]; current.State != "sending" || current.Attempts != 1 || current.ProviderID != "" {
				test.Fatal("failed completion lost the committed reclaimable lease", current)
			}
		})
	}
}

func TestWave46InvitationEmailDeadlineAndCallerCancellationRemainDistinct(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newWave46EmailFixture(test, 1)
		fixture.sender.send = func(ctx context.Context, _ string, _ domain.EmailMessage) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}
		worker := wave46EmailWorker(test, fixture.config)
		started := time.Now()
		if worked, err := worker.ProcessInvitationEmail(context.Background()); !worked || err != nil {
			test.Fatal("provider timeout prevented live-caller completion", worked, err)
		}
		if time.Since(started) != 10*time.Second || fixture.storage.jobs[fixture.id].State != "retrying" {
			test.Fatal("provider timeout or retry completion changed")
		}
	})
	fixture := newWave46EmailFixture(test, 1)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), wave46EmailValueKey{}, "cancelled-caller"))
	defer cancel()
	fixture.sender.send = func(sendCtx context.Context, _ string, _ domain.EmailMessage) (string, error) {
		cancel()
		return "", sendCtx.Err()
	}
	worker := wave46EmailWorker(test, fixture.config)
	if worked, err := worker.ProcessInvitationEmail(ctx); !worked || !errors.Is(err, context.Canceled) {
		test.Fatal("completion detached from the cancelled caller", worked, err)
	}
	if len(fixture.storage.contexts) != 2 || fixture.storage.contexts[1] != ctx || fixture.storage.jobs[fixture.id].State != "sending" {
		test.Fatal("cancelled finish changed the original caller context or committed lease")
	}
}

func TestWave46InvitationEmailLocalScopeSharesLegacyNestedKeyAndMutex(test *testing.T) {
	fixture := newWave46EmailFixture(test, 1)
	fixture.config.Profile, fixture.config.Transactions, fixture.storage.local = InvitationEmailCompatibility, nil, true
	legacy := &IdentityService{}
	fixture.storage.onInvitation = func(ctx context.Context) {
		if localIdentityMutation.TryLock() {
			localIdentityMutation.Unlock()
			test.Fatal("owner claim did not hold the original development identity mutex")
		}
		if err := legacy.withIdentity(ctx, func(bound context.Context) error {
			return withIdentityTransactions(bound, nil, func(nested context.Context) error {
				if nested != ctx || nested.Value(identityMutationKey{}) == nil {
					test.Fatal("legacy and owner scopes no longer share the nested mutation key")
				}
				return nil
			})
		}); err != nil {
			test.Fatal(err)
		}
	}
	fixture.sender.send = func(ctx context.Context, _ string, _ domain.EmailMessage) (string, error) {
		if !localIdentityMutation.TryLock() {
			test.Fatal("provider request retained the original development identity mutex")
		}
		localIdentityMutation.Unlock()
		if err := legacy.withIdentity(ctx, func(bound context.Context) error {
			if bound.Value(identityMutationKey{}) == nil {
				test.Fatal("provider-side identity command lost the original development scope")
			}
			return nil
		}); err != nil {
			test.Fatal(err)
		}
		return "local-provider", nil
	}
	worker := wave46EmailWorker(test, fixture.config)
	if worked, err := worker.ProcessInvitationEmail(context.Background()); !worked || err != nil {
		test.Fatal(worked, err)
	}
	if !localIdentityMutation.TryLock() {
		test.Fatal("claim or finish leaked the original development identity mutex")
	}
	localIdentityMutation.Unlock()
}

func TestWave46InvitationEmailRunImmediateTenItemBatchesThreeSecondPollAndStop(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newWave46EmailFixture(test, 25)
		worker := wave46EmailWorker(test, fixture.config)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		finished := make(chan struct{})
		go func() { worker.Run(ctx); close(finished) }()
		synctest.Wait()
		sent, claims := fixture.counts()
		if sent != 10 || claims != 10 {
			test.Fatal("Run lost its immediate ten-item batch", sent, claims)
		}
		time.Sleep(3*time.Second - time.Millisecond)
		synctest.Wait()
		sent, claims = fixture.counts()
		if sent != 10 {
			test.Fatal("worker polled before its original three-second timer")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		sent, claims = fixture.counts()
		if sent != 20 || claims != 20 {
			test.Fatal("second timer did not process exactly ten messages")
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		sent, claims = fixture.counts()
		if sent != 25 || claims != 26 {
			test.Fatal("empty queue did not stop the current batch")
		}
		cancel()
		<-finished
		time.Sleep(6 * time.Second)
		synctest.Wait()
		_, claims = fixture.counts()
		if claims != 26 {
			test.Fatal("worker polled after cancellation and join")
		}
	})
	for _, empty := range []bool{false, true} {
		test.Run(fmt.Sprintf("stop-batch-empty-%t", empty), func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				fixture := newWave46EmailFixture(test, 0)
				if !empty {
					fixture.storage.nextError = errors.New("queue unavailable")
				}
				worker := wave46EmailWorker(test, fixture.config)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				finished := make(chan struct{})
				go func() { worker.Run(ctx); close(finished) }()
				synctest.Wait()
				_, claims := fixture.counts()
				if claims != 1 {
					test.Fatal("empty/error batch did not stop immediately")
				}
				time.Sleep(3 * time.Second)
				synctest.Wait()
				_, claims = fixture.counts()
				if claims != 2 {
					test.Fatal("empty/error batch changed the original polling interval")
				}
				cancel()
				<-finished
			})
		})
	}
}
