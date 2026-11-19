// Pure helpers for the sessions tab: they turn the caller's own session
// reports into the lists the screen shows. Nothing here guesses: a list holds
// only what a report or the branch ledger states.
import type {
  AnchoredAgentCall, AnchoredCommit, AnchoredLinkRule, AnchoredPlace, AnchoredProvider, AnchoredRange,
  AnchoredSessionBody, AnchoredSessionSummary, BranchLifetime,
} from './types';

export const ANCHORED_PROVIDERS: Record<AnchoredProvider, { name: string; ink: string }> = {
  claude: { name: 'Claude', ink: '#D97757' },
  codex: { name: 'Codex', ink: '#5E68F0' },
};

/** Identifies a session across tools: the same identifier under another tool is another session. */
export function anchoredKey(provider: string, sessionId: string): string {
  return `${provider}:${sessionId}`;
}
export function anchoredShortId(sessionId: string): string {
  return sessionId.length > 12 ? sessionId.slice(0, 8) : sessionId;
}
export function anchoredShortSha(sha: string): string {
  return sha.slice(0, 10);
}

export interface AnchoredCommitLink { provider: AnchoredProvider; session_id: string; segment_no: number; rule: AnchoredLinkRule }
export interface AnchoredCommitRow { sha: string; subject: string; at: string; links: AnchoredCommitLink[] }
export interface AnchoredWorktreeRow { key: string; name: string; machine: string; removed: boolean; sessions: string[]; branches: string[] }
/** One use of a branch name: the name as it is now, or an earlier lifetime the ledger closed. */
export interface AnchoredBranchUse { lifetime?: BranchLifetime; sessions: string[] }
export interface AnchoredBranchRow { name: string; current: AnchoredBranchUse; earlier: AnchoredBranchUse[] }
export interface AnchoredIndex {
  sessions: Map<string, AnchoredSessionSummary>;
  /** Sessions whose parent is not among the reports, newest first. */
  roots: AnchoredSessionSummary[];
  /** Spawned sessions by their parent's key, oldest first. */
  children: Map<string, AnchoredSessionSummary[]>;
  /** Every commit a segment ends at, newest first. */
  commits: AnchoredCommitRow[];
  commitBySha: Map<string, AnchoredCommitRow>;
  worktrees: AnchoredWorktreeRow[];
  branches: AnchoredBranchRow[];
  /** Sessions whose uncommitted part could not be placed. */
  unplaced: string[];
}

function time(value?: string): number {
  const parsed = value ? Date.parse(value) : NaN;
  return Number.isFinite(parsed) ? parsed : NaN;
}
/** Newest first; an unknown time sorts last. */
function newestFirst(a?: string, b?: string): number {
  const left = time(a), right = time(b);
  if (Number.isNaN(left) || Number.isNaN(right)) return Number(Number.isNaN(left)) - Number(Number.isNaN(right));
  return right - left;
}
function oldestFirst(a?: string, b?: string): number {
  const left = time(a), right = time(b);
  if (Number.isNaN(left) || Number.isNaN(right)) return Number(Number.isNaN(left)) - Number(Number.isNaN(right));
  return left - right;
}
function pushUnique(list: string[], value: string): void {
  if (!list.includes(value)) list.push(value);
}

/** The lifetime an uncommitted part belongs to: a closed one that held the name at that time, otherwise the name as it is now. */
function branchUse(row: AnchoredBranchRow, at?: string): AnchoredBranchUse {
  const moment = time(at);
  if (Number.isNaN(moment)) return row.current;
  return row.earlier.find(use => {
    const lifetime = use.lifetime!;
    return moment >= time(lifetime.opened_at) && moment <= time(lifetime.closed_at);
  }) ?? row.current;
}

export function buildAnchoredIndex(summaries: AnchoredSessionSummary[], lifetimes: BranchLifetime[] = []): AnchoredIndex {
  const sessions = new Map(summaries.map(summary => [anchoredKey(summary.provider, summary.session_id), summary]));
  const children = new Map<string, AnchoredSessionSummary[]>();
  const roots: AnchoredSessionSummary[] = [];
  for (const summary of summaries) {
    const parent = summary.parent ? anchoredKey(summary.provider, summary.parent) : '';
    if (parent && sessions.has(parent)) children.set(parent, [...(children.get(parent) ?? []), summary]);
    else roots.push(summary);
  }
  roots.sort((a, b) => newestFirst(a.started_at, b.started_at) || a.session_id.localeCompare(b.session_id));
  for (const list of children.values()) list.sort((a, b) => oldestFirst(a.started_at, b.started_at) || a.session_id.localeCompare(b.session_id));

  const commitBySha = new Map<string, AnchoredCommitRow>();
  for (const summary of summaries) {
    for (const commit of summary.commits) {
      const row = commitBySha.get(commit.sha) ?? { sha: commit.sha, subject: '', at: '', links: [] };
      row.subject ||= commit.subject ?? '';
      row.at ||= commit.created_at ?? commit.authored_at ?? '';
      row.links.push({ provider: summary.provider, session_id: summary.session_id, segment_no: commit.segment_no, rule: commit.rule });
      commitBySha.set(commit.sha, row);
    }
  }
  const commits = [...commitBySha.values()].sort((a, b) => newestFirst(a.at, b.at) || a.sha.localeCompare(b.sha));

  const branchRows = new Map<string, AnchoredBranchRow>();
  const branchRow = (name: string) => {
    let row = branchRows.get(name);
    if (!row) branchRows.set(name, row = { name, current: { sessions: [] }, earlier: [] });
    return row;
  };
  for (const lifetime of [...lifetimes].sort((a, b) => newestFirst(a.opened_at, b.opened_at))) {
    const row = branchRow(lifetime.name);
    if (lifetime.closed_at) row.earlier.push({ lifetime, sessions: [] });
    else row.current.lifetime = lifetime;
  }

  const worktreeRows = new Map<string, AnchoredWorktreeRow>();
  const unplaced: string[] = [];
  for (const summary of summaries) {
    const part = summary.uncommitted;
    if (!part) continue;
    const key = anchoredKey(summary.provider, summary.session_id);
    if (part.unplaced) unplaced.push(key);
    for (const place of part.places) {
      const machine = summary.machine ?? '';
      const worktreeKey = `${machine}\n${place.worktree}`;
      let row = worktreeRows.get(worktreeKey);
      if (!row) worktreeRows.set(worktreeKey, row = { key: worktreeKey, name: place.worktree, machine, removed: false, sessions: [], branches: [] });
      row.removed ||= Boolean(place.removed);
      pushUnique(row.sessions, key);
      if (place.branch) {
        pushUnique(row.branches, place.branch);
        pushUnique(branchUse(branchRow(place.branch), part.ended_at ?? part.started_at).sessions, key);
      }
    }
  }
  const worktrees = [...worktreeRows.values()].sort((a, b) => a.name.localeCompare(b.name) || a.machine.localeCompare(b.machine));
  const branches = [...branchRows.values()].sort((a, b) => a.name.localeCompare(b.name));
  return { sessions, roots, children, commits, commitBySha, worktrees, branches, unplaced };
}

