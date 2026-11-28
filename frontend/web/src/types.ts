// Backend wire(snake_case) and 1:1 type. (No separate mapping, use as is)

export interface User {
  id: string;
  email: string;
  name: string;
/** Global unique personal namespace handle — first URL segment. Changes are heavy (warning needed). */
  username: string;
/** Display alias — URL agnostic, free to change */
  nickname?: string;
/** Context load default fidelity (account global personal settings) — CLI consumes at point of use */
  load_mode?: string;
/** Profile picture data URL (data:image/…;base64,…). Fallback to initials if none */
  avatar?: string;
/** UI display language personal setting (ko|en). Fallback to client browser detection if none */
  locale?: string;
}

/** Allowlist user field for anonymous/profile public endpoint. No account ID, email, or personal settings. */
export interface PublicUser {
  name: string;
  username: string;
  nickname?: string;
  avatar?: string;
  created_at: string;
}

export interface Repository {
  /** Server-computed direct/team/Organization Owner permission for the caller. */
  effective_role?: '' | 'viewer' | 'puller' | 'member' | 'maintainer' | 'owner';
  can_transfer_ownership?: boolean;
  id: string;
  name: string;
  owner_id: string;
/** Namespace that owns the canonical URL. Empty only for legacy records. */
  owner_namespace_id?: string;
/** Unique URL segment for owner (automatically generated from name) */
  slug: string;
/** Owner handle normalization (for URL assembly) */
  owner_username: string;
/** Scope — 'public' | 'private' (empty value also private) */
  visibility?: 'private' | 'public';
/** .cxtsecrets config permission — ''|'members' (role-based = maintainer and above) | 'owner' */
  secrets_policy?: string;
/** Team settings upload permission; values have the same meaning as the backend enum. */
  settings_policy?: string;
/** Synchronize GitHub public status (locks manual visibility setting when enabled) */
  gh_visibility_sync?: boolean;
  gh_synced_at?: string;
/** Archive (read-only) — 403 for write attempts by viewers exceeding the limit */
  archived?: boolean;
/** Alert webhook (Slack incoming webhook compatibility) */
  webhook_url?: string;
/** Default role for public members (including anonymous) — '' (viewer) | 'viewer' | 'puller' */
  public_role?: string;
  created_at: string;
}

/** Allowlist repository field for public endpoints. No operational policy or webhook capability. */
export interface PublicRepository {
  id: string;
  name: string;
  slug: string;
  owner_username: string;
  visibility?: 'private' | 'public';
  public_role?: 'viewer' | 'puller';
  archived?: boolean;
  created_at: string;
}

export type OrganizationRole = 'member' | 'admin' | 'owner';

export interface Organization {
  effective_role?: OrganizationRole;
  id: string;
  namespace_id: string;
  name: string;
  slug: string;
  logo?: string;
  created_by: string;
  created_at: string;
}

export interface PublicOrganization {
  id: string;
  name: string;
  slug: string;
  logo?: string;
  created_at: string;
  repositories: PublicRepository[];
}

export interface OrganizationMembership {
  organization_id: string;
  user_id: string;
  role: OrganizationRole;
  user?: User;
  created_at: string;
}

export interface OrganizationPolicy {
  organization_id: string;
  repository_creation: 'admins' | 'members';
  default_repository_visibility: 'private' | 'public';
  default_repository_role?: '' | import('./roles').Role;
  allow_public_repositories: boolean;
  break_glass_enabled: boolean;
  break_glass_max_minutes: number;
  updated_by?: string;
  updated_at: string;
}

export interface OrganizationAuditEvent {
  correlation_id?: string;
  id: string;
  organization_id: string;
  actor_id: string;
  action: string;
  target_type?: string;
  target_id?: string;
  reason?: string;
  created_at: string;
}

export interface BreakGlassGrant {
  id: string;
  organization_id: string;
  repository_id: string;
  user_id: string;
  reason: string;
  created_at: string;
  expires_at: string;
}

