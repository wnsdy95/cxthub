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

export interface Workspace {
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

/** Allowlist workspace field for public endpoints. No operational policy or webhook capability. */
export interface PublicWorkspace {
  id: string;
  name: string;
  slug: string;
  owner_username: string;
  visibility?: 'private' | 'public';
  public_role?: 'viewer' | 'puller';
  archived?: boolean;
  created_at: string;
}

export type EnterpriseRole = 'member' | 'admin' | 'owner';

export interface Enterprise {
  id: string;
  namespace_id: string;
  name: string;
  slug: string;
  logo?: string;
  created_by: string;
  created_at: string;
}

export interface PublicEnterprise {
  id: string;
  name: string;
  slug: string;
  logo?: string;
  created_at: string;
  workspaces: PublicWorkspace[];
}

export interface EnterpriseMembership {
  enterprise_id: string;
  user_id: string;
  role: EnterpriseRole;
  user?: User;
  created_at: string;
}

export interface EnterprisePolicy {
  enterprise_id: string;
  workspace_creation: 'admins' | 'members';
  default_workspace_visibility: 'private' | 'public';
  allow_public_workspaces: boolean;
  break_glass_enabled: boolean;
  break_glass_max_minutes: number;
  updated_by?: string;
  updated_at: string;
}

export interface EnterpriseAuditEvent {
  id: string;
  enterprise_id: string;
  actor_id: string;
  action: string;
  target_type?: string;
  target_id?: string;
  reason?: string;
  created_at: string;
}

export interface BreakGlassGrant {
  id: string;
  enterprise_id: string;
  workspace_id: string;
  user_id: string;
  reason: string;
  created_at: string;
  expires_at: string;
}

/** User profile activity feed — monthly commit bundles + workspace creation */
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

/** Update workspace settings (owner only) */
export interface WorkspacePatch {
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
  workspace_id: string;
  email: string;
  role: string;
  status: string;
  created_at?: string;
  expires_at?: string;
}

export interface Repo {
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
