import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  anchoredBranchCommits, anchoredCompare, anchoredKey, anchoredNotesFor, anchoredOpenStarts, anchoredPartIndex, anchoredPartners, anchoredParts, anchoredSegmentCommits, anchoredStart,
  anchoredStream, buildAnchoredIndex,
  type AnchoredRemote,
} from '../src/anchoredSessions';
import type { AnchoredNote, AnchoredSessionBody, AnchoredSessionSummary, BranchLifetime, RemoteCommit } from '../src/types';

const sha = (letter: string) => letter.repeat(40);
const at = (minute: number) => `2031-01-02T03:${String(minute).padStart(2, '0')}:00Z`;
const summary = (id: string, extra: Partial<AnchoredSessionSummary> = {}): AnchoredSessionSummary => ({
  provider: 'claude', session_id: id, sub: false, revision: 1, body_hash: `sha256:${'0'.repeat(64)}`, records: 4, commands: 2, segments: 0,
  content: true, commits: [], updated_at: at(59), ...extra,
});
const lifetime = (id: string, name: string, opened: number, closed?: number): BranchLifetime => ({
  repo_id: `sha256:${'1'.repeat(64)}`, ledger_id: id.repeat(32), name, opened_at: at(opened), opened_by: { kind: 'registered', account: 'member' },
  ...(closed === undefined ? {} : { closed_at: at(closed), closed_by: { kind: 'hosting-push' as const } }), linked: [],
});

test('sessions are listed under the parent that is among the reports', () => {
  const index = buildAnchoredIndex([
    summary('older', { started_at: at(1) }),
    summary('parent', { started_at: at(5), segments: 1 }),
    summary('late-child', { sub: true, parent: 'parent', started_at: at(9) }),
    summary('early-child', { sub: true, parent: 'parent', started_at: at(6) }),
    summary('orphan', { sub: true, parent: 'not-uploaded', started_at: at(7) }),
    summary('parent', { provider: 'codex', started_at: at(2) }),
  ]);
  assert.deepEqual(index.roots.map(root => anchoredKey(root.provider, root.session_id)), ['claude:orphan', 'claude:parent', 'codex:parent', 'claude:older']);
  assert.deepEqual(index.children.get('claude:parent')?.map(child => child.session_id), ['early-child', 'late-child']);
  assert.equal(index.children.has('codex:parent'), false, 'the same identifier under another tool is another session');
  assert.equal(index.sessions.size, 6);
});

test('a commit lists every segment that ends at it, newest commit first', () => {
  const index = buildAnchoredIndex([
    summary('one', { segments: 2, commits: [
      { sha: sha('a'), segment_no: 1, rule: 'printed-number', subject: 'first', created_at: at(10) },
      { sha: sha('b'), segment_no: 2, rule: 'same-folder-and-time', created_at: at(20) },
      { sha: sha('c'), segment_no: 2, rule: 'same-folder-and-time' },
    ] }),
    summary('two', { segments: 1, commits: [{ sha: sha('a'), segment_no: 1, rule: 'printed-number', subject: 'ignored when the first one named it' }] }),
  ]);
  assert.deepEqual(index.commits.map(commit => commit.sha), [sha('b'), sha('a'), sha('c')], 'a commit without a time sorts last');
  const shared = index.commitBySha.get(sha('a'))!;
  assert.equal(shared.subject, 'first');
  assert.deepEqual(shared.links.map(link => `${link.session_id}#${link.segment_no}`), ['one#1', 'two#1']);
  assert.deepEqual(anchoredSegmentCommits(index.sessions.get('claude:one'), 2), [sha('b'), sha('c')], 'one command can make several commits');
  assert.deepEqual(anchoredSegmentCommits(undefined, 1), []);
});

