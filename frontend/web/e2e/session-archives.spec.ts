import { expect, test, type Page } from '@playwright/test';
import type { Role } from '../src/roles';
import type { HistoryEvent, SessionArchiveView, Snapshot } from '../src/types';
import { capturePageErrors, installApiFixture } from './api-fixture';

const id = (value: number) => `sha256:${String(value).repeat(64)}`;
const baseline = 'f'.repeat(40);
const snapshots: Snapshot[] = [6, 5, 4, 3, 2, 1].map(value => ({
  id: id(value), doc_hash: id(value), repo_id: 'r', provider: 'codex', branch: 'main', fidelity: 'full',
  session_id: value >= 5 ? 'uncommitted' : value === 4 ? 'unpushed' : value >= 2 ? 'committed' : 'parent',
  parents: value === 1 ? [] : [id(value === 4 || value === 5 ? 3 : value - 1)],
  message: value >= 5 ? `hook: capture ${value}` : `Snapshot ${value}`,
  created_at: `2026-10-01T0${value}:00:00Z`, author: {name: 'Alice', email: 'alice@example.test', team: ''},
}));

function archiveFor(snapshotId: string, unknown = false, source = snapshots): SessionArchiveView {
  const selected = source.find(snapshot => snapshot.id === snapshotId)!;
  const session = source.filter(snapshot => snapshot.session_id === selected.session_id);
  return {
    repo_id: 'r', key: selected.session_id!, snapshot_id: snapshotId, provider: 'codex', session_id: selected.session_id!,
    archived_at: '2026-10-02T00:00:00Z', archived_by: 'alice', latest_snapshot_id: session[0].id,
    snapshot_ids: session.map(snapshot => snapshot.id), message: session[0].message!, branch: 'main',
    author: selected.author, updated_at: session[0].created_at,
    origin: unknown ? {main_branch: 'main'} : {parent_snapshot_id: id(1), parent_session_id: 'parent', parent_provider: 'codex', main_snapshot_id: id(1), main_git_commit: baseline, main_branch: 'main'},
  };
}

async function fixture(page: Page, role: Role = 'maintainer', initial: SessionArchiveView[] = [], noBranches = false) {
  const state = {archives: initial, snapshots, head: id(3), history: [] as HistoryEvent[], fail: false, pendingRemoved: false,
    wait: undefined as Promise<void> | undefined, mutations: [] as {snapshot: string; archived: boolean}[]};
  const errors = capturePageErrors(page);
  const unexpected = await installApiFixture(page, ({method, pathname}) => {
    if (method !== 'GET') return undefined;
    if (pathname === '/api/v1/me') return {body: {id: 'alice', username: 'alice', locale: 'en'}};
    if (pathname === '/api/v1/repositories') return {body: [{id: 'repository', name: 'cxthub', slug: 'cxthub', owner_username: 'alice', visibility: 'private', effective_role: role}]};
    if (pathname === '/api/v1/repositories/repository/members') return {body: [{repository_id: 'repository', user_id: 'alice', role}]};
    if (pathname === '/api/v1/repos') return {body: [{id: 'r', default_branch: 'main', remote_url: 'https://cxthub.com/alice/cxthub'}]};
    const resource = pathname.replace('/api/v1/repos/r/', '');
    if (resource === 'view') return {body: {
      refs: noBranches ? [] : [{kind: 'branch', name: 'main', branch_id: 'main-id', repo_id: 'r', target: state.head}],
      snapshots: state.snapshots, pending: state.pendingRemoved ? [] : [{repo_id: 'r', session_id: 'uncommitted', provider: 'codex', branch: 'main', target: id(6), updated_at: snapshots[0].created_at}],
      unsync: [{repo_id: 'r', branch: 'main', user: 'alice', target: id(4), updated_at: '2026-10-01T04:00:00Z'}],
      history: state.history, reflog: [], archived_sessions: state.archives,
    }};
    if (resource.startsWith('settings/') || resource === 'secrets') return {body: null};
    if (resource.startsWith('docs/')) {
      const hash = decodeURIComponent(resource.split('/')[1]);
      const envelope = {cir_version: '2', source_provider: 'codex'};
      const events = [{kind: 'message', role: 'user', seq: 0, blocks: [{type: 'text', text: `Conversation ${hash}`}]}];
      return {body: resource.endsWith('/events') ? {hash, envelope, events, total: 1, offset: 0, next: -1, inherited: 0} : {cir: {envelope, events}}};
    }
    return undefined;
  });
  await page.route('**/api/v1/repos/r/snapshots/*/archive', async route => {
    const snapshot = decodeURIComponent(new URL(route.request().url()).pathname.split('/').at(-2)!);
    const {archived} = route.request().postDataJSON() as {archived: boolean};
    state.mutations.push({snapshot, archived});
    await state.wait;
    if (state.fail) {
      await route.fulfill({status: 503, json: {error: {message: 'Archive service unavailable'}}});
      return;
    }
    state.archives = archived ? [...state.archives, archiveFor(snapshot, false, state.snapshots)] : state.archives.filter(archive => archive.snapshot_id !== snapshot);
    await route.fulfill({json: {archived}});
  });
  return {state, errors, unexpected};
}