/** Update repository settings (owner only) */
export interface RepositoryPatch {
  visibility?: 'private' | 'public';
  secrets_policy?: 'members' | 'owner';
  settings_policy?: 'members' | 'owner';
  gh_visibility_sync?: boolean;
  archived?: boolean;
  webhook_url?: string;
  slug?: string;
  public_role?: 'viewer' | 'puller';
}

export interface Membership {
  role: string;
  user_id: string;
  user?: User;
}

export interface Invite {
  token: string;
  repository_id: string;
  email: string;
  role: string;
  status: string;
  created_at?: string;
  expires_at?: string;
}

export interface Repo {
  id: string;
  remote_url: string;
/** Code repo git origin (GitHub etc.) — Linked tab link */
  git_remote_url?: string;
  description?: string;
  website?: string;
  topics?: string[] | null;
}

/** Team default settings bundle upload payload */
export interface SettingsUpload {
  files: { path: string; content_b64: string }[];
}

/** Branch/tag/HEAD pointer */
export interface Ref {
  branch_id?: string;
  kind: string;
  name: string;
  repo_id: string;
  target: string;
  symbolic?: string;
}

/** Uncommitted context pointer for each session — latest captured snapshot outside branch refs. This is durable capture state, not proof that a provider process is currently alive. */
export interface Pending {
  repo_id: string;
  session_id: string;
  branch: string;
  provider: string;
/** Hash of the latest hook capture snapshot (= doc hash, content-addressed). */
  target: string;
  author?: { name: string; email: string; team: string };
  updated_at: string;
  /** Actual transcript activity, not the time this pointer was synchronized. */
  activity_at?: string;
/** Session hidden in the user's uncommitted list (data not deleted, sticky) — true to exclude from list. */
  dismissed?: boolean;
}

/** CIR event block (text / tool_use / tool_result / thinking, etc.) */
interface CIRBlock {
  type: string;
  text?: string;
  name?: string;
}

export interface DocViewEvent extends CIREvent {
  deferred?: boolean;
  event_index?: number;
  event_bytes?: number;
}

export interface DocFragmentPage {
  event_index: number;
  byte_offset: number;
  next: number;
  total_bytes: number;
  json_fragment: string;
}

export interface CIREvent {
  kind: string;
  id?: string;
  ts?: string;
  seq?: number;
/** Explicitly modeled provider-local replay identity (CIR v2). */
  provider_metadata?: { turn_id?: string; create_time?: number };
  role?: string;
  blocks?: CIRBlock[];
/** tool_call: Regular/tool name + input (original strings like Edit's old/new preserved) */
  tool_name?: string;
  provider_tool_name?: string;
  input?: Record<string, unknown>;
  call_id?: string;
/** tool_result: Tool output (string, object, or provider content block array) */
  output?: unknown;
/** reasoning: Plain text summary (separate from locked original text) */
  redacted_summary?: string;
/** Summary message created by agent compression (claude isCompactSummary / codex compacted.message) */
  compact_summary?: boolean;
/** Codex multi-agent message metadata; visible text is rendered as assistant conversation */
  agent_message?: boolean;
  agent_author?: string;
  agent_recipient?: string;
/** compaction: provider-visible replacement context, kept nested so archival events remain lossless without duplicate rendering */
  replacement?: CIREvent[];
/** false means the nested projection is partial and archival replay is authoritative */
  replacement_complete?: boolean;
/** reasoning/compaction provider-locked opaque state */
  locked?: { provider?: string; scheme?: string; blob?: string };
}

/** Compressed memory distilled from snapshot */
export interface MemoryDigest {
  claims_version?: 1;
  snapshot_id: string;
  previous_memory_hash?: string;
  summary: string;
  key_facts: string[] | null;
  open_tasks: string[] | null;
  provider: string;
  fragments?: Array<{
    source_snapshot: string;
    summary?: string;
    key_facts?: string[];
    open_tasks?: string[];
    tasks_authoritative?: boolean;
    claims?: Array<{ kind: 'code' | 'decision' | 'rationale'; text: string; code?: { commit: string; parent?: string; paths: string[] } }>;
  }>;
  graft_coverage?: {
    projection_version: number;
    projection_complete: boolean;
    lineage_fingerprint?: string;
    graft_seq: number;
    graft_parents?: string[];
    pinned_sources?: string[];
  };
}

