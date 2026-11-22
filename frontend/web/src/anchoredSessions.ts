// Pure helpers for the sessions tab: they turn the caller's own session
// reports into the lists the screen shows. Nothing here guesses: a list holds
// only what a report or the branch ledger states.
import type {
  AnchoredAgentCall, AnchoredCommit, AnchoredLinkRule, AnchoredNote, AnchoredNoteTarget, AnchoredPlace, AnchoredProvider, AnchoredRange,
  AnchoredSessionBody, AnchoredSessionSummary, BranchLifetime, RemoteBranch, RemoteCommit, RemotePull, ReportedWorktree,
} from './types';
import { commitsNotOn, type GitNode } from './anchoredGraph';

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

/** A segment that ends at a commit. `copyOf` is the commit the session made, when this row is a copy of it on the remote. */
export interface AnchoredCommitLink { provider: AnchoredProvider; session_id: string; segment_no: number; rule: AnchoredLinkRule; copyOf?: string }
export interface AnchoredCommitRow extends GitNode {
  subject: string;
  links: AnchoredCommitLink[];
  /** The remote holds this commit. False for a commit only a report names. */
  remote: boolean;
  author: string;
  /** Remote branches whose newest commit this is. */
  heads: string[];
  /** Pull requests that list this commit, and the one merged as it. */
  pulls: number[];
  merged?: number;
}
export interface AnchoredWorktreeRow {
  key: string; name: string; machine: string; removed: boolean; sessions: string[]; branches: string[];
  /** The branch checked out in it, as the machine's own list last said. */
  current?: string;
}
/** One use of a branch name: the name as it is now, or an earlier lifetime the ledger closed. */
export interface AnchoredBranchUse { lifetime?: BranchLifetime; sessions: string[] }
export interface AnchoredBranchRow {
  name: string; current: AnchoredBranchUse; earlier: AnchoredBranchUse[];
  /** The branch's newest commit on the remote, and the pull requests opened from it. */
  head?: string;
  pulls: RemotePull[];
}
/** What the server read from the hosting provider: the newest commits, every branch and pull request. */
export interface AnchoredRemote {
  /** False until the server has read the remote once: nothing can then be said about what the remote lacks. */
  read: boolean;
  defaultBranch: string; branches: RemoteBranch[]; pulls: RemotePull[]; commits: RemoteCommit[];
}
export interface AnchoredIndex {
  sessions: Map<string, AnchoredSessionSummary>;
  /** Sessions whose parent is not among the reports, newest first. */
  roots: AnchoredSessionSummary[];
  /** Spawned sessions by their parent's key, oldest first. */
  children: Map<string, AnchoredSessionSummary[]>;
  /** Every commit the remote lists or a segment ends at, newest first. */
  commits: AnchoredCommitRow[];
  commitBySha: Map<string, AnchoredCommitRow>;
  worktrees: AnchoredWorktreeRow[];
  branches: AnchoredBranchRow[];
  /** Pull requests: open ones first, then the highest number first. */
  pulls: RemotePull[];
  defaultBranch: string;
  /** The server has read the remote, so a commit it does not list is known to be missing there. */
  remoteRead: boolean;
  /** Sessions whose uncommitted part could not be placed. */
  unplaced: string[];
  /** What of the caller's own sessions nobody else reads yet. */
  held: AnchoredHeld;
  /** The archived sessions by key, the one archived last first. Nothing else in the index holds them. */
  archived: Map<string, AnchoredSessionSummary>;
}
/**
 * The caller's own work before a push. `commits` are the commits of the
 * caller's sessions that the server found neither on the remote nor copied
 * there, newest first; a commit the server could not say this about is left
 * out. `uncommitted` are the caller's sessions with a part after their last
 * commit, newest first.
 */
export interface AnchoredHeld { commits: string[]; uncommitted: string[] }

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

