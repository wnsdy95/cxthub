import { expect, test, type Page } from '@playwright/test';
import { capturePageErrors, installApiFixture } from './api-fixture';

const origin = 'http://127.0.0.1:4174';
const sha = (letter: string) => letter.repeat(40);
const doc = `sha256:${'2'.repeat(64)}`;
const at = (minute: number) => `2031-01-02T03:${String(minute).padStart(2, '0')}:00Z`;
const hash = `sha256:${'0'.repeat(64)}`;

const summaries = [
  { provider: 'claude', session_id: 'parent-session', sub: false, revision: 2, body_hash: hash, records: 6, commands: 3, segments: 2, machine: 'laptop',
    started_at: at(0), ended_at: at(50), content: true, updated_at: at(55),
    commits: [
      { sha: sha('a'), segment_no: 1, rule: 'printed-number', subject: 'Add the parser', created_at: at(10) },
      { sha: sha('b'), segment_no: 2, rule: 'same-folder-and-time', subject: 'Fix the parser', created_at: at(20) },
      { sha: sha('c'), segment_no: 2, rule: 'same-folder-and-time', subject: 'Fix the parser tests', created_at: at(21) },
    ],
    uncommitted: { commands: 1, started_at: at(40), ended_at: at(50), unplaced: false, places: [{ worktree: 'app', branch: 'feature', rule: 'recorded-by-tool' }] } },
  { provider: 'claude', session_id: 'child-session', sub: true, parent: 'parent-session', revision: 1, body_hash: hash, records: 2, commands: 0, segments: 0,
    machine: 'laptop', started_at: at(15), content: false, commits: [], spawned_in: { segment_no: 2, rule: 'tool-call' }, updated_at: at(55),
    uncommitted: { commands: 0, unplaced: true, places: [] } },
];
const segments = [
  { first: 'm1', last: 'm2', first_ordinal: 0, last_ordinal: 1, commands: 1, events: 20, closes_turn: false, commits: [{ sha: sha('a'), rule: 'printed-number', subject: 'Add the parser' }] },
  { first: 'm3', last: 'm4', first_ordinal: 2, last_ordinal: 3, commands: 1, first_event: 20, events: 5, closes_turn: true,
    commits: [{ sha: sha('b'), rule: 'same-folder-and-time', subject: 'Fix the parser' }, { sha: sha('c'), rule: 'same-folder-and-time', subject: 'Fix the parser tests' }] },
];
const session = {
  repo_id: 'repo', provider: 'claude', session_id: 'parent-session', uploader: 'member', revision: 2, body_hash: hash, created_at: at(55), updated_at: at(55),
  body: { rule_version: 1, sub: false, scope: 'working-folder', machine: 'laptop', started_at: at(0), ended_at: at(50), records: 6, commands: 3, copied_records: 0, segments,
    uncommitted: { first: 'm5', last: 'm6', first_ordinal: 4, last_ordinal: 5, commands: 1, first_event: 25, events: 20, unplaced: false, places: [{ worktree: 'app', branch: 'feature', rule: 'recorded-by-tool' }] },
    content: { doc, events: 45 } },
};
const lifetimes = [
  { repo_id: 'repo', ledger_id: 'a'.repeat(32), name: 'feature', opened_at: at(1), opened_by: { kind: 'registered', account: 'member' }, closed_at: at(5), closed_by: { kind: 'hosting-push' }, linked: [] },
  { repo_id: 'repo', ledger_id: 'b'.repeat(32), name: 'feature', opened_at: at(30), opened_by: { kind: 'registered', account: 'member' }, linked: [] },
];

// What the server read from the provider: main z - a - e (merge of pull request 7: b, c), and d on a branch no session worked on.
const remoteCommit = (letter: string, minute: number, subject: string, ...parents: string[]) =>
  ({ sha: sha(letter), parents: parents.map(sha), author_name: 'Dev', authored_at: at(minute), committed_at: at(minute), subject });
