package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// SaveSession is a use-case port that saves the active session of the current cwd as a snapshot (SPINE §6.2).
// Called from MCP session_save / CLI cxt save / hook Stop|SessionEnd.
type SaveSession interface {
	// Save takes SaveInput, creates a snapshot, and returns SaveOutput.
	Save(ctx context.Context, in SaveInput) (SaveOutput, error)
}

// ForkSession is a use-case port that forks a new branch from a specified snapshot (SPINE §6.2).
// Called from MCP session_fork / CLI cxt fork.
// Fork = ref clone (O(1)). The original snapshot/ref is immutable (Invariant F1).
type ForkSession interface {
	// Fork takes ForkInput, creates a new branch, and returns ForkOutput.
	Fork(ctx context.Context, in ForkInput) (ForkOutput, error)
}

// LoadSession is a use-case port that restores a snapshot to the target provider session file (SPINE §6.2).
// Called from MCP session_load / memory_load / CLI cxt load / cxt memory load.
// Mode=full|reconstructed for full-context restoration, Mode=memory for memory-form summary injection.
type LoadSession interface {
	// Load takes LoadInput, restores the session, and returns LoadOutput.
	// LoadOutput.Fidelity is the empirically verified fidelity and may differ from the requested Mode.
	Load(ctx context.Context, in LoadInput) (LoadOutput, error)
}

// ListSessions is a use-case port that retrieves the list of snapshots/refs for repo/branch (SPINE §6.2).
// Called from MCP session_list / CLI cxt list.
type ListSessions interface {
	// List takes ListInput and returns the list of snapshots/refs.
	List(ctx context.Context, in ListInput) (ListOutput, error)
}

// SyncRepo is a use-case port for syncing with the central server (SPINE §6.2).
// Called by MCP sync_push / sync_pull / CLI cxt push / cxt pull.
// ConnectOutput is the result of SyncRepo.Connect.
type ConnectOutput struct {
	// Repo is a server-confirmed record. Binds to workspace if WorkspaceID != "" (web display).
	Repo domain.Repo
}

type SyncRepo interface {
	// Push uploads local snapshot/ref to the central server.
	Push(ctx context.Context, in SyncInput) (SyncOutput, error)
	// Pull downloads central server's snapshot/ref to local and merges it.
	Pull(ctx context.Context, in SyncInput) (SyncOutput, error)
	// Connect immediately registers the origin repo with the server (for remote add connection check).
	Connect(ctx context.Context, in SyncInput) (ConnectOutput, error)
	// SyncPendings reflects the in-progress context pointer to the server (hook detached helper path).
	// resolveSessions is a list of sessions to be removed from the remote after commit resolution.
	SyncPendings(ctx context.Context, in SyncInput, resolveSessions []string) (int, error)
	// ResolveRemoteBranch queries the remote (server) branch ref and prepares it for fetch-only if the target snapshot object is not present locally (for web fork connection). Returns ErrNotFound if not found.
	ResolveRemoteBranch(ctx context.Context, in SyncInput, branch string) (domain.Ref, error)
	// AppendBranch appends the server branch ref to the target (lossless graft) — path for merging PR merge context into branch. On success, local ref mirrors only if fast-forward (local is ahead). Already reflected (behind) targets are rejected by the server as non_fast_forward — caller treats as idempotent no-op.
	AppendBranch(ctx context.Context, in SyncInput, branch string, target domain.ContentHash) error
}

// TagInput is the input DTO for TagRef.Tag.
type TagInput struct {
	// Cwd is the repository interpretation base directory.
	Cwd string
	// Name is the tag name.
	Name string
	// Ref is the target (branch/snapshot ID/HEAD) that the tag points to. If empty, HEAD.
	Ref string
}

// TagOutput is the information of the created tag.
type TagOutput struct {
	Name   string
	Target domain.ContentHash
}

// TagRef is a use-case for tag creation/listing (corresponds to git tag). Tags are immutable —
// to point to a different target with the same name, delete and recreate (the server requires --force).
type TagRef interface {
	Tag(ctx context.Context, in TagInput) (TagOutput, error)
	Tags(ctx context.Context, cwd string) ([]domain.Ref, error)
}