test('the caller\'s work before a push is the commits the remote lacks and the parts not committed yet', () => {
  const part = (minute: number) => ({ commands: 1, ended_at: at(minute), unplaced: false, places: [] });
  const index = buildAnchoredIndex([
    summary('mine', { segments: 3, uncommitted: part(40), commits: [
      { sha: sha('a'), segment_no: 1, rule: 'printed-number', created_at: at(10), on_remote: true },
      { sha: sha('b'), segment_no: 2, rule: 'printed-number', created_at: at(20), on_remote: false },
      { sha: sha('c'), segment_no: 3, rule: 'printed-number', created_at: at(30), on_remote: false },
      { sha: sha('d'), segment_no: 3, rule: 'printed-number', created_at: at(31), on_remote: false, copies: [sha('e')] },
      { sha: sha('f'), segment_no: 3, rule: 'printed-number', created_at: at(32) },
    ] }),
    summary('idle', { started_at: at(1) }),
    summary('talk', { uncommitted: part(50) }),
    summary('theirs', { shared: true, uploader: 'bob', segments: 1, uncommitted: part(55),
      commits: [{ sha: sha('9'), segment_no: 1, rule: 'printed-number', created_at: at(56), on_remote: false }] }),
  ]);
  assert.deepEqual(index.held.commits, [sha('c'), sha('b')], 'newest first; not one on the remote, copied there, or not yet checked');
  assert.deepEqual(index.held.uncommitted, ['claude:talk', 'claude:mine'], 'newest first; never another member\'s');
});

test('an archived session is held apart from everything the tab lists', () => {
  const part = (minute: number) => ({ commands: 1, ended_at: at(minute), unplaced: false, places: [{ worktree: 'app', branch: 'feature', rule: 'recorded-by-tool' as const }] });
  const index = buildAnchoredIndex([
    summary('kept', { segments: 1, commits: [{ sha: sha('a'), segment_no: 1, rule: 'printed-number', created_at: at(10), on_remote: false }] }),
    summary('old', { archived_at: at(50), segments: 1, uncommitted: part(40),
      commits: [{ sha: sha('a'), segment_no: 1, rule: 'printed-number', created_at: at(10), on_remote: false }, { sha: sha('b'), segment_no: 1, rule: 'printed-number', created_at: at(20), on_remote: false }] }),
    summary('older', { archived_at: at(45) }),
    summary('child', { parent: 'old', sub: true }),
  ]);
  assert.deepEqual([...index.archived.keys()], ['claude:old', 'claude:older'], 'the one archived last comes first');
  assert.deepEqual([...index.sessions.keys()], ['claude:kept', 'claude:child']);
  assert.deepEqual(index.roots.map(root => root.session_id).sort(), ['child', 'kept'], 'a session whose parent is archived is listed on its own');
  assert.deepEqual(index.commits.map(commit => commit.sha), [sha('a')], 'a commit only an archived session made is not listed');
  assert.deepEqual(index.commitBySha.get(sha('a'))!.links.map(link => link.session_id), ['kept']);
  assert.deepEqual(index.held, { commits: [sha('a')], uncommitted: [] }, 'what is archived does not wait for a push');
  assert.deepEqual(index.worktrees, [], 'and makes no worktree row');
  assert.equal(buildAnchoredIndex([summary('one')]).archived.size, 0);
});

