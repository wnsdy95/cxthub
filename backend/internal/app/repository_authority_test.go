package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type rwa59AuthorityMetadata struct {
	fixture *rwa59AuthorityFixture
}

type rwa59AuthorityRepositories struct {
	fixture *rwa59AuthorityFixture
}

type rwa59AuthorityTeams struct {
	fixture *rwa59AuthorityFixture
}

type rwa59AuthorityOrganizations struct {
	fixture *rwa59AuthorityFixture
}

type rwa59AuthorityLocker struct {
	fixture *rwa59AuthorityFixture
}

type rwa59AuthorityAudit struct {
	fixture *rwa59AuthorityFixture
}

var (
	_ RepositoryAuthorityMetadata      = (*rwa59AuthorityMetadata)(nil)
	_ RepositoryAuthorityRepositories  = (*rwa59AuthorityRepositories)(nil)
	_ RepositoryAuthorityTeams         = (*rwa59AuthorityTeams)(nil)
	_ RepositoryAuthorityOrganizations = (*rwa59AuthorityOrganizations)(nil)
	_ outbound.RepositoryAccessLocker  = (*rwa59AuthorityLocker)(nil)
	_ RepositoryAuthorityAudit         = (*rwa59AuthorityAudit)(nil)
)

type rwa59AuthorityFixture struct {
	test              *testing.T
	repo              domain.ContentHash
	actor             string
	expectedActor     string
	expectedContext   context.Context
	calls             []string
	metadata          domain.Repo
	repository        domain.Repository
	members           []domain.Membership
	grants            []domain.TeamRepositoryAccess
	organization      domain.OrganizationRepositoryAccess
	namespace         domain.Namespace
	failures          map[string]error
	audits            []domain.OrganizationAuditEvent
	onLock            func()
	metadataPort      *rwa59AuthorityMetadata
	repositoriesPort  *rwa59AuthorityRepositories
	teamsPort         *rwa59AuthorityTeams
	organizationsPort *rwa59AuthorityOrganizations
	lockerPort        *rwa59AuthorityLocker
	auditPort         *rwa59AuthorityAudit
}

func newRWA59AuthorityFixture(test *testing.T) *rwa59AuthorityFixture {
	test.Helper()
	fixture := &rwa59AuthorityFixture{
		test: test, repo: domain.HashContent([]byte(test.Name())), actor: "authority-actor",
		failures: make(map[string]error),
		repository: domain.Repository{
			ID: "ws_11111111111111111111111111111111", OwnerID: "authority-creator",
			OwnerNamespaceID: "ns_22222222222222222222222222222222",
		},
		namespace: domain.Namespace{
			ID: "ns_22222222222222222222222222222222", Kind: domain.NamespaceOrganization,
			OrganizationID: "ent_33333333333333333333333333333333",
		},
	}
	fixture.expectedActor = fixture.actor
	fixture.metadata = domain.Repo{ID: fixture.repo, RepositoryID: fixture.repository.ID}
	fixture.members = []domain.Membership{{RepositoryID: fixture.repository.ID, UserID: fixture.actor, Role: domain.RoleMaintainer}}
	fixture.metadataPort = &rwa59AuthorityMetadata{fixture: fixture}
	fixture.repositoriesPort = &rwa59AuthorityRepositories{fixture: fixture}
	fixture.teamsPort = &rwa59AuthorityTeams{fixture: fixture}
	fixture.organizationsPort = &rwa59AuthorityOrganizations{fixture: fixture}
	fixture.lockerPort = &rwa59AuthorityLocker{fixture: fixture}
	fixture.auditPort = &rwa59AuthorityAudit{fixture: fixture}
	return fixture
}

func (fixture *rwa59AuthorityFixture) config() RepositoryAuthorityConfig {
	return RepositoryAuthorityConfig{
		Profile: RepositoryAuthorityComplete, Metadata: fixture.metadataPort,
		Repositories: fixture.repositoriesPort, Teams: fixture.teamsPort,
		Organizations: fixture.organizationsPort, Locker: fixture.lockerPort,
		Audit: fixture.auditPort, Transactional: true,
	}
}

func (fixture *rwa59AuthorityFixture) construct(config RepositoryAuthorityConfig) *RepositoryAuthority {
	fixture.test.Helper()
	authority, err := NewRepositoryAuthority(config)
	if err != nil || authority == nil {
		fixture.test.Fatal("narrow authority construction failed", authority, err)
	}
	fixture.expectCalls()
	return authority
}

func (fixture *rwa59AuthorityFixture) begin(ctx context.Context) {
	fixture.expectedContext = ctx
	fixture.calls = nil
}

func (fixture *rwa59AuthorityFixture) record(ctx context.Context, stage string) error {
	fixture.test.Helper()
	fixture.calls = append(fixture.calls, stage)
	if ctx != fixture.expectedContext {
		fixture.test.Fatalf("%s replaced the caller's context", stage)
	}
	return fixture.failures[stage]
}

func (fixture *rwa59AuthorityFixture) expectCalls(expected ...string) {
	fixture.test.Helper()
	if !reflect.DeepEqual(fixture.calls, expected) {
		fixture.test.Fatalf("authority calls = %v, want %v", fixture.calls, expected)
	}
}

func (fixture *rwa59AuthorityFixture) teamGrant(role domain.MemberRole) domain.TeamRepositoryAccess {
	return domain.TeamRepositoryAccess{
		TeamID: "team_44444444444444444444444444444444", RepositoryID: fixture.repository.ID,
		OrganizationNamespaceID: fixture.repository.OwnerNamespaceID, UserID: fixture.actor,
		OrganizationMember: true, TeamMember: true, Role: role,
	}
}

func (fixture *rwa59AuthorityFixture) organizationAccess(role domain.OrganizationRole, base domain.MemberRole) domain.OrganizationRepositoryAccess {
	return domain.OrganizationRepositoryAccess{
		RepositoryID: fixture.repository.ID, OrganizationNamespaceID: fixture.repository.OwnerNamespaceID,
		UserID: fixture.actor, Role: role, DefaultRepositoryRole: base,
	}
}

