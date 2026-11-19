import assert from 'node:assert/strict';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { I18nProvider, translate } from '../src/i18n/index.tsx';
import { AnchoredView } from '../src/components/AnchoredView.tsx';
import { anchoredPath, type AnchoredSelection } from '../src/route';
import type { AnchoredCommitSegment, AnchoredSession, AnchoredSessionSummary, BranchLifetime, Repo } from '../src/types';

const repo = { id: `sha256:${'9'.repeat(64)}`, remote_url: 'https://example.test/alice/app', default_branch: 'main' } as Repo;
const repository = { id: 'repository-1', owner_username: 'alice', slug: 'app' };
const sha = (letter: string) => letter.repeat(40);
const doc = `sha256:${'2'.repeat(64)}`;
const at = (minute: number) => `2031-01-02T03:${String(minute).padStart(2, '0')}:00Z`;

const summaries: AnchoredSessionSummary[] = [
  { provider: 'claude', session_id: 'parent-session', sub: false, revision: 2, body_hash: `sha256:${'0'.repeat(64)}`, records: 6, commands: 3, segments: 2,
    machine: 'laptop', started_at: at(0), ended_at: at(50), content: true, updated_at: at(55),
    commits: [
      { sha: sha('a'), segment_no: 1, rule: 'printed-number', subject: 'Add the parser', created_at: at(10) },
      { sha: sha('b'), segment_no: 2, rule: 'same-folder-and-time', subject: 'Fix the parser', created_at: at(20) },
      { sha: sha('c'), segment_no: 2, rule: 'same-folder-and-time', subject: 'Fix the parser tests', created_at: at(21) },
    ],
    uncommitted: { commands: 1, started_at: at(40), ended_at: at(50), unplaced: false, places: [{ worktree: 'app', branch: 'feature', rule: 'recorded-by-tool' }] } },
  { provider: 'claude', session_id: 'child-session', sub: true, parent: 'parent-session', revision: 1, body_hash: `sha256:${'0'.repeat(64)}`, records: 2, commands: 0,
    segments: 0, machine: 'laptop', started_at: at(15), content: false, commits: [], spawned_in: { segment_no: 2, rule: 'tool-call' }, updated_at: at(55),
    uncommitted: { commands: 0, unplaced: true, places: [] } },
];
const lifetimes: BranchLifetime[] = [
  { repo_id: repo.id, ledger_id: 'a'.repeat(32), name: 'feature', opened_at: at(1), opened_by: { kind: 'registered', account: 'alice' }, closed_at: at(5), closed_by: { kind: 'hosting-push' }, linked: [] },
  { repo_id: repo.id, ledger_id: 'b'.repeat(32), name: 'feature', opened_at: at(30), opened_by: { kind: 'registered', account: 'alice' }, linked: [] },
];
const session: AnchoredSession = {
  repo_id: repo.id, provider: 'claude', session_id: 'parent-session', uploader: 'alice', revision: 2, body_hash: `sha256:${'0'.repeat(64)}`, created_at: at(55), updated_at: at(55),
  body: {
    rule_version: 1, sub: false, scope: 'working-folder', machine: 'laptop', started_at: at(0), ended_at: at(50), records: 6, commands: 3, copied_records: 0,
    segments: [
      { first: 'm1', last: 'm2', first_ordinal: 0, last_ordinal: 1, commands: 1, events: 2, closes_turn: false, commits: [{ sha: sha('a'), rule: 'printed-number', subject: 'Add the parser' }] },
      { first: 'm3', last: 'm4', first_ordinal: 2, last_ordinal: 3, commands: 1, first_event: 2, events: 1, closes_turn: true,
        commits: [{ sha: sha('b'), rule: 'same-folder-and-time', subject: 'Fix the parser' }, { sha: sha('c'), rule: 'same-folder-and-time', subject: 'Fix the parser tests' }] },
    ],
    uncommitted: { first: 'm5', last: 'm6', first_ordinal: 4, last_ordinal: 5, commands: 1, first_event: 3, events: 1, unplaced: false, places: [{ worktree: 'app', branch: 'feature', rule: 'recorded-by-tool' }] },
    agent_calls: [{ tool: 'Task', from: 'parent-session', to: ['child-session'], status: 'completed', at: at(15) }],
    content: { doc, events: 4 },
  },
};
const commitSegments: AnchoredCommitSegment[] = [{ provider: 'claude', session_id: 'parent-session', segment_no: 2, rule: 'same-folder-and-time', segment: session.body.segments[1] }];
const message = (index: number, role: string, text: string) => ({ kind: 'message', role, blocks: [{ type: 'text', text }], event_index: index });