/** The other commits that share a segment with this one: one command can make several commits. */
export function anchoredSegmentCommits(summary: AnchoredSessionSummary | undefined, segmentNo: number): string[] {
  return (summary?.commits ?? []).filter(commit => commit.segment_no === segmentNo).map(commit => commit.sha);
}

/** A session's parts in order: each segment, then the part after the last commit. */
export interface AnchoredPart {
  kind: 'segment' | 'uncommitted';
  /** 1-based segment number; 0 for the uncommitted part. */
  no: number;
  range: AnchoredRange;
  firstEvent: number;
  events: number;
  commits: AnchoredCommit[];
  places: AnchoredPlace[];
  unplaced: boolean;
}
export function anchoredParts(body: AnchoredSessionBody): AnchoredPart[] {
  const parts: AnchoredPart[] = body.segments.map((segment, index) => ({
    kind: 'segment', no: index + 1, range: segment, firstEvent: segment.first_event ?? 0, events: segment.events ?? 0,
    commits: segment.commits, places: [], unplaced: false,
  }));
  if (body.uncommitted) {
    const part = body.uncommitted;
    parts.push({ kind: 'uncommitted', no: 0, range: part, firstEvent: part.first_event ?? 0, events: part.events ?? 0,
      commits: [], places: part.places, unplaced: part.unplaced });
  }
  return parts;
}
/** Index of the part a link names: a segment by its number, or the part after the last commit. It is the first part when the session has no such part. */
export function anchoredPartIndex(parts: AnchoredPart[], segment?: number, rest?: boolean): number {
  const found = segment ? parts.findIndex(part => part.kind === 'segment' && part.no === segment)
    : rest ? parts.findIndex(part => part.kind === 'uncommitted') : -1;
  return found < 0 ? 0 : found;
}

export type AnchoredStreamItem<Event> = { kind: 'divider'; part: AnchoredPart } | { kind: 'events'; part: AnchoredPart; events: Event[] };
/**
 * Lays out the events read so far with a divider where each part begins.
 * `events` holds the document's events from the first shown part onward, in
 * order and without gaps. A part is announced once reading has reached it, so
 * a part with no event of its own still gets its divider.
 */
export function anchoredStream<Event>(events: Event[], parts: AnchoredPart[], first: number): AnchoredStreamItem<Event>[] {
  const start = parts[first]?.firstEvent ?? 0;
  const items: AnchoredStreamItem<Event>[] = [];
  for (const part of parts.slice(first)) {
    if (part.firstEvent - start > events.length) break;
    items.push({ kind: 'divider', part });
    const own = events.slice(part.firstEvent - start, part.firstEvent - start + part.events);
    if (own.length) items.push({ kind: 'events', part, events: own });
  }
  return items;
}

/** Another agent this session exchanged recorded calls with. */
export interface AnchoredPartner { id: string; sent: number; received: number; calls: AnchoredAgentCall[] }
/** Groups recorded agent calls by the other party. A call is never attributed to a party it does not name. */
export function anchoredPartners(sessionId: string, calls: AnchoredAgentCall[]): AnchoredPartner[] {
  const partners = new Map<string, AnchoredPartner>();
  const partner = (id: string) => {
    let entry = partners.get(id);
    if (!entry) partners.set(id, entry = { id, sent: 0, received: 0, calls: [] });
    return entry;
  };
  for (const call of [...calls].sort((a, b) => oldestFirst(a.at, b.at))) {
    if (call.from === sessionId) {
      for (const receiver of call.to) {
        if (receiver === sessionId) continue;
        const entry = partner(receiver);
        entry.sent += 1;
        entry.calls.push(call);
      }
    } else if (call.to.includes(sessionId)) {
      const entry = partner(call.from);
      entry.received += 1;
      entry.calls.push(call);
    }
  }
  return [...partners.values()];
}