test('uncommitted parts are listed by worktree and by the use of a branch name they fall in', () => {
  const index = buildAnchoredIndex([
    summary('now', { machine: 'laptop', uncommitted: { commands: 1, started_at: at(40), ended_at: at(45), unplaced: false,
      places: [{ worktree: 'app', branch: 'feature', rule: 'recorded-by-tool' }, { worktree: 'app-fix', rule: 'head-log' }] } }),
    summary('before', { machine: 'laptop', uncommitted: { commands: 1, started_at: at(12), ended_at: at(15), unplaced: false,
      places: [{ worktree: 'app', branch: 'feature', rule: 'head-log' }] } }),
    summary('elsewhere', { machine: 'desktop', uncommitted: { commands: 0, unplaced: false,
      places: [{ worktree: 'app', branch: 'local-only', removed: true, rule: 'folder-no-longer-a-worktree' }] } }),
    summary('lost', { uncommitted: { commands: 3, unplaced: true, places: [] } }),
  ], [lifetime('a', 'feature', 10, 20), lifetime('b', 'feature', 30), lifetime('c', 'remote-only', 5)]);

  assert.deepEqual(index.worktrees.map(row => [row.name, row.machine, row.removed, row.sessions, row.branches]), [
    ['app', 'desktop', true, ['claude:elsewhere'], ['local-only']],
    ['app', 'laptop', false, ['claude:now', 'claude:before'], ['feature']],
    ['app-fix', 'laptop', false, ['claude:now'], []],
  ], 'the same folder name on another machine is another worktree');
  assert.deepEqual(index.unplaced, ['claude:lost']);

  assert.deepEqual(index.branches.map(row => row.name), ['feature', 'local-only', 'remote-only']);
  const feature = index.branches[0];
  assert.equal(feature.current.lifetime?.ledger_id, 'b'.repeat(32));
  assert.deepEqual(feature.current.sessions, ['claude:now']);
  assert.deepEqual(feature.earlier.map(use => [use.lifetime?.ledger_id, use.sessions]), [['a'.repeat(32), ['claude:before']]]);
  const local = index.branches[1];
  assert.equal(local.current.lifetime, undefined, 'a name the remote never held has no lifetime');
  assert.deepEqual(local.current.sessions, ['claude:elsewhere'], 'a part without a time stays with the name as it is now');
  assert.deepEqual(index.branches[2].current.sessions, []);
});

const body: AnchoredSessionBody = {
  rule_version: 1, sub: false, scope: 'working-folder', records: 6, commands: 3, copied_records: 0,
  segments: [
    { first: 'm1', last: 'm2', first_ordinal: 0, last_ordinal: 1, commands: 1, events: 6, closes_turn: false, commits: [{ sha: sha('a'), rule: 'printed-number' }] },
    { first: 'm3', last: 'm4', first_ordinal: 2, last_ordinal: 3, commands: 1, first_event: 6, closes_turn: true, commits: [{ sha: sha('b'), rule: 'printed-number' }] },
  ],
  uncommitted: { first: 'm5', last: 'm6', first_ordinal: 4, last_ordinal: 5, commands: 1, first_event: 6, events: 4, unplaced: false,
    places: [{ worktree: 'app', branch: 'main', rule: 'recorded-by-tool' }] },
  content: { doc: `sha256:${'2'.repeat(64)}`, events: 10 },
};

test('parts default the range fields the wire leaves out when they are zero', () => {
  const parts = anchoredParts(body);
  assert.deepEqual(parts.map(part => [part.kind, part.no, part.firstEvent, part.events]), [['segment', 1, 0, 6], ['segment', 2, 6, 0], ['uncommitted', 0, 6, 4]]);
  assert.equal(anchoredPartIndex(parts, 2), 1);
  assert.equal(anchoredPartIndex(parts, undefined, true), 2);
  assert.equal(anchoredPartIndex(parts, 9), 0, 'a segment the session does not have opens the beginning');
  assert.equal(anchoredPartIndex(parts), 0);
  assert.deepEqual(anchoredParts({ ...body, segments: [], uncommitted: undefined }), []);
});

test('the conversation gets a divider where each part begins, also for a part with no event', () => {
  const parts = anchoredParts(body);
  const events = (from: number, count: number) => Array.from({ length: count }, (_, offset) => from + offset);
  const shape = (loaded: number[], first: number) => anchoredStream(loaded.length ? [{ offset: loaded[0], events: loaded }] : [], parts, first)
    .map(item => (item.kind === 'divider' ? `|${item.part.kind}${item.part.no}` : item.events.join(',')));

  assert.deepEqual(shape([], 0), ['|segment1'], 'before anything is read only the first part is announced');
  assert.deepEqual(shape(events(0, 4), 0), ['|segment1', '0,1,2,3']);
  assert.deepEqual(shape(events(0, 6), 0), ['|segment1', '0,1,2,3,4,5', '|segment2', '|uncommitted0'], 'reading reached the next parts');
  assert.deepEqual(shape(events(0, 10), 0), ['|segment1', '0,1,2,3,4,5', '|segment2', '|uncommitted0', '6,7,8,9']);
  assert.deepEqual(shape(events(6, 2), 1), ['|segment2', '|uncommitted0', '6,7'], 'opened at the second segment');
  assert.deepEqual(shape(events(6, 4), 2), ['|uncommitted0', '6,7,8,9'], 'opened at the part after the last commit');
});