func (port *rwa59AuthorityMetadata) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	fixture := port.fixture
	if repo != fixture.repo {
		fixture.test.Fatalf("metadata repo = %q, want %q", repo, fixture.repo)
	}
	return fixture.metadata, fixture.record(ctx, "metadata")
}

func (port *rwa59AuthorityRepositories) GetRepository(ctx context.Context, repository string) (domain.Repository, error) {
	fixture := port.fixture
	if repository != fixture.metadata.RepositoryID {
		fixture.test.Fatalf("repository lookup = %q, current binding = %q", repository, fixture.metadata.RepositoryID)
	}
	return fixture.repository, fixture.record(ctx, "repository")
}

func (port *rwa59AuthorityRepositories) ListMembers(ctx context.Context, repository string) ([]domain.Membership, error) {
	fixture := port.fixture
	if repository != fixture.repository.ID {
		fixture.test.Fatalf("membership repository = %q, want %q", repository, fixture.repository.ID)
	}
	return fixture.members, fixture.record(ctx, "members")
}

func (port *rwa59AuthorityTeams) RepositoryTeamAccess(ctx context.Context, repository, actor string) ([]domain.TeamRepositoryAccess, error) {
	fixture := port.fixture
	if repository != fixture.repository.ID || actor != fixture.expectedActor {
		fixture.test.Fatalf("team access = (%q, %q), want (%q, %q)", repository, actor, fixture.repository.ID, fixture.expectedActor)
	}
	return fixture.grants, fixture.record(ctx, "teams")
}

func (port *rwa59AuthorityOrganizations) RepositoryOrganizationAccess(ctx context.Context, repository, actor string) (domain.OrganizationRepositoryAccess, error) {
	fixture := port.fixture
	if repository != fixture.repository.ID || actor != fixture.expectedActor {
		fixture.test.Fatalf("organization access = (%q, %q), want (%q, %q)", repository, actor, fixture.repository.ID, fixture.expectedActor)
	}
	return fixture.organization, fixture.record(ctx, "organization")
}

func (port *rwa59AuthorityLocker) LockRepositoryAccess(ctx context.Context, repository, actor string) error {
	fixture := port.fixture
	if repository != fixture.metadata.RepositoryID || actor != fixture.expectedActor {
		fixture.test.Fatalf("access lock = (%q, %q), want (%q, %q)", repository, actor, fixture.metadata.RepositoryID, fixture.expectedActor)
	}
	if err := fixture.record(ctx, "lock"); err != nil {
		return err
	}
	if fixture.onLock != nil {
		fixture.onLock()
	}
	return nil
}

func (port *rwa59AuthorityAudit) GetNamespace(ctx context.Context, namespace string) (domain.Namespace, error) {
	fixture := port.fixture
	if namespace != fixture.repository.OwnerNamespaceID {
		fixture.test.Fatalf("audit namespace = %q, want %q", namespace, fixture.repository.OwnerNamespaceID)
	}
	return fixture.namespace, fixture.record(ctx, "namespace")
}

func (port *rwa59AuthorityAudit) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	fixture := port.fixture
	fixture.audits = append(fixture.audits, event)
	return fixture.record(ctx, "audit")
}

func TestRWA59RepositoryAuthorityUsesGenuinelyNarrowPorts(test *testing.T) {
	fixture := newRWA59AuthorityFixture(test)
	if _, broad := any(fixture.metadataPort).(outbound.MetadataStore); broad {
		test.Fatal("metadata fake unexpectedly implements the broad store")
	}
	if _, broad := any(fixture.repositoriesPort).(outbound.RepositoryStore); broad {
		test.Fatal("repository fake unexpectedly implements the broad store")
	}
	if _, broad := any(fixture.teamsPort).(outbound.TeamStore); broad {
		test.Fatal("team fake unexpectedly implements the broad store")
	}
	if _, broad := any(fixture.auditPort).(outbound.OrganizationStore); broad {
		test.Fatal("audit fake unexpectedly implements the broad store")
	}
	authority := fixture.construct(fixture.config())
	ctx := inbound.WithRepositoryActor(context.Background(), fixture.actor)
	fixture.begin(ctx)
	if err := authority.AuthorizeWrite(ctx, fixture.repo); err != nil {
		test.Fatal("narrow-only authority rejected a maintainer", err)
	}
	fixture.expectCalls("metadata", "lock", "repository", "members", "teams", "organization")
}

func TestRWA59RepositoryAuthorityConstructorProfilesAndNoIO(test *testing.T) {
	for _, scenario := range []string{"complete-transactional", "complete-nontransactional", "compatibility-absent-optional", "compatibility-empty"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newRWA59AuthorityFixture(test)
			config := fixture.config()
			switch scenario {
			case "complete-nontransactional":
				config.Transactional, config.Locker = false, nil
			case "compatibility-absent-optional":
				config.Profile = RepositoryAuthorityCompatibility
				config.Repositories, config.Teams, config.Organizations, config.Locker, config.Audit = nil, nil, nil, nil, nil
			case "compatibility-empty":
				config = RepositoryAuthorityConfig{Profile: RepositoryAuthorityCompatibility}
			}
			fixture.construct(config)
		})
	}
	for _, profile := range []RepositoryAuthorityProfile{RepositoryAuthorityCompatibility, RepositoryAuthorityComplete} {
		for _, dependency := range []string{"metadata", "repositories", "teams", "organizations", "locker", "audit"} {
			test.Run(string(profile)+"/typed-nil-"+dependency, func(test *testing.T) {
				fixture := newRWA59AuthorityFixture(test)
				config := fixture.config()
				config.Profile = profile
				switch dependency {
				case "metadata":
					config.Metadata = (*rwa59AuthorityMetadata)(nil)
				case "repositories":
					config.Repositories = (*rwa59AuthorityRepositories)(nil)
				case "teams":
					config.Teams = (*rwa59AuthorityTeams)(nil)
				case "organizations":
					config.Organizations = (*rwa59AuthorityOrganizations)(nil)
				case "locker":
					config.Transactional = false
					config.Locker = (*rwa59AuthorityLocker)(nil)
				case "audit":
					config.Audit = (*rwa59AuthorityAudit)(nil)
				}
				if authority, err := NewRepositoryAuthority(config); authority != nil || !errors.Is(err, domain.ErrValidation) {
					test.Fatal("typed-nil port was treated as an absent capability", authority, err)
				}
				fixture.expectCalls()
			})
		}
	}
	for _, profile := range []RepositoryAuthorityProfile{"", "unknown"} {
		test.Run("invalid-profile-"+string(profile), func(test *testing.T) {
			fixture := newRWA59AuthorityFixture(test)
			config := fixture.config()
			config.Profile = profile
			if authority, err := NewRepositoryAuthority(config); authority != nil || !errors.Is(err, domain.ErrValidation) {
				test.Fatal("unknown profile accepted", authority, err)
			}
			fixture.expectCalls()
		})
	}
}