async function confirmAction(page: Page, action: 'Archive session' | 'Restore session') {
  await page.locator('.viewer').getByRole('button', {name: action, exact: true}).click();
  const dialog = page.getByRole('dialog', {name: action});
  await expect(dialog).toContainText('repository-wide');
  await expect(dialog).toContainText('memory');
  await expect(dialog).toContainText('branches');
  await dialog.getByRole('button', {name: action, exact: true}).click();
}

for (const [session, selected, latest] of [['committed', 2, 3], ['unpushed', 4, 4], ['uncommitted', 6, 6]] as const) {
  test(`archives and restores the whole ${session} session with authoritative reload`, async ({page}, testInfo) => {
    const {state, errors, unexpected} = await fixture(page);
    const url = `/alice/cxthub${session === 'committed' ? '' : '?tab=onhold'}`;
    await page.goto(url);
    await page.locator('.commit-row').filter({has: page.locator('code', {hasText: String(selected).repeat(10)})}).click();
    await expect(page.locator('.viewer')).toContainText(`Conversation ${id(selected)}`);
    await expect(page.locator('.viewer .dl-btns').getByRole('button', {name: '↓ raw'})).toBeVisible();
    expect(await page.getByRole('button', {name: 'Archive session', exact: true}).evaluate(element => element.previousElementSibling?.textContent)).toBe('↓ raw');
    const download = page.waitForEvent('download');
    await page.getByRole('button', {name: '↓ raw'}).click();
    expect((await download).suggestedFilename()).toBe(`${String(selected).repeat(10)}-context.json`);
    await confirmAction(page, 'Archive session');
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expect(page.locator('.viewer-head > code')).toHaveText(String(selected).repeat(10));
    await expect(page.locator('.viewer')).toContainText(`Conversation ${id(selected)}`);
    const archivedIDs = snapshots.filter(snapshot => snapshot.session_id === session).map(snapshot => snapshot.id);
    for (const archivedID of archivedIDs) {
      await expect(page.locator(`[data-graph-snapshot="${archivedID}"]`)).toHaveCount(0);
      await expect(page.locator('.commit-row code').filter({hasText: archivedID.slice(7, 17)})).toHaveCount(0);
    }
    await page.reload();
    const panel = page.locator('.graph-session-archives');
    await panel.locator(':scope > summary').click();
    await expect(panel).toContainText('Archived sessions (1)');
    expect(await panel.evaluate(element => element.previousElementSibling?.querySelector('summary')?.textContent)).toContain('Archived branches');
    await panel.locator('.graph-session-archive > summary').click();
    await expect(panel).toContainText(baseline);
    await expect(panel).toContainText('codex / parent');
    await expect(panel.locator('time')).toHaveAttribute('datetime', snapshots.find(snapshot => snapshot.id === id(latest))!.created_at);
    await expect(panel).toContainText(snapshots.find(snapshot => snapshot.id === id(latest))!.message!);
    await panel.getByRole('button', {name: `Open archived session codex / ${session}`}).click();
    await expect(page.locator('.viewer-head > code')).toHaveText(String(latest).repeat(10));
    await expect(page.locator('.session-archive-notice')).toContainText(baseline);
    for (const archivedID of archivedIDs) await expect(page.locator(`[data-graph-snapshot="${archivedID}"]`)).toHaveCount(0);
    expect(state.mutations).toEqual([{snapshot: id(selected), archived: true}]);
    for (const label of ['Parent session', 'Parent snapshot', 'Main snapshot (main)']) {
      await page.locator('.session-archive-notice dt').filter({hasText: label}).locator('+ dd').getByRole('link').click();
      await expect(page.locator('.viewer-head > code')).toHaveText('1111111111');
      await panel.getByRole('button', {name: `Open archived session codex / ${session}`}).click();
    }
    if (session === 'committed') await page.screenshot({path: testInfo.outputPath('archived-session-desktop.png'), fullPage: true});
    await confirmAction(page, 'Restore session');
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expect(panel.locator(':scope > summary')).toHaveText('Archived sessions (0)');
    await expect(page.locator('.commit-row code').filter({hasText: String(selected).repeat(10)})).toHaveCount(1);
    await expect(page.locator(`[data-graph-snapshot="${id(latest)}"]`)).toHaveCount(1);
    expect(state.mutations).toEqual([{snapshot: id(selected), archived: true}, {snapshot: id(selected), archived: false}]);
    await page.reload();
    await expect(page.locator('.graph-session-archives > summary')).toHaveText('Archived sessions (0)');
    expect(errors).toEqual([]);
    expect(unexpected).toEqual([]);
  });
}

