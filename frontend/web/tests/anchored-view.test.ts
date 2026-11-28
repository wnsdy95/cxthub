import assert from 'node:assert/strict';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { I18nProvider, translate } from '../src/i18n/index.tsx';
import { AnchoredView } from '../src/components/AnchoredView.tsx';
import { anchoredPath, type AnchoredSelection } from '../src/route';
import type { AnchoredCommitSegment, AnchoredNote, AnchoredSession, AnchoredSearchResult, AnchoredSessionSummary, AnchoredSummary, BranchLifetime, RemoteCommit, RemoteGitView, Repo, ReportedWorktree } from '../src/types';

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
const commitSummaries: AnchoredSummary[] = [{ repo_id: repo.id, provider: 'claude', session_id: 'parent-session', commit: sha('b'), body_hash: `sha256:${'3'.repeat(64)}`,
  body: { summary: 'Fixed the **parser** edge case.', key_facts: ['The parser reads TOML.'], open_tasks: ['Document the parser.'] }, created_at: at(21), updated_at: at(21), segment_no: 2, uploader: 'alice-id' }];
const message = (index: number, role: string, text: string) => ({ kind: 'message', role, blocks: [{ type: 'text', text }], event_index: index });

// What the server read from the provider: main a - e (merge of pull request 7: b, c), and d on a branch no session worked on.
const remoteCommit = (letter: string, minute: number, subject: string, ...parents: string[]): RemoteCommit =>
  ({ sha: sha(letter), parents: parents.map(sha), author_name: 'Dev', authored_at: at(minute), committed_at: at(minute), subject });
const remotePage: RemoteGitView = {
  default_branch: 'main', synced_at: at(58),
  branches: [{ name: 'main', sha: sha('e') }, { name: 'feature', sha: sha('c') }, { name: 'quiet', sha: sha('d') }],
  pulls: [{ number: 7, title: 'Fix the parser', state: 'merged', head_ref: 'feature', head_sha: sha('c'), base_ref: 'main', merge_sha: sha('e'), commits: [sha('b'), sha('c')], updated_at: at(57) }],
  commits: [remoteCommit('a', 10, 'Add the parser'), remoteCommit('b', 20, 'Fix the parser', 'a'), remoteCommit('c', 21, 'Fix the parser tests', 'b'),
    remoteCommit('d', 30, 'Quiet change', 'a'), remoteCommit('e', 40, 'Merge pull request', 'a', 'c')],
};
const worktrees: ReportedWorktree[] = [
  { machine: 'laptop', name: 'app', branch: 'feature', removed: false, first_seen_at: at(0), last_seen_at: at(55) },
  { machine: 'laptop', name: 'scratch', removed: true, first_seen_at: at(0), last_seen_at: at(5) },
];
const notes: AnchoredNote[] = [
  { repo_id: repo.id, id: `note_${'a'.repeat(32)}`, target: { kind: 'commit', commit: sha('d') }, author: 'alice-id', body: 'Applied by hand.\nSee the ticket.', created_at: at(31), updated_at: at(32) },
];