func TestRWA59RepositoryAuthorityCompleteRejectsMissingCapabilitiesBeforeIO(test *testing.T) {
	for _, dependency := range []string{"metadata", "repositories", "teams", "organizations", "locker", "audit"} {
		test.Run(dependency, func(test *testing.T) {
			fixture := newRWA59AuthorityFixture(test)
			config := fixture.config()
			switch dependency {
			case "metadata":
				config.Metadata = nil
			case "repositories":
				config.Repositories = nil
			case "teams":
				config.Teams = nil
			case "organizations":
				config.Organizations = nil
			case "locker":
				config.Locker = nil
			case "audit":
				config.Audit = nil
			}
			if authority, err := NewRepositoryAuthority(config); authority != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
				test.Fatal("incomplete authority did not fail before use", authority, err)
			}
			fixture.expectCalls()
		})
	}
}

func TestRWA59RepositoryAuthorityCapturesConfigFieldsWithoutCachingFacts(test *testing.T) {
	fixture := newRWA59AuthorityFixture(test)
	config := fixture.config()
	authority := fixture.construct(config)
	if authority.metadata != config.Metadata || authority.repositories != config.Repositories || authority.teams != config.Teams ||
		authority.organizations != config.Organizations || authority.locker != config.Locker || authority.audit != config.Audit || !authority.transactional {
		test.Fatal("constructor did not capture the explicit narrow dependencies")
	}
	replacement := newRWA59AuthorityFixture(test)
	config = replacement.config()
	config.Profile, config.Transactional = RepositoryAuthorityCompatibility, false
	ctx := inbound.WithRepositoryActor(context.Background(), fixture.actor)
	fixture.begin(ctx)
	if err := authority.AuthorizeWrite(ctx, fixture.repo); err != nil {
		test.Fatal(err)
	}
	fixture.expectCalls("metadata", "lock", "repository", "members", "teams", "organization")
	fixture.begin(ctx)
	if err := authority.AuthorizeSecrets(ctx, fixture.repo, fixture.actor); err != nil {
		test.Fatal(err)
	}
	fixture.expectCalls("metadata", "lock", "repository", "members", "teams", "organization")
	fixture.begin(ctx)
	if err := authority.AuditWrite(ctx, fixture.repo); err != nil {
		test.Fatal(err)
	}
	fixture.expectCalls("metadata", "repository", "namespace", "audit")
	replacement.expectCalls()
	fixture.members = nil
	fixture.begin(ctx)
	if err := authority.AuthorizeWrite(ctx, fixture.repo); err != domain.ErrForbidden {
		test.Fatal("captured dependencies cached a removed membership", err)
	}
	fixture.expectCalls("metadata", "lock", "repository", "members", "teams", "organization")
}

func TestRWA59RepositoryAuthorityWriteGuardOrder(test *testing.T) {
	for _, scenario := range []string{"system", "system-invalid-action", "anonymous", "anonymous-register", "authenticated-register", "absent-repositories", "absent-metadata"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newRWA59AuthorityFixture(test)
			config := fixture.config()
			config.Profile = RepositoryAuthorityCompatibility
			ctx := inbound.WithRepositoryActor(context.Background(), fixture.actor)
			var expected error
			switch scenario {
			case "system":
				ctx = inbound.WithSystemActor(context.Background())
				config.Metadata, config.Repositories = nil, nil
			case "system-invalid-action":
				ctx = writeAction(inbound.WithSystemActor(context.Background()), "invalid")
			case "anonymous":
				ctx, expected = context.Background(), domain.ErrUnauthorized
				config.Metadata, config.Repositories = nil, nil
			case "anonymous-register":
				ctx, expected = writeAction(context.Background(), "register"), domain.ErrUnauthorized
			case "authenticated-register":
				ctx = writeAction(ctx, "register")
				config.Metadata, config.Repositories = nil, nil
			case "absent-repositories":
				config.Repositories, expected = nil, domain.ErrForbidden
			case "absent-metadata":
				config.Metadata, expected = nil, domain.ErrForbidden
			}
			fixture.failures["metadata"] = errors.New("guard reached metadata")
			authority := fixture.construct(config)
			fixture.begin(ctx)
			if err := authority.AuthorizeWrite(ctx, fixture.repo); err != expected {
				test.Fatalf("guard error = %v, want %v", err, expected)
			}
			fixture.expectCalls()
		})
	}
}