for (const width of [1280, 390]) {
  for (const tab of ['', '?tab=onhold']) {
    test(`archived session titles independently toggle details at ${width}px ${tab || 'context'}`, async ({page}, testInfo) => {
      await page.setViewportSize({width, height: 900});
      const first = {...archiveFor(id(2)), session_id: `synthetic-session-${'a'.repeat(80)}`};
      const {state, errors, unexpected} = await fixture(page, 'maintainer', [first, archiveFor(id(6))]);
      await page.goto(`/alice/cxthub${tab}`);
      if (tab) await page.locator('.commit-row').filter({has: page.locator('code', {hasText: '4444444444'})}).click();
      const panel = page.locator('.graph-session-archives');
      await panel.locator(':scope > summary').click();
      const entries = panel.locator('.graph-session-archive');
      await expect(entries).toHaveCount(2);
      const selectedBefore = await page.locator('.viewer-head > code').textContent();
      for (const entry of await entries.all()) {
        await expect(entry.locator('summary')).toBeVisible();
        await expect(entry.locator('.session-archive-details')).toBeHidden();
        await expect(entry.locator('.graph-archive-entry')).toBeHidden();
      }
      const firstTitle = entries.nth(0).locator('summary');
      await expect(firstTitle).toHaveAttribute('title', `codex / ${first.session_id}`);
      expect(await panel.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
      await firstTitle.focus();
      await page.keyboard.press('Enter');
      await expect(entries.nth(0).locator('.session-archive-details')).toBeVisible();
      await expect(entries.nth(0).getByRole('button', {name: `Open archived session codex / ${first.session_id}`})).toBeVisible();
      await expect(entries.nth(1).locator('.session-archive-details')).toBeHidden();
      await entries.nth(1).locator('summary').click();
      await expect(entries.nth(1).locator('.session-archive-details')).toBeVisible();
      await firstTitle.focus();
      await page.keyboard.press('Space');
      await expect(entries.nth(0).locator('.session-archive-details')).toBeHidden();
      await expect(entries.nth(1).locator('.session-archive-details')).toBeVisible();
      await entries.nth(1).locator('summary').click();
      await expect(entries.nth(1).locator('.session-archive-details')).toBeHidden();
      await expect(page.locator('.viewer-head > code')).toHaveText(selectedBefore!);
      expect(state.mutations).toEqual([]);
      await page.screenshot({path: testInfo.outputPath('collapsed-archive-titles.png'), fullPage: true});
      await firstTitle.click();
      await page.reload();
      await panel.locator(':scope > summary').click();
      for (const entry of await entries.all()) {
        await expect(entry.locator('.session-archive-details')).toBeHidden();
      }
      expect(errors).toEqual([]);
      expect(unexpected).toEqual([]);
    });
  }
}

test('failed archive and restore keep selection, content and server state', async ({page}) => {
  const {state, errors, unexpected} = await fixture(page, 'owner');
  await page.goto('/alice/cxthub');
  await expect(page.locator('.viewer-head > code')).toHaveText('3333333333');
  state.fail = true;
  await confirmAction(page, 'Archive session');
  await expect(page.getByRole('dialog').getByRole('alert')).toHaveText('Archive service unavailable');
  await expect(page.locator('.viewer-head > code')).toHaveText('3333333333');
  await expect(page.locator('[data-graph-snapshot="' + id(3) + '"]')).toHaveCount(1);
  await page.getByRole('button', {name: 'Cancel', exact: true}).click();
  state.fail = false;
  await confirmAction(page, 'Archive session');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  state.fail = true;
  await confirmAction(page, 'Restore session');
  await expect(page.getByRole('dialog').getByRole('alert')).toHaveText('Archive service unavailable');
  await expect(page.locator('.viewer')).toContainText(`Conversation ${id(3)}`);
  await expect(page.locator('.session-archive-notice')).toBeAttached();
  expect(state.archives).toHaveLength(1);
  expect(errors).toEqual([]);
  expect(unexpected).toEqual([]);
});

for (const role of ['viewer', 'puller', 'member'] as const) {
  test(`${role} can read archived content but cannot archive or restore`, async ({page}) => {
    const {state, errors, unexpected} = await fixture(page, role, [archiveFor(id(2), true)]);
    await page.goto('/alice/cxthub');
    await expect(page.locator('.viewer-head > code')).toHaveText('1111111111');
    await expect(page.getByRole('button', {name: 'Archive session', exact: true})).toHaveCount(0);
    const panel = page.locator('.graph-session-archives');
    await panel.locator(':scope > summary').click();
    await panel.locator('.graph-session-archive > summary').click();
    await expect(panel.locator('dd')).toHaveText(['Unknown', 'Unknown', 'Unknown', 'Unknown']);
    await panel.getByRole('button', {name: 'Open archived session codex / committed'}).click();
    await expect(page.locator('.viewer')).toContainText(`Conversation ${id(3)}`);
    await expect(page.getByRole('button', {name: 'Restore session', exact: true})).toHaveCount(0);
    await page.getByRole('button', {name: 'On Hold', exact: true}).click();
    await page.locator('.commit-row').filter({has: page.locator('code', {hasText: '4444444444'})}).click();
    await expect(page.locator('.viewer')).toContainText(`Conversation ${id(4)}`);
    await expect(page.getByRole('button', {name: 'Archive session', exact: true})).toHaveCount(0);
    await page.locator('.graph-session-archives > summary').click();
    await page.locator('.graph-session-archive > summary').click();
    await page.getByRole('button', {name: 'Open archived session codex / committed'}).click();
    await expect(page.locator('.viewer')).toContainText(`Conversation ${id(3)}`);
    await expect(page.getByRole('button', {name: 'Restore session', exact: true})).toHaveCount(0);
    expect(state.mutations).toEqual([]);
    expect(errors).toEqual([]);
    expect(unexpected).toEqual([]);
  });
}

test('archived sessions remain readable and restorable without any branches', async ({page}) => {
  const {state, errors, unexpected} = await fixture(page, 'maintainer', [archiveFor(id(2))], true);
  await page.goto('/alice/cxthub');
  await page.locator('.graph-session-archives > summary').click();
  await page.locator('.graph-session-archive > summary').click();
  await page.getByRole('button', {name: 'Open archived session codex / committed'}).click();
  await expect(page.locator('.viewer')).toContainText(`Conversation ${id(3)}`);
  await confirmAction(page, 'Restore session');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  expect(state.archives).toEqual([]);
  expect(errors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('archived captures remain readable after their pending pointer is removed', async ({page}) => {
  const {state, errors, unexpected} = await fixture(page, 'maintainer', [archiveFor(id(6))]);
  state.pendingRemoved = true;
  for (const url of ['/alice/cxthub', '/alice/cxthub?tab=onhold']) {
    await page.goto(url);
    await page.locator('.graph-session-archives > summary').click();
    await page.locator('.graph-session-archive > summary').click();
    await page.getByRole('button', {name: 'Open archived session codex / uncommitted'}).click();
    await expect(page.locator('.viewer')).toContainText(`Conversation ${id(6)}`);
    await expect(page.getByRole('button', {name: 'Restore session', exact: true})).toBeEnabled();
    await expect(page.locator(`[data-graph-id="${id(6)}"]`)).toHaveCount(0);
  }
  expect(state.mutations).toEqual([]);
  expect(errors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('archive dialog traps focus, closes with Escape and disables actions while saving', async ({page}) => {
  const {state, errors, unexpected} = await fixture(page);
  await page.goto('/alice/cxthub');
  const trigger = page.getByRole('button', {name: 'Archive session', exact: true});
  await trigger.click();
  const dialog = page.getByRole('dialog', {name: 'Archive session'});
  await expect(dialog.getByRole('button', {name: 'Cancel', exact: true})).toBeFocused();
  await page.keyboard.press('Shift+Tab');
  await expect(dialog.getByRole('button', {name: 'Archive session', exact: true})).toBeFocused();
  await page.keyboard.press('Tab');
  await expect(dialog.getByRole('button', {name: 'Cancel', exact: true})).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
  await expect(trigger).toBeFocused();
  let release!: () => void;
  state.wait = new Promise<void>(resolve => { release = resolve; });
  await confirmAction(page, 'Archive session');
  await expect(dialog.getByRole('status')).toHaveText('Saving session archive…');
  await expect(dialog.getByRole('button', {name: 'Archive session', exact: true})).toBeDisabled();
  await expect(dialog.getByRole('button', {name: 'Cancel', exact: true})).toBeDisabled();
  await page.keyboard.press('Escape');
  await expect(dialog).toBeVisible();
  release();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole('button', {name: 'Restore session', exact: true})).toBeFocused();
  expect(errors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('archiving a merged session hides captures without erasing PR evidence or bridging hidden parents', async ({page}) => {
  const {state, errors, unexpected} = await fixture(page);
  state.head = id(7);
  state.snapshots = [{...snapshots[0], id: id(7), doc_hash: id(7), session_id: 'current', parents: [id(3)], message: 'Current main', created_at: '2026-10-01T07:00:00Z'},
    ...snapshots.map(snapshot => snapshot.session_id === 'committed' ? {...snapshot, branch: 'topic'} : snapshot)];
  const birth: HistoryEvent = {repo_id: 'r', id: 'topic-birth', branch: 'topic', branch_id: 'topic-id', kind: 'birth',
    source: id(1), target: id(1), created_at: '2026-10-01T01:30:00Z'};
  state.history = [birth, {...birth, id: 'topic-merge', kind: 'pr-merge', branch: 'main', branch_id: 'main-id', source_branch_id: 'topic-id',
    source: id(3), target: id(3), shared_target: id(1), pr_completed: true, created_at: '2026-10-01T03:30:00Z',
    pr: {number: 42, head_branch: 'topic', base_branch: 'main', head_sha: 'a'.repeat(40), merge_sha: 'b'.repeat(40)}}];
  await page.goto('/alice/cxthub');
  const evidence = page.locator('.graph-merge-records');
  await evidence.locator('summary').click();
  const before = await evidence.innerText();
  const operations = await page.locator('[data-graph-event]').evaluateAll(elements => elements.map(element => element.getAttribute('data-graph-id')));
  await page.locator('.commit-row').filter({has: page.locator('code', {hasText: '2222222222'})}).click();
  await confirmAction(page, 'Archive session');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(page.locator(`[data-graph-id="${id(2)}"], [data-graph-id="${id(3)}"]`)).toHaveCount(0);
  await expect(evidence).toHaveText(before, {useInnerText: true});
  expect(await page.locator('[data-graph-event]').evaluateAll(elements => elements.map(element => element.getAttribute('data-graph-id')))).toEqual(operations);
  await expect(page.locator('.graph-folded-edges')).toBeVisible();
  await expect(page.locator('.graph-status-item.pushed')).toContainText('Pushed 2');
  await expect(evidence.locator('[data-branch-lineage="natural"]')).toContainText('PR #42');
  await evidence.getByRole('button', {name: 'View merged context', exact: true}).click();
  await expect(page.locator('.viewer')).toContainText(`Conversation ${id(3)}`);
  await expect(page.getByRole('button', {name: 'Restore session', exact: true})).toBeEnabled();
  expect(state.mutations).toHaveLength(1);
  expect(errors).toEqual([]);
  expect(unexpected).toEqual([]);
});
