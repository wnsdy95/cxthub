//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGSmoke(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset — PG smoke test skipped")
	}
	ctx := context.Background()
	st, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	defer t.Run("document jobs", func(t *testing.T) { checkDocJobs(t, st); checkDocJobRollback(t, st) })
	defer t.Run("pairing rollback", func(t *testing.T) { checkPairingRollbackPG(t, st) })
	defer t.Run("storage accounting", func(t *testing.T) { checkStorageAccountingPG(t, st) })
	defer t.Run("shared runtime", func(t *testing.T) { checkRuntime(t, st) })

	// Other integration tests may have initialized this shared test database.
	// Applying migrations must always be idempotent.
	n1, err := st.ApplyMigrations(ctx, "../../../../schemas/db/migrations")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n1 == 0 {
		t.Log("migrations already applied by an earlier integration test")
	}
	n2, err := st.ApplyMigrations(ctx, "../../../../schemas/db/migrations")
	if err != nil || n2 != 0 {
		t.Fatalf("idempotency violation: 2nd application n=%d err=%v", n2, err)
	}

	// User, repository, member(5 role) CHECK passed — 0016.
	uid := "dev:pgsmoke@t.io"
	if err := st.UpsertUser(ctx, domain.User{ID: uid, Email: "pgsmoke@t.io", Name: "PG", Username: "pgsmoke"}); err != nil {
		t.Fatalf("user: %v", err)
	}
	// Namespace ownership is globally serialized across the users, namespaces,
	// and aliases tables. An Organization slug must not be claimable as a later
	// user handle even though ordinary per-table UNIQUE constraints would allow
	// that cross-table collision.
	now := time.Now().UTC()
	organization := domain.Organization{
		ID: domain.NewID("ent_"), NamespaceID: domain.NewID("ns_"), Name: "PG Organization",
		Slug: "pg-organization", CreatedBy: uid, CreatedAt: now,
	}
	organizationNamespace := domain.Namespace{
		ID: organization.NamespaceID, Slug: organization.Slug, Kind: domain.NamespaceOrganization,
		OrganizationID: organization.ID, CreatedAt: now,
	}
	organizationOwner := domain.OrganizationMembership{
		OrganizationID: organization.ID, UserID: uid, Role: domain.OrganizationOwner, CreatedAt: now,
	}
	organizationPolicy := domain.DefaultOrganizationPolicy(organization.ID)
	organizationPolicy.UpdatedBy, organizationPolicy.UpdatedAt = uid, now
	organizationAudit := domain.OrganizationAuditEvent{
		ID: domain.NewID("aud_"), OrganizationID: organization.ID, ActorID: uid,
		Action: "organization.created", TargetType: "organization", TargetID: organization.ID, CreatedAt: now,
	}
	if err := st.CreateOrganization(ctx, organization, organizationNamespace, organizationOwner, organizationPolicy, organizationAudit); err != nil {
		t.Fatalf("organization bootstrap: %v", err)
	}
	demotedOnlyOwner := organizationOwner
	demotedOnlyOwner.Role = domain.OrganizationAdmin
	if err := st.AddOrganizationMember(ctx, demotedOnlyOwner); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("PostgreSQL allowed the last Organization owner to be demoted: %v", err)
	}
	if err := st.RemoveOrganizationMember(ctx, organization.ID, uid); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("PostgreSQL allowed the last Organization owner to be removed: %v", err)
	}
	if err := st.UpsertUser(ctx, domain.User{ID: "namespace-collision-user", Email: "collision@t.io", Name: "Collision", Username: organization.Slug}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("cross-table namespace collision = %v, want ErrConflict", err)
	}
	blockedOrganization := domain.Organization{
		ID: domain.NewID("ent_"), NamespaceID: domain.NewID("ns_"), Name: "Blocked Organization",
		Slug: "pgsmoke", CreatedBy: uid, CreatedAt: now,
	}
	blockedNamespace := domain.Namespace{
		ID: blockedOrganization.NamespaceID, Slug: blockedOrganization.Slug, Kind: domain.NamespaceOrganization,
		OrganizationID: blockedOrganization.ID, CreatedAt: now,
	}
	blockedOwner := organizationOwner
	blockedOwner.OrganizationID = blockedOrganization.ID
	blockedPolicy := domain.DefaultOrganizationPolicy(blockedOrganization.ID)
	blockedPolicy.UpdatedBy, blockedPolicy.UpdatedAt = uid, now
	blockedAudit := organizationAudit
	blockedAudit.ID, blockedAudit.OrganizationID, blockedAudit.TargetID = domain.NewID("aud_"), blockedOrganization.ID, blockedOrganization.ID
	if err := st.CreateOrganization(ctx, blockedOrganization, blockedNamespace, blockedOwner, blockedPolicy, blockedAudit); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("user-handle Organization collision = %v, want ErrConflict", err)
	}

	// Every Organization management mutation and its audit row share one
	// transaction. Reuse the existing audit primary key to force the audit
	// insert to fail after the mutation statement, then prove no mutation leaks.
	profileUpdate := organization
	profileUpdate.Name = "Must Roll Back"
	profileAudit := organizationAudit
	profileAudit.Action = "organization.profile.updated"
	if err := st.UpdateOrganizationWithAudit(ctx, profileUpdate, profileAudit); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("profile duplicate-audit failure = %v, want ErrConflict", err)
	}
	if got, err := st.GetOrganization(ctx, organization.ID); err != nil || got.Name != organization.Name {
		t.Fatalf("profile mutation survived audit rollback: organization=%+v err=%v", got, err)
	}

	atomicMemberID := "organization-atomic-member"
	if err := st.UpsertUser(ctx, domain.User{ID: atomicMemberID, Email: "organization-atomic@t.io", Name: "Atomic Member", Username: atomicMemberID}); err != nil {
		t.Fatalf("organization atomic member fixture: %v", err)
	}
	atomicMember := domain.OrganizationMembership{OrganizationID: organization.ID, UserID: atomicMemberID, Role: domain.OrganizationMember, CreatedAt: now}
	memberAudit := organizationAudit
	memberAudit.Action, memberAudit.TargetType, memberAudit.TargetID = "organization.member.updated", "user", atomicMemberID
	if err := st.AddOrganizationMemberWithAudit(ctx, atomicMember, memberAudit); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("member duplicate-audit failure = %v, want ErrConflict", err)
	}
	if _, err := st.GetOrganizationMembership(ctx, organization.ID, atomicMemberID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("member mutation survived audit rollback: %v", err)
	}

	policyUpdate := organizationPolicy
	policyUpdate.RepositoryCreation = domain.OrganizationRepositoryMembers
	policyUpdate.UpdatedAt = now.Add(time.Second)
	policyAudit := organizationAudit
	policyAudit.Action, policyAudit.TargetType, policyAudit.TargetID = "organization.policy.updated", "organization", organization.ID
	if err := st.PutOrganizationPolicyWithAudit(ctx, policyUpdate, policyAudit); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("policy duplicate-audit failure = %v, want ErrConflict", err)
	}
	if got, err := st.GetOrganizationPolicy(ctx, organization.ID); err != nil || got.RepositoryCreation != organizationPolicy.RepositoryCreation {
		t.Fatalf("policy mutation survived audit rollback: policy=%+v err=%v", got, err)
	}

	atomicRepository := domain.Repository{
		ID: domain.NewID("ws_"), Name: "Atomic rollback", OwnerID: uid,
		OwnerUsername: organization.Slug, OwnerNamespaceID: organization.NamespaceID,
		Slug: "atomic-rollback", Visibility: domain.VisibilityPrivate, CreatedAt: now,
	}
	atomicRepositoryOwner := domain.Membership{RepositoryID: atomicRepository.ID, UserID: uid, Role: domain.RoleOwner, CreatedAt: now}
	repositoryAudit := organizationAudit
	repositoryAudit.Action, repositoryAudit.TargetType, repositoryAudit.TargetID = "organization.repository.created", "repository", atomicRepository.ID
	if err := st.CreateOrganizationRepositoryWithAudit(ctx, atomicRepository, atomicRepositoryOwner, repositoryAudit); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("Repository duplicate-audit failure = %v, want ErrConflict", err)
	}
	if _, err := st.GetRepository(ctx, atomicRepository.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Repository mutation survived audit rollback: %v", err)
	}
	if member, err := st.IsMember(ctx, atomicRepository.ID, uid); err != nil || member {
		t.Fatalf("Repository owner membership survived audit rollback: member=%v err=%v", member, err)
	}

	repositoryRecord := domain.Repository{ID: domain.NewID("ws_"), Name: "PGSmoke", OwnerID: uid, Slug: "pgsmoke", OwnerUsername: "pgsmoke", CreatedAt: time.Now().UTC()}
	if err := st.CreateRepository(ctx, repositoryRecord); err != nil {
		t.Fatalf("repository: %v", err)
	}
	for _, role := range []domain.MemberRole{domain.RoleViewer, domain.RolePuller, domain.RoleMaintainer} {
		muid := uid + ":" + string(role)
		// Create real users first to satisfy membership FK (user_id → users.id).
		if err := st.UpsertUser(ctx, domain.User{ID: muid, Email: string(role) + "@t.io", Name: string(role), Username: "pgsmoke-" + string(role)}); err != nil {
			t.Fatalf("member user(%s): %v", role, err)
		}
		if err := st.AddMember(ctx, domain.Membership{RepositoryID: repositoryRecord.ID, UserID: muid, Role: role, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("member(%s): %v", role, err)
		}
	}

	// Session (hash storage — hint/kind columns, 0015).
	sess := domain.Session{Token: domain.HashToken("sess_x"), UserID: uid, Hint: "xxxxxxxx", Kind: "web", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateSession(ctx, sess); err != nil {
		t.Fatalf("session: %v", err)
	}
	if got, err := st.GetSession(ctx, sess.Token); err != nil || got.Kind != "web" {
		t.Fatalf("get session: kind=%q err=%v", got.Kind, err)
	}

	// Remote MCP OAuth state uses PostgreSQL transactions in production: a
	// consent request becomes one client/PKCE-bound authorization code and that
	// code can be consumed exactly once.
	oauthClient := domain.OAuthClient{
		ID: domain.NewID("mcp_client_"), Name: "PG MCP smoke",
		RedirectURIs: []string{"http://127.0.0.1/callback"}, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateOAuthClient(ctx, oauthClient); err != nil {
		t.Fatalf("oauth client: %v", err)
	}
	oauthRequest := domain.OAuthAuthorizationRequest{
		ID: domain.NewID("oauth_req_"), ClientID: oauthClient.ID, RedirectURI: oauthClient.RedirectURIs[0],
		CodeChallenge: "pg-smoke-challenge", Resource: "https://cxthub.test/mcp", Scope: "mcp:read",
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := st.CreateOAuthAuthorizationRequest(ctx, oauthRequest); err != nil {
		t.Fatalf("oauth request: %v", err)
	}
	codeHash := domain.HashToken("oauth_code_pg_smoke")
	code, err := st.ApproveOAuthAuthorizationRequest(ctx, oauthRequest.ID, uid, codeHash, now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("oauth approve: %v", err)
	}
	if _, err := st.ConsumeOAuthAuthorizationCode(ctx, code.CodeHash, code.ClientID, code.RedirectURI, "wrong-challenge"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("oauth wrong PKCE = %v", err)
	}
	if _, err := st.ConsumeOAuthAuthorizationCode(ctx, code.CodeHash, code.ClientID, code.RedirectURI, code.CodeChallenge); err != nil {
		t.Fatalf("oauth consume: %v", err)
	}
	if _, err := st.ConsumeOAuthAuthorizationCode(ctx, code.CodeHash, code.ClientID, code.RedirectURI, code.CodeChallenge); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("oauth replay = %v", err)
	}

	repoID := domain.HashContent([]byte("pgsmoke-repo"))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repoID, RepositoryID: repositoryRecord.ID, GitRemoteURL: "https://github.com/acme/shared.git", DefaultBranch: "main"}); err != nil {
		t.Fatalf("repo: %v", err)
	}
	// Bounded chunk staging: store/mutual exclusion dedup/repo ownership isolation verified by actual PG transaction.
	chunkBody := []byte("pg smoke bounded chunk")
	chunkHash := domain.HashContent(chunkBody)
	stored, deduped, err := st.PutChunks(ctx, repoID, map[domain.ContentHash][]byte{chunkHash: chunkBody})
	if err != nil || stored != 1 || deduped != 0 {
		t.Fatalf("stage chunk: stored=%d deduped=%d err=%v", stored, deduped, err)
	}
	stored, deduped, err = st.PutChunks(ctx, repoID, map[domain.ContentHash][]byte{chunkHash: chunkBody})
	if err != nil || stored != 0 || deduped != 1 {
		t.Fatalf("dedup chunk: stored=%d deduped=%d err=%v", stored, deduped, err)
	}
	if got, err := st.GetChunk(ctx, repoID, chunkHash); err != nil || string(got) != string(chunkBody) {
		t.Fatalf("get chunk: body=%q err=%v", got, err)
	}
}