func TestRWA59RepositoryAuthorityMetadataFailuresAndUnboundDeny(test *testing.T) {
	for _, secrets := range []bool{false, true} {
		for _, scenario := range []string{"missing", "error", "unbound"} {
			test.Run(fmt.Sprintf("secrets=%v/%s", secrets, scenario), func(test *testing.T) {
				fixture := newRWA59AuthorityFixture(test)
				expected := domain.ErrForbidden
				switch scenario {
				case "missing":
					expected = domain.ErrNotFound
					fixture.failures["metadata"] = expected
				case "error":
					expected = errors.New("metadata unavailable")
					fixture.failures["metadata"] = expected
				case "unbound":
					fixture.metadata.RepositoryID = ""
				}
				authority := fixture.construct(fixture.config())
				ctx := inbound.WithRepositoryActor(context.Background(), fixture.actor)
				fixture.begin(ctx)
				var err error
				if secrets {
					err = authority.AuthorizeSecrets(ctx, fixture.repo, fixture.actor)
				} else {
					err = authority.AuthorizeWrite(ctx, fixture.repo)
				}
				if err != expected {
					test.Fatalf("metadata decision = %v, want exact %v", err, expected)
				}
				fixture.expectCalls("metadata")
			})
		}
	}
}

func TestRWA59RepositoryAuthorityStrongPortErrorsNeverUseWeakerRole(test *testing.T) {
	stages := []string{"metadata", "lock", "repository", "members", "teams", "organization"}
	for _, secrets := range []bool{false, true} {
		for stageIndex, stage := range stages {
			for _, failure := range []error{errors.New("strong port failure"), domain.ErrNotFound, domain.ErrCatalogUnsupported, context.Canceled} {
				test.Run(fmt.Sprintf("secrets=%v/%s/%s", secrets, stage, failure), func(test *testing.T) {
					fixture := newRWA59AuthorityFixture(test)
					fixture.repository.OwnerID = fixture.actor
					fixture.grants = []domain.TeamRepositoryAccess{fixture.teamGrant(domain.RoleOwner)}
					fixture.organization = fixture.organizationAccess(domain.OrganizationOwner, domain.RoleOwner)
					fixture.failures[stage] = failure
					authority := fixture.construct(fixture.config())
					ctx := inbound.WithRepositoryActor(context.Background(), fixture.actor)
					fixture.begin(ctx)
					var err error
					if secrets {
						err = authority.AuthorizeSecrets(ctx, fixture.repo, fixture.actor)
					} else {
						err = authority.AuthorizeWrite(ctx, fixture.repo)
					}
					if err != failure {
						test.Fatal("strong error was replaced by a weaker role or missing-capability fallback", err, failure)
					}
					fixture.expectCalls(stages[:stageIndex+1]...)
				})
			}
		}
	}
}

func TestRWA59RepositoryAuthorityMissingTransactionalLockKeepsDistinctErrors(test *testing.T) {
	fixture := newRWA59AuthorityFixture(test)
	config := fixture.config()
	config.Profile, config.Locker = RepositoryAuthorityCompatibility, nil
	authority := fixture.construct(config)
	ctx := inbound.WithRepositoryActor(context.Background(), fixture.actor)
	fixture.begin(ctx)
	if err := authority.AuthorizeWrite(ctx, fixture.repo); err != domain.ErrForbidden {
		test.Fatal("normal write lost missing-lock forbidden behavior", err)
	}
	fixture.expectCalls("metadata")
	fixture.begin(ctx)
	if err := authority.AuthorizeSecrets(ctx, fixture.repo, fixture.actor); err != domain.ErrSecretsConsistency {
		test.Fatal("secrets lost missing-lock consistency behavior", err)
	}
	fixture.expectCalls("metadata")
	fixture.begin(ctx)
	config.Profile = RepositoryAuthorityComplete
	if authority, err := NewRepositoryAuthority(config); authority != nil || err != domain.ErrCatalogUnsupported {
		test.Fatal("complete missing-lock authority was not rejected during construction", authority, err)
	}
	fixture.expectCalls()
}

func TestRWA59RepositoryAuthorityNontransactionalDoesNotAcquireOptionalLock(test *testing.T) {
	fixture := newRWA59AuthorityFixture(test)
	config := fixture.config()
	config.Transactional = false
	authority := fixture.construct(config)
	config.Transactional = true
	fixture.failures["lock"] = errors.New("nontransactional authority tried to lock")
	ctx := inbound.WithRepositoryActor(context.Background(), fixture.actor)
	for _, secrets := range []bool{false, true} {
		fixture.begin(ctx)
		var err error
		if secrets {
			err = authority.AuthorizeSecrets(ctx, fixture.repo, fixture.actor)
		} else {
			err = authority.AuthorizeWrite(ctx, fixture.repo)
		}
		if err != nil {
			test.Fatal("nontransactional authority changed its captured mode", err)
		}
		fixture.expectCalls("metadata", "repository", "members", "teams", "organization")
	}
}

func TestRWA59RepositoryAuthorityLocksBeforeReadingCurrentPolicyAndRoles(test *testing.T) {
	for _, scenario := range []string{"removed-member", "archived", "removed-team", "demoted-organization-owner"} {
		for _, secrets := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/secrets=%v", scenario, secrets), func(test *testing.T) {
				fixture := newRWA59AuthorityFixture(test)
				switch scenario {
				case "removed-member":
					fixture.onLock = func() { fixture.members = nil }
				case "archived":
					fixture.onLock = func() { fixture.repository.Archived = true }
				case "removed-team":
					fixture.members = nil
					fixture.grants = []domain.TeamRepositoryAccess{fixture.teamGrant(domain.RoleMaintainer)}
					fixture.onLock = func() { fixture.grants[0].TeamMember = false }
				case "demoted-organization-owner":
					fixture.members = nil
					fixture.organization = fixture.organizationAccess(domain.OrganizationOwner, "")
					fixture.onLock = func() { fixture.organization.Role = domain.OrganizationAdmin }
				}
				authority := fixture.construct(fixture.config())
				ctx := inbound.WithRepositoryActor(context.Background(), fixture.actor)
				fixture.begin(ctx)
				var err error
				if secrets {
					err = authority.AuthorizeSecrets(ctx, fixture.repo, fixture.actor)
				} else {
					err = authority.AuthorizeWrite(ctx, fixture.repo)
				}
				if err != domain.ErrForbidden {
					test.Fatal("authorization used access facts from before the lock", err)
				}
				fixture.expectCalls("metadata", "lock", "repository", "members", "teams", "organization")
			})
		}
	}
}

