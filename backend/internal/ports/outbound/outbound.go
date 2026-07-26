// Package outbound defines the driven(outbound) ports that the server depends on.
//
// In the hexagonal architecture, this is the interface that the app(use-case) calls to the infrastructure.
//
//   - MetadataStore: repo/branch/ref/snapshot meta/manifest metadata (Postgres; currently stub).
//   - BlobStore:     content-addressed body (CIR doc / memory; v1 Postgres BYTEA, later S3).
//   - AuthProvider:  team token ↔ team mapping, repo visibility (sync protocol).
//   - GitEngine:     git semantic engine (commit/branch/fork/diff/ref; CIR/hash-based, format-agnostic).
package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// MetadataStore persists metadata (immutable snapshot metadata + mutable ref/manifest).
//
// Implementation (impl step): PostgreSQL. Tables (repos, branches, refs, snapshots(meta),
// memories(meta), team_identities) source DDL = schemas/db/migrations/*.sql.
// Currently, it is a stdlib-only stub (pgx not imported).
//
// The body (CIR doc / memory) is not stored here but in BlobStore (meta/body separation, data model).
// Mutable state (ref/manifest) is protected by manifest version-CAS boundaries (immutable invariant C1/M2).
type MetadataStore interface {
	// Repo.
	GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error)
	PutRepo(ctx context.Context, repo domain.Repo) (domain.Repo, error) // idempotent (same id returns existing)
	ListRepos(ctx context.Context, team string) ([]domain.Repo, error)

	// Snapshot metadata (immutable). ID is CIR body hash (S-ID). Duplicate ID re-save is dedup.
	GetSnapshot(ctx context.Context, repoID, id domain.ContentHash) (domain.Snapshot, error)
	PutSnapshot(ctx context.Context, snap domain.Snapshot) error
	// UpdateSnapshotMessage is a metadata update for hook labels → commit message promotion (unique message exception for immutable objects). The implementation enforces the one-way rule in the final CAS operation:
	// Stored message starts with hook prefix → update, same message → no-op (idempotent), otherwise → ErrConflict. Placing check-then-act in the service breaks the rule during concurrent promotions.
	UpdateSnapshotMessage(ctx context.Context, repoID, id domain.ContentHash, message string) error
	// SetGraftParents replaces the entire graft (set, seq) register (LWW, seq current+1).
	// Unlike AddGraftParents, it expresses edge removal (join supersede).
	SetGraftParents(ctx context.Context, repoID, id domain.ContentHash, parents []domain.ContentHash) error
	// ApplyJoin applies the reordering of session branches to the storage atomic boundary.
	// It validates and reflects all graft patches' ExpectedSeq, target ref's ExpectedHead, and optionally creates session refs in one go. The PostgreSQL implementation uses a single transaction/row lock.
	ApplyJoin(ctx context.Context, mutation JoinMutation) error
	ListSnapshots(ctx context.Context, repoID domain.ContentHash, branch string) ([]domain.Snapshot, error)
	HasSnapshots(ctx context.Context, repoID domain.ContentHash, ids []domain.ContentHash) (have []domain.ContentHash, err error)
	// AddGraftParents appends a graft (UpdateRef Append) for diverged push to the current head, adding to the snapshot's GraftParents overlay and marking Grafted=true.
	// Parents (original) are never modified — local/server maintain the same Parents for the same snapshot to prevent replica parent agreement breakage (permanent divergence prevention). Reachability is Parents ∪ GraftParents. GraftParents/Grafted are content-hash excluded, so ID/body are immutable.
	AddGraftParents(ctx context.Context, repoID, id domain.ContentHash, add []domain.ContentHash) error
	// AddGraftParentsCAS is for client delayed graft events. When adding a new edge, the current seq must exactly match the expected. If the edge already exists, the event is confirmed once at current==expected (seq+1) and only responds to a successful retry at current==expected+1. Other versions are conflicts. Version/cycle checks and writes are atomic boundaries of the store.
	AddGraftParentsCAS(ctx context.Context, repoID, id domain.ContentHash, add []domain.ContentHash, expected uint64) error
	// SetSnapshotMemory attaches a memory pointer (memory_hash) to the snapshot — after a snapshot push, reflecting memorize (compatibility rules: raw+memory co-possess). ID remains unchanged.
	SetSnapshotMemory(ctx context.Context, repoID, id, memoryHash domain.ContentHash) error

	// Ref / manifest (mutable). Progress is CAS (immutable REF4/C1). UpdateRepoAbout updates About(description/website/topics).
	UpdateRepoAbout(ctx context.Context, id domain.ContentHash, description, website string, topics []string) error
	// UpdateRepoConfig updates repo structure settings (default branch, protected branch). nil = no change.
	UpdateRepoConfig(ctx context.Context, id domain.ContentHash, defaultBranch *string, protectDefault *bool) error
	// Team default settings bundle (.claude/.agents) — kind ∈ {claude, agents}.
	PutSettingsBundle(ctx context.Context, repoID domain.ContentHash, bundle domain.SettingsBundle) error
	GetSettingsBundle(ctx context.Context, repoID domain.ContentHash, kind string) (domain.SettingsBundle, error)
	// Secret ciphertext envelope (E2E — server cannot decrypt, stored as opaque bytes only).
	PutSecretsEnvelope(ctx context.Context, repoID domain.ContentHash, raw []byte) error
	GetSecretsEnvelope(ctx context.Context, repoID domain.ContentHash) ([]byte, error)
	// Commit attachment settings object (content-addressed — target of snapshot's claude_settings/agents_settings).
	PutSettingsObject(ctx context.Context, repoID domain.ContentHash, hash domain.ContentHash, bundle domain.SettingsBundle) error
	GetSettingsObject(ctx context.Context, repoID domain.ContentHash, hash domain.ContentHash) (domain.SettingsBundle, error)
	// In-progress context pointer (session-based upsert — CLI hook capture mirror, resolved by deletion on commit).
	PutPending(ctx context.Context, repoID domain.ContentHash, p domain.Pending) error
	ListPendings(ctx context.Context, repoID domain.ContentHash) ([]domain.Pending, error)
	DeletePending(ctx context.Context, repoID domain.ContentHash, sessionID string) error
	// Push wait pointer ((user, branch) upsert — resolve local ahead commits, delete on git push).
	PutUnsync(ctx context.Context, repoID domain.ContentHash, u domain.Unsync) error
	ListUnsyncs(ctx context.Context, repoID domain.ContentHash) ([]domain.Unsync, error)
	DeleteUnsync(ctx context.Context, repoID domain.ContentHash, user, branch string) error
	// DeleteSnapshot removes snapshot metadata (idempotent). Unique purpose: pending sliding replacement
	// of hook capture leaves GC — called by service layer only when hook prefix·ref reference guard passes.
	DeleteSnapshot(ctx context.Context, repoID, id domain.ContentHash) error

	GetRef(ctx context.Context, repoID domain.ContentHash, kind domain.RefKind, name string) (domain.Ref, error)
	ListRefs(ctx context.Context, repoID domain.ContentHash) ([]domain.Ref, error)
	// CompareAndSwapRef: move to next only if expected matches current disk target (optimistic locking).
	CompareAndSwapRef(ctx context.Context, repoID domain.ContentHash, next domain.Ref, expected domain.ContentHash) error
	// ReadReflog: return append-only log of ref movements in latest order (tip recovery safety net, git reflog equivalent).
	ReadReflog(ctx context.Context, repoID domain.ContentHash) ([]domain.RefLogEntry, error)
	GetManifest(ctx context.Context, repoID domain.ContentHash) (domain.Manifest, error)

	// Memory meta (derivative; body is BlobStore). Target snapshot must exist (sync protocol).
	GetMemoryMeta(ctx context.Context, repoID, snapshotID domain.ContentHash) (domain.MemoryDigest, error)
	PutMemoryMeta(ctx context.Context, repoID domain.ContentHash, digest domain.MemoryDigest) error
}