function render(selection?: AnchoredSelection, withRemote = false, sessions = summaries): string {
  const client = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity } } });
  if (withRemote) {
    client.setQueryData(['remote-git', repo.id], { pageParams: [''], pages: [remotePage] });
    client.setQueryData(['reported-worktrees', repo.id], worktrees);
    client.setQueryData(['anchored-notes', repo.id], notes);
    client.setQueryData(['me'], { id: 'alice-id', username: 'alice' });
  }
  client.setQueryData(['anchored-sessions', repo.id], sessions);
  client.setQueryData(['branch-lifetimes', repo.id], lifetimes);
  client.setQueryData(['anchored-commit', repo.id, sha('b'), 'claude:parent-session@2'], commitSegments);
  client.setQueryData(['anchored-commit-summaries', repo.id, sha('b'), 'claude:parent-session@2'], commitSummaries);
  client.setQueryData(['anchored-session', repo.id, 'claude', 'parent-session', 2], session);
  // The conversation is read by the session, never by its document's hash.
  client.setQueryData(['anchored-events', repo.id, 'claude', 'parent-session', 2, 0], { pageParams: [0], pages: [{ offset: 0, next: -1, events: [
    message(0, 'user', 'Write the parser.'), message(1, 'assistant', 'Committed the parser.'), message(2, 'assistant', 'Fixed the parser.'), message(3, 'user', 'What is left?'),
  ] }] });
  client.setQueryData(['anchored-events', repo.id, 'claude', 'parent-session', 2, 2], { pageParams: [2], pages: [{ offset: 2, next: 3, events: [message(2, 'assistant', 'Fixed the parser.')] }] });
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
assert.match(overview, /Your sessions are shown in full; a teammate&#x27;s, as far as it is pushed\./);

// A commit: the segment that ends at it, the other commit of the same segment, the session spawned in it.
const commit = render({ kind: 'commit', sha: sha('b') });
assert.match(commit, /class="anchor-commit on"[^>]*aria-current="true" data-commit="b{40}"/);
assert.match(commit, /data-segment="claude:parent-session#2"/);
assert.match(commit, /Segment 2 of 2 · linked by the same folder and time · 1 command/);
assert.match(commit, /Commits sharing this segment:.*?href="\/alice\/app\?tab=sessions&amp;commit=c{40}"/s);
assert.match(commit, /Sub-sessions spawned in this segment:.*?session=claude%3Achild-session/s);
assert.match(commit, /href="\/alice\/app\?tab=sessions&amp;session=claude%3Aparent-session&amp;segment=2"/, 'the session opens at that segment');
assert.match(commit, /data-summary="claude:parent-session"><summary class="label">◆ Summary at this commit<\/summary>.*?<strong>parser<\/strong>.*?Worth keeping.*?The parser reads TOML\..*?Left to do.*?Document the parser\./s,
  'the summary written at the commit sits with its segment');
assert.doesNotMatch(render({ kind: 'commit', sha: sha('f') }), /Summary at this commit/, 'a commit without a summary shows none');
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

// With what the server read from the provider: the graph also holds commits no session made.
const graph = render(undefined, true);
assert.deepEqual([...graph.matchAll(/data-commit="([0-9a-f])[0-9a-f]{39}" data-lane="(\d)"/g)].map(match => match[1] + match[2]), ['e0', 'd2', 'c1', 'b1', 'a0'],
  'the default branch keeps the first lane; the merged line and the unmerged branch get their own');
assert.equal(count(graph, /<svg class="anchor-lanes"/g), 5);
assert.match(graph, /data-commit="e{40}".*?<span class="ref-badge branch">main<\/span><span class="ref-badge">#7<\/span>Merge pull request<\/span>.*?No context/s);
assert.match(graph, /data-commit="d{40}".*?Quiet change<\/span>.*?No context/s);
assert.match(graph, /data-commit="b{40}".*?1 segment/s);
assert.doesNotMatch(graph, /not on the remote/, 'every commit of the reports is on the remote here');
assert.doesNotMatch(overview, /not on the remote/, 'before the remote was read no commit is called missing');
// A commit of a report that the remote does not list is marked, once the remote was read.
const unpushed = render(undefined, true, [{ ...summaries[0], segments: 3, commits: [...summaries[0].commits, { sha: sha('f'), segment_no: 3, rule: 'printed-number', subject: 'Local only', created_at: at(45) }] }]);
assert.match(unpushed, /data-commit="f{40}".*?Local only<\/span>.*?1 segment.*?not on the remote/s);
assert.equal(count(unpushed, /not on the remote/g), 1);
assert.match(graph, /Pull requests<\/h5>.*?href="\/alice\/app\?tab=sessions&amp;pull=7".*?<code>#7<\/code>.*?Fix the parser.*?merged/s);
assert.match(graph, /worktree=scratch&amp;machine=laptop"[^>]*>.*?removed worktree/s, 'a worktree no session worked in is listed from the machine\'s own list');
assert.match(graph, /Remote read at/);

// A commit no session made: no context, the note written about it, and the form to write one.
const quiet = render({ kind: 'commit', sha: sha('d') }, true);
assert.match(quiet, /No context<\/p>/);
assert.match(quiet, /data-note="note_a{32}".*?manual note<\/span><em>you<\/em>.*?Applied by hand\.\nSee the ticket\./s);
assert.match(quiet, />Write a note<\/button>/);
assert.match(quiet, /href="\/alice\/app\?tab=sessions&amp;branch=quiet"/);
// A commit a segment ends at takes no new note.
assert.doesNotMatch(render({ kind: 'commit', sha: sha('b') }, true), /Write a note/);
// The merge commit leads through its pull request to the segments of the commits it merged.
const mergeCommit = render({ kind: 'commit', sha: sha('e') }, true);
assert.match(mergeCommit, /Merged pull request .*?pull=7.*?commit=c{40}.*?parent-s · Segment 2.*?commit=b{40}.*?parent-s · Segment 2/s);
const pull = render({ kind: 'pull', number: 7 }, true);
assert.match(pull, /Pull request #7 <span class="ref-badge">merged<\/span>/);
assert.match(pull, /branch=feature"[^>]*>feature<\/a> → .*?branch=main.*?merged as .*?commit=e{40}/s);
assert.match(pull, /class="anchor-commit in"[^>]*data-commit="c{40}"/, 'the commits of the selected pull request are marked in the graph');
assert.match(render({ kind: 'pull', number: 99 }, true), /Pull request #99 has not been read yet\./);
// A branch: its pull request's commits when it has one, otherwise what its base does not have yet.
assert.match(render({ kind: 'branch', name: 'feature' }, true), /Commits of pull request #7<\/h6>.*?commit=c{40}.*?commit=b{40}/s);
const quietBranch = render({ kind: 'branch', name: 'quiet' }, true);
assert.match(quietBranch, /Commits not on main yet<\/h6>.*?commit=d{40}.*?Quiet change.*?No context/s);
assert.match(quietBranch, />Write a note<\/button>/);
// The name as it is now is explained by sessions and takes no new note; its earlier use, which nothing explains, does.
const featureBranch = render({ kind: 'branch', name: 'feature' }, true);
assert.doesNotMatch(featureBranch.slice(featureBranch.indexOf('<h6>Current'), featureBranch.indexOf('<h6>Earlier use')), /Write a note|No context/);
assert.match(featureBranch.slice(featureBranch.indexOf('<h6>Earlier use')), /No context.*?Write a note/s);
// A worktree: the branch checked out in it; one no session worked in has no context and takes a note.
assert.match(render({ kind: 'worktree', name: 'app', machine: 'laptop' }, true), /Checked out now: <a class="ref-badge branch"[^>]*>feature<\/a>/);
assert.match(render({ kind: 'worktree', name: 'scratch', machine: 'laptop' }, true), /removed worktree.*?No context.*?Write a note/s);

// A teammate's session: listed with its uploader, read as far as it is pushed. The second segment is not open; the part
// after the last commit is open up to the last push. Its places name no worktree, and its notes are read but not changed.
const theirs: AnchoredSessionSummary = { provider: 'claude', session_id: 'their-session', sub: false, revision: 1, body_hash: `sha256:${'0'.repeat(64)}`,
  records: 3, commands: 1, segments: 1, content: true, updated_at: at(55), uploader: 'bob-id', uploader_name: 'bob', shared: true,
  commits: [{ sha: sha('a'), segment_no: 1, rule: 'printed-number', subject: 'Add the parser', on_remote: true }],
  uncommitted: { commands: 0, unplaced: false, places: [{ worktree: '', branch: 'feature', rule: 'recorded-by-tool' }] } };
const theirSession: AnchoredSession = { ...session, session_id: 'their-session', uploader: 'bob-id', revision: 1, body: {
  rule_version: 1, sub: false, scope: 'working-folder', records: 3, commands: 1, copied_records: 0,
  segments: [session.body.segments[0], { first: '', last: '', first_ordinal: 0, last_ordinal: 0, commands: 0, closes_turn: false, commits: [], hidden: true }],
  uncommitted: { first: '', last: '', first_ordinal: 4, last_ordinal: 4, commands: 0, first_event: 3, events: 1, unplaced: false, partial: true,
    places: [{ worktree: '', branch: 'feature', rule: 'recorded-by-tool' }] },
  content: { events: 4 },
} };
// The names come with the sessions and notes: not every reader may read the member list.
function renderShared(selection?: AnchoredSelection, reader: { member?: boolean; manage?: boolean; signedIn?: boolean; listed?: AnchoredSessionSummary } = {}): string {
  const client = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity } } });
  client.setQueryData(['me'], reader.signedIn === false ? null : { id: 'alice-id', username: 'alice' });
  client.setQueryData(['anchored-sessions', repo.id], [reader.listed ?? theirs]);
  client.setQueryData(['branch-lifetimes', repo.id], lifetimes);
  client.setQueryData(['anchored-notes', repo.id], [{ ...notes[0], author: 'bob-id', author_name: 'bob' }]);
  client.setQueryData(['remote-git', repo.id], { pageParams: [''], pages: [remotePage] });
  client.setQueryData(['reported-worktrees', repo.id], []);
  client.setQueryData(['anchored-session', repo.id, 'claude', 'their-session', 1], theirSession);
  client.setQueryData(['anchored-events', repo.id, 'claude', 'their-session', 1, 0], { pageParams: [0, 3], pages: [
    { offset: 0, next: -1, events: [message(0, 'user', 'Write the parser.'), message(1, 'assistant', 'Committed the parser.')] },
    { offset: 3, next: -1, events: [message(3, 'user', 'What is left?')] },
  ] });
  return renderToStaticMarkup(createElement(I18nProvider, null, createElement(QueryClientProvider, { client },
    createElement(AnchoredView, { repo, member: reader.member, manage: reader.manage, selection, href: (target?: AnchoredSelection) => anchoredPath(repository, target) }))));
}
const sharedList = renderShared();
assert.match(sharedList, /their-se<\/code><\/a><span class="ref-badge joined">bob<\/span>/, 'a teammate\'s session names its uploader');
assert.doesNotMatch(sharedList, /worktree=&amp;|<span class="anchor-nav-name"><\/span>/, 'a teammate\'s places make no worktree');
const sharedOpened = renderShared({ kind: 'session', provider: 'claude', id: 'their-session' });
assert.match(sharedOpened, /Session of bob\. You see the parts that are pushed\./);
assert.match(sharedOpened, /<span>Segment 2<\/span><span class="ref-badge pending">not pushed yet<\/span>/);
assert.match(sharedOpened, /shown up to the last push/);
assert.match(sharedOpened, /Write the parser\..*?Committed the parser\..*?class="session-divider anchor-divider hidden"[^>]*data-part="2">Segment 2<span> · not pushed yet, so not shown<\/span>.*?data-part="rest">After the last commit<\/div>.*?What is left\?/s,
  'reading goes on past the part that is not open');
assert.doesNotMatch(sharedOpened, /Fixed the parser\./);
assert.doesNotMatch(sharedOpened, /Calls between agents/);
const sharedNote = renderShared({ kind: 'commit', sha: sha('d') });
assert.match(sharedNote, /data-note="note_a{32}".*?manual note<\/span><em>bob<\/em>.*?Applied by hand\./s);
assert.doesNotMatch(sharedNote, /class="ghost mini"[^>]*>(Edit|Delete)<\/button>/, 'only its author changes or removes a note');
assert.match(quiet, /class="ghost mini"[^>]*>Edit<\/button>/, 'the author does');

// Whoever may view the repository reads what is pushed: a viewer, or a visitor who is not signed in. Such a reader has
// no work of their own to list and writes no note.
for (const signedIn of [true, false]) {
  const viewed = renderShared(undefined, { member: false, signedIn });
  assert.match(viewed, /their-se<\/code><\/a><span class="ref-badge joined">bob<\/span>/);
  assert.match(viewed, /Sessions are shown as far as they are pushed\./);
  assert.doesNotMatch(viewed, /On Hold|only you|>Worktrees<|Your sessions are shown in full/);
  assert.match(viewed, /<h5 class="label">Branches<\/h5>/, 'branches and pull requests are everyone\'s');
  const viewedNote = renderShared({ kind: 'commit', sha: sha('d') }, { member: false, signedIn });
  assert.match(viewedNote, /manual note<\/span><em>bob<\/em>.*?Applied by hand\./s);
  assert.doesNotMatch(viewedNote, /Write a note|class="ghost mini"/, 'a note is written by a member');
  assert.doesNotMatch(renderShared({ kind: 'held' }, { member: false, signedIn }), /Only you see this list/, 'the list of work before a push is a member\'s own');
}
assert.match(renderShared(undefined, { member: false, signedIn: false, listed: { ...theirs, uploader_name: undefined } }), /<span class="ref-badge joined">a member<\/span>/,
  'an uploader whose handle is not given is not shown by account');
const nothingPushed = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity } } });
nothingPushed.setQueryData(['me'], null);
for (const key of ['anchored-sessions', 'branch-lifetimes', 'anchored-notes', 'reported-worktrees']) nothingPushed.setQueryData([key, repo.id], []);
nothingPushed.setQueryData(['remote-git', repo.id], { pageParams: [''], pages: [remotePage] });
const empty = renderToStaticMarkup(createElement(I18nProvider, null, createElement(QueryClientProvider, { client: nothingPushed },
  createElement(AnchoredView, { repo, member: false, href: (target?: AnchoredSelection) => anchoredPath(repository, target) }))));