const remotePages: Record<string, unknown> = {
  '': { default_branch: 'main', synced_at: at(58), next: 'older',
    branches: [{ name: 'main', sha: sha('e') }, { name: 'feature', sha: sha('c') }, { name: 'quiet', sha: sha('d') }],
    pulls: [{ number: 7, title: 'Fix the parser', state: 'merged', head_ref: 'feature', head_sha: sha('c'), base_ref: 'main', merge_sha: sha('e'), commits: [sha('b'), sha('c')], updated_at: at(57) }],
    commits: [remoteCommit('a', 10, 'Add the parser', '0'), remoteCommit('b', 20, 'Fix the parser', 'a'), remoteCommit('c', 21, 'Fix the parser tests', 'b'),
      remoteCommit('d', 30, 'Quiet change', 'a'), remoteCommit('e', 40, 'Merge pull request', 'a', 'c')] },
  older: { commits: [remoteCommit('0', 1, 'Start the project')] },
};
const reportedWorktrees = [
  { machine: 'laptop', name: 'app', branch: 'feature', removed: false, first_seen_at: at(0), last_seen_at: at(55) },
  { machine: 'laptop', name: 'scratch', removed: true, first_seen_at: at(0), last_seen_at: at(5) },
];

function fixture(role: string, reads: string[], withRemote = false) {
  return ({ method, pathname, searchParams }: { method: string; pathname: string; searchParams: URLSearchParams }) => {
    if (method !== 'GET') return undefined;
    if (pathname === '/api/v1/me') return { body: { id: 'member', username: 'alice', locale: 'en' } };
    if (pathname === '/api/v1/repositories') return { body: [{ id: 'repository', name: 'app', slug: 'app', owner_username: 'alice', visibility: 'private', effective_role: role }] };
    if (pathname === '/api/v1/repositories/repository/members') return { body: [{ repository_id: 'repository', user_id: 'member', role }] };
    if (pathname === '/api/v1/repositories/repository/invites') return { body: [] };
    if (pathname === '/api/v1/repos') return { body: [{ id: 'repo', default_branch: 'main', remote_url: 'https://example.test/alice/app' }] };
    const resource = decodeURIComponent(pathname).match(/^\/api\/v1\/repos\/repo\/(.+)$/)?.[1];
    if (!resource) return undefined;
    if (['refs', 'snapshots', 'pending', 'unsync', 'history', 'reflog'].includes(resource)) return { body: [] };
    if (resource.startsWith('settings/') || resource === 'secrets') return { body: null };
    if (['branch-lifetimes', 'worktrees', 'remote-git'].includes(resource) || resource.startsWith('anchored-')) reads.push(resource);
    if (resource === 'worktrees') return { body: withRemote ? reportedWorktrees : [] };
    if (resource === 'anchored-notes') return { body: [] };
    if (resource === 'anchored-search') {
      return { body: searchParams.get('q') === 'parser' ? { truncated: false, hits: [
        { kind: 'pull', title: 'Fix the parser', pull: 7, branch: 'feature' },
        { kind: 'commit', title: 'Add the parser', commit: sha('a'), at: at(10) },
        { kind: 'segment', title: 'assistant', snippet: 'Fixed the parser.', provider: 'claude', session_id: 'parent-session', segment_no: 2, event: 21 },
      ] } : { truncated: false, hits: [] } };
    }
    if (resource === 'remote-git') return { body: withRemote ? remotePages[searchParams.get('before') ?? ''] : { commits: [] } };
    if (resource === 'anchored-sessions/claude/parent-session/calls') return { body: [] };
    if (resource === 'anchored-sessions') return { body: summaries };
    if (resource === 'branch-lifetimes') return { body: lifetimes };
    if (resource === 'anchored-sessions/claude/parent-session') return { body: session };
    if (resource === `anchored-commits/${sha('b')}/summaries`) {
      return { body: [{ repo_id: 'repo', provider: 'claude', session_id: 'parent-session', commit: sha('b'), body_hash: hash,
        body: { summary: 'Fixed the parser edge case.', key_facts: ['The parser reads TOML.'], open_tasks: [] }, created_at: at(21), updated_at: at(21), segment_no: 2, uploader: 'member' }] };
    }
    if (/^anchored-commits\/[0-9a-f]{40}\/summaries$/.test(resource)) return { body: [] };
    if (resource === `anchored-commits/${sha('b')}`) return { body: [{ provider: 'claude', session_id: 'parent-session', segment_no: 2, rule: 'same-folder-and-time', segment: segments[1] }] };
    if (resource === `anchored-commits/${sha('c')}`) return { body: [{ provider: 'claude', session_id: 'parent-session', segment_no: 2, rule: 'same-folder-and-time', segment: segments[1] }] };
    // A session's conversation is read by the session, never by its document's hash.
    if (resource === 'anchored-sessions/claude/parent-session/events' && !searchParams.has('view') && searchParams.get('limit') === '20') {
      const offset = Number(searchParams.get('offset'));
      const end = Math.min(45, offset + 20);
      return { body: { offset, next: end === 45 ? -1 : end,
        events: Array.from({ length: end - offset }, (_, index) => ({ kind: 'message', role: index % 2 ? 'assistant' : 'user', seq: offset + index, event_index: offset + index,
          blocks: [{ type: 'text', text: `conversation ${offset + index}` }] })) } };
    }
    return undefined;
  };
}