// GraftPatch is a single graft LWW register to be replaced by join.
type GraftPatch struct {
	SnapshotID  domain.ContentHash
	ExpectedSeq uint64
	Parents     []domain.ContentHash
}

// JoinMutation is an atomic join change set sent to the store.
type JoinMutation struct {
	RepoID domain.ContentHash
	Branch string
	// Source is the commit X pulled by the user. The store revalidates that the entire Segment is still attached to the target branch or scoped internal session ref, and that the single-leaf condition of first-parent is maintained within the repo graph lock/transaction.
	Source domain.ContentHash
	// Segment is the unique first-parent child path calculated by the server from X to tip X…tip.
	// Used for revalidation of attachment/cross-git-branch in storage atomic boundary.
	Segment      []domain.ContentHash
	ExpectedHead domain.ContentHash
	NewHead      domain.ContentHash
	ForkName     string
	ForkTip      domain.ContentHash
	Grafts       []GraftPatch
}

// BlobStore stores content-addressed immutable bodies (data model).
//
// Implementation (impl step): v1 = Postgres BYTEA — blobs(hash PK, bytes BYTEA). hash deduplication (if same hash, existing, skip re-recording = idempotent put). Later replace with S3/MinIO adapter without downtime. Current is stdlib only stub.
//
// Storage units: SessionDoc (CIR body) and MemoryDigest body. Both have sha256:<hex> keys. Integrity: Recalculate hash on Get to verify key (sync protocol, verification is in impl).
type BlobStore interface {
	// PutDoc: Store SessionDoc body. content-hash deduplication (Stored=false means already exists).
	PutDoc(ctx context.Context, repoID domain.ContentHash, doc domain.SessionDoc) (stored bool, err error)
	GetDoc(ctx context.Context, repoID, hash domain.ContentHash) (domain.SessionDoc, error)
	HasDocs(ctx context.Context, repoID domain.ContentHash, hashes []domain.ContentHash) (have []domain.ContentHash, err error)
	// DeleteDoc: Remove doc body (idempotent). Hook capture leaf GC exclusive — commit doc is not target (service layer guard).
	DeleteDoc(ctx context.Context, repoID, hash domain.ContentHash) error

	// PutMemory: Store MemoryDigest body (content-hash deduplication).
	PutMemory(ctx context.Context, repoID domain.ContentHash, digest domain.MemoryDigest) (hash domain.ContentHash, err error)
	GetMemory(ctx context.Context, repoID, hash domain.ContentHash) (domain.MemoryDigest, error)

	// Chunk wire (push/pull delta transmission) — chunks are isolated by repo ownership.
	// PutChunks: Store validated uncompressed chunks with repo ownership and idempotently. Chunks arriving before complete doc are content-addressed staging objects and safe to remain on failure/retry.
	// HasChunks: List of repo owned chunks (negotiate). GetChunk: Uncompressed chunk body.
	// GetDocManifest: Chunk manifest of doc (even if stored in chunks, canonical produces same plan; plan impossible forms ErrNotFound — caller fallback on complete reply).
	PutChunks(ctx context.Context, repoID domain.ContentHash, chunks map[domain.ContentHash][]byte) (stored, deduped int, err error)
	HasChunks(ctx context.Context, repoID domain.ContentHash, hashes []domain.ContentHash) (have []domain.ContentHash, err error)
	GetChunk(ctx context.Context, repoID, hash domain.ContentHash) ([]byte, error)
	GetDocManifest(ctx context.Context, repoID, hash domain.ContentHash) (domain.DocChunkManifest, error)
}