assert.match(empty, /Nothing of this repository&#x27;s sessions is pushed yet\./);
assert.doesNotMatch(empty, /cxt segments push/, 'a reader who cannot upload is not told to');

// The caller's own work before a push: commits the remote does not hold, then what is not committed yet.
const waiting: AnchoredSessionSummary[] = [
  { ...summaries[0], segments: 3, commits: [...summaries[0].commits, { sha: sha('9'), segment_no: 3, rule: 'printed-number', subject: 'Draft the lexer', created_at: at(45), on_remote: false }] },
  summaries[1],
];
assert.match(overview, /href="\/alice\/app\?tab=sessions&amp;show=held"[^>]*>On Hold <span class="count-badge">2<\/span><span class="ref-badge pending">only you<\/span>/,
  'the list counts the uncommitted parts while no commit is known to be missing from the remote');
const held = render({ kind: 'held' }, true, waiting);
assert.match(held, /class="anchor-nav-item on" aria-current="page" href="\/alice\/app\?tab=sessions&amp;show=held">On Hold <span class="count-badge">3<\/span>/);
assert.match(held, /Only you see this list\./);
assert.match(held, /Committed, not pushed yet <span class="count-badge">1<\/span>.*?commit=9{40}.*?Draft the lexer.*?session=claude%3Aparent-session&amp;segment=3/s);
assert.match(held, /Not committed yet <span class="count-badge">2<\/span>.*?session=claude%3Aparent-session&amp;segment=rest.*?Worktree <code>app<\/code>.*?branch=feature/s);
assert.doesNotMatch(held, /class="anchor-commits"/, 'the list stands in place of the commit graph');
assert.match(render({ kind: 'held' }, true, [{ ...summaries[0], uncommitted: undefined }]), /Nothing of yours is waiting for a push\./);
assert.match(render({ kind: 'held' }, false, waiting), /The commit list has not been read from the remote yet\./, 'until the remote is read no commit is said to be missing there');
// A search lists what the query was found in, each hit beginning with what it is and leading to it.
function renderSearch(query: string, found?: AnchoredSearchResult): string {
  const client = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity } } });
  client.setQueryData(['me'], { id: 'alice-id', username: 'alice' });
  for (const key of ['anchored-sessions', 'branch-lifetimes', 'anchored-notes', 'reported-worktrees']) client.setQueryData([key, repo.id], key === 'anchored-sessions' ? summaries : []);
  client.setQueryData(['remote-git', repo.id], { pageParams: [''], pages: [remotePage] });
  if (found) client.setQueryData(['anchored-search', repo.id, query.trim()], found);
  return renderToStaticMarkup(createElement(I18nProvider, null, createElement(QueryClientProvider, { client },
    createElement(AnchoredView, { repo, selection: { kind: 'search', query }, href: (target?: AnchoredSelection) => anchoredPath(repository, target) }))));
}
const searched = renderSearch('parser', { truncated: true, hits: [
  { kind: 'pull', title: 'Fix the parser', pull: 7, branch: 'feature' },
  { kind: 'branch', title: 'parser-notes', branch: 'parser-notes' },
  { kind: 'commit', title: 'Add the parser', commit: sha('a'), at: at(10) },
  { kind: 'segment', title: 'assistant', snippet: 'Committed the parser.', provider: 'claude', session_id: 'parent-session', segment_no: 2, event: 1 },
  { kind: 'segment', title: 'user', snippet: 'Is the parser done?', provider: 'claude', session_id: 'parent-session', rest: true, event: 3 },
  { kind: 'note', title: 'bob', snippet: 'The parser was applied by hand.', note_id: `note_${'a'.repeat(32)}`, target: { kind: 'commit', commit: sha('d') } },
  { kind: 'note', title: 'alice', snippet: 'parser scratch', note_id: `note_${'b'.repeat(32)}`, target: { kind: 'worktree', worktree: 'app', machine: 'laptop' } },
] });
assert.match(searched, /<input[^>]*type="search"[^>]*name="q"[^>]*value="parser"/, 'the box keeps what was asked');
assert.match(searched, /Found for &quot;parser&quot;/);
assert.deepEqual([...searched.matchAll(/data-hit="([a-z]+)"><span class="ref-badge hit-[a-z]+">([^<]+)</g)].map(match => `${match[1]}:${match[2]}`),
  ['pull:PR', 'branch:Branch', 'commit:Commit', 'segment:Segment', 'segment:Segment', 'note:Note', 'note:Note'], 'every hit begins with what it is');