// StashInput is the input DTO for StashSession.Stash.
type StashInput struct {
	Cwd      string
	Provider domain.ProviderKind // if empty, claude
	Message  string              // empty means Git-style "WIP on <branch>"
	Author   domain.TeamIdentity
}

// StashOutput is the result of a stash operation.
type StashOutput struct {
	// StashID is the hash of the stashed snapshot.
	StashID domain.ContentHash
	// Branch is the branch at the time of the stash.
	Branch string
	// Depth is the stack depth after the stash.
	Depth int
	// RestoredHead indicates whether the branch head context was restored (false if no head).
	RestoredHead bool
	// ResumeCmd is the command to resume the restored head session.
	ResumeCmd string
}

// StashPopOutput is the result of a pop operation.
type StashPopOutput struct {
	Entry     domain.StashEntry
	Fidelity  string
	ResumeCmd string
	// Depth is the stack depth after popping.
	Depth int
}

// StashSession corresponds to the use-case for git stash:
// Stash = save the active session to the stack and return to the branch head (commit chain) context,
// StashPop = restore the saved session to the active context.
type StashSession interface {
	Stash(ctx context.Context, in StashInput) (StashOutput, error)
	StashPop(ctx context.Context, cwd string) (StashPopOutput, error)
	StashList(ctx context.Context, cwd string) ([]domain.StashEntry, error)
}

// InitRepo is a use-case port for registering the current repo and creating a local .cxt/ store (_RECONCILIATION D section).
// Called by MCP repo_init / CLI cxt init / cxt repo create.
type InitRepo interface {
	// Init takes an InitInput and registers the repo and creates a .cxt/ store.
	Init(ctx context.Context, in InitInput) (InitOutput, error)
}

// CheckoutSession is a use-case port for restoring the target provider session by integrating fork(+load) (_RECONCILIATION D section).
// Called by MCP session_checkout / CLI cxt checkout.
// If NewBranch != "", fork then load (checkout -b), if NewBranch=="" then simple load (checkout).
type CheckoutSession interface {
	// Checkout takes a CheckoutInput, restores the session (forking if needed), and returns a CheckoutOutput.
	Checkout(ctx context.Context, in CheckoutInput) (CheckoutOutput, error)
}

// Memorize is a use-case port for distilling the active session into a MemoryDigest and attaching it to the current branch (_RECONCILIATION D section).
// Called by MCP memorize / memory_save / CLI cxt memorize.
type Memorize interface {
	// Memorize takes a MemorizeInput, distills the active session, attaches it, and returns a MemorizeOutput.
	Memorize(ctx context.Context, in MemorizeInput) (MemorizeOutput, error)
}

// --- DTOs (SPINE §6.2 comments detailed DTO. Field names/types are contracts) ---

// SaveInput is the input DTO for SaveSession.Save.
type SaveInput struct {
	// Cwd is the capture target working directory (session detection criterion).
	Cwd string
	// Provider is the capture provider (claude|codex).
	Provider domain.ProviderKind
	// Message is the snapshot description (commit message meaning). Auto-generated if blank.
	Message string
	// Author is the author identifier.
	Author domain.TeamIdentity
	// Branch is the forced snapshot branch name (inferred from session/git if blank).
	// Used as the "previous branch" context checkpoint when saving.
	Branch string
	// SessionPath is the explicit session file path (inferred from active session if blank, CAPTURE §2.4).
	// Skips detection when using the transcript/rollout path from the hook payload.
	// The explicit path must pass the isolation/non-growth materialization (CaptureExcluded) gate.
	SessionPath string
	// Pending is "in-progress context" mode (hook auto-capture exclusive, CAPTURE §2.2).
	// Snapshot/doc are saved identically, but branch ref is not moved; only session-specific pending
	// pointer is upserted — branch DAG is maintained with commit snapshots only, and in-progress
	// conversation is rendered as "continuing conversation tail" or "Uncommitted" in UI.
	Pending bool
}

// SaveOutput is the output DTO of SaveSession.Save.
type SaveOutput struct {
	// SnapshotID is the generated snapshot ID (= CIR content hash).
	SnapshotID domain.ContentHash
	// Branch is the stored branch name (auto-detection result).
	Branch string
	// SessionID is the original session identifier captured (for pending resolution and matching).
	SessionID string
}

