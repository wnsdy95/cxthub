//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var (
	_ RepositoryAuthorityMetadata      = (*store.PostgresStore)(nil)
	_ RepositoryAuthorityRepositories  = (*store.PostgresStore)(nil)
	_ RepositoryAuthorityTeams         = (*store.PostgresStore)(nil)
	_ RepositoryAuthorityOrganizations = (*store.PostgresStore)(nil)
	_ RepositoryAuthorityAudit         = (*store.PostgresStore)(nil)
	_ outbound.RepositoryAccessLocker  = (*store.PostgresStore)(nil)
)

func repositoryAuthoritySeedPG(test *testing.T) (context.Context, *store.PostgresStore, authorityScopeFixture, *pgxpool.Pool) {
	test.Helper()
	_, storage, repo := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
	test.Cleanup(cancel)
	observer := authorityScopeObserver(test, ctx)
	return ctx, storage, authorityScopeSeed(test, ctx, storage, observer, repo), observer
}

func repositoryAuthorityConfigPG(storage *store.PostgresStore) RepositoryAuthorityConfig {
	return RepositoryAuthorityConfig{
		Profile:  RepositoryAuthorityComplete,
		Metadata: struct{ RepositoryAuthorityMetadata }{storage},
		Repositories: struct {
			RepositoryAuthorityRepositories
		}{storage},
		Teams: struct{ RepositoryAuthorityTeams }{storage},
		Organizations: struct {
			RepositoryAuthorityOrganizations
		}{storage},
		Locker: struct {
			outbound.RepositoryAccessLocker
		}{storage},
		Audit:         struct{ RepositoryAuthorityAudit }{storage},
		Transactional: true,
	}
}

func repositoryAuthorityOwnerPG(test *testing.T, storage *store.PostgresStore) *RepositoryAuthority {
	test.Helper()
	authority, err := NewRepositoryAuthority(repositoryAuthorityConfigPG(storage))
	if err != nil || authority == nil {
		test.Fatalf("complete authority rejected actual narrow PostgreSQL views: %v", err)
	}
	return authority
}

func repositoryAuthorityWriterPG(test *testing.T, storage documentPublicationStorePG, fixture authorityScopeFixture, kind string) func(context.Context) error {
	test.Helper()
	if kind == "common" {
		work := newRepositoryWork(storage, storage, ReadDocumentIdentities(storage, storage))
		if err := work.validateCompleteAuthority(); err != nil {
			test.Fatal(err)
		}
		work.repositories = nil
		work.meta = struct{ outbound.MetadataStore }{storage}
		return func(ctx context.Context) error {
			ctx = writeAction(auditOperation(inbound.WithRepositoryActor(ctx, fixture.team.member.ID), "context.authority.pg"), "settings")
			return repositoryWriteErrorWithWork(ctx, work, fixture.repo, func(bound context.Context) error {
				return storage.PutSettingsBundle(bound, fixture.repo, authorityScopeSettings("bmV3"))
			})
		}
	}
	service := documentPublicationServicePG(storage)
	public, err := service.RepositoryAssetCommands()
	if err != nil {
		test.Fatal(err)
	}
	commands, ok := public.(*RepositoryAssetCommands)
	if !ok || commands == nil {
		test.Fatalf("complete asset factory returned %T", public)
	}
	scope, ok := commands.scope.(*repositoryAssetScope)
	if !ok || scope == nil {
		test.Fatalf("complete asset owner uses %T", commands.scope)
	}
	scope.work.repositories = nil
	scope.work.meta = struct{ outbound.MetadataStore }{storage}
	service.meta, service.blobs, service.repositories, service.engine, service.auth = nil, nil, nil, nil, nil
	return func(ctx context.Context) error {
		output, err := commands.SaveSecrets(ctx, fixture.secrets)
		if err != nil && output != (inbound.SaveSecretsOutput{}) {
			return errors.New("failed asset secrets write exposed an acknowledgement")
		}
		if err == nil && (output.Status != "stored" || output.Revision == "" || output.Revision == fixture.secrets.Edit.ExpectedRevision) {
			return errors.New("asset secrets write lost its fresh editing revision")
		}
		return err
	}
}