// Another member reads the first segment and the beginning of the uncommitted part; the second segment is not open.
const shared: AnchoredSessionBody = {
  ...body,
  segments: [
    body.segments[0],
    { first: '', last: '', first_ordinal: 0, last_ordinal: 0, commands: 0, closes_turn: false, commits: [], hidden: true },
    { first: 'm7', last: 'm8', first_ordinal: 6, last_ordinal: 7, commands: 1, first_event: 6, events: 2, closes_turn: false, commits: [{ sha: sha('c'), rule: 'printed-number' }] },
  ],
  uncommitted: { first_ordinal: 8, last_ordinal: 8, commands: 0, first_event: 12, events: 1, unplaced: false, partial: true, first: '', last: '',
    places: [{ worktree: '', branch: 'main', rule: 'recorded-by-tool' }] },
  content: { events: 16 },
};

test('a part that is not open keeps its place and holds no events', () => {
  const parts = anchoredParts(shared);
  assert.deepEqual(parts.map(part => [part.kind, part.no, part.hidden, part.partial]),
    [['segment', 1, false, false], ['segment', 2, true, false], ['segment', 3, false, false], ['uncommitted', 0, false, true]]);
  assert.equal(anchoredStart(parts, 0), 0);
  assert.equal(anchoredStart(parts, 1), 6, 'a link to a part that is not open starts at the next open one');
  assert.equal(anchoredStart(parts.slice(1, 2), 0), undefined);
  assert.deepEqual(anchoredOpenStarts(parts), [0, 6, 12]);
});

test('reading goes on past a part that is not open', () => {
  const parts = anchoredParts(shared);
  const run = (offset: number, count: number) => ({ offset, events: Array.from({ length: count }, (_, position) => offset + position) });
  const shape = (runs: { offset: number; events: number[] }[], first: number) => anchoredStream(runs, parts, first)
    .map(item => (item.kind === 'divider' ? `|${item.part.kind}${item.part.no}` : item.events.join(',')));
  assert.deepEqual(shape([run(0, 6)], 0), ['|segment1', '0,1,2,3,4,5', '|segment2', '|segment3'], 'the closed part is announced once what is before it is read');
  assert.deepEqual(shape([run(0, 6), run(6, 2)], 0), ['|segment1', '0,1,2,3,4,5', '|segment2', '|segment3', '6,7']);
  assert.deepEqual(shape([run(0, 6), run(6, 2), run(12, 1)], 0), ['|segment1', '0,1,2,3,4,5', '|segment2', '|segment3', '6,7', '|uncommitted0', '12']);
  assert.deepEqual(shape([], 1), ['|segment2', '|segment3'], 'opened at the closed part');
});

test('another member\'s places name no worktree and make no worktree row', () => {
  const index = buildAnchoredIndex([{
    provider: 'claude', session_id: 'theirs', sub: false, revision: 1, body_hash: '', records: 2, commands: 1, segments: 0, content: true, commits: [],
    uncommitted: { commands: 1, places: [{ worktree: '', branch: 'main', rule: 'recorded-by-tool' }], unplaced: false }, updated_at: at(1),
    uploader: 'u_other', shared: true,
  }], []);
  assert.deepEqual(index.worktrees, []);
  assert.deepEqual(index.branches.find(branch => branch.name === 'main')?.current.sessions, [anchoredKey('claude', 'theirs')]);
});

test('agent calls are grouped by the other party they name', () => {
  const partners = anchoredPartners('lead', [
    { tool: 'send_input', from: 'lead', to: ['worker-b'], at: at(30) },
    { tool: 'spawn_agent', from: 'lead', to: ['worker-a', 'worker-b'], status: 'completed', at: at(10) },
    { tool: 'send_input', from: 'worker-a', to: ['lead'], at: at(20) },
    { tool: 'send_input', from: 'worker-a', to: ['worker-b'], at: at(25) },
    { tool: 'wait', from: 'lead', to: [] },
  ]);
  assert.deepEqual(partners.map(partner => [partner.id, partner.sent, partner.received, partner.calls.map(call => call.tool)]), [
    ['worker-a', 1, 1, ['spawn_agent', 'send_input']],
    ['worker-b', 2, 0, ['spawn_agent', 'send_input']],
  ], 'a call between two other agents belongs to neither list');
});