test('the sessions tab shows a member their sessions by commit, branch and worktree, and opens one cut at its commits', async ({ page }) => {
  const errors = capturePageErrors(page);
  const reads: string[] = [];
  const unexpected = await installApiFixture(page, fixture('member', reads));
  const url = (search: string) => new URL(`/alice/app${search}`, origin).href;

  await page.goto('/alice/app?tab=members');
  await page.getByRole('button', { name: 'Sessions', exact: true }).click();
  await expect(page).toHaveURL(url('?tab=sessions'));
  await expect(page.locator('.tab.on')).toHaveText('Sessions');
  await expect(page.locator('.anchor-commit .anchor-commit-subject')).toHaveText(['Fix the parser tests', 'Fix the parser', 'Add the parser']);
  const nav = page.getByRole('navigation', { name: 'Worktrees and branches' });
  await expect(nav.getByRole('link', { name: /^app/ })).toBeVisible();
  await expect(nav.getByRole('link', { name: /^feature/ })).toContainText('1 earlier use');
  // A spawned session sits under its parent; one with no place says so instead of guessing one.
  await page.getByText('1 sub-session', { exact: true }).click();
  await expect(page.getByText('location unknown')).toBeVisible();

  // A commit shows the segment that ends at it; pointing at the segment marks every commit that shares it.
  await page.locator(`[data-commit="${sha('b')}"]`).click();
  await expect(page).toHaveURL(url(`?tab=sessions&commit=${sha('b')}`));
  const card = page.locator('[data-segment="claude:parent-session#2"]');
  await expect(card).toContainText('Segment 2 of 2 · linked by the same folder and time · 1 command');
  await expect(card).toContainText('Sub-sessions spawned in this segment:');
  await expect(page.locator('.anchor-commit.hl')).toHaveCount(0);
  await card.hover();
  await expect(page.locator('.anchor-commit.hl .anchor-commit-subject')).toHaveText(['Fix the parser tests', 'Fix the parser']);
  // The summary written at the commit sits with its segment, closed until asked for.
  await expect(card.getByText('The parser reads TOML.')).toBeHidden();
  await card.getByText('Summary at this commit').click();
  await expect(card.getByText('Fixed the parser edge case.')).toBeVisible();
  await expect(card.getByText('The parser reads TOML.')).toBeVisible();

  // The session opens at that segment; the earlier part is one click away.
  await card.getByRole('link', { name: 'parent-s', exact: true }).click();
  await expect(page).toHaveURL(url('?tab=sessions&session=claude%3Aparent-session&segment=2'));
  const conversation = page.getByRole('region', { name: 'Conversation' });
  await expect(conversation.getByRole('separator')).toHaveText([/^Segment 2/, 'After the last commit']);
  await expect(conversation.getByText('conversation 20', { exact: true })).toBeVisible();
  await expect(conversation.getByText('conversation 19', { exact: true })).toHaveCount(0);
  await conversation.getByRole('link', { name: 'Show from the beginning' }).click();
  await expect(page).toHaveURL(url('?tab=sessions&session=claude%3Aparent-session'));
  await expect(conversation.getByText('conversation 0', { exact: true })).toBeVisible();
  // The rest is read as the end of what is shown scrolls into view.
  for (const last of [39, 44]) {
    await conversation.getByText(`conversation ${last - (last === 39 ? 20 : 5)}`, { exact: true }).scrollIntoViewIfNeeded();
    await page.mouse.wheel(0, 20000);
    await expect(conversation.getByText(`conversation ${last}`, { exact: true })).toBeAttached();
  }
  await expect(conversation.getByRole('separator')).toHaveText([/^Segment 1/, /^Segment 2/, 'After the last commit']);
  await expect(conversation.getByRole('button', { name: 'Load more' })).toHaveCount(0);

  // Back and Forward walk the selections; a reload keeps one.
  await page.goBack();
  await expect(page).toHaveURL(url('?tab=sessions&session=claude%3Aparent-session&segment=2'));
  await page.goBack();
  await expect(page).toHaveURL(url(`?tab=sessions&commit=${sha('b')}`));
  await page.reload();
  await expect(page.locator('.anchor-commit.on .anchor-commit-subject')).toHaveText('Fix the parser');

  // A reused branch name is split into the name as it is now and its earlier use.
  await nav.getByRole('link', { name: /^feature/ }).click();
  await expect(page).toHaveURL(url('?tab=sessions&branch=feature'));
  await expect(page.locator('.anchor-use h6')).toHaveText([/^Current/, /^Earlier use/]);
  await expect(page.locator('.anchor-use').first()).toContainText('After the last commit · 1 command');
  await expect(page.locator('.anchor-use').nth(1)).toContainText('No context');
  await nav.getByRole('link', { name: /^app/ }).click();
  await expect(page).toHaveURL(url('?tab=sessions&worktree=app&machine=laptop'));
  await expect(page.getByRole('region', { name: 'Selected item' })).toContainText('Uncommitted conversations worked in this worktree.');

  // The member's own work before a push stands in one list, in place of the graph.
  await nav.getByRole('link', { name: /^On Hold/ }).click();
  await expect(page).toHaveURL(url('?tab=sessions&show=held'));
  const held = page.getByRole('region', { name: 'On Hold' });
  await expect(held).toContainText('Only you see this list.');
  await expect(held.locator('.anchor-use h6')).toHaveText([/^Not committed yet/]);
  await expect(held.locator('.anchor-card')).toHaveCount(2);
  await expect(held.locator('.anchor-card').first()).toContainText('After the last commit · 1 command');
  await expect(page.locator('.anchor-commits')).toHaveCount(0);
  await held.getByRole('link', { name: 'parent-s', exact: true }).click();
  await expect(page).toHaveURL(url('?tab=sessions&session=claude%3Aparent-session&segment=rest'));

  // A search lists what the query was found in; each hit says what it is and leads to it.
  const box = page.getByRole('searchbox', { name: 'Search' });
  await box.fill('parser');
  await box.press('Enter');
  await expect(page).toHaveURL(url('?tab=sessions&q=parser'));
  const results = page.getByRole('region', { name: 'Found for "parser"' });
  await expect(results.locator('[data-hit] > .ref-badge')).toHaveText(['PR', 'Commit', 'Segment']);
  await expect(results).toContainText('Fixed the parser.');
  await page.reload();
  await expect(box).toHaveValue('parser');
  await results.getByRole('link', { name: /parent-s · Segment 2/ }).click();
  await expect(page).toHaveURL(url('?tab=sessions&session=claude%3Aparent-session&segment=2'));
  await page.goBack();
  await box.fill('nothing like it');
  await box.press('Enter');
  await expect(page.getByText('Nothing was found.')).toBeVisible();

  expect(errors).toEqual([]);
  expect(unexpected).toEqual([]);
});

