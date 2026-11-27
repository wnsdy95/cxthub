// Package outbound defines the driven(outbound) ports that the server depends on.
//
// In the hexagonal architecture, this is the interface that the app(use-case) calls to the infrastructure.
//
//   - MetadataStore: repo/branch/ref/snapshot meta/manifest metadata (PostgreSQL in production; filesystem in development).
//   - BlobStore:     content-addressed body (CIR doc / memory; v1 Postgres BYTEA, later S3).
//   - GitEngine:     git semantic engine (commit/branch/fork/diff/ref; CIR/hash-based, format-agnostic).
package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// MetadataStore persists metadata (immutable snapshot metadata + mutable ref/manifest).
//
// PostgreSQL adapters implement production persistence and transactions; the
// filesystem adapter supports development. DDL lives in schemas/db/migrations.
//
// The body (CIR doc / memory) is not stored here but in BlobStore (meta/body separation, data model).
// Mutable state (ref/manifest) is protected by manifest version-CAS boundaries (immutable invariant C1/M2).
type MetadataStore interface {
	// Repo.
	RepositoryReader
	PutRepo(ctx context.Context, repo domain.Repo) (domain.Repo, error) // idempotent (same id returns existing)
	ListRepos(ctx context.Context, team string) ([]domain.Repo, error)

	// Ref / manifest (mutable). Progress is CAS (immutable REF4/C1). UpdateRepoAbout updates About(description/website/topics).
	UpdateRepoAbout(ctx context.Context, id domain.ContentHash, description, website string, topics []string) error
	// UpdateRepoConfig updates repo structure settings (default branch, protected branch). nil = no change.
	UpdateRepoConfig(ctx context.Context, id domain.ContentHash, defaultBranch *string, protectDefault *bool) error
	// Team default settings bundle (.claude/.agents/.codex) — kind ∈ {claude, agents, codex}.
	PutSettingsBundle(ctx context.Context, repoID domain.ContentHash, bundle domain.SettingsBundle) error
	GetSettingsBundle(ctx context.Context, repoID domain.ContentHash, kind string) (domain.SettingsBundle, error)
	// Secret ciphertext envelope (E2E — server cannot decrypt, stored as opaque bytes only).
	GetSecretsEnvelope(ctx context.Context, repoID domain.ContentHash) ([]byte, error)
}

// BlobStore stores content-addressed immutable bodies (data model).
//
// Implementation (impl step): v1 = Postgres BYTEA — blobs(hash PK, bytes BYTEA). hash deduplication (if same hash, existing, skip re-recording = idempotent put). Later replace with S3/MinIO adapter without downtime. Current is stdlib only stub.
//
// Storage units: SessionDoc (CIR body) and MemoryDigest body. Both have sha256:<hex> keys. Their at-rest bodies may be storage-only chunk manifests, while Get always reconstructs and verifies the complete identity.
type BlobStore interface {
	// PutDoc: Store SessionDoc body. content-hash deduplication (Stored=false means already exists).
	PutDoc(ctx context.Context, repoID domain.ContentHash, doc domain.SessionDoc) (stored bool, err error)
	GetDoc(ctx context.Context, repoID, hash domain.ContentHash) (domain.SessionDoc, error)
	HasDocs(ctx context.Context, repoID domain.ContentHash, hashes []domain.ContentHash) (have []domain.ContentHash, err error)
	// DeleteDoc: Remove doc body (idempotent). Hook capture leaf GC exclusive — commit doc is not target (service layer guard).
	DeleteDoc(ctx context.Context, repoID, hash domain.ContentHash) error

	// Chunk wire (push/pull delta transmission) — chunks are isolated by repo ownership.
	// PutChunks: Store validated uncompressed chunks with repo ownership and idempotently. Chunks arriving before complete doc are content-addressed staging objects and safe to remain on failure/retry.
	// HasChunks: List of repo owned chunks (negotiate). GetChunk: Uncompressed chunk body.
	// GetDocManifest: Chunk manifest of doc (even if stored in chunks, canonical produces same plan; plan impossible forms ErrNotFound — caller fallback on complete reply).
	PutChunks(ctx context.Context, repoID domain.ContentHash, chunks map[domain.ContentHash][]byte) (stored, deduped int, err error)
	HasChunks(ctx context.Context, repoID domain.ContentHash, hashes []domain.ContentHash) (have []domain.ContentHash, err error)
	GetChunk(ctx context.Context, repoID, hash domain.ContentHash) ([]byte, error)
	GetDocManifest(ctx context.Context, repoID, hash domain.ContentHash) (domain.DocChunkManifest, error)
}