// main: a - b - m (merge commit of pull request 7: f1, f2); topic: b - t1 (no pull request); f3 was made locally and never pushed.
const remoteCommit = (letter: string, minute: number, ...parents: string[]): RemoteCommit =>
  ({ sha: sha(letter), parents: parents.map(sha), author_name: 'Dev', authored_at: at(minute), committed_at: at(minute), subject: `commit ${letter}` });
const remote: AnchoredRemote = {
  read: true, defaultBranch: 'main',
  branches: [{ name: 'main', sha: sha('e') }, { name: 'topic', sha: sha('9') }, { name: 'feature', sha: sha('2') }],
  pulls: [
    { number: 7, title: 'Add the feature', state: 'merged', head_ref: 'feature', head_sha: sha('2'), base_ref: 'main', merge_sha: sha('e'), commits: [sha('1'), sha('2')], updated_at: at(30) },
    { number: 9, title: 'From a fork', state: 'open', head_ref: 'feature', head_sha: sha('8'), base_ref: 'main', fork: true, commits: [sha('8')], updated_at: at(40) },
  ],
  commits: [remoteCommit('a', 1), remoteCommit('b', 2, 'a'), remoteCommit('1', 3, 'b'), remoteCommit('2', 4, '1'), remoteCommit('e', 5, 'b', '2'), remoteCommit('9', 6, 'b')],
};

test('two commits are compared by what one descends from and the other does not', () => {
  const index = buildAnchoredIndex([summary('one', { segments: 2, commits: [
    { sha: sha('1'), segment_no: 1, rule: 'printed-number', on_remote: true },
    { sha: sha('f'), segment_no: 2, rule: 'printed-number', on_remote: false },
  ] })], [], { remote });
  assert.deepEqual(anchoredCompare(index, sha('a'), sha('e')), { shas: [sha('e'), sha('2'), sha('1'), sha('b')], complete: true, comparable: true }, 'newest first, through a merge');
  assert.deepEqual(anchoredCompare(index, sha('b'), sha('9')), { shas: [sha('9')], complete: true, comparable: true });
  assert.deepEqual(anchoredCompare(index, sha('9'), sha('e')).shas, [sha('e'), sha('2'), sha('1')], 'two branches: what the one has and the other does not');
  assert.deepEqual(anchoredCompare(index, sha('e'), sha('a')), { shas: [], complete: true, comparable: true }, 'nothing lies before the base');
  assert.deepEqual(anchoredCompare(index, sha('a'), sha('f')), { shas: [], complete: false, comparable: false }, 'a commit that is not on the remote has no parents to follow');
  assert.deepEqual(anchoredCompare(index, sha('7'), sha('e')).comparable, false, 'nor has a commit that was not read');
  // The commits read so far end before the range does.
  const window = buildAnchoredIndex([], [], { remote: { ...remote, commits: remote.commits.filter(commit => commit.sha !== sha('1')) } });
  assert.deepEqual(anchoredCompare(window, sha('a'), sha('e')), { shas: [sha('e'), sha('2'), sha('b')], complete: false, comparable: true });
});