export interface StorageUsageReport {
  namespace_id: string;
  policy: { plan: '' | 'free' | 'team' | 'enterprise'; included_bytes: number; pay_as_you_go: boolean; max_bytes: number | null; grace_bytes: number; grace_until: string | null };
  policy_revision: number;
  current_bytes: number;
  excess_bytes: number;
  state: 'metering' | 'active' | 'warning' | 'overage' | 'grace' | 'read_only';
  metered_since: string | null;
  period_start: string;
  period_end: string;
  overage_byte_hours: string;
  entries: { sequence: number; delta_bytes: number; bytes_after: number; reason: string; occurred_at: string }[];
}

export interface NotificationJob {
  id: string;
  repository_id: string;
  kind: string;
  text: string;
  state: 'pending' | 'running' | 'retrying' | 'delivered' | 'attention';
  attempts: number;
  version: number;
  reason?: string;
  http_status?: number;
  created_at: string;
  updated_at: string;
  next_attempt: string;
  lease_until: string;
}

export interface Team {
  id: string; organization_id: string; name: string; slug: string; description?: string; created_at: string;
  can_manage: boolean; can_delete: boolean;
}
export interface TeamMembership { source?: 'github'; team_id: string; organization_id: string; user_id: string; role: 'member' | 'maintainer'; created_at: string }
export interface TeamRepositoryGrant { team_id: string; organization_id: string; repository_id: string; role: import('./roles').Role; created_at: string }
export interface EnterprisePolicy { repository_creation: 'admins' | 'members'; allow_public_repositories: boolean; allow_break_glass: boolean }
export interface Enterprise { id: string; name: string; slug: string; logo?: string; policy: EnterprisePolicy; created_by: string; created_at: string }
export interface EnterpriseMembership { user?: Pick<User, 'id' | 'username' | 'name' | 'nickname'>; enterprise_id: string; user_id: string; role: 'owner' | 'admin' | 'member'; created_at: string }
export interface EnterpriseAuditEvent { id: string; enterprise_id: string; actor_id: string; action: string; target_id: string; created_at: string }

export interface CollaborationInvitation {
 email_enabled?: boolean;
 email_status?: 'not_sent' | 'queued' | 'sending' | 'retrying' | 'accepted' | 'attention' | 'cancelled';
 email_reason?: string;
 id: string;
 kind: 'organization' | 'enterprise';
 space_id: string;
 space_name: string;
 space_path: string;
 email: string;
 recipient_id?: string;
 role: OrganizationRole;
 status: 'pending' | 'accepted' | 'revoked' | 'expired';
 created_at: string;
 expires_at: string;
 updated_at: string;
}

export interface MCPApplication { client_id:string; name:string; scope:string; created_at:string; expires_at:string }
export interface AccountAuditEvent { grant_id?:string; id:string; user_id:string; client_id:string; action:string; created_at:string }