// IdentityVerifier verifies authentication token (Firebase ID token or dev token) and returns User.
//
// Implementation: FirebaseVerifier (RS256 JWT verification with Google public key, stdlib) / DevVerifier (local demo).
type IdentityVerifier interface {
	// Verify token and return user identifier (uid/email/name). Invalid → domain.ErrUnauthorized.
	Verify(ctx context.Context, idToken string) (domain.User, error)
}

// RepositoryStore persists users, repositories, memberships, and invitations (0002 schema).
//
// Visibility boundary = repository.owner creates invite, invited user joins with token.
type RepositoryStore interface {
	// User
	UpsertUser(ctx context.Context, user domain.User) error
	GetUser(ctx context.Context, id string) (domain.User, error)
	// GetUserByUsername finds user by handle (username). Returns ErrNotFound if not found.
	// Used for global uniqueness guarantee (auto-create on first login + collision avoidance).
	GetUserByUsername(ctx context.Context, username string) (domain.User, error)

	// Repository
	CreateRepository(ctx context.Context, repositoryRecord domain.Repository) error
	GetRepository(ctx context.Context, id string) (domain.Repository, error)
	// GetRepositoryByPath finds repository by URL segments (owner_username, slug) — for repo binding.
	GetRepositoryByPath(ctx context.Context, ownerUsername, slug string) (domain.Repository, error)
	// GetRepositoryByNamespacePath resolves current and aliased Namespace slugs
	// without rewriting URL-derived RepoIDs.
	GetRepositoryByNamespacePath(ctx context.Context, namespaceID, slug string) (domain.Repository, error)
	ListRepositoriesForUser(ctx context.Context, userID string) ([]domain.Repository, error)

	// Membership. AddMember upserts member (re-adds member to update role).
	AddMember(ctx context.Context, m domain.Membership) error
	RemoveMember(ctx context.Context, repositoryID, userID string) error
	IsMember(ctx context.Context, repositoryID, userID string) (bool, error)
	ListMembers(ctx context.Context, repositoryID string) ([]domain.Membership, error)

	// Invite
	CreateInvite(ctx context.Context, inv domain.Invite) error
	GetInvite(ctx context.Context, token string) (domain.Invite, error)
	UpdateInviteStatus(ctx context.Context, token string, status domain.InviteStatus) error
	ListInvites(ctx context.Context, repositoryID string) ([]domain.Invite, error)

	// Session (server login session — CLI token also represented as long-lived session)
	CreateSession(ctx context.Context, s domain.Session) error
	GetSession(ctx context.Context, token string) (domain.Session, error)
	MigrateSession(ctx context.Context, token string) (domain.Session, error)
	// ConsumeSession atomically deletes and returns a session only when kind and
	// label match. It is used for one-time OAuth refresh-token rotation.
	ConsumeSession(ctx context.Context, token, kind, label string) (domain.Session, error)
	DeleteSession(ctx context.Context, token string) error
	ListSessionsForUser(ctx context.Context, userID string) ([]domain.Session, error)
}