func TestRWA59RepositoryAuthorityRechecksCurrentBindingMembershipAndPolicy(test *testing.T) {
	fixture := newRWA59AuthorityFixture(test)
	authority := fixture.construct(fixture.config())
	ctx := inbound.WithRepositoryActor(context.Background(), fixture.actor)
	check := func(expected error) {
		test.Helper()
		fixture.begin(ctx)
		if err := authority.AuthorizeWrite(ctx, fixture.repo); err != expected {
			test.Fatalf("current authority = %v, want %v", err, expected)
		}
		fixture.expectCalls("metadata", "lock", "repository", "members", "teams", "organization")
	}
	check(nil)
	fixture.members = nil
	check(domain.ErrForbidden)
	fixture.members = []domain.Membership{{RepositoryID: fixture.repository.ID, UserID: fixture.actor, Role: domain.RoleMaintainer}}
	check(nil)
	fixture.repository.Archived = true
	check(domain.ErrForbidden)
	fixture.repository.Archived = false
	ctx = writeAction(ctx, "settings")
	fixture.repository.SettingsPolicy = "owner"
	check(domain.ErrForbidden)
	fixture.repository.SettingsPolicy = "members"
	check(nil)
	fixture.metadata.RepositoryID = "ws_55555555555555555555555555555555"
	fixture.repository.ID = fixture.metadata.RepositoryID
	check(domain.ErrForbidden)
	fixture.members[0].RepositoryID = fixture.repository.ID
	check(nil)
}

func TestRWA59RepositoryAuthorityCurrentDirectTeamAndOrganizationRoles(test *testing.T) {
	for _, scenario := range []string{
		"direct-member", "direct-maintainer", "creator", "co-owner", "viewer", "puller", "invalid-direct", "foreign-direct", "other-actor-direct",
		"team-maintainer", "team-outranks-direct", "direct-outranks-team", "removed-team", "removed-organization-team", "foreign-team", "foreign-team-namespace", "other-actor-team", "invalid-team",
		"organization-owner", "organization-default-maintainer", "organization-admin-alone", "organization-default-viewer", "demoted-organization-owner", "foreign-organization", "foreign-organization-namespace", "other-actor-organization", "invalid-organization", "public-nonmember",
	} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newRWA59AuthorityFixture(test)
			fixture.members = nil
			expectedWrite, expectedSecrets := domain.ErrForbidden, domain.ErrForbidden
			switch scenario {
			case "direct-member":
				fixture.members = []domain.Membership{{RepositoryID: fixture.repository.ID, UserID: fixture.actor, Role: domain.RoleMember}}
				expectedWrite = nil
			case "direct-maintainer", "co-owner", "viewer", "puller", "invalid-direct", "foreign-direct", "other-actor-direct":
				member := domain.Membership{RepositoryID: fixture.repository.ID, UserID: fixture.actor, Role: domain.RoleMaintainer}
				switch scenario {
				case "direct-maintainer":
					expectedWrite, expectedSecrets = nil, nil
				case "co-owner":
					member.Role = domain.RoleOwner
					fixture.repository.SecretsPolicy = "owner"
					expectedWrite, expectedSecrets = nil, nil
				case "viewer":
					member.Role = domain.RoleViewer
				case "puller":
					member.Role = domain.RolePuller
				case "invalid-direct":
					member.Role = "administrator"
				case "foreign-direct":
					member.RepositoryID = "foreign-repository"
				case "other-actor-direct":
					member.UserID = "other-actor"
				}
				fixture.members = []domain.Membership{member}
			case "creator":
				fixture.repository.OwnerID, fixture.repository.SecretsPolicy = fixture.actor, "owner"
				expectedWrite, expectedSecrets = nil, nil
			case "team-maintainer", "team-outranks-direct", "direct-outranks-team", "removed-team", "removed-organization-team", "foreign-team", "foreign-team-namespace", "other-actor-team", "invalid-team":
				grant := fixture.teamGrant(domain.RoleMaintainer)
				switch scenario {
				case "team-maintainer":
					expectedWrite, expectedSecrets = nil, nil
				case "team-outranks-direct":
					fixture.members = []domain.Membership{{RepositoryID: fixture.repository.ID, UserID: fixture.actor, Role: domain.RoleViewer}}
					expectedWrite, expectedSecrets = nil, nil
				case "direct-outranks-team":
					fixture.members = []domain.Membership{{RepositoryID: fixture.repository.ID, UserID: fixture.actor, Role: domain.RoleMaintainer}}
					grant.Role = domain.RoleViewer
					expectedWrite, expectedSecrets = nil, nil
				case "removed-team":
					grant.TeamMember = false
				case "removed-organization-team":
					grant.OrganizationMember = false
				case "foreign-team":
					grant.RepositoryID = "foreign-repository"
				case "foreign-team-namespace":
					grant.OrganizationNamespaceID = "foreign-namespace"
				case "other-actor-team":
					grant.UserID = "other-actor"
				case "invalid-team":
					grant.Role = "administrator"
				}
				fixture.grants = []domain.TeamRepositoryAccess{grant}
			case "organization-owner", "organization-default-maintainer", "organization-admin-alone", "organization-default-viewer", "demoted-organization-owner", "foreign-organization", "foreign-organization-namespace", "other-actor-organization", "invalid-organization":
				access := fixture.organizationAccess(domain.OrganizationOwner, "")
				switch scenario {
				case "organization-owner":
					fixture.members = []domain.Membership{{RepositoryID: fixture.repository.ID, UserID: fixture.actor, Role: domain.RoleViewer}}
					fixture.repository.SecretsPolicy = "owner"
					expectedWrite, expectedSecrets = nil, nil
				case "organization-default-maintainer":
					access.Role, access.DefaultRepositoryRole = domain.OrganizationMember, domain.RoleMaintainer
					expectedWrite, expectedSecrets = nil, nil
				case "organization-admin-alone", "demoted-organization-owner":
					access.Role = domain.OrganizationAdmin
				case "organization-default-viewer":
					access.Role, access.DefaultRepositoryRole = domain.OrganizationMember, domain.RoleViewer
				case "foreign-organization":
					access.RepositoryID = "foreign-repository"
				case "foreign-organization-namespace":
					access.OrganizationNamespaceID = "foreign-namespace"
				case "other-actor-organization":
					access.UserID = "other-actor"
				case "invalid-organization":
					access.Role = "administrator"
				}
				fixture.organization = access
			case "public-nonmember":
				fixture.repository.Visibility, fixture.repository.PublicRole = domain.VisibilityPublic, "owner"
			}
			authority := fixture.construct(fixture.config())
			ctx := inbound.WithRepositoryActor(context.Background(), fixture.actor)
			fixture.begin(ctx)
			if err := authority.AuthorizeWrite(ctx, fixture.repo); err != expectedWrite {
				test.Fatal("context write disagrees with current effective role", err, expectedWrite)
			}
			fixture.expectCalls("metadata", "lock", "repository", "members", "teams", "organization")
			fixture.begin(ctx)
			if err := authority.AuthorizeSecrets(ctx, fixture.repo, fixture.actor); err != expectedSecrets {
				test.Fatal("secrets edit disagrees with current effective role", err, expectedSecrets)
			}
			fixture.expectCalls("metadata", "lock", "repository", "members", "teams", "organization")
		})
	}
}