// Anchored sessions: a coding-agent session cut at the commits it made, as the
// uploader's CLI read it from the records the tools and Git left behind.
export type AnchoredProvider = 'claude' | 'codex';
export type AnchoredLinkRule = 'printed-number' | 'same-folder-and-time';
type AnchoredSpawnRule = 'tool-call' | 'start-time';
type AnchoredPlaceRule = 'recorded-by-tool' | 'head-log' | 'current-branch' | 'folder-no-longer-a-worktree';
/** A part of one session. `first_event` and `events` are absent when zero. */
export interface AnchoredRange {
  first: string; last: string; first_ordinal: number; last_ordinal: number; commands: number;
  started_at?: string; ended_at?: string; first_event?: number; events?: number;
}
export interface AnchoredCommit { sha: string; rule: AnchoredLinkRule; created_at?: string; authored_at?: string; author_email?: string; subject?: string }
/** `hidden` is set in what another member reads of a session: the segment is not open and keeps only its place. */
interface AnchoredSegment extends AnchoredRange { closes_turn: boolean; commits: AnchoredCommit[]; hidden?: boolean }
export interface AnchoredPlace { worktree: string; branch?: string; removed?: boolean; rule: AnchoredPlaceRule }
/** `partial` is set in what another member reads of a session: the part is open up to here and goes on. */
interface AnchoredUncommitted extends AnchoredRange { places: AnchoredPlace[]; unplaced: boolean; partial?: boolean }
interface AnchoredSpawn { at_record: string; at_ordinal: number; rule: AnchoredSpawnRule }
export interface AnchoredAgentCall { tool: string; from: string; to: string[]; status?: string; at?: string; record?: string }
/** The conversation of a session. `doc` is given to the uploader only; everyone reads the conversation through the session. */
interface AnchoredContent { events: number; parts?: { doc: string; identity?: string; events: number }[] }
export interface AnchoredSessionBody {
  rule_version: number; sub: boolean; parent?: string; spawn?: AnchoredSpawn; scope: 'working-folder' | 'origin-url'; machine?: string;
  started_at?: string; ended_at?: string; records: number; commands: number; copied_records: number; copied_from?: string[]; file_copies?: number;
  segments: AnchoredSegment[]; uncommitted?: AnchoredUncommitted; agent_calls?: AnchoredAgentCall[]; content?: AnchoredContent;
}
export interface AnchoredSession {
  repo_id: string; provider: AnchoredProvider; session_id: string; uploader: string; revision: number; body_hash: string;
  created_at: string; updated_at: string; body: AnchoredSessionBody;
  /** Present while the session is archived. `archived_by` is given to the session's uploader only. */
  archived_at?: string; archived_by?: string;
}
/** Whether a session is archived after a request to archive or restore it. */
export interface AnchoredSessionArchive { archived: boolean; archived_at?: string }
interface AnchoredSummaryCommit {
  sha: string; segment_no: number; rule: AnchoredLinkRule; subject?: string; created_at?: string; authored_at?: string;
  /** True when the remote holds the commit; otherwise `copies` names the remote commits that are copies of it. */
  on_remote?: boolean; copies?: string[];
}
interface AnchoredSummaryUncommitted { commands: number; started_at?: string; ended_at?: string; places: AnchoredPlace[]; unplaced: boolean }
interface AnchoredSummarySpawn { segment_no?: number; uncommitted?: boolean; rule: AnchoredSpawnRule }
export interface AnchoredSessionSummary {
  provider: AnchoredProvider; session_id: string; parent?: string; sub: boolean; revision: number; body_hash: string;
  records: number; commands: number; segments: number; machine?: string; started_at?: string; ended_at?: string; content: boolean;
  commits: AnchoredSummaryCommit[]; uncommitted?: AnchoredSummaryUncommitted; spawned_in?: AnchoredSummarySpawn; updated_at: string;
  /** The account the session belongs to. `shared` is true when that is another member: the summary then holds only what is open to the team. */
  uploader?: string; shared?: boolean;
  /** The uploader's public handle, given with a session that is not the caller's. */
  uploader_name?: string;
  /** Present for an archived session: it is listed only among the archived sessions. */
  archived_at?: string;
}
/**
 * What a CLI wrote of a session when the session made a commit. It is read under the rule of the segment that ends at
 * the commit: by the uploader always, by anyone else once the commit is pushed.
 */
export interface AnchoredSummary {
  repo_id: string; provider: AnchoredProvider; session_id: string; commit: string; body_hash: string;
  /** `tasks_authoritative`: the open tasks are the tool's own list, not what a CLI picked out of the conversation. */
  body: { summary: string; key_facts: string[]; open_tasks: string[]; tasks_authoritative?: boolean };
  created_at: string; updated_at: string; segment_no?: number; uploader?: string; uploader_name?: string;
}
/**
 * One thing a search of the sessions tab found the query in. `kind` says which: a pull request, a branch, a commit, a
 * part of a session's conversation, or a manual note.
 */