// AuthProvider validates team tokens and determines visibility boundaries (sync protocol).
//
// Visibility unit = team (v1 simple model): Team members can read-write all team repos.
// Implementation (impl step): Token store (e.g., Postgres team_tokens table). Current stub.
type AuthProvider interface {
	// ResolveTeam: Opaque bearer team token → team identifier. Invalid token returns error (401 unauthenticated).
	ResolveTeam(ctx context.Context, token string) (team string, err error)
	// RepoVisible: Whether the team can access the repo (403 forbidden reason).
	RepoVisible(ctx context.Context, team string, repoID domain.ContentHash) (bool, error)
}

// GitEngine is a git engine (module boundary: backend has).
//
// It performs commit/branch/fork/diff/ref(HEAD/tag) based on CIR/hash, independent of provider raw format.
// Core responsibility is DAG reachability (fast-forward determination, LCA, parents transitive closure) (sync protocol, data model).
// Meta/body access via above store port. Current stub.
type GitEngine interface {
	// IsAncestor: Whether ancestor is an ancestor of descendant (DAG parent reachability). FF determination based.
	IsAncestor(ctx context.Context, repoID, ancestor, descendant domain.ContentHash) (bool, error)
	// MergeBase: LCA (lowest common ancestor) of two snapshots. Used for diff/diverged determination.
	MergeBase(ctx context.Context, repoID, a, b domain.ContentHash) (domain.ContentHash, error)
	// AncestorsClosure: Parents transitive closure of ids (includes missing ancestors, sync protocol).
	AncestorsClosure(ctx context.Context, repoID domain.ContentHash, ids []domain.ContentHash) ([]domain.ContentHash, error)
	// ClassifyRefMove: Relationship determination between old (server target) and next (ff/up-to-date/non-ff/diverged).
	ClassifyRefMove(ctx context.Context, repoID, old, next domain.ContentHash) (RefMoveClass, error)
	// VerifyIntegrity checks Snapshot.ID == ContentHash(canonical_bytes(doc.CIR)).
	VerifyIntegrity(ctx context.Context, snap domain.Snapshot, doc domain.SessionDoc) error
}