assert.match(searched, /href="\/alice\/app\?tab=sessions&amp;pull=7"><code>#7<\/code><\/a> <span>Fix the parser<\/span>/);
assert.match(searched, /href="\/alice\/app\?tab=sessions&amp;branch=parser-notes"><code>parser-notes<\/code>/);
assert.match(searched, /href="\/alice\/app\?tab=sessions&amp;commit=a{40}"><code>a{10}<\/code><\/a> <span>Add the parser<\/span>/);
assert.match(searched, /session=claude%3Aparent-session&amp;segment=2"><code>parent-s · Segment 2<\/code><\/a> <span>assistant<\/span>.*?Committed the parser\./s);
assert.match(searched, /session=claude%3Aparent-session&amp;segment=rest"><code>parent-s · After the last commit<\/code>/);
assert.match(searched, /commit=d{40}"><code>d{10}<\/code><\/a> <span>bob<\/span>.*?applied by hand/s, 'a note leads to what it was written about');
assert.match(searched, /worktree=app&amp;machine=laptop"><code>app<\/code>/);
assert.match(searched, /More was found than is listed\./);
assert.doesNotMatch(searched, /class="anchor-commits"/, 'the results stand in place of the commit graph');
assert.match(renderSearch('parser', { truncated: false, hits: [] }), /Nothing was found\./);
assert.match(renderSearch('p'), /Type at least two characters\./);
assert.match(translate('ko', 'anchored.hit_segment'), /\uC870\uAC01/);

assert.match(translate('ko', 'anchored.heldOnlyYou'), /\uB098\uB9CC \uBD04/);

// Two commits are compared: one is set as the base, and every commit opened from the list afterwards shows what lies
// between the two, each commit with the segments that explain it.
assert.match(commit, /<a class="ghost" href="\/alice\/app\?tab=sessions&amp;commit=b{40}&amp;base=b{40}">⇆ Set as compare base<\/a>/);
const picking = render({ kind: 'commit', sha: sha('a'), base: sha('a') }, true);
assert.match(picking, /This commit is the compare base\. Open another commit from the list/);
assert.match(picking, /data-commit="e{40}"[^>]*href="\/alice\/app\?tab=sessions&amp;commit=e{40}&amp;base=a{40}"/, 'the list keeps the base while another commit is opened');
assert.match(picking, /data-commit="a{40}"[^>]*>.*?<span class="ref-badge pending">base<\/span>Add the parser/s);
assert.match(picking, /<a class="ghost" href="\/alice\/app\?tab=sessions&amp;commit=a{40}">Clear compare base<\/a>/);
const compared = render({ kind: 'commit', sha: sha('e'), base: sha('a') }, true);
assert.match(compared, /<section class="anchor-use anchor-compare" aria-label="Compare"><h6>Compare: <a href="[^"]*commit=a{40}&amp;base=a{40}"><code>a{10}<\/code><\/a> → <code>e{10}<\/code><span class="anchor-card-meta"> · 3 commits<\/span>/);
assert.deepEqual([...compared.match(/<section class="anchor-use anchor-compare".*?<\/section>/s)![0].matchAll(/commit=([0-9a-f]{40})"><code>/g)].map(match => match[1][0]), ['e', 'c', 'b'],
  'the commits between the two, newest first');
assert.match(compared, /<section class="anchor-use anchor-compare".*?commit=b{40}"><code>b{10}<\/code><\/a> <span>Fix the parser<\/span>.*?session=claude%3Aparent-session&amp;segment=2/s,
  'each with the segment that explains it');
assert.equal(count(compared, /class="anchor-commit[^"]* in"/g), 3, 'and they are marked in the graph');
assert.match(render({ kind: 'commit', sha: sha('a'), base: sha('e') }, true), /This commit has nothing the base does not have\./);
assert.match(render({ kind: 'commit', sha: sha('b'), base: sha('a') }), /Only commits read from the remote can be compared\./);
assert.match(translate('ko', 'anchored.compareSet'), /\uBE44\uAD50 \uAE30\uC900/);

// A commit can be forked from: the page shows the command that makes a Git branch there, and, for each session a
// segment of which ends at the commit, the command that continues that conversation up to it.
assert.match(commit, /<details class="anchor-fork" data-fork="b{40}"><summary class="ghost">⑂ Fork from this commit<\/summary>/);
assert.match(commit, /<pre class="anchor-fork-command"><code>cxt segments fork --at b{40} --branch &lt;name&gt;<\/code><\/pre>/);
assert.match(commit, /<pre class="anchor-fork-command"><code>cxt segments fork claude parent-session --at b{40}<\/code><\/pre>/);
assert.equal(count(commit, /class="anchor-fork-command"/g), 1 + count(commit, /data-segment="/g), 'one command for the branch and one for each session at the commit');
const bare = render({ kind: 'commit', sha: sha('e') }, true);
assert.match(bare, /cxt segments fork --at e{40} --branch &lt;name&gt;/, 'a commit no session made can still be branched from');
assert.equal(count(bare, /class="anchor-fork-command"/g), 1);
assert.match(translate('ko', 'anchored.forkBranch'), /Git \uBE0C\uB79C\uCE58/);

// An archived session is listed only among the archived sessions, and is opened from there. Its uploader archives and
// restores it, and so does a reader who manages the repository.
assert.doesNotMatch(overview, /Archived sessions/, 'the list is offered once a session is archived');
const openedOwn = render({ kind: 'session', provider: 'claude', id: 'parent-session' });
assert.match(openedOwn, /<button type="button" class="ghost">Archive session<\/button>/);
assert.doesNotMatch(openedOwn, /class="ref-badge archived">archived</);
const withArchived: AnchoredSessionSummary[] = [{ ...summaries[0], archived_at: at(56) }, summaries[1]];
const afterArchive = render(undefined, false, withArchived);
assert.match(afterArchive, /href="\/alice\/app\?tab=sessions&amp;show=archived"[^>]*>Archived sessions <span class="count-badge">1<\/span>/);
assert.match(afterArchive, />All sessions <span class="count-badge">1<\/span>/, 'an archived session is not counted among the sessions');
assert.doesNotMatch(afterArchive, /data-commit="/, 'the commits only an archived session made are not listed');
assert.doesNotMatch(afterArchive, /session=claude%3Aparent-session/);
const archivedList = render({ kind: 'archived' }, false, withArchived);
assert.match(archivedList, /class="anchor-nav-item on" aria-current="page" href="\/alice\/app\?tab=sessions&amp;show=archived">/);
assert.match(archivedList, /It is opened from here; nothing of it is removed\./);
assert.match(archivedList, /<ul class="anchor-archived"><li><a href="\/alice\/app\?tab=sessions&amp;session=claude%3Aparent-session">.*?<code>parent-s<\/code><\/a>.*?archived <time/s);
assert.doesNotMatch(archivedList, /class="anchor-commits"/, 'the list stands in place of the commit graph');
assert.match(render({ kind: 'archived' }), /No archived sessions\./);
const openedArchived = render({ kind: 'session', provider: 'claude', id: 'parent-session' }, false, withArchived);
assert.match(openedArchived, /<span class="ref-badge archived">archived<\/span> This session is listed only among the archived sessions\. Archived <time/);
assert.match(openedArchived, /<button type="button" class="ghost">Restore session<\/button>/);
assert.match(openedArchived, /Write the parser\./, 'its conversation is still read');
assert.doesNotMatch(renderShared({ kind: 'session', provider: 'claude', id: 'their-session' }), /Archive session/, 'another member\'s session is not theirs to archive');
assert.match(renderShared({ kind: 'session', provider: 'claude', id: 'their-session' }, { manage: true }), /<button type="button" class="ghost">Archive session<\/button>/,
  'a reader who manages the repository archives any session');
assert.match(renderShared({ kind: 'archived' }, { member: false, signedIn: false, listed: { ...theirs, archived_at: at(56) } }),
  /session=claude%3Atheir-session">.*?<code>their-se<\/code><\/a>.*? · bob.*?archived <time/s, 'a visitor opens what is pushed of an archived session');
assert.match(translate('ko', 'anchored.archivedSessions'), /\uBCF4\uAD00\uD55C \uC138\uC158/);

assert.match(translate('ko', 'anchored.noContext'), /\uB9E5\uB77D \uC5C6\uC74C/);
assert.match(translate('ko', 'anchored.unplaced'), /\uC704\uCE58 \uBBF8\uC0C1/);
console.log('sessions tab: lists by worktree, branch and commit; a session opens with a divider at each segment');
