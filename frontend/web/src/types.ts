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
  allow_public_repositories: boolean;
  break_glass_enabled: boolean;
  break_glass_max_minutes: number;
  updated_by?: string;
  updated_at: string;
}

export interface OrganizationAuditEvent {
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

/** User profile activity feed — monthly commit bundles + repository creation */
export interface ActivityRepo {
  name: string;
  path: string;
  count: number;
}
export interface ActivityCreated {
  name: string;
  path: string;
  visibility: string;
  date: string;
}
export interface ActivityMonth {
  month: string; // YYYY-MM
  commit_total: number;
  commit_repos: ActivityRepo[];
  created: ActivityCreated[];
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
  context_protocol?: number;
  id: string;
  remote_url: string;
  default_branch: string;
/** Default branch --force move forbidden (protected branch) */
  protect_default?: boolean;
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

/** Context snapshot (= git commit) */
export interface Snapshot {
  id: string;
  repo_id: string;
  branch: string;
/** branch reflog projection of git branch membership */
  branches?: string[];
  parents: string[] | null;
  doc_hash: string;
/** Attached compressed memory (MemoryDigest) hash — if present, provides memory view */
  memory_hash?: string;
  provider: string;
  author?: { name: string; email: string; team: string };
  fidelity: string;
  message: string;
  created_at: string;
/** Graft (diverged append) overlay snapshot — join point for new context session */
  grafted?: boolean;
  /** Reachability-overlay parents attached by a server graft. Natural parents remain immutable. */
  graft_parents?: string[];
/** Graft LWW register version (owned by server, not used for display) */
  graft_seq?: number;
/** Original agent session identifier — if different from parent, marks session boundary (new session start) */
  session_id?: string;
/** List of models that appeared in the session (in order) — for participant AI icons. No legacy snapshots. */
  models?: string[];
/** Number of context compression (compact_boundary) operations in the session — compress at this point (◈) if greater than parent. */
  compaction_count?: number;
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

/** Pending-push pointer keyed by (user, branch): the tip of a local commit chain not yet pushed to Git.
 *  Object reaches server first via shadow push, resolved by deletion on git push. Rendered on the "On Hold" tab. */
export interface Unsync {
  repo_id: string;
  user: string;
  branch: string;
  target: string;
  author?: { name: string; email: string; team: string };
  updated_at: string;
}

/** Search result item (similar to backend inbound.SearchHit) */
export interface SearchHit {
  snapshot_id: string;
  branch: string;
  kind: 'commit' | 'event';
  role?: string;
  seq?: number;
  snippet: string;
  created_at: string;
}

/** Single event change in two snapshot diffs (similar to backend inbound.DiffEntry) */
export interface DiffEntry {
  op: 'add' | 'remove';
  seq: number;
  summary: string;
}

/** CIR event block (text / tool_use / tool_result / thinking, etc.) */
export interface CIRBlock {
  type: string;
  text?: string;
  name?: string;
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

/** Content-addressed session body (CIR) */
export interface SessionDoc {
  hash: string;
  cir: {
    envelope: {
      cir_version?: '1' | '2';
      source_provider?: string;
      source_model?: string;
      captured_at?: string;
      git_branch?: string;
/** Context window size (tokens) at session end — if none, fallback to old capture (unobserved) */
      context_tokens?: number;
/** Total number of assistant output tokens in session */
      output_tokens?: number;
/** All models that appeared in the session (in order of appearance) — source_model is the last used representative value */
      source_models?: string[];
    };
    events: CIREvent[];
  };
}

/** ref log entry 1 (git reflog — GET /repos/{id}/reflog). */
export interface RefLogEntry {
  kind: string;
  name: string;
  old: string;
  new: string;
  created_at: string;
}

/** Immutable server-confirmed context operations, independent of conversation events. */
export interface HistoryEvent {
 creation?: GitCreation;
  pr_completed?: boolean;
  shared_target?: string;
  local_branch?: string;
  previous_branch?: string;
  binding_parent?: string;
  name_parent?: string;
  id: string;
  repo_id: string;
  branch_id: string;
  branch: string;
  kind: 'birth' | 'attach' | 'orphan' | 'position' | 'publish' | 'advance' | 'rename' | 'archive' | 'pr-merge';
  pr?: {number: number; base_branch: string; head_branch: string; head_sha: string; merge_sha: string};
  source_branch_id?: string;
  recovery_evidence?: 'user-confirmed-unborn-head';
  source?: string;
  target?: string;
  memory_source?: string;
  memory_hash?: string;
  memory_pinned?: boolean;
  git_before?: string;
  git_after?: string;
  worktree_id?: string;
  created_at: string;
}

export interface PRPromotionJob {
 id: string;
 repo_id: string;
 pr: { number: number; base_branch: string; head_branch: string; head_sha: string; merge_sha: string };
 state: 'waiting' | 'retrying' | 'running' | 'completed' | 'attention';
 attempts: number;
 reason?: string;
 updated_at: string;
 next_attempt: string;
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

/** A complete graph generation, read in one backend transaction. */
export interface RepositoryRevision { graph: string; pending: string; evidence?: string }
/** Raw capture patch: branch memberships belong to the full graph generation. */
export type PendingSnapshot = Omit<Snapshot, 'branches'>;
export interface PendingView { graph: GraphState; revision: RepositoryRevision; pending: Pending[]; snapshots: PendingSnapshot[] }
export type BranchLineage = 'natural' | 'unchanged' | 'graft' | 'disconnected' | 'missing' | 'unknown' | 'orphan';
export interface ContextSemantics {
  version: 1;
  merges: { event_id: string; birth_id?: string; completed: boolean; placement_intact: boolean; source_available: boolean; lineage: BranchLineage }[];
}
export interface RepositoryView {
  graph: GraphState;
  default_branch: string;
  /** Absent only during rolling upgrades from pre-projection servers. */
  semantics?: ContextSemantics;
  revision?: RepositoryRevision;
  refs: Ref[];
  snapshots: Snapshot[];
  reflog: RefLogEntry[];
  history: HistoryEvent[];
  pending: Pending[];
  unsync: Unsync[];
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

export interface GitChangeSummary {
 id: string;
 request: {target: string; commit: string; parent?: string; target_parent?: string};
 state: 'waiting' | 'running' | 'retrying' | 'completed' | 'attention';
 version: string;
 reason?: string;
 updated_at: string;
 coverage?: 'full' | 'partial' | 'unverified';
 verified_paths: number;
 unverified_paths: number;
}
export interface GitChangePage {items: GitChangeSummary[]; next_cursor?: string}

export interface GitScanJob {
 id: string; commit: string; state: GitChangeSummary['state']; indexed: boolean; tree_indexed?: boolean;
 version: string; reason?: string; updated_at: string;
}
export interface GitScanPage {items: GitScanJob[]; next_cursor?: string; reconciliation?: {state: 'waiting' | 'running' | 'retrying' | 'completed'; page: number; reason?: string}}

export interface CodeSelection {code_commit: string; source_commit: string; source_parent?: string; paths: string[]}
export interface CodeApplicabilityResult {
 selection: CodeSelection; revision: RepositoryRevision; state_hash: string;
 relation: 'ancestor' | 'not_ancestor' | 'unknown'; reason?: string;
 paths: {path: string; state: 'applied' | 'before' | 'changed' | 'equivalent' | 'not_in_history' | 'unknown'; reason?: string; before: {oid?: string; mode?: string}; after: {oid?: string; mode?: string}; selected: {oid?: string; mode?: string}}[];
}

export interface EffectiveMemorySelection {snapshot_id: string; code_commit: string; memory_hash?: string; branch?: string}
export interface MemoryPositions {
 snapshot_id: string;
 event_id?: string;
 code_commit?: string;
 reason: 'selected_event' | 'unique' | 'ambiguous' | 'unavailable';
 options: {event_id: string; code_commit: string; branch: string; kind: string; pr_number?: number; created_at: string}[];
}
export interface EffectiveMemoryItem {
 id: string;
 source_snapshot: string;
 kind: 'code' | 'decision' | 'rationale' | 'legacy_summary' | 'legacy_fact' | 'legacy_task';
 text: string;
 text_hash?: string;
 part?: number;
 code?: {commit: string; parent?: string; paths: string[]};
 state: 'applied' | 'inactive' | 'retained' | 'review';
 reason: string;
 publication_ids?: string[];
 integration_receipt?: string;
 paths?: CodeApplicabilityResult['paths'];
}
export interface EffectiveMemoryPage {
	 inclusion?: BranchContext;
 selection: EffectiveMemorySelection;
 revision: RepositoryRevision;
 state_hash: string;
 lineage_hash: string;
 items: EffectiveMemoryItem[];
 total: number;
 next_cursor: string;
}

export interface JoinPreview {
 snapshot: string;
 branch: string;
 branch_id: string;
 branches: Array<{branch:string; branch_id:string}>;
 reason?: 'branch_required' | 'no_branch' | 'already_head' | 'unpushed' | 'uncommitted' | 'natural_history' | 'cross_branch' | 'branched';
 expected_head?: string;
 tip?: string;
 descendants: number;
 drop_targets: string[];
 only_revision?: string;
 all_revision?: string;
}

/** Server-owned business facts. UI owns only filtering, folding and coordinates. */
export interface GraphState {
	integrations?: {branch: string; scope: string; target: string; head: string; parents: Record<string,string[]>; extra_parents: string[]}[];
	branch_contexts?: Record<string, BranchContext>;
  version: 1; revision: RepositoryRevision; position_event?: string; primary_branch: string;
  snapshot_ids: string[]; graph_ids: string[]; committed_ids: string[]; historical_ids: string[];
  shared_ids: string[]; pushed_ids: string[]; unpushed_ids: string[]; uncommitted_ids: string[];
  tagged_ids: string[]; archived_only_ids: string[]; ahead_ids: string[]; ahead_tips: string[];
  markers: {branch: string; target: string; kind: 'joined' | 'archived'; unique_count: number; target_available: boolean}[];
  branch_heads: Record<string, {branch: string; event_id: string; archived: boolean}>;
  ref_scopes: Record<string,string>; snapshot_scopes: Record<string,string>; scope_labels: Record<string,string>;
  hold: {tips: Unsync[]; ids: string[]}[]; orphan_sessions: string[]; hold_counts: Record<string,number>;
  positions: {event_id: string; branch: string; branch_id: string; snapshot: string; archived: boolean; created_at: string}[];
  previous: {key: string; branch: string; before: string; after: string; created_at: string; snapshot_ids: string[]; collapsible_ids: string[]}[];
  branch_snapshots: Record<string,string[]>;
  continuations: Record<string,string>;
  operations: {births: GraphBirth[]; merges: GraphMerge[]};
}
export interface BranchContext {
  branch_id: string; snapshot_id: string; code_commit?: string; reason: string;
  roots: string[]; snapshot_ids: string[];
  merges: {event_id: string; destination_branch_id?: string; source: string; before?: string; merge_sha: string; pr_number: number; state: 'included' | 'not_selected' | 'review'; reason: string; order: number}[];
}
export interface GraphBirth {
  id: string; event_id: string; branch: string; source: string; orphan: boolean; created_at: string; children: string[];
}
export interface GraphMerge {
  id: string; event_id: string; before: string; after: string; source: string; branch: string; scope: string; from: string;
  created_at: string; pr_number?: number; historical_only: boolean; withdrawn: boolean;
  represented_grafts: string[]; redirect_children: string[]; source_birth?: string; lifecycle_birth?: string;
}

export interface GitCreation {
 evidence: 'process-argv' | 'unavailable'; command?: string[]; start_ref?: string;
 start_commit?: string; origin_branch?: string; origin_branch_id?: string;
}
export interface SyncAuditCheck {
 id: string; event_id?: string; snapshot?: string; branch?: string;
 state: 'verified' | 'mismatch' | 'incomplete' | 'unavailable'; code: string;
 expected?: string; actual?: string; creation?: GitCreation;
}
export interface SyncAuditPage {
 phase?: string;
 version: number; revision: string; checked_at: string; checks: SyncAuditCheck[];
 next_cursor?: string; processed: number; total: number;
}

export interface Team {
  id: string; organization_id: string; name: string; slug: string; description?: string; created_at: string;
  can_manage: boolean; can_delete: boolean;
}
export interface TeamMembership { team_id: string; organization_id: string; user_id: string; role: 'member' | 'maintainer'; created_at: string }
export interface TeamRepositoryGrant { team_id: string; organization_id: string; repository_id: string; role: import('./roles').Role; created_at: string }
export interface EnterprisePolicy { repository_creation: 'admins' | 'members'; allow_public_repositories: boolean; allow_break_glass: boolean }
export interface Enterprise { id: string; name: string; slug: string; logo?: string; policy: EnterprisePolicy; created_by: string; created_at: string }
export interface EnterpriseMembership { user?: Pick<User, 'id' | 'username' | 'name' | 'nickname'>; enterprise_id: string; user_id: string; role: 'owner' | 'admin' | 'member'; created_at: string }
export interface EnterpriseAuditEvent { id: string; enterprise_id: string; actor_id: string; action: string; target_id: string; created_at: string }
