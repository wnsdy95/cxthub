import { expect, test } from '@playwright/test';
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

function fixture(role: string, reads: string[]) {
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
    if (resource === 'anchored-sessions' || resource === 'branch-lifetimes' || resource.startsWith('anchored-')) reads.push(resource);
    if (resource === 'anchored-sessions') return { body: summaries };
    if (resource === 'branch-lifetimes') return { body: lifetimes };
    if (resource === 'anchored-sessions/claude/parent-session') return { body: session };
    if (resource === `anchored-commits/${sha('b')}`) return { body: [{ provider: 'claude', session_id: 'parent-session', segment_no: 2, rule: 'same-folder-and-time', segment: segments[1] }] };
    if (resource === `anchored-commits/${sha('c')}`) return { body: [{ provider: 'claude', session_id: 'parent-session', segment_no: 2, rule: 'same-folder-and-time', segment: segments[1] }] };
    // A session's content is named by no snapshot, so only the plain pager may be asked for it.
    if (resource === `docs/${doc}/events` && !searchParams.has('projection') && searchParams.get('view') === 'all') {
      const offset = Number(searchParams.get('offset'));
      const end = Math.min(45, offset + 20);
      return { body: { hash: doc, envelope: { cir_version: '2', source_provider: 'claude' }, total: 45, offset, next: end === 45 ? -1 : end, inherited: 0,
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

  expect(errors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('the sessions tab is not offered to a viewer and reads nothing for one', async ({ page }) => {
  const errors = capturePageErrors(page);
  const reads: string[] = [];
  const unexpected = await installApiFixture(page, fixture('viewer', reads));
  await page.goto('/alice/app?tab=sessions');
  await expect(page.getByRole('alert')).toContainText('The Sessions tab requires member access.');
  await expect(page.getByRole('button', { name: 'Sessions', exact: true })).toHaveCount(0);
  expect(reads).toEqual([]);
  expect(errors).toEqual([]);
  expect(unexpected).toEqual([]);
});