// ForkInput is the input DTO of ForkSession.Fork.
type ForkInput struct {
	// RepoID is the ID of the target repo.
	RepoID string
	// FromSnapshot is the content hash of the parent (base) snapshot for the fork.
	FromSnapshot domain.ContentHash
	// NewBranch is the name of the new branch to create (must not exist in the same repo, invariant F2).
	NewBranch string
	// Author is the branch author.
	Author domain.TeamIdentity
}

// ForkOutput is the DTO for ForkSession.Fork.
type ForkOutput struct {
	// Branch is the name of the created branch.
	Branch string
	// Head is the ContentHash of the new branch's HEAD (immediately after branching = FromSnapshot).
	Head domain.ContentHash
}

// LoadInput is the DTO for LoadSession.Load.
type LoadInput struct {
	// RepoID is the ID of the target repo.
	RepoID string
	// Ref is the name of the ref to load (branch name/tag name/snapshot ID/HEAD).
	Ref string
	// TargetProvider is the restoration target provider.
	TargetProvider domain.ProviderKind
	// Mode is the requested fidelity tier (full|reconstructed|memory).
	Mode domain.FidelityTier
	// Cwd is the working directory for writing session files.
	Cwd string
	// PreferPendingTail true means that when a latest pending (uncommitted hook capture) exists to connect the Ref interpretation result (branch head), it loads that instead — "on" seed continues from the previous session (e.g., the most recent codex task) before the commit.
	PreferPendingTail bool
}

// LoadOutput is the DTO output of LoadSession.Load.
type LoadOutput struct {
	// WrittenPath is the path of the restored session/memory file.
	WrittenPath string
	// Fidelity is the actual achieved fidelity (may differ from requested Mode).
	// Example: cross-provider full request → reconstructed fidelity.
	Fidelity domain.FidelityTier
	// ResumeCmd is the native resume command for full restoration (_RECONCILIATION C.2).
	// Example: "claude --resume <id>", "codex resume <id>". Empty for memory mode/fallback.
	ResumeCmd string
	// TrimmedEvents is the number of old events omitted due to seed budget exceeded (0 = include all).
	// The session doc is a full transcript, so materializing only the recent tail to avoid exceeding the target model window — for the caller (UI) to inform of the omission.
	TrimmedEvents int
}

// ListInput is the input DTO for ListSessions.List.
type ListInput struct {
	// RepoID is the ID of the target repo.
	RepoID string
	// Branch is the branch name to filter by. Empty value means the entire repo.
	Branch string
}

// ListOutput is the output DTO for ListSessions.List.
type ListOutput struct {
	// Snapshots is the list of snapshots that meet the conditions.
	Snapshots []domain.Snapshot
	// Refs is the list of refs for the given repo.
	Refs []domain.Ref
}

// SeedBranch creates a new branch context for branch switching/creation.
// Seed = [main head memory] ⊕ [origin branch memory (inherited, duplicates removed)] + [latest commit raw tail].
// The seed is committed as the first snapshot of the new branch (cutting meaning — the reason for the branch's existence)
// and materialized as a session file (ledger record — excluding capture at restart).
type SeedBranch interface {
	Seed(ctx context.Context, in SeedInput) (SeedOutput, error)
}

// SeedInput is the DTO for SeedBranch.Seed input.
type SeedInput struct {
	// Cwd is the target working directory.
	Cwd string
	// FromBranch is the source branch (origin of seed material).
	FromBranch string
	// NewBranch is the new branch where the seed will be planted.
	NewBranch string
	// Provider is the materialization target provider (e.g., Claude).
	Provider domain.ProviderKind
	// Author is the author of the seed snapshot.
	Author domain.TeamIdentity
}

// SeedOutput is the DTO for SeedBranch.Seed output.
type SeedOutput struct {
	// SnapshotID is the ID of the first (seed) snapshot of the new branch.
	SnapshotID domain.ContentHash
	// SessionID is a distilled seed session identifier (resume target).
	SessionID string
	// WrittenPath is the path to the materialized session file.
	WrittenPath string
	// ResumeCmd is a command to continue from the seed.
	ResumeCmd string
}