func repositoryAuthorityAuditsPG(test *testing.T, ctx context.Context, storage *store.PostgresStore, fixture authorityScopeFixture) []domain.OrganizationAuditEvent {
	test.Helper()
	events, err := storage.ListOrganizationAudit(ctx, fixture.team.organization.ID, 100)
	if err != nil {
		test.Fatal(err)
	}
	selected := []domain.OrganizationAuditEvent{}
	for _, event := range events {
		if event.Action == "context.authority.pg" || event.Action == "repository.secrets.updated" {
			selected = append(selected, event)
		}
	}
	return selected
}

func repositoryAuthorityImagePG(test *testing.T, ctx context.Context, storage *store.PostgresStore, fixture authorityScopeFixture) []any {
	test.Helper()
	return append(authorityScopeImage(test, ctx, storage, fixture), repositoryAuthorityAuditsPG(test, ctx, storage, fixture))
}

func repositoryAuthorityAuditPG(test *testing.T, ctx context.Context, storage *store.PostgresStore, fixture authorityScopeFixture, before []domain.OrganizationAuditEvent, action, correlation string) {
	test.Helper()
	after := repositoryAuthorityAuditsPG(test, ctx, storage, fixture)
	if len(after) != len(before)+1 {
		test.Fatalf("write audits=%d, want %d", len(after), len(before)+1)
	}
	previous := make(map[string]domain.OrganizationAuditEvent, len(before))
	for _, event := range before {
		previous[event.ID] = event
	}
	for _, event := range after {
		if prior, exists := previous[event.ID]; exists {
			if !reflect.DeepEqual(prior, event) {
				test.Fatal("authority changed a prior audit event")
			}
			continue
		}
		if domain.ValidateAuditID(event.ID) != nil || event.CreatedAt.IsZero() || event.ActorID != fixture.team.member.ID || event.OrganizationID != fixture.team.organization.ID || event.Action != action || event.TargetType != "repository" || event.TargetID != fixture.team.repository.ID || event.Reason != "" || event.CorrelationID != correlation {
			test.Fatalf("authority audit leaked payload or lost request metadata: %+v", event)
		}
	}
}