// IdentityVerifier verifies authentication token (Firebase ID token or dev token) and returns User.
//
// Implementation: FirebaseVerifier (RS256 JWT verification with Google public key, stdlib) / DevVerifier (local demo).
type IdentityVerifier interface {
	// Verify token and return user identifier (uid/email/name). Invalid → domain.ErrUnauthorized.
	Verify(ctx context.Context, idToken string) (domain.User, error)
}

// WorkspaceStore persists users, workspaces, memberships, and invitations (0002 schema).
//
// Visibility boundary = workspace.owner creates invite, invited user joins with token.
type WorkspaceStore interface {
	// User
	UpsertUser(ctx context.Context, user domain.User) error
	GetUser(ctx context.Context, id string) (domain.User, error)
	// GetUserByUsername finds user by handle (username). Returns ErrNotFound if not found.
	// Used for global uniqueness guarantee (auto-create on first login + collision avoidance).
	GetUserByUsername(ctx context.Context, username string) (domain.User, error)

	// Workspace
	CreateWorkspace(ctx context.Context, ws domain.Workspace) error
	GetWorkspace(ctx context.Context, id string) (domain.Workspace, error)
	// GetWorkspaceByPath finds workspace by URL segments (owner_username, slug) — for repo binding.
	GetWorkspaceByPath(ctx context.Context, ownerUsername, slug string) (domain.Workspace, error)
	ListWorkspacesForUser(ctx context.Context, userID string) ([]domain.Workspace, error)

	// Membership. AddMember upserts member (re-adds member to update role).
	AddMember(ctx context.Context, m domain.Membership) error
	RemoveMember(ctx context.Context, workspaceID, userID string) error
	IsMember(ctx context.Context, workspaceID, userID string) (bool, error)
	ListMembers(ctx context.Context, workspaceID string) ([]domain.Membership, error)

	// Invite
	CreateInvite(ctx context.Context, inv domain.Invite) error
	GetInvite(ctx context.Context, token string) (domain.Invite, error)
	UpdateInviteStatus(ctx context.Context, token string, status domain.InviteStatus) error
	ListInvites(ctx context.Context, workspaceID string) ([]domain.Invite, error)

	// Session (server login session — CLI token also represented as long-lived session)
	CreateSession(ctx context.Context, s domain.Session) error
	GetSession(ctx context.Context, token string) (domain.Session, error)
	DeleteSession(ctx context.Context, token string) error
	ListSessionsForUser(ctx context.Context, userID string) ([]domain.Session, error)
}

// RefMoveClass is the classification returned by ClassifyRefMove (sync protocol).
type RefMoveClass string

const (
	MoveFastForward    RefMoveClass = "fast_forward"     // next is descendant of old
	MoveUpToDate       RefMoveClass = "up_to_date"       // next == old
	MoveNonFastForward RefMoveClass = "non_fast_forward" // next is an ancestor of old (behind)
	MoveDiverged       RefMoveClass = "diverged"         // common ancestor exists but not descendants → fork
)