func TestRWA59RepositoryAuthorityDoesNotCacheTeamOrOrganizationMembership(test *testing.T) {
	for _, source := range []string{"team", "organization-owner", "organization-default"} {
		test.Run(source, func(test *testing.T) {
			fixture := newRWA59AuthorityFixture(test)
			fixture.members = nil
			switch source {
			case "team":
				fixture.grants = []domain.TeamRepositoryAccess{fixture.teamGrant(domain.RoleMaintainer)}
			case "organization-owner":
				fixture.organization = fixture.organizationAccess(domain.OrganizationOwner, "")
			case "organization-default":
				fixture.organization = fixture.organizationAccess(domain.OrganizationMember, domain.RoleMaintainer)
			}
			authority := fixture.construct(fixture.config())
			ctx := inbound.WithRepositoryActor(context.Background(), fixture.actor)
			check := func(expected error) {
				test.Helper()
				fixture.begin(ctx)
				if err := authority.AuthorizeWrite(ctx, fixture.repo); err != expected {
					test.Fatal("context write reused old team/organization facts", err, expected)
				}
				fixture.expectCalls("metadata", "lock", "repository", "members", "teams", "organization")
				fixture.begin(ctx)
				if err := authority.AuthorizeSecrets(ctx, fixture.repo, fixture.actor); err != expected {
					test.Fatal("secrets edit reused old team/organization facts", err, expected)
				}
				fixture.expectCalls("metadata", "lock", "repository", "members", "teams", "organization")
			}
			check(nil)
			switch source {
			case "team":
				fixture.grants[0].TeamMember = false
			case "organization-owner":
				fixture.organization.Role = domain.OrganizationAdmin
			case "organization-default":
				fixture.organization.DefaultRepositoryRole = domain.RoleViewer
			}
			check(domain.ErrForbidden)
			fixture.members = []domain.Membership{{RepositoryID: fixture.repository.ID, UserID: fixture.actor, Role: domain.RoleMaintainer}}
			check(nil)
			fixture.members = nil
			fixture.grants, fixture.organization = nil, domain.OrganizationRepositoryAccess{}
			check(domain.ErrForbidden)
		})
	}
}

func TestRWA59RepositoryAuthorityCompatibilityOnlyOmitsAbsentOptionalReads(test *testing.T) {
	fixture := newRWA59AuthorityFixture(test)
	config := fixture.config()
	config.Profile, config.Transactional = RepositoryAuthorityCompatibility, false
	config.Teams, config.Organizations, config.Audit, config.Locker = nil, nil, nil, nil
	authority := fixture.construct(config)
	ctx := inbound.WithRepositoryActor(context.Background(), fixture.actor)
	fixture.begin(ctx)
	if err := authority.AuthorizeWrite(ctx, fixture.repo); err != nil {
		test.Fatal("absent optional role sources invalidated direct membership", err)
	}
	fixture.expectCalls("metadata", "repository", "members")
	fixture.begin(ctx)
	if err := authority.AuthorizeSecrets(ctx, fixture.repo, fixture.actor); err != nil {
		test.Fatal("absent optional role sources invalidated direct secrets permission", err)
	}
	fixture.expectCalls("metadata", "repository", "members")
	fixture.begin(ctx)
	if err := authority.AuditWrite(ctx, fixture.repo); err != domain.ErrForbidden {
		test.Fatal("missing relevant organization audit port silently allowed auditing", err)
	}
	fixture.expectCalls("metadata", "repository")
	fixture.repository.OwnerNamespaceID = ""
	fixture.begin(ctx)
	if err := authority.AuditWrite(ctx, fixture.repo); err != nil {
		test.Fatal("legacy unnamespaced repository gained an audit dependency", err)
	}
	fixture.expectCalls("metadata", "repository")
}