// What whoever may view the repository is given: another account's session as far as it is pushed, with its uploader's handle.
const pushedOnly = [{ ...summaries[0], session_id: 'their-session', uploader: 'other-account', uploader_name: 'bob', shared: true, machine: undefined, segments: 1,
  commits: [{ ...summaries[0].commits[0], on_remote: true }], uncommitted: undefined }];

test('a viewer is offered the sessions tab and reads what is pushed', async ({ page }) => {
  const errors = capturePageErrors(page);
  const reads: string[] = [];
  const member = fixture('viewer', reads, true);
  const unexpected = await installApiFixture(page, request => {
    if (request.method === 'GET' && request.pathname === '/api/v1/repos/repo/anchored-sessions') return { body: pushedOnly };
    return member(request);
  });
  await page.goto('/alice/app');
  await page.getByRole('button', { name: 'Sessions', exact: true }).click();
  await expect(page).toHaveURL(new URL('/alice/app?tab=sessions', origin).href);
  await expect(page.getByText('Sessions are shown as far as they are pushed.')).toBeVisible();
  const nav = page.getByRole('navigation', { name: 'Worktrees and branches' });
  // Work before a push and worktrees are a member's own.
  await expect(nav.getByRole('link', { name: /^On Hold/ })).toHaveCount(0);
  await expect(nav.getByText('Worktrees', { exact: true })).toHaveCount(0);
  await expect(nav.getByRole('link', { name: /^feature/ })).toBeVisible();
  await expect(page.locator('.anchor-card')).toContainText('bob');
  await expect(page.locator('.anchor-commit')).toHaveCount(5);
  // A commit nothing explains offers no note to one who is not a member.
  await page.locator(`[data-commit="${sha('d')}"]`).click();
  await expect(page.getByRole('region', { name: 'Selected item' })).toContainText('No context');
  await expect(page.getByRole('button', { name: 'Write a note' })).toHaveCount(0);
  // The member list is not asked for names: they come with the sessions.
  expect(reads).toContain('remote-git');
  expect(errors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('a visitor who is not signed in reads the pushed sessions of a public repository', async ({ page }) => {
  const errors = capturePageErrors(page);
  const reads: string[] = [];
  const member = fixture('viewer', reads, true);
  const unexpected = await installApiFixture(page, request => {
    const { method, pathname, searchParams } = request;
    if (method !== 'GET') return undefined;
    if (pathname === '/api/v1/me') return { status: 401, body: { error: { message: 'not signed in' } } };
    if (pathname === '/api/v1/public/repositories/alice/app') {
      return { body: { id: 'repository', name: 'app', slug: 'app', owner_username: 'alice', visibility: 'public', public_role: 'viewer', created_at: at(0) } };
    }
    if (pathname === '/api/v1/repos' && searchParams.get('repository') === 'repository') return { body: [{ id: 'repo', default_branch: 'main', remote_url: 'https://example.test/alice/app' }] };
    if (pathname === '/api/v1/repositories' || pathname.startsWith('/api/v1/repositories/')) return { status: 401, body: { error: { message: 'not signed in' } } };
    if (pathname === '/api/v1/repos/repo/anchored-sessions') return { body: pushedOnly };
    if (pathname === `/api/v1/repos/repo/anchored-commits/${sha('a')}`) {
      return { body: [{ provider: 'claude', session_id: 'their-session', segment_no: 1, rule: 'printed-number', segment: segments[0] }] };
    }
    return member(request);
  });
  await page.goto('/alice/app');
  await page.getByRole('button', { name: 'Sessions', exact: true }).click();
  await expect(page).toHaveURL(new URL('/alice/app?tab=sessions', origin).href);
  await expect(page.getByText('Sessions are shown as far as they are pushed.')).toBeVisible();
  await expect(page.locator('.anchor-card')).toContainText('bob');
  await expect(page.locator('.anchor-commit')).toHaveCount(5);
  await expect(page.getByRole('link', { name: /^On Hold/ })).toHaveCount(0);
  // A selection is part of the address here too, and a reload keeps it.
  await page.locator(`[data-commit="${sha('a')}"]`).click();
  await expect(page).toHaveURL(new URL(`/alice/app?tab=sessions&commit=${sha('a')}`, origin).href);
  await page.reload();
  await expect(page.locator('.anchor-commit.on .anchor-commit-subject')).toContainText('Add the parser');
  await expect(page.locator('[data-segment="claude:their-session#1"]')).toContainText('Segment 1 of 1');
  expect(errors).toEqual([]);
  expect(unexpected).toEqual([]);
});

/** Keeps notes the way the server does: a write answers the stored note, with its text masked. */
async function stubNotes(page: Page) {
  const state = { notes: [] as Record<string, unknown>[], fail: false, writes: [] as string[] };
  await page.route('**/api/v1/repos/repo/anchored-notes**', async route => {
    const request = route.request();
    const id = new URL(request.url()).pathname.split('/').at(-1)!;
    if (request.method() === 'GET') return route.fulfill({ json: state.notes });
    state.writes.push(`${request.method()} ${id}`);
    if (state.fail) return route.fulfill({ status: 422, json: { error: { code: 'validation', message: 'note text is empty' } } });
    if (request.method() === 'DELETE') {
      state.notes = state.notes.filter(note => note.id !== id);
      return route.fulfill({ status: 204, body: '' });
    }
    const sent = request.postDataJSON() as { target?: unknown; body: string };
    const body = sent.body.trim().replace(/ghp_[A-Za-z0-9]{36,}/g, '\u00abredacted:github-token\u00bb');
    if (request.method() === 'POST') {
      const note = { repo_id: 'repo', id: `note_${String(state.notes.length + 1).repeat(32).slice(0, 32)}`, target: sent.target, author: 'member', body, created_at: at(31), updated_at: at(31) };
      state.notes = [...state.notes, note];
      return route.fulfill({ json: note });
    }
    state.notes = state.notes.map(note => (note.id === id ? { ...note, body, updated_at: at(33) } : note));
    return route.fulfill({ json: state.notes.find(note => note.id === id) });
  });
  return state;
}

test('the graph holds commits no session made, and a note can be written where nothing explains them', async ({ page }) => {
  const errors = capturePageErrors(page);
  const unexpected = await installApiFixture(page, fixture('member', [], true));
  const notes = await stubNotes(page);
  const url = (search: string) => new URL(`/alice/app${search}`, origin).href;
  const subjects = page.locator('.anchor-commit .anchor-commit-subject');
  const detail = page.getByRole('region', { name: 'Selected item' });

  await page.goto('/alice/app?tab=sessions');
  await expect(subjects).toHaveText([/Merge pull request$/, /Quiet change$/, /Fix the parser tests$/, 'Fix the parser', 'Add the parser']);
  await expect(page.locator(`[data-commit="${sha('e')}"]`)).toContainText('main');
  await expect(page.locator(`[data-commit="${sha('e')}"]`)).toContainText('#7');
  await expect(page.locator(`[data-commit="${sha('d')}"]`)).toContainText('No context');
  await expect(page.locator(`[data-commit="${sha('b')}"]`)).toContainText('1 segment');
  await expect(page.locator('.anchor-commit svg.anchor-lanes')).toHaveCount(5);
  // Older commits are read on request.
  await page.getByRole('button', { name: 'Show older commits' }).click();
  await expect(subjects).toHaveCount(6);
  await expect(subjects.last()).toHaveText('Start the project');
  await expect(page.getByRole('button', { name: 'Show older commits' })).toHaveCount(0);

  // A commit no session made says so, and takes a note. The stored note is what the server kept: masked.
  await page.locator(`[data-commit="${sha('d')}"]`).click();
  await expect(detail).toContainText('No context');
  await detail.getByRole('button', { name: 'Write a note' }).click();
  const text = detail.getByRole('textbox', { name: 'Note text' });
  await expect(detail.getByRole('button', { name: 'Save', exact: true })).toBeDisabled();
  notes.fail = true;
  await text.fill('Applied by hand with ghp_' + 'a1B2'.repeat(9));
  await detail.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(detail.getByRole('alert')).toHaveText('note text is empty');
  await expect(text).toHaveValue(/^Applied by hand/);
  notes.fail = false;
  await detail.getByRole('button', { name: 'Save', exact: true }).click();
  const note = detail.locator('.anchor-note');
  await expect(note).toContainText('manual note');
  await expect(note.locator('.anchor-note-body')).toHaveText('Applied by hand with \u00abredacted:github-token\u00bb');
  await expect(text).toHaveCount(0);
  // Rewriting keeps the note; cancelling changes nothing.
  await note.getByRole('button', { name: 'Edit', exact: true }).click();
  await detail.getByRole('textbox', { name: 'Note text' }).fill('Discarded');
  await detail.getByRole('button', { name: 'Cancel', exact: true }).click();
  await expect(note.locator('.anchor-note-body')).toHaveText(/^Applied by hand/);
  await note.getByRole('button', { name: 'Edit', exact: true }).click();
  await detail.getByRole('textbox', { name: 'Note text' }).fill('Applied by hand; see the ticket.');
  await detail.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(note.locator('.anchor-note-body')).toHaveText('Applied by hand; see the ticket.');
  await page.reload();
  await expect(detail.locator('.anchor-note-body')).toHaveText('Applied by hand; see the ticket.');
  await detail.locator('.anchor-note').getByRole('button', { name: 'Delete', exact: true }).click();
  await expect(detail.locator('.anchor-note')).toHaveCount(0);
  expect(notes.writes.map(write => write.split(' ')[0])).toEqual(['POST', 'POST', 'PUT', 'DELETE']);
  // A commit a segment ends at takes no new note.
  await page.locator(`[data-commit="${sha('b')}"]`).click();
  await expect(detail.locator('.anchor-card')).toHaveCount(1);
  await expect(detail.getByRole('button', { name: 'Write a note' })).toHaveCount(0);

  // The merge commit leads through its pull request to the segments of the commits it merged.
  await page.locator(`[data-commit="${sha('e')}"]`).click();
  await expect(detail).toContainText('Merged pull request');
  await expect(detail.locator('.anchor-refs li')).toHaveCount(2);
  await detail.getByRole('link', { name: '#7', exact: true }).first().click();
  await expect(page).toHaveURL(url('?tab=sessions&pull=7'));
  await expect(detail).toContainText('Pull request #7');
  await expect(page.locator('.anchor-commit.in .anchor-commit-subject')).toHaveText([/Fix the parser tests$/, 'Fix the parser']);
  await detail.locator('.anchor-refs').getByRole('link', { name: /Segment 2/ }).first().click();
  await expect(page).toHaveURL(url('?tab=sessions&session=claude%3Aparent-session&segment=2'));
  await page.goBack();

  // A branch nothing explains, and a worktree no session worked in.
  const nav = page.getByRole('navigation', { name: 'Worktrees and branches' });
  await nav.getByRole('link', { name: /^quiet/ }).click();
  await expect(detail).toContainText('Commits not on main yet');
  await expect(detail.locator('.anchor-refs')).toContainText('Quiet change');
  await expect(detail.getByRole('button', { name: 'Write a note' })).toBeVisible();
  await nav.getByRole('link', { name: /^scratch/ }).click();
  await expect(page).toHaveURL(url('?tab=sessions&worktree=scratch&machine=laptop'));
  await expect(detail).toContainText('removed worktree');
  await expect(detail).toContainText('No context');
  await nav.getByRole('link', { name: /^app/ }).click();
  await expect(detail).toContainText('Checked out now:');

  expect(errors).toEqual([]);
  expect(unexpected).toEqual([]);
});