export interface AnchoredSearchHit {
  kind: 'pull' | 'branch' | 'commit' | 'segment' | 'note';
  title: string; snippet?: string; at?: string;
  pull?: number; branch?: string; commit?: string;
  provider?: AnchoredProvider; session_id?: string; segment_no?: number; rest?: boolean; event?: number;
  note_id?: string; target?: AnchoredNoteTarget;
}
/** `truncated` is true when more was found than is listed. */
export interface AnchoredSearchResult { hits: AnchoredSearchHit[]; truncated: boolean }
export interface AnchoredCommitSegment { provider: AnchoredProvider; session_id: string; segment_no: number; rule: AnchoredLinkRule; segment: AnchoredSegment }
interface BranchEvidence { kind: 'registered' | 'hosting-push'; account?: string; source?: string }
export interface BranchLifetime {
  repo_id: string; ledger_id: string; name: string; opened_at: string; opened_by: BranchEvidence; closed_at?: string; closed_by?: BranchEvidence;
  linked: { id: string; linked_at: string; by: BranchEvidence }[];
  reopenings?: { closed_at: string; closed_by: BranchEvidence; reopened_at: string; reopened_by: BranchEvidence; name?: string }[];
}
/** What a manual note is attached to. Exactly the fields of its kind are present. */
export type AnchoredNoteTarget =
  | { kind: 'commit'; commit: string }
  | { kind: 'branch'; branch: string; ledger_id?: string }
  | { kind: 'worktree'; worktree: string; machine?: string };
/** What a person wrote about a commit, a branch or a worktree. Kept apart from captured sessions. */
export interface AnchoredNote {
  repo_id: string; id: string; target: AnchoredNoteTarget; author: string; body: string; created_at: string; updated_at: string;
  /** The author's public handle, as the server gives it with a listed note. */
  author_name?: string;
}
/** A working folder of the repository on one of the caller's machines, by name. */
export interface ReportedWorktree { machine?: string; name: string; branch?: string; removed: boolean; first_seen_at: string; last_seen_at: string }
/** Commits, branches and pull requests as the server read them from the hosting provider. */
/** `head` is the newest commit of the push the server was reading when it first stored the commit; absent when it came with no push the server saw. */
export interface RemoteCommit { sha: string; parents: string[]; author_name?: string; authored_at: string; committed_at: string; subject: string; head?: string }
/** A collector is the program of cxt that uploads sessions from one computer. `run_error` is given to its owner only; `by`, `machine` and `cxt_version` to signed-in readers only. */
export interface PushCollector { by?: string; machine?: string; cxt_version?: string; heard_at: string; failing: boolean; run_error?: string; own: boolean }
/**
 * Where a push came from. `cxt`: a computer where cxt runs made it and its collector has ended a run well since.
 * `cxt_error`: such a computer made it and its collector has only failed since. `no_cxt`: no collector claims it and
 * every known collector was heard from after it. `unknown`: a collector that has not been heard from since may still claim it.
 * `collector` and `waiting` are positions in the view's `collectors`.
 */
export interface PushOrigin { branch: string; sha: string; at: string; state: 'cxt' | 'cxt_error' | 'no_cxt' | 'unknown'; collector?: number; claimed_at?: string; waiting?: number[] }
export interface PushOriginView { collectors: PushCollector[]; pushes: PushOrigin[] }
/**
 * Whether the server reads the repository on GitHub through the GitHub App. `connected`: an installation in
 * use was told to read it. `not_connected`: it is not. `not_configured`: the server has
 * no GitHub App. `connect_url` is the page where a maintainer of the repository connects it.
 */
export interface GitHubRepositoryConnection { state: 'connected' | 'not_connected' | 'not_configured'; connect_url?: string }
export interface RemoteBranch { name: string; sha: string }
export interface RemotePull {
  number: number; title: string; state: 'open' | 'closed' | 'merged'; head_ref: string; head_sha: string; base_ref: string;
  merge_sha?: string; fork?: boolean; commits: string[]; truncated?: boolean; updated_at: string;
}
/** One page. `branches`, `pulls`, `default_branch` and `synced_at` come with the first page only. */
export interface RemoteGitView { default_branch?: string; synced_at?: string; branches?: RemoteBranch[]; pulls?: RemotePull[]; commits: RemoteCommit[]; next?: string }