func TestRWA59RepositoryAuthoritySecretsUsesExplicitActorWithoutSystemOrRegisterBypass(test *testing.T) {
	fixture := newRWA59AuthorityFixture(test)
	authority := fixture.construct(fixture.config())
	for _, ctx := range []context.Context{
		context.Background(), inbound.WithRepositoryActor(context.Background(), fixture.repository.OwnerID),
		writeAction(inbound.WithSystemActor(context.Background()), "register"),
	} {
		fixture.begin(ctx)
		if err := authority.AuthorizeSecrets(ctx, fixture.repo, fixture.actor); err != nil {
			test.Fatal("secrets ignored its explicit actor", err)
		}
		fixture.expectCalls("metadata", "lock", "repository", "members", "teams", "organization")
	}
	for _, actor := range []string{"", "removed-actor"} {
		fixture.expectedActor = actor
		ctx := writeAction(inbound.WithSystemActor(context.Background()), "register")
		fixture.begin(ctx)
		if err := authority.AuthorizeSecrets(ctx, fixture.repo, actor); err != domain.ErrForbidden {
			test.Fatal("system context or register action bypassed explicit secrets authority", err)
		}
		fixture.expectCalls("metadata", "lock", "repository", "members", "teams", "organization")
	}
}

func TestRWA59RepositoryAuthorityActionArchiveAndAssetPolicies(test *testing.T) {
	scenarios := []struct {
		name      string
		role      domain.MemberRole
		action    string
		policy    string
		archived  bool
		secrets   bool
		wantAllow bool
	}{
		{name: "member-context", role: domain.RoleMember, wantAllow: true},
		{name: "member-manage", role: domain.RoleMember, action: "manage"},
		{name: "maintainer-manage", role: domain.RoleMaintainer, action: "manage", wantAllow: true},
		{name: "owner-manage", role: domain.RoleOwner, action: "manage", wantAllow: true},
		{name: "member-settings", role: domain.RoleMember, action: "settings", policy: "members"},
		{name: "maintainer-default-settings", role: domain.RoleMaintainer, action: "settings", wantAllow: true},
		{name: "maintainer-member-settings", role: domain.RoleMaintainer, action: "settings", policy: "members", wantAllow: true},
		{name: "maintainer-owner-settings", role: domain.RoleMaintainer, action: "settings", policy: "owner"},
		{name: "maintainer-unknown-settings", role: domain.RoleMaintainer, action: "settings", policy: "unknown"},
		{name: "owner-owner-settings", role: domain.RoleOwner, action: "settings", policy: "owner", wantAllow: true},
		{name: "owner-unknown-settings", role: domain.RoleOwner, action: "settings", policy: "unknown", wantAllow: true},
		{name: "owner-invalid-action", role: domain.RoleOwner, action: "invalid"},
		{name: "owner-archived-context", role: domain.RoleOwner, archived: true},
		{name: "owner-archived-manage", role: domain.RoleOwner, action: "manage", archived: true},
		{name: "owner-archived-settings", role: domain.RoleOwner, action: "settings", archived: true},
		{name: "member-secrets", role: domain.RoleMember, secrets: true},
		{name: "maintainer-default-secrets", role: domain.RoleMaintainer, secrets: true, wantAllow: true},
		{name: "maintainer-member-secrets", role: domain.RoleMaintainer, policy: "members", secrets: true, wantAllow: true},
		{name: "maintainer-owner-secrets", role: domain.RoleMaintainer, policy: "owner", secrets: true},
		{name: "maintainer-unknown-secrets", role: domain.RoleMaintainer, policy: "unknown", secrets: true},
		{name: "owner-owner-secrets", role: domain.RoleOwner, policy: "owner", secrets: true, wantAllow: true},
		{name: "owner-unknown-secrets", role: domain.RoleOwner, policy: "unknown", secrets: true, wantAllow: true},
		{name: "owner-archived-secrets", role: domain.RoleOwner, archived: true, secrets: true},
	}
	for _, scenario := range scenarios {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newRWA59AuthorityFixture(test)
			fixture.members[0].Role = scenario.role
			fixture.repository.Archived = scenario.archived
			fixture.repository.SettingsPolicy, fixture.repository.SecretsPolicy = scenario.policy, scenario.policy
			authority := fixture.construct(fixture.config())
			ctx := writeAction(inbound.WithRepositoryActor(context.Background(), fixture.actor), scenario.action)
			fixture.begin(ctx)
			var err error
			if scenario.secrets {
				err = authority.AuthorizeSecrets(ctx, fixture.repo, fixture.actor)
			} else {
				err = authority.AuthorizeWrite(ctx, fixture.repo)
			}
			if scenario.wantAllow && err != nil || !scenario.wantAllow && err != domain.ErrForbidden {
				test.Fatal("repository action/archive/policy decision changed", err, scenario.wantAllow)
			}
			fixture.expectCalls("metadata", "lock", "repository", "members", "teams", "organization")
		})
	}
}

func TestRWA59CanWriteRepositoryContextPreservesIndependentDecisionMatrix(test *testing.T) {
	allowedRoles := map[string]map[domain.MemberRole]bool{
		"":         {domain.RoleMember: true, domain.RoleMaintainer: true, domain.RoleOwner: true},
		"manage":   {domain.RoleMaintainer: true, domain.RoleOwner: true},
		"settings": {domain.RoleMaintainer: true, domain.RoleOwner: true},
	}
	for _, role := range []domain.MemberRole{"", "administrator", domain.RoleViewer, domain.RolePuller, domain.RoleMember, domain.RoleMaintainer, domain.RoleOwner} {
		for _, action := range []string{"", "manage", "settings", "register", "invalid", "Settings"} {
			for _, policy := range []string{"", "members", "owner", "unknown"} {
				for _, exists := range []bool{false, true} {
					for _, archived := range []bool{false, true} {
						expected := exists && !archived && allowedRoles[action][role]
						if action == "settings" && role != domain.RoleOwner && policy != "" && policy != "members" {
							expected = false
						}
						repository := domain.Repository{Archived: archived, SettingsPolicy: policy}
						if actual := domain.CanWriteRepositoryContext(repository, role, exists, action); actual != expected {
							test.Fatalf("role=%q action=%q policy=%q exists=%v archived=%v: allow=%v want=%v", role, action, policy, exists, archived, actual, expected)
						}
					}
				}
			}
		}
	}
}

type rwa59AuthorityPayloadKey struct{}