// OAuthStore persists the public OAuth client registry and short-lived PKCE
// authorization state used by the remote MCP endpoint. Authorization codes
// are stored only by hash and consumed atomically exactly once.
type OAuthStore interface {
	CreateOAuthClient(ctx context.Context, client domain.OAuthClient) error
	GetOAuthClient(ctx context.Context, clientID string) (domain.OAuthClient, error)
	CreateOAuthAuthorizationRequest(ctx context.Context, req domain.OAuthAuthorizationRequest) error
	GetOAuthAuthorizationRequest(ctx context.Context, requestID string) (domain.OAuthAuthorizationRequest, error)
	ApproveOAuthAuthorizationRequest(ctx context.Context, requestID, userID, codeHash string, codeExpiresAt time.Time) (domain.OAuthAuthorizationCode, error)
	DenyOAuthAuthorizationRequest(ctx context.Context, requestID string) (domain.OAuthAuthorizationRequest, error)
	ConsumeOAuthAuthorizationCode(ctx context.Context, codeHash, clientID, redirectURI, codeChallenge string, resources ...string) (domain.OAuthAuthorizationCode, error)
}

// OrganizationStore persists global namespaces, organization organization state,
// policies, audit events, and short-lived break-glass grants. Organization roles
// are intentionally separate from Repository roles and never imply repository
// context access.
type OrganizationStore interface {
	CreateNamespace(ctx context.Context, ns domain.Namespace) error
	GetNamespace(ctx context.Context, id string) (domain.Namespace, error)
	GetNamespaceBySlug(ctx context.Context, slug string) (domain.Namespace, error)
	RenameNamespace(ctx context.Context, id, nextSlug string) error

	CreateOrganization(
		ctx context.Context,
		organization domain.Organization,
		namespace domain.Namespace,
		owner domain.OrganizationMembership,
		policy domain.OrganizationPolicy,
		audit domain.OrganizationAuditEvent,
	) error
	GetOrganization(ctx context.Context, id string) (domain.Organization, error)
	GetOrganizationBySlug(ctx context.Context, slug string) (domain.Organization, error)
	ListOrganizationsForUser(ctx context.Context, userID string) ([]domain.Organization, error)
	UpdateOrganization(ctx context.Context, organization domain.Organization) error
	UpdateOrganizationWithAudit(ctx context.Context, organization domain.Organization, audit domain.OrganizationAuditEvent) error

	AddOrganizationMember(ctx context.Context, membership domain.OrganizationMembership) error
	AddOrganizationMemberWithAudit(ctx context.Context, membership domain.OrganizationMembership, audit domain.OrganizationAuditEvent) error
	RemoveOrganizationMemberWithAudit(ctx context.Context, organizationID, userID string, audit domain.OrganizationAuditEvent) error
	GetOrganizationMembership(ctx context.Context, organizationID, userID string) (domain.OrganizationMembership, error)
	ListOrganizationMembers(ctx context.Context, organizationID string) ([]domain.OrganizationMembership, error)

	PutOrganizationPolicy(ctx context.Context, policy domain.OrganizationPolicy) error
	PutOrganizationPolicyWithAudit(ctx context.Context, policy domain.OrganizationPolicy, audit domain.OrganizationAuditEvent) error
	GetOrganizationPolicy(ctx context.Context, organizationID string) (domain.OrganizationPolicy, error)

	ListRepositoriesForNamespace(ctx context.Context, namespaceID string) ([]domain.Repository, error)
	CreateOrganizationRepositoryWithAudit(ctx context.Context, repository domain.Repository, owner domain.Membership, audit domain.OrganizationAuditEvent) error

	AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error
	ListOrganizationAudit(ctx context.Context, organizationID string, limit int) ([]domain.OrganizationAuditEvent, error)

	CreateBreakGlassGrant(ctx context.Context, grant domain.BreakGlassGrant) error
	GetActiveBreakGlassGrant(ctx context.Context, organizationID, repositoryID, userID string, now time.Time) (domain.BreakGlassGrant, error)
	// CreateBreakGlassGrantWithAudit makes exceptional access and its creation
	// audit indivisible. UseActiveBreakGlassGrant appends the use audit before
	// returning authorization; audit failure must therefore fail closed.
	CreateBreakGlassGrantWithAudit(ctx context.Context, grant domain.BreakGlassGrant, event domain.OrganizationAuditEvent) error
	UseActiveBreakGlassGrant(ctx context.Context, organizationID, repositoryID, userID string, now time.Time, event domain.OrganizationAuditEvent) (domain.BreakGlassGrant, error)
}