func TestPGRepositoryAuthorityCompleteNarrowViewsReadCurrentPolicy(test *testing.T) {
	ctx, storage, fixture, _ := repositoryAuthoritySeedPG(test)
	authority := repositoryAuthorityOwnerPG(test, storage)
	member := inbound.WithRepositoryActor(ctx, fixture.team.member.ID)
	if err := authority.AuthorizeWrite(member, fixture.repo); !errors.Is(err, domain.ErrConflict) {
		test.Fatalf("transactional authority authorized outside its actual transaction: %v", err)
	}
	if err := authority.AuthorizeSecrets(member, fixture.repo, fixture.team.member.ID); !errors.Is(err, domain.ErrConflict) {
		test.Fatalf("secrets authority bypassed its actual lock requirement: %v", err)
	}
	check := func(writeExpected, secretsExpected error) {
		test.Helper()
		if err := storage.WithinRepository(member, fixture.repo, func(bound context.Context) error {
			if err := authority.AuthorizeWrite(writeAction(bound, "settings"), fixture.repo); !errors.Is(err, writeExpected) {
				return errors.New("narrow write owner used stale or broader settings authority")
			}
			if err := authority.AuthorizeSecrets(bound, fixture.repo, fixture.team.member.ID); !errors.Is(err, secretsExpected) {
				return errors.New("narrow secrets owner used stale or broader secrets authority")
			}
			return nil
		}); err != nil {
			test.Fatal(err)
		}
	}
	check(nil, nil)
	ownerPolicy := "owner"
	if _, err := fixture.team.identity.UpdateRepositorySettings(ctx, fixture.team.owner.ID, fixture.team.repository.ID, RepositoryPatch{SecretsPolicy: &ownerPolicy}); err != nil {
		test.Fatal(err)
	}
	check(nil, domain.ErrForbidden)
	if _, err := fixture.team.identity.UpdateRepositorySettings(ctx, fixture.team.owner.ID, fixture.team.repository.ID, RepositoryPatch{SettingsPolicy: &ownerPolicy}); err != nil {
		test.Fatal(err)
	}
	check(domain.ErrForbidden, domain.ErrForbidden)
	if err := fixture.team.identity.UpdateOrganizationMember(ctx, fixture.team.owner.ID, fixture.team.organization.ID, fixture.team.member.ID, domain.OrganizationOwner); err != nil {
		test.Fatal(err)
	}
	check(nil, nil)
	before := repositoryAuthorityAuditsPG(test, ctx, storage, fixture)
	request := inbound.WithCorrelation(member, "repository-authority-narrow-pg")
	if err := storage.WithinRepository(request, fixture.repo, func(bound context.Context) error {
		return authority.AuditWrite(auditOperation(bound, "context.authority.pg"), fixture.repo)
	}); err != nil {
		test.Fatal(err)
	}
	repositoryAuthorityAuditPG(test, ctx, storage, fixture, before, "context.authority.pg", "repository-authority-narrow-pg")
	if err := fixture.team.identity.UpdateOrganizationMember(ctx, fixture.team.owner.ID, fixture.team.organization.ID, fixture.team.member.ID, domain.OrganizationMember); err != nil {
		test.Fatal(err)
	}
	check(domain.ErrForbidden, domain.ErrForbidden)
	freshRepo := hh(string(fixture.repo) + " late repository association")
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: freshRepo}); err != nil {
		test.Fatal(err)
	}
	checkAssociation := func(expected error) {
		test.Helper()
		if err := storage.WithinRepository(member, freshRepo, func(bound context.Context) error {
			if err := authority.AuthorizeWrite(bound, freshRepo); !errors.Is(err, expected) {
				return errors.New("authority did not read the current metadata-to-repository association")
			}
			return nil
		}); err != nil {
			test.Fatal(err)
		}
	}
	checkAssociation(domain.ErrForbidden)
	freshRepository, err := fixture.team.identity.CreateOrganizationRepository(ctx, fixture.team.owner, fixture.team.organization.ID, "authority-"+domain.NewID(""))
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.AddMember(ctx, domain.Membership{RepositoryID: freshRepository.ID, UserID: fixture.team.member.ID, Role: domain.RoleMember}); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: freshRepo, RepositoryID: freshRepository.ID}); err != nil {
		test.Fatal(err)
	}
	checkAssociation(nil)
}

type repositoryAuthorityIdentityHoldPG struct {
	*store.PostgresStore
	ready   chan struct{}
	release <-chan struct{}
}