export function buildAnchoredIndex(
  listed: AnchoredSessionSummary[], lifetimes: BranchLifetime[] = [],
  extra: { worktrees?: ReportedWorktree[]; remote?: AnchoredRemote } = {},
): AnchoredIndex {
  const remote = extra.remote;
  // An archived session is left out of everything the tab lists; it is kept apart for the list of archived sessions.
  const summaries = listed.filter(summary => !summary.archived_at);
  const archived = new Map(listed.filter(summary => summary.archived_at)
    .sort((a, b) => newestFirst(a.archived_at, b.archived_at) || a.session_id.localeCompare(b.session_id))
    .map(summary => [anchoredKey(summary.provider, summary.session_id), summary]));
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
  const heldCommits: string[] = [];
  for (const commit of remote?.commits ?? []) {
    commitBySha.set(commit.sha, { sha: commit.sha, parents: commit.parents, at: commit.committed_at, subject: commit.subject, links: [], remote: true,
      author: commit.author_name ?? '', heads: [], pulls: [] });
  }
  for (const summary of summaries) {
    for (const commit of summary.commits) {
      const link = { provider: summary.provider, session_id: summary.session_id, segment_no: commit.segment_no, rule: commit.rule };
      // A commit that was rebased or cherry-picked is shown at its copies on the remote.
      const copies = commit.on_remote ? [] : (commit.copies ?? []).filter(copy => commitBySha.get(copy)?.remote);
      for (const copy of copies) commitBySha.get(copy)!.links.push({ ...link, copyOf: commit.sha });
      if (copies.length > 0) continue;
      const row = commitBySha.get(commit.sha) ?? { sha: commit.sha, parents: [], at: '', subject: '', links: [], remote: false, author: '', heads: [], pulls: [] };
      row.subject ||= commit.subject ?? '';
      row.at ||= commit.created_at ?? commit.authored_at ?? '';
      row.links.push(link);
      commitBySha.set(commit.sha, row);
      if (!summary.shared && commit.on_remote === false && !commit.copies?.length) pushUnique(heldCommits, commit.sha);
    }
  }
  for (const branch of remote?.branches ?? []) commitBySha.get(branch.sha)?.heads.push(branch.name);
  const pulls = [...(remote?.pulls ?? [])].sort((a, b) => Number(b.state === 'open') - Number(a.state === 'open') || b.number - a.number);
  for (const pull of pulls) {
    for (const sha of pull.commits) commitBySha.get(sha)?.pulls.push(pull.number);
    const merged = pull.merge_sha ? commitBySha.get(pull.merge_sha) : undefined;
    if (merged) merged.merged = pull.number;
  }
  const commits = [...commitBySha.values()].sort((a, b) => newestFirst(a.at, b.at) || a.sha.localeCompare(b.sha));

  const branchRows = new Map<string, AnchoredBranchRow>();
  const branchRow = (name: string) => {
    let row = branchRows.get(name);
    if (!row) branchRows.set(name, row = { name, current: { sessions: [] }, earlier: [], pulls: [] });
    return row;
  };
  for (const branch of remote?.branches ?? []) branchRow(branch.name).head = branch.sha;
  for (const pull of pulls) if (!pull.fork) branchRows.get(pull.head_ref)?.pulls.push(pull);
  for (const lifetime of [...lifetimes].sort((a, b) => newestFirst(a.opened_at, b.opened_at))) {
    const row = branchRow(lifetime.name);
    if (lifetime.closed_at) row.earlier.push({ lifetime, sessions: [] });
    else row.current.lifetime = lifetime;
  }

  const worktreeRows = new Map<string, AnchoredWorktreeRow>();
  // The machine's own list says which worktrees exist, also one no session worked in.
  const reported = new Set<string>();
  for (const worktree of extra.worktrees ?? []) {
    const machine = worktree.machine ?? '';
    const key = `${machine}\n${worktree.name}`;
    reported.add(key);
    worktreeRows.set(key, { key, name: worktree.name, machine, removed: worktree.removed, sessions: [], branches: [], ...(worktree.branch ? { current: worktree.branch } : {}) });
  }
  const unplaced: string[] = [];
  for (const summary of summaries) {
    const part = summary.uncommitted;
    if (!part) continue;
    const key = anchoredKey(summary.provider, summary.session_id);
    if (part.unplaced) unplaced.push(key);
    for (const place of part.places) {
      // Another member's worktrees are theirs alone: their places name only the branch.
      if (place.worktree && !summary.shared) {
        const machine = summary.machine ?? '';
        const worktreeKey = `${machine}\n${place.worktree}`;
        let row = worktreeRows.get(worktreeKey);
        if (!row) worktreeRows.set(worktreeKey, row = { key: worktreeKey, name: place.worktree, machine, removed: false, sessions: [], branches: [] });
        if (!reported.has(worktreeKey)) row.removed ||= Boolean(place.removed);
        pushUnique(row.sessions, key);
        if (place.branch) pushUnique(row.branches, place.branch);
      }
      if (place.branch) pushUnique(branchUse(branchRow(place.branch), part.ended_at ?? part.started_at).sessions, key);
    }
  }
  const worktrees = [...worktreeRows.values()].sort((a, b) => a.name.localeCompare(b.name) || a.machine.localeCompare(b.machine));
  const branches = [...branchRows.values()].sort((a, b) => a.name.localeCompare(b.name));
  heldCommits.sort((a, b) => newestFirst(commitBySha.get(a)?.at, commitBySha.get(b)?.at) || a.localeCompare(b));
  const heldParts = summaries.filter(summary => !summary.shared && summary.uncommitted)
    .sort((a, b) => newestFirst(a.uncommitted!.ended_at ?? a.ended_at, b.uncommitted!.ended_at ?? b.ended_at) || a.session_id.localeCompare(b.session_id))
    .map(summary => anchoredKey(summary.provider, summary.session_id));
  return { sessions, roots, children, commits, commitBySha, worktrees, branches, pulls, defaultBranch: remote?.defaultBranch ?? '', remoteRead: Boolean(remote?.read), unplaced,
    held: { commits: heldCommits, uncommitted: heldParts }, archived };
}