func TestRWA59RepositoryAuthorityAuditOnlyCopiesExactMetadata(test *testing.T) {
	for _, operation := range []string{"", "repository.secrets.updated", "repository.settings.updated"} {
		test.Run(operation, func(test *testing.T) {
			fixture := newRWA59AuthorityFixture(test)
			fixture.metadata.RemoteURL = "https://credential-token@example.invalid/private.git"
			fixture.metadata.Description = "private document payload"
			fixture.metadata.LocalPath = "/private/local-payload"
			fixture.repository.WebhookURL = "https://example.invalid/secret-webhook-token"
			authority := fixture.construct(fixture.config())
			ctx := inbound.WithCorrelation(inbound.WithRepositoryActor(context.Background(), fixture.actor), "request-authority-audit")
			ctx = auditOperation(writeAction(ctx, "settings"), operation)
			ctx = context.WithValue(ctx, rwa59AuthorityPayloadKey{}, "secret-envelope-payload")
			fixture.begin(ctx)
			before := time.Now().UTC()
			if err := authority.AuditWrite(ctx, fixture.repo); err != nil {
				test.Fatal(err)
			}
			after := time.Now().UTC()
			fixture.expectCalls("metadata", "repository", "namespace", "audit")
			if len(fixture.audits) != 1 {
				test.Fatal("write did not append exactly one metadata audit", fixture.audits)
			}
			event := fixture.audits[0]
			if err := domain.ValidateAuditID(event.ID); err != nil {
				test.Fatal("invalid audit identity", err)
			}
			if event.CreatedAt.Before(before) || event.CreatedAt.After(after) || event.CreatedAt.Location() != time.UTC {
				test.Fatal("audit timestamp is not a current UTC instant", event.CreatedAt)
			}
			action := operation
			if action == "" {
				action = "context.updated"
			}
			expected := domain.OrganizationAuditEvent{
				ID: event.ID, CreatedAt: event.CreatedAt, OrganizationID: fixture.namespace.OrganizationID,
				ActorID: fixture.actor, CorrelationID: "request-authority-audit", Action: action,
				TargetType: "repository", TargetID: fixture.metadata.RepositoryID,
			}
			if !reflect.DeepEqual(event, expected) {
				test.Fatalf("audit metadata = %+v, want %+v", event, expected)
			}
			raw, err := json.Marshal(event)
			if err != nil {
				test.Fatal(err)
			}
			for _, sensitive := range []string{"credential-token", "private document payload", "local-payload", "secret-webhook-token", "secret-envelope-payload"} {
				if strings.Contains(string(raw), sensitive) {
					test.Fatal("repository audit copied payload or credential material", sensitive)
				}
			}
		})
	}
}

func TestRWA59RepositoryAuthorityAuditBypassesAndNamespaceKinds(test *testing.T) {
	for _, scenario := range []string{"system", "unbound", "no-namespace", "personal", "unknown-namespace"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newRWA59AuthorityFixture(test)
			config := fixture.config()
			ctx := inbound.WithRepositoryActor(context.Background(), fixture.actor)
			expected := []string{"metadata", "repository", "namespace"}
			switch scenario {
			case "system":
				config = RepositoryAuthorityConfig{Profile: RepositoryAuthorityCompatibility}
				ctx = inbound.WithSystemActor(context.Background())
				expected = nil
			case "unbound":
				fixture.metadata.RepositoryID = ""
				expected = []string{"metadata"}
			case "no-namespace":
				fixture.repository.OwnerNamespaceID = ""
				expected = []string{"metadata", "repository"}
			case "personal":
				fixture.namespace.Kind = domain.NamespaceUser
			case "unknown-namespace":
				fixture.namespace.Kind = "unknown"
			}
			authority := fixture.construct(config)
			fixture.begin(ctx)
			if err := authority.AuditWrite(ctx, fixture.repo); err != nil {
				test.Fatal("non-organization/system audit behavior changed", err)
			}
			fixture.expectCalls(expected...)
			if len(fixture.audits) != 0 {
				test.Fatal("system or non-organization write appended an organization audit", fixture.audits)
			}
		})
	}
}

func TestRWA59RepositoryAuthorityAuditPreservesExactErrorsAndStops(test *testing.T) {
	stages := []string{"metadata", "repository", "namespace", "audit"}
	for stageIndex, stage := range stages {
		for _, failure := range []error{errors.New("audit source failure"), domain.ErrNotFound, domain.ErrCatalogUnsupported, context.DeadlineExceeded} {
			test.Run(stage+"/"+failure.Error(), func(test *testing.T) {
				fixture := newRWA59AuthorityFixture(test)
				fixture.failures[stage] = failure
				authority := fixture.construct(fixture.config())
				ctx := inbound.WithRepositoryActor(context.Background(), fixture.actor)
				fixture.begin(ctx)
				if err := authority.AuditWrite(ctx, fixture.repo); err != failure {
					test.Fatal("audit error was swallowed, wrapped, or replaced", err, failure)
				}
				fixture.expectCalls(stages[:stageIndex+1]...)
				if stage != "audit" && len(fixture.audits) != 0 {
					test.Fatal("audit append followed an earlier source error", fixture.audits)
				}
			})
		}
	}
}

func TestRWA59RepositoryAuthorityNilReceiverFailsWithoutPanic(test *testing.T) {
	var authority *RepositoryAuthority
	ctx := inbound.WithRepositoryActor(context.Background(), "authority-actor")
	repo := domain.HashContent([]byte(test.Name()))
	if err := authority.AuthorizeWrite(ctx, repo); err != domain.ErrValidation {
		test.Fatal("nil write authority did not reject use", err)
	}
	if err := authority.AuthorizeSecrets(ctx, repo, "authority-actor"); err != domain.ErrValidation {
		test.Fatal("nil secrets authority did not reject use", err)
	}
	if err := authority.AuditWrite(ctx, repo); err != domain.ErrValidation {
		test.Fatal("nil audit authority did not reject use", err)
	}
}
