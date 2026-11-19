import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  anchoredKey, anchoredPartIndex, anchoredPartners, anchoredParts, anchoredSegmentCommits, anchoredStream, buildAnchoredIndex,
} from '../src/anchoredSessions';
import type { AnchoredSessionBody, AnchoredSessionSummary, BranchLifetime } from '../src/types';

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
  const shape = (loaded: number[], first: number) => anchoredStream(loaded, parts, first)
    .map(item => (item.kind === 'divider' ? `|${item.part.kind}${item.part.no}` : item.events.join(',')));

  assert.deepEqual(shape([], 0), ['|segment1'], 'before anything is read only the first part is announced');
  assert.deepEqual(shape(events(0, 4), 0), ['|segment1', '0,1,2,3']);
  assert.deepEqual(shape(events(0, 6), 0), ['|segment1', '0,1,2,3,4,5', '|segment2', '|uncommitted0'], 'reading reached the next parts');
  assert.deepEqual(shape(events(0, 10), 0), ['|segment1', '0,1,2,3,4,5', '|segment2', '|uncommitted0', '6,7,8,9']);
  assert.deepEqual(shape(events(6, 2), 1), ['|segment2', '|uncommitted0', '6,7'], 'opened at the second segment');
  assert.deepEqual(shape(events(6, 4), 2), ['|uncommitted0', '6,7,8,9'], 'opened at the part after the last commit');
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