/**
 * The commits of a branch: the commit list of its pull request when it has
 * one, otherwise the commits its base branch does not have yet. Newest first.
 * `complete` is false when the list may be longer than what is known.
 */
export function anchoredBranchCommits(index: AnchoredIndex, name: string): { shas: string[]; complete: boolean; pull?: RemotePull } {
  const branch = index.branches.find(row => row.name === name);
  const pull = branch?.pulls.find(candidate => candidate.state === 'open') ?? branch?.pulls[0];
  if (pull) return { shas: [...pull.commits].reverse(), complete: !pull.truncated, pull };
  const base = index.branches.find(row => row.name === index.defaultBranch)?.head;
  if (!branch?.head || !base || name === index.defaultBranch) return { shas: [], complete: true };
  return commitsNotOn(index.commitBySha, branch.head, base);
}

/**
 * What lies between two commits, as `git log base..head` lists it: the commits
 * `head` descends from and `base` does not, newest first. Only a commit the
 * server read from the remote has its parents, so both must be such commits:
 * `comparable` is false otherwise. `complete` is false when the commits read
 * so far end before the range does.
 */
export function anchoredCompare(index: AnchoredIndex, base: string, head: string): { shas: string[]; complete: boolean; comparable: boolean } {
  if (!index.commitBySha.get(base)?.remote || !index.commitBySha.get(head)?.remote) return { shas: [], complete: false, comparable: false };
  return { ...commitsNotOn(index.commitBySha, head, base), comparable: true };
}