function render(selection?: AnchoredSelection): string {
  const client = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity } } });
  client.setQueryData(['anchored-sessions', repo.id], summaries);
  client.setQueryData(['branch-lifetimes', repo.id], lifetimes);
  client.setQueryData(['anchored-commit', repo.id, sha('b'), 'claude:parent-session@2'], commitSegments);
  client.setQueryData(['anchored-session', repo.id, 'claude', 'parent-session', 2], session);
  client.setQueryData(['anchored-events', repo.id, doc, 0], { pageParams: [0], pages: [{ hash: doc, offset: 0, total: 4, inherited: 0, next: -1, events: [
    message(0, 'user', 'Write the parser.'), message(1, 'assistant', 'Committed the parser.'), message(2, 'assistant', 'Fixed the parser.'), message(3, 'user', 'What is left?'),
  ] }] });
  client.setQueryData(['anchored-events', repo.id, doc, 2], { pageParams: [2], pages: [{ hash: doc, offset: 2, total: 4, inherited: 0, next: 3, events: [message(2, 'assistant', 'Fixed the parser.')] }] });
  return renderToStaticMarkup(createElement(I18nProvider, null, createElement(QueryClientProvider, { client },
    createElement(AnchoredView, { repo, selection, href: (target?: AnchoredSelection) => anchoredPath(repository, target) }))));
}
const count = (html: string, pattern: RegExp) => html.match(pattern)?.length ?? 0;

// Nothing selected: the lists on the left, every commit in the middle, all sessions on the right.
const overview = render();
assert.match(overview, /<nav class="anchor-nav" aria-label="Worktrees and branches">/);
assert.match(overview, /href="\/alice\/app\?tab=sessions&amp;worktree=app&amp;machine=laptop"/);
assert.match(overview, /href="\/alice\/app\?tab=sessions&amp;branch=feature"[^>]*>.*?1 earlier use/s, 'a reused branch name shows its earlier use');
assert.deepEqual([...overview.matchAll(/data-commit="([0-9a-f]{40})"/g)].map(match => match[1]), [sha('c'), sha('b'), sha('a')], 'commits are listed newest first');
assert.match(overview, /Add the parser<\/span>.*?1 segment</s);
assert.match(overview, /href="\/alice\/app\?tab=sessions&amp;session=claude%3Aparent-session"/);
assert.match(overview, /<summary>1 sub-session<\/summary>/, 'a spawned session sits under its parent');
assert.match(overview, /location unknown/, 'an uncommitted part with no place is marked, not guessed');
assert.match(overview, /Only your own sessions are shown/);

// A commit: the segment that ends at it, the other commit of the same segment, the session spawned in it.
const commit = render({ kind: 'commit', sha: sha('b') });
assert.match(commit, /class="anchor-commit on"[^>]*aria-current="true" data-commit="b{40}"/);
assert.match(commit, /data-segment="claude:parent-session#2"/);
assert.match(commit, /Segment 2 of 2 · linked by the same folder and time · 1 command/);
assert.match(commit, /Commits sharing this segment:.*?href="\/alice\/app\?tab=sessions&amp;commit=c{40}"/s);
assert.match(commit, /Sub-sessions spawned in this segment:.*?session=claude%3Achild-session/s);
assert.match(commit, /href="\/alice\/app\?tab=sessions&amp;session=claude%3Aparent-session&amp;segment=2"/, 'the session opens at that segment');
assert.match(render({ kind: 'commit', sha: sha('f') }), /No context/, 'a commit no segment ends at has no context');

// A branch: the name as it is now and its earlier use, each with the uncommitted parts that fall in it.
const branch = render({ kind: 'branch', name: 'feature' });
assert.match(branch, /<h6>Current .*?<\/h6><ul class="anchor-cards">.*?segment=rest/s);
assert.match(branch, /<h6>Earlier use .*?<\/h6><p class="empty-box">No context<\/p>/s);
assert.match(render({ kind: 'worktree', name: 'app', machine: 'laptop' }), /Worktree <code>app<\/code> <em>laptop<\/em>.*?After the last commit · 1 command/s);

// A session: its parts, the calls it exchanged, and the conversation with a divider where each part begins.
const opened = render({ kind: 'session', provider: 'claude', id: 'parent-session' });
assert.equal(count(opened, /role="separator"/g), 3);
assert.match(opened, /data-part="1">Segment 1<code>a{10}<\/code><\/div>.*?Write the parser\..*?Committed the parser\..*?data-part="2".*?Fixed the parser\..*?data-part="rest">After the last commit<\/div>.*?What is left\?/s);
assert.match(opened, /Sub-sessions spawned in this segment:.*?session=claude%3Achild-session/s);
assert.match(opened, /Calls between agents/);
assert.match(opened, /child-se · 1 sent · 0 received/);
assert.doesNotMatch(opened, /Show from the beginning/);
const second = render({ kind: 'session', provider: 'claude', id: 'parent-session', segment: 2 });
assert.match(second, /Show from the beginning/);
assert.equal(count(second, /role="separator"/g), 2, 'reading has reached the part after the second segment');
assert.doesNotMatch(second, /Write the parser\./);
assert.match(second, /Load more/, 'more of the conversation is read on request');

assert.match(translate('ko', 'anchored.noContext'), /\uB9E5\uB77D \uC5C6\uC74C/);
assert.match(translate('ko', 'anchored.unplaced'), /\uC704\uCE58 \uBBF8\uC0C1/);
console.log('sessions tab: lists by worktree, branch and commit; a session opens with a divider at each segment');