test('the commit list holds what the remote lists and what only a report names', () => {
  const index = buildAnchoredIndex([
    summary('one', { segments: 3, commits: [
      { sha: sha('1'), segment_no: 1, rule: 'printed-number', on_remote: true },
      // Rebased before it was pushed: the remote holds a copy under another number.
      { sha: sha('c'), segment_no: 2, rule: 'printed-number', copies: [sha('2')] },
      // Never pushed.
      { sha: sha('3'), segment_no: 3, rule: 'printed-number', subject: 'local work', created_at: at(50) },
    ] }),
  ], [], { remote });
  assert.deepEqual(index.commits.map(commit => commit.sha[0]), ['3', '9', 'e', '2', '1', 'b', 'a']);
  const row = (letter: string) => index.commitBySha.get(sha(letter))!;
  assert.equal(index.commitBySha.has(sha('c')), false, 'a commit shown at its copy is not listed a second time');
  assert.deepEqual(row('2').links, [{ provider: 'claude', session_id: 'one', segment_no: 2, rule: 'printed-number', copyOf: sha('c') }]);
  assert.deepEqual([row('3').remote, row('3').parents, row('3').subject], [false, [], 'local work']);
  assert.deepEqual([row('1').remote, row('1').links.length, row('b').links.length], [true, 1, 0], 'a remote commit no segment ends at has no link');
  assert.deepEqual([row('e').heads, row('e').merged, row('2').heads, row('2').pulls, row('9').heads], [['main'], 7, ['feature'], [7], ['topic']]);
  assert.deepEqual(index.pulls.map(pull => pull.number), [9, 7], 'open pull requests first');
  assert.equal(index.defaultBranch, 'main');
  assert.equal(index.remoteRead, true);
  assert.equal(buildAnchoredIndex([], [], { remote: { ...remote, read: false } }).remoteRead, false, 'before the first read nothing is known to be missing');
  assert.equal(buildAnchoredIndex([]).remoteRead, false);
});

test('a branch shows its pull request\'s commits, or the commits its base does not have', () => {
  const index = buildAnchoredIndex([], [], { remote });
  const feature = anchoredBranchCommits(index, 'feature');
  assert.deepEqual([feature.shas, feature.complete, feature.pull?.number], [[sha('2'), sha('1')], true, 7], 'a pull request from a fork is not the branch\'s own');
  assert.deepEqual(anchoredBranchCommits(index, 'topic'), { shas: [sha('9')], complete: true });
  assert.deepEqual(anchoredBranchCommits(index, 'main'), { shas: [], complete: true }, 'the base branch has nothing of its own');
  assert.deepEqual(anchoredBranchCommits(index, 'never-pushed'), { shas: [], complete: true });
  assert.deepEqual(index.branches.map(branch => [branch.name, branch.head?.[0], branch.pulls.map(pull => pull.number)]), [['feature', '2', [7]], ['main', 'e', []], ['topic', '9', []]]);
  const truncated = buildAnchoredIndex([], [], { remote: { ...remote, pulls: [{ ...remote.pulls[0], truncated: true }] } });
  assert.equal(anchoredBranchCommits(truncated, 'feature').complete, false);
});

test('a machine\'s own list says which worktrees exist, also one no session worked in', () => {
  const index = buildAnchoredIndex([
    summary('worked', { machine: 'laptop', uncommitted: { commands: 1, unplaced: false, places: [
      { worktree: 'app', branch: 'main', rule: 'recorded-by-tool' },
      // The session saw the folder gone, but the machine lists it again: the list is newer.
      { worktree: 'app-back', removed: true, rule: 'folder-no-longer-a-worktree' },
      { worktree: 'unlisted', removed: true, rule: 'folder-no-longer-a-worktree' },
    ] } }),
  ], [], { worktrees: [
    { machine: 'laptop', name: 'app', branch: 'feature', removed: false, first_seen_at: at(1), last_seen_at: at(9) },
    { machine: 'laptop', name: 'app-back', removed: false, first_seen_at: at(1), last_seen_at: at(9) },
    { machine: 'laptop', name: 'app-gone', branch: 'old', removed: true, first_seen_at: at(1), last_seen_at: at(2) },
    { name: 'no-label', removed: false, first_seen_at: at(1), last_seen_at: at(9) },
  ] });
  assert.deepEqual(index.worktrees.map(row => [row.name, row.machine, row.removed, row.current ?? '', row.sessions.length]), [
    ['app', 'laptop', false, 'feature', 1], ['app-back', 'laptop', false, '', 1], ['app-gone', 'laptop', true, 'old', 0], ['no-label', '', false, '', 0], ['unlisted', 'laptop', true, '', 1],
  ]);
});