func (storage *repositoryAuthorityIdentityHoldPG) WithinIdentity(ctx context.Context, operation func(context.Context) error) error {
	return storage.PostgresStore.WithinIdentity(ctx, func(bound context.Context) error {
		if err := operation(bound); err != nil {
			return err
		}
		close(storage.ready)
		select {
		case <-storage.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

func repositoryAuthorityPrepareInheritancePG(test *testing.T, ctx context.Context, storage *store.PostgresStore, fixture authorityScopeFixture, mutation string) {
	test.Helper()
	if mutation == "direct" {
		return
	}
	if err := storage.RemoveMember(ctx, fixture.team.repository.ID, fixture.team.member.ID); err != nil {
		test.Fatal(err)
	}
	identity := fixture.team.identity
	switch mutation {
	case "team-grant":
		if err := identity.UpdateTeamMember(ctx, fixture.team.owner.ID, fixture.team.organization.ID, fixture.team.team.ID, fixture.team.member.ID, domain.TeamMember); err != nil {
			test.Fatal(err)
		}
		if err := identity.SetTeamRepository(ctx, fixture.team.owner.ID, fixture.team.organization.ID, fixture.team.team.ID, fixture.team.repository.ID, domain.RoleMaintainer); err != nil {
			test.Fatal(err)
		}
	case "organization-owner":
		if err := identity.UpdateOrganizationMember(ctx, fixture.team.owner.ID, fixture.team.organization.ID, fixture.team.member.ID, domain.OrganizationOwner); err != nil {
			test.Fatal(err)
		}
		policy := "owner"
		if _, err := identity.UpdateRepositorySettings(ctx, fixture.team.owner.ID, fixture.team.repository.ID, RepositoryPatch{SettingsPolicy: &policy, SecretsPolicy: &policy}); err != nil {
			test.Fatal(err)
		}
	case "organization-policy":
		policy, err := identity.GetOrganizationPolicy(ctx, fixture.team.owner.ID, fixture.team.organization.ID)
		if err != nil {
			test.Fatal(err)
		}
		policy.DefaultRepositoryRole = domain.RoleMaintainer
		if _, err := identity.UpdateOrganizationPolicy(ctx, fixture.team.owner.ID, policy); err != nil {
			test.Fatal(err)
		}
	default:
		test.Fatalf("unknown inheritance fixture %q", mutation)
	}
	if role, present := identity.RoleOf(ctx, fixture.team.repository.ID, fixture.team.member.ID); !present || !role.AtLeast(domain.RoleMaintainer) {
		test.Fatalf("actual inherited fixture has no write authority: %q %v", role, present)
	}
}

func repositoryAuthorityRevokePG(ctx context.Context, identity *IdentityService, fixture authorityScopeFixture, mutation string) error {
	switch mutation {
	case "direct":
		return identity.RemoveMember(ctx, fixture.team.owner.ID, fixture.team.repository.ID, fixture.team.member.ID)
	case "team-grant":
		return identity.RemoveTeamRepository(ctx, fixture.team.owner.ID, fixture.team.organization.ID, fixture.team.team.ID, fixture.team.repository.ID)
	case "organization-owner":
		return identity.UpdateOrganizationMember(ctx, fixture.team.owner.ID, fixture.team.organization.ID, fixture.team.member.ID, domain.OrganizationMember)
	case "organization-policy":
		policy, err := identity.GetOrganizationPolicy(ctx, fixture.team.owner.ID, fixture.team.organization.ID)
		if err != nil {
			return err
		}
		policy.DefaultRepositoryRole = domain.RoleViewer
		_, err = identity.UpdateOrganizationPolicy(ctx, fixture.team.owner.ID, policy)
		return err
	default:
		return domain.ErrValidation
	}
}

func repositoryAuthorityOrderingPG(test *testing.T, kind, mutation, order string) {
	test.Helper()
	ctx, storage, fixture, observer := repositoryAuthoritySeedPG(test)
	repositoryAuthorityPrepareInheritancePG(test, ctx, storage, fixture, mutation)
	writer, writerName := authorityScopePeer(test, ctx)
	revoker, revokerName := authorityScopePeer(test, ctx)
	before := repositoryAuthorityImagePG(test, ctx, storage, fixture)
	audits := repositoryAuthorityAuditsPG(test, ctx, storage, fixture)
	release := make(chan struct{})
	var released sync.Once
	defer released.Do(func() { close(release) })
	first, second := make(chan error, 1), make(chan error, 1)
	var run func(context.Context) error
	if order == "revoke-first" {
		run = repositoryAuthorityWriterPG(test, writer, fixture, kind)
		var identity *IdentityService
		var ready <-chan struct{}
		if mutation == "direct" {
			paused := &authorityScopeHoldPG{PostgresStore: revoker, ready: make(chan struct{}), release: release}
			identity, ready = NewIdentityService(nil, paused), paused.ready
		} else {
			paused := &repositoryAuthorityIdentityHoldPG{PostgresStore: revoker, ready: make(chan struct{}), release: release}
			identity, ready = NewIdentityService(nil, paused), paused.ready
		}
		go func() { first <- repositoryAuthorityRevokePG(ctx, identity, fixture, mutation) }()
		authorityScopeAwait(test, ctx, ready, first)
		go func() { second <- run(ctx) }()
		statement := "repositories"
		if mutation != "direct" {
			statement = "pg_advisory_xact_lock_shared"
		}
		authorityScopeBlocked(test, ctx, observer, writerName, revokerName, statement, second)
	} else {
		paused := &authorityScopeWriteHoldPG{PostgresStore: writer, ready: make(chan struct{}), release: release}
		run = repositoryAuthorityWriterPG(test, paused, fixture, kind)
		go func() { first <- run(ctx) }()
		authorityScopeAwait(test, ctx, paused.ready, first)
		go func() {
			second <- repositoryAuthorityRevokePG(ctx, NewIdentityService(nil, revoker), fixture, mutation)
		}()
		statement := "repositories"
		if mutation != "direct" {
			statement = "pg_advisory_xact_lock("
		}
		authorityScopeBlocked(test, ctx, observer, revokerName, writerName, statement, second)
	}
	if current := repositoryAuthorityImagePG(test, ctx, storage, fixture); !reflect.DeepEqual(current, before) {
		test.Fatal("held authority/write transaction exposed uncommitted durable effects")
	}
	released.Do(func() { close(release) })
	if err := authorityScopeDone(test, ctx, first); err != nil {
		test.Fatal(err)
	}
	secondError := authorityScopeDone(test, ctx, second)
	after := repositoryAuthorityImagePG(test, ctx, storage, fixture)
	if order == "revoke-first" {
		if !errors.Is(secondError, domain.ErrForbidden) || !reflect.DeepEqual(after, before) {
			test.Fatalf("queued writer retained revoked current authority or durable effects: %v", secondError)
		}
	} else {
		if secondError != nil {
			test.Fatal(secondError)
		}
		expectedRevision := before[3].(domain.RepositoryRevision)
		expectedRevision.Graph++
		if after[3].(domain.RepositoryRevision) != expectedRevision {
			test.Fatal("captured common writer did not commit exactly one graph revision")
		}
		action, notifications := "context.authority.pg", 0
		if kind == "secrets" {
			action, notifications = "repository.secrets.updated", 1
			if domain.SecretsRevision(after[2].([]byte)) == fixture.secrets.Edit.ExpectedRevision {
				test.Fatal("in-flight asset secrets write lost its durable editing generation")
			}
		} else if after[1].(domain.SettingsBundle).Files[0].ContentB64 != "bmV3" {
			test.Fatal("in-flight common writer lost its durable settings")
		}
		if len(after[5].([]domain.NotificationJob)) != len(before[5].([]domain.NotificationJob))+notifications {
			test.Fatal("authority extraction changed the writer's notification budget")
		}
		repositoryAuthorityAuditPG(test, ctx, storage, fixture, audits, action, "")
	}
	if err := run(ctx); !errors.Is(err, domain.ErrForbidden) {
		test.Fatalf("reused writer retained revoked current authority: %v", err)
	}
	if current := repositoryAuthorityImagePG(test, ctx, storage, fixture); !reflect.DeepEqual(current, after) {
		test.Fatal("late denial changed settings, secrets, revision, reflog, outbox or audit")
	}
}

func TestPGRepositoryAuthorityDirectMembershipOrdersCommonAndSecrets(test *testing.T) {
	for _, kind := range []string{"common", "secrets"} {
		for _, order := range []string{"revoke-first", "write-first"} {
			test.Run(kind+"/"+order, func(test *testing.T) {
				repositoryAuthorityOrderingPG(test, kind, "direct", order)
			})
		}
	}
}

func TestPGRepositoryAuthorityInheritedCurrentPolicyOrdersCommonAndSecrets(test *testing.T) {
	for _, mutation := range []string{"team-grant", "organization-owner", "organization-policy"} {
		for _, kind := range []string{"common", "secrets"} {
			for _, order := range []string{"revoke-first", "write-first"} {
				test.Run(mutation+"/"+kind+"/"+order, func(test *testing.T) {
					repositoryAuthorityOrderingPG(test, kind, mutation, order)
				})
			}
		}
	}
}

func TestPGRepositoryAuthorityCommonAndSecretsRollbackAuditAndOuterFailure(test *testing.T) {
	for _, kind := range []string{"common", "secrets"} {
		for _, phase := range []string{"audit", "outer"} {
			test.Run(kind+"/"+phase, func(test *testing.T) {
				ctx, storage, fixture, _ := repositoryAuthoritySeedPG(test)
				failure := errors.New("repository authority native " + phase + " failure")
				probe := &documentPublicationProbePG{PostgresStore: storage, calls: make(map[string]int), failure: failure}
				if phase == "audit" {
					probe.failAt = "audit"
				}
				run := repositoryAuthorityWriterPG(test, probe, fixture, kind)
				before := repositoryAuthorityImagePG(test, ctx, storage, fixture)
				var err error
				if phase == "outer" {
					err = probe.WithinRepository(ctx, fixture.repo, func(bound context.Context) error {
						if err := run(bound); err != nil {
							return err
						}
						if reflect.DeepEqual(repositoryAuthorityImagePG(test, bound, storage, fixture), before) {
							return errors.New("bound outer transaction did not contain actual body and audit writes")
						}
						if !reflect.DeepEqual(repositoryAuthorityImagePG(test, ctx, storage, fixture), before) {
							return errors.New("authority committed ahead of its surrounding transaction")
						}
						return failure
					})
				} else {
					err = run(ctx)
				}
				if !errors.Is(err, failure) || probe.transactions != 1 || probe.calls["revision"] != 1 || probe.calls["audit"] != 1 {
					test.Fatalf("actual body/revision/audit rollback boundary changed: calls=%v transactions=%d err=%v", probe.calls, probe.transactions, err)
				}
				if current := repositoryAuthorityImagePG(test, ctx, storage, fixture); !reflect.DeepEqual(current, before) {
					test.Fatal("failed common authority left settings, secret CAS, revision, reflog, outbox or audit effects")
				}
			})
		}
	}
}

type repositoryAuthorityReadsPG struct {
	RepositoryAuthorityRepositories
	reads int
}

func (reader *repositoryAuthorityReadsPG) GetRepository(ctx context.Context, repository string) (domain.Repository, error) {
	reader.reads++
	return reader.RepositoryAuthorityRepositories.GetRepository(ctx, repository)
}

func (reader *repositoryAuthorityReadsPG) ListMembers(ctx context.Context, repository string) ([]domain.Membership, error) {
	reader.reads++
	return reader.RepositoryAuthorityRepositories.ListMembers(ctx, repository)
}

func TestPGRepositoryAuthorityCancelledActualAccessLockDoesNotBypass(test *testing.T) {
	for _, kind := range []string{"narrow-write", "narrow-secrets", "common", "secrets"} {
		test.Run(kind, func(test *testing.T) {
			ctx, storage, fixture, observer := repositoryAuthoritySeedPG(test)
			writer, writerName := authorityScopePeer(test, ctx)
			holderName := "rwa59-membership-" + domain.NewID("")
			holder, err := observer.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = holder.Rollback(cleanup)
			}()
			var acceptedName string
			if err := holder.QueryRow(ctx, `SELECT set_config('application_name',$1,true)`, holderName).Scan(&acceptedName); err != nil || acceptedName != holderName {
				test.Fatal("PostgreSQL changed the SQL lock observer identity", acceptedName, holderName, err)
			}
			if _, err := holder.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE repository_id=$1 AND user_id=$2`, fixture.team.repository.ID, fixture.team.member.ID); err != nil {
				test.Fatal(err)
			}
			reads := &repositoryAuthorityReadsPG{RepositoryAuthorityRepositories: writer}
			config := repositoryAuthorityConfigPG(writer)
			config.Repositories = reads
			authority, err := NewRepositoryAuthority(config)
			if err != nil {
				test.Fatal(err)
			}
			probe := &documentPublicationProbePG{PostgresStore: writer, calls: make(map[string]int)}
			var run func(context.Context) error
			if strings.HasPrefix(kind, "narrow-") {
				run = func(request context.Context) error {
					return writer.WithinRepository(request, fixture.repo, func(bound context.Context) error {
						if kind == "narrow-secrets" {
							return authority.AuthorizeSecrets(bound, fixture.repo, fixture.team.member.ID)
						}
						return authority.AuthorizeWrite(bound, fixture.repo)
					})
				}
			} else {
				run = repositoryAuthorityWriterPG(test, probe, fixture, kind)
			}
			before := repositoryAuthorityImagePG(test, ctx, storage, fixture)
			request, cancel := context.WithCancel(inbound.WithRepositoryActor(ctx, fixture.team.member.ID))
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- run(request) }()
			defer func() {
				if !test.Failed() {
					return
				}
				diagnostic, stop := context.WithTimeout(context.Background(), 2*time.Second)
				defer stop()
				var report string
				err := observer.QueryRow(diagnostic, `SELECT COALESCE(json_agg(json_build_object('application',application_name,'state',state,'wait_type',wait_event_type,'wait',wait_event,'query',query,'blocked_by',pg_blocking_pids(pid)))::text,'[]') FROM pg_stat_activity WHERE application_name=$1 OR application_name=$2`, writerName, holderName).Scan(&report)
				test.Log("authority SQL boundary diagnostic", report, err)
			}()
			authorityScopeBlocked(test, ctx, observer, writerName, holderName, "memberships", done)
			cancel()
			if err := authorityScopeDone(test, ctx, done); !errors.Is(err, context.Canceled) || reads.reads != 0 || len(probe.calls) != 0 {
				test.Fatalf("cancelled access-lock waiter read roles or crossed revision/audit: reads=%d calls=%v err=%v", reads.reads, probe.calls, err)
			}
			if err := holder.Rollback(ctx); err != nil {
				test.Fatal(err)
			}
			if current := repositoryAuthorityImagePG(test, ctx, storage, fixture); !reflect.DeepEqual(current, before) {
				test.Fatal("cancelled real access-lock waiter left durable write effects")
			}
		})
	}
}

func TestPGRepositoryAuthorityNestedCommonScopeKeepsOneRevisionAuditAndCompletion(test *testing.T) {
	for _, kind := range []string{"common", "secrets"} {
		test.Run(kind, func(test *testing.T) {
			ctx, storage, fixture, _ := repositoryAuthoritySeedPG(test)
			probe := &documentPublicationProbePG{PostgresStore: storage, calls: make(map[string]int)}
			run := repositoryAuthorityWriterPG(test, probe, fixture, kind)
			work := newRepositoryWork(probe, probe, ReadDocumentIdentities(probe, probe))
			work.repositories = nil
			before := repositoryAuthorityImagePG(test, ctx, storage, fixture)
			audits := repositoryAuthorityAuditsPG(test, ctx, storage, fixture)
			member := auditOperation(inbound.WithRepositoryActor(ctx, fixture.team.member.ID), "context.authority.pg")
			abort := errors.New("nested repository authority abort")
			callbacks := 0
			for _, rollback := range []bool{true, false} {
				err := repositoryWriteErrorWithWork(member, work, fixture.repo, func(bound context.Context) error {
					if err := run(bound); err != nil {
						return err
					}
					afterRepositoryCommit(bound, func() { callbacks++ })
					if callbacks != 0 || !reflect.DeepEqual(repositoryAuthorityImagePG(test, ctx, storage, fixture), before) || !reflect.DeepEqual(repositoryAuthorityAuditsPG(test, bound, storage, fixture), audits) {
						return errors.New("nested writer committed, audited or completed before the common outer scope")
					}
					if rollback {
						return abort
					}
					return nil
				})
				if rollback {
					if !errors.Is(err, abort) || callbacks != 0 || !reflect.DeepEqual(repositoryAuthorityImagePG(test, ctx, storage, fixture), before) {
						test.Fatalf("nested rollback escaped the shared authority transaction: callbacks=%d err=%v", callbacks, err)
					}
				} else if err != nil || callbacks != 1 {
					test.Fatalf("nested common completion did not run once: callbacks=%d err=%v", callbacks, err)
				}
			}
			after := repositoryAuthorityImagePG(test, ctx, storage, fixture)
			expected := before[3].(domain.RepositoryRevision)
			expected.Graph++
			if after[3].(domain.RepositoryRevision) != expected || probe.transactions != 2 || probe.calls["revision"] != 1 || probe.calls["audit"] != 1 {
				test.Fatalf("nested authority split transaction/revision/audit ownership: calls=%v transactions=%d", probe.calls, probe.transactions)
			}
			repositoryAuthorityAuditPG(test, ctx, storage, fixture, audits, "context.authority.pg", "")
		})
	}
}

func TestPGRepositoryAuthorityCommonAndSecretsRejectIncompatibleTransactions(test *testing.T) {
	for _, kind := range []string{"common", "secrets"} {
		for _, boundary := range []string{"read-only", "foreign-store", "foreign-repository", "foreign-locker"} {
			test.Run(kind+"/"+boundary, func(test *testing.T) {
				ctx, storage, fixture, _ := repositoryAuthoritySeedPG(test)
				peer, _ := authorityScopePeer(test, ctx)
				probe := &documentPublicationProbePG{PostgresStore: storage, calls: make(map[string]int)}
				run := repositoryAuthorityWriterPG(test, probe, fixture, kind)
				if boundary == "foreign-store" {
					run = repositoryAuthorityWriterPG(test, peer, fixture, kind)
				}
				if boundary == "foreign-locker" {
					config := repositoryAuthorityConfigPG(storage)
					config.Locker = struct {
						outbound.RepositoryAccessLocker
					}{peer}
					authority, err := NewRepositoryAuthority(config)
					if err != nil {
						test.Fatal(err)
					}
					run = func(bound context.Context) error {
						if kind == "secrets" {
							return authority.AuthorizeSecrets(bound, fixture.repo, fixture.team.member.ID)
						}
						return authority.AuthorizeWrite(bound, fixture.repo)
					}
				}
				before := repositoryAuthorityImagePG(test, ctx, storage, fixture)
				member := inbound.WithRepositoryActor(ctx, fixture.team.member.ID)
				check := func(bound context.Context) error {
					if err := run(bound); !errors.Is(err, domain.ErrConflict) {
						return errors.New("authority escaped its incompatible bound PostgreSQL transaction")
					}
					return nil
				}
				var err error
				if boundary == "read-only" {
					err = storage.WithinReadSnapshot(member, check)
				} else {
					repo := fixture.repo
					if boundary == "foreign-repository" {
						repo = hh(string(fixture.repo) + " another authority transaction")
					}
					err = storage.WithinRepository(member, repo, check)
				}
				if err != nil || len(probe.calls) != 0 || !reflect.DeepEqual(repositoryAuthorityImagePG(test, ctx, storage, fixture), before) {
					test.Fatalf("incompatible authority transaction bypassed its fence: calls=%v err=%v", probe.calls, err)
				}
			})
		}
	}
}