// SyncInput is an input DTO for SyncRepo.Push / SyncRepo.Pull.
type SyncInput struct {
	// RepoID is the ID of the sync target repo (empty means Cwd gitctx interpretation).
	RepoID string
	// Cwd is the working directory for repoID interpretation (used if RepoID is empty).
	Cwd string
	// Force means git --force: push forces non-fast-forward moves, pull overwrites diverged branches. Default false = git default policy.
	Force bool
	// Append is push-specific: appends a new context with no common ancestor (unrelated history) to the remote head (server grafts head to root — unlike Force, no loss).
	Append bool
	// FetchOnly is for pull only: fetches objects (snapshot/doc/memory) only and does not move local refs (git fetch meaning). Hooks' auto-pull is used — context does not force convergence (local history = truth of my session, ref movement is user's choice with cxt pull).
	FetchOnly bool
}

// SyncOutput is the DTO for SyncRepo.Push / SyncRepo.Pull.
type SyncOutput struct {
	// Pushed is the number of snapshots pushed.
	Pushed int
	// Pulled is the number of snapshots pulled.
	Pulled int
	// NewRefs is the list of refs updated/added after synchronization (includes branches created by forks).
	NewRefs []domain.Ref
	// Conflicts is the list of ref names skipped during pull due to non-fast-forward. If not empty, the caller is advised to abort merge like git (can be adopted remotely with --force).
	Conflicts []string
	// RemoteAhead is the list of branches in the remote that have new context after the local — used by caller to hint "pull/load if needed" (not enforced).
	RemoteAhead []string
}

// InitInput is the DTO for InitRepo.Init (_RECONCILIATION section).
type InitInput struct {
	// Cwd is the target working directory for registration.
	Cwd string
	// RemoteURL is the explicit git remote URL. "" means auto-detect origin (fallback to cwd if not found).
	RemoteURL string
}

// InitOutput is the DTO output of InitRepo.Init (_RECONCILIATION section).
type InitOutput struct {
	// RepoID is the ID of the registered repo (normalized remote URL or ContentHash of cwd).
	RepoID string
	// LocalStorePath is the local .cxt/ store path created.
	LocalStorePath string
}

// CheckoutInput is the DTO input of CheckoutSession.Checkout (_RECONCILIATION section).
type CheckoutInput struct {
	// RepoID is the ID of the target repo.
	RepoID string
	// From is the restore base ref (branch name/tag name/snapshot ID/HEAD).
	From string
	// NewBranch is the name of the new branch to fork.
	// != "" => fork then load (checkout -b). == "" => simple load (checkout).
	NewBranch string
	// TargetProvider is the recovery target provider.
	TargetProvider domain.ProviderKind
	// Mode is the requested fidelity tier (full|reconstructed|memory).
	Mode domain.FidelityTier
	// Cwd is the working directory to use for session/memory file operations.
	Cwd string
}

// CheckoutOutput is the DTO for CheckoutSession.Checkout output (_RECONCILIATION section).
type CheckoutOutput struct {
	// Branch is the name of the checked out/created branch.
	Branch string
	// Head is the ContentHash of the HEAD of the branch.
	Head domain.ContentHash
	// WrittenPath is the path of the session/memory file that was restored.
	WrittenPath string
	// ResumeCmd is the native resume command for full recovery (memory mode/fallback to empty value).
	ResumeCmd string
	// Fidelity is the actual achieved fidelity (may differ from requested Mode).
	Fidelity domain.FidelityTier
}

// MemorizeInput is the input DTO for Memorize.Memorize (_RECONCILIATION section).
type MemorizeInput struct {
	// Cwd is the working directory for detecting the active session target for distillation.
	Cwd string
	// Provider is the distillation target provider (claude|codex).
	Provider domain.ProviderKind
	// Ref is the distillation target snapshot specification (branch name/snapshot ID, default to current branch head).
	// It is needed in the memorize at the point where head already points to a different location, similar to a checkpoint (branch save just before transition).
	Ref string
}

// MemorizeOutput is the output DTO for Memorize.Memorize (_RECONCILIATION section).
type MemorizeOutput struct {
	// SnapshotID is the ContentHash of the distillation target snapshot.
	SnapshotID domain.ContentHash
	// MemoryHash is the ContentHash of the created or attached MemoryDigest.
	MemoryHash domain.ContentHash
	// Attached indicates whether memory is attached to the current branch snapshot.
	Attached bool
}