test('notes are found by exactly the target they were written about', () => {
  const note = (id: string, target: AnchoredNote['target']): AnchoredNote =>
    ({ repo_id: 'repo', id, target, author: 'member', body: id, created_at: at(1), updated_at: at(1) });
  const notes = [
    note('commit', { kind: 'commit', commit: sha('a') }), note('other-commit', { kind: 'commit', commit: sha('b') }),
    note('branch-now', { kind: 'branch', branch: 'feature' }), note('branch-earlier', { kind: 'branch', branch: 'feature', ledger_id: 'a'.repeat(32) }),
    note('worktree', { kind: 'worktree', worktree: 'app', machine: 'laptop' }), note('worktree-elsewhere', { kind: 'worktree', worktree: 'app' }),
  ];
  const found = (target: AnchoredNote['target']) => anchoredNotesFor(notes, target).map(entry => entry.id);
  assert.deepEqual(found({ kind: 'commit', commit: sha('a') }), ['commit']);
  assert.deepEqual(found({ kind: 'branch', branch: 'feature' }), ['branch-now']);
  assert.deepEqual(found({ kind: 'branch', branch: 'feature', ledger_id: 'a'.repeat(32) }), ['branch-earlier'], 'a note about an earlier use stays with that use');
  assert.deepEqual(found({ kind: 'worktree', worktree: 'app', machine: 'laptop' }), ['worktree']);
  assert.deepEqual(found({ kind: 'worktree', worktree: 'app' }), ['worktree-elsewhere']);
  assert.deepEqual(found({ kind: 'commit', commit: sha('f') }), []);
});

test('a pull request merged by squash joins its branch to the commit it became; a merge commit needs no join', () => {
  const commit = (letter: string, minute: number, ...parents: string[]): RemoteCommit =>
    ({ sha: sha(letter), parents: parents.map(sha), author_name: 'Dev', authored_at: at(minute), committed_at: at(minute), subject: letter });
  const pull = (number: number, head: string, merged: string, extra: Record<string, unknown> = {}) =>
    ({ number, title: `pull ${number}`, state: 'merged', head_ref: `b${number}`, head_sha: sha(head), base_ref: 'main', merge_sha: sha(merged), commits: [sha(head)], updated_at: at(50), ...extra });
  const remote = {
    default_branch: 'main', synced_at: at(58), branches: [{ name: 'main', sha: sha('m') }],
    commits: [commit('a', 1), commit('f', 2, 'a'), commit('s', 3, 'a'), commit('g', 4, 's'), commit('m', 5, 's', 'g'), commit('h', 6, 'a'), commit('o', 7, 'a')],
    pulls: [pull(1, 'f', 's'), pull(2, 'g', 'm'), pull(3, 'h', 'o', { fork: true }), pull(4, 'o', 'o'), pull(5, 'h', 'a', { state: 'closed' })],
  } as unknown as AnchoredRemote;
  const index = buildAnchoredIndex([], [], { remote });
  const row = (letter: string) => index.commits.find(entry => entry.sha === sha(letter))!;
  assert.deepEqual(row('s').joined, [sha('f')], 'the squash commit joins the branch it came from');
  assert.equal(row('s').merged, 1);
  assert.equal(row('m').joined, undefined, 'a merge commit already has the branch as a parent');
  assert.equal(row('o').joined, undefined, 'a fork lives in another repository, and a commit does not join itself');
  assert.equal(row('a').joined, undefined, 'only a merged pull request joins');

  // A merge commit that only a session report names has no known parents yet: it is not joined.
  const report = summary('local-merge', { segments: 1, commits: [{ sha: sha('l'), segment_no: 1, rule: 'printed-number', subject: 'Merge feature', created_at: at(9), on_remote: true }] });
  const local = buildAnchoredIndex([report], [], { remote: { ...remote, pulls: [pull(6, 'f', 'l')] } as unknown as AnchoredRemote });
  const merge = local.commits.find(entry => entry.sha === sha('l'))!;
  assert.equal(merge.remote, false);
  assert.equal(merge.merged, 6);
  assert.equal(merge.joined, undefined, 'its parents are not known, so a join could stand where a parent is');
});