function sameTarget(a: AnchoredNoteTarget, b: AnchoredNoteTarget): boolean {
  if (a.kind === 'commit' && b.kind === 'commit') return a.commit === b.commit;
  if (a.kind === 'branch' && b.kind === 'branch') return a.branch === b.branch && (a.ledger_id ?? '') === (b.ledger_id ?? '');
  if (a.kind === 'worktree' && b.kind === 'worktree') return a.worktree === b.worktree && (a.machine ?? '') === (b.machine ?? '');
  return false;
}
/** The notes written about one target, oldest first as the server lists them. */
export function anchoredNotesFor(notes: AnchoredNote[], target: AnchoredNoteTarget): AnchoredNote[] {
  return notes.filter(note => sameTarget(note.target, target));
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
  /** Another member's session: the part is not open, and only its place is known. */
  hidden: boolean;
  /** Another member's session: the part is open up to here and goes on. */
  partial: boolean;
}
export function anchoredParts(body: AnchoredSessionBody): AnchoredPart[] {
  const parts: AnchoredPart[] = body.segments.map((segment, index) => ({
    kind: 'segment', no: index + 1, range: segment, firstEvent: segment.first_event ?? 0, events: segment.events ?? 0,
    commits: segment.commits, places: [], unplaced: false, hidden: Boolean(segment.hidden), partial: false,
  }));
  if (body.uncommitted) {
    const part = body.uncommitted;
    parts.push({ kind: 'uncommitted', no: 0, range: part, firstEvent: part.first_event ?? 0, events: part.events ?? 0,
      commits: [], places: part.places, unplaced: part.unplaced, hidden: false, partial: Boolean(part.partial) });
  }
  return parts;
}
/** Index of the part a link names: a segment by its number, or the part after the last commit. It is the first part when the session has no such part. */
export function anchoredPartIndex(parts: AnchoredPart[], segment?: number, rest?: boolean): number {
  const found = segment ? parts.findIndex(part => part.kind === 'segment' && part.no === segment)
    : rest ? parts.findIndex(part => part.kind === 'uncommitted') : -1;
  return found < 0 ? 0 : found;
}
/** Where reading the conversation starts for the parts from `first` on: the first open part's first event, or undefined when none is open. */
export function anchoredStart(parts: AnchoredPart[], first: number): number | undefined {
  return parts.slice(first).find(part => !part.hidden)?.firstEvent;
}
/** The first events of the open parts that hold events: where reading goes on after a part that is not open. */
export function anchoredOpenStarts(parts: AnchoredPart[]): number[] {
  return parts.filter(part => !part.hidden && part.events > 0).map(part => part.firstEvent).sort((a, b) => a - b);
}

export type AnchoredStreamItem<Event> = { kind: 'divider'; part: AnchoredPart } | { kind: 'events'; part: AnchoredPart; events: Event[] };
/** Events read in one request, from `offset` on, in order. */
export interface AnchoredEventRun<Event> { offset: number; events: Event[] }
/**
 * Lays out the events read so far with a divider where each part begins.
 * `runs` hold the conversation's events from the first shown part onward, in
 * order: each run starts where the one before ended, or at a later open part
 * when the parts between are not open. A part is announced once reading has
 * reached it, so a part with no event of its own still gets its divider, and
 * so does a part that is not open, which holds none.
 */
export function anchoredStream<Event>(runs: AnchoredEventRun<Event>[], parts: AnchoredPart[], first: number): AnchoredStreamItem<Event>[] {
  const start = anchoredStart(parts, first) ?? Infinity;
  const read = new Map<number, Event>();
  for (const run of runs) run.events.forEach((event, position) => read.set(run.offset + position, event));
  const last = runs.at(-1);
  const reached = last ? last.offset + last.events.length : start;
  const items: AnchoredStreamItem<Event>[] = [];
  let cursor = start;
  for (const part of parts.slice(first)) {
    if (part.hidden) {
      if (cursor > reached) break;
      items.push({ kind: 'divider', part });
      continue;
    }
    if (part.firstEvent > reached) break;
    items.push({ kind: 'divider', part });
    const own: Event[] = [];
    for (let index = part.firstEvent; index < part.firstEvent + part.events && read.has(index); index++) own.push(read.get(index) as Event);
    if (own.length) items.push({ kind: 'events', part, events: own });
    cursor = part.firstEvent + part.events;
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
