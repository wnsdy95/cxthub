import { expect, test, type Page } from '@playwright/test';
import { capturePageErrors, installApiFixture } from './api-fixture';

async function repositoryFixture(page: Page, anonymous = false) {
  const user = { id: 'owner', username: 'alice', name: 'Alice', locale: 'en' };
  const repositories = [
    { id: 'repo-main', owner_username: 'alice', name: 'cxthub', slug: 'cxthub', visibility: 'public', owner_id: user.id, effective_role: 'owner' },
    { id: 'repo-org', owner_username: 'acme', name: 'cxthub', slug: 'cxthub', visibility: 'private', owner_id: user.id, effective_role: 'owner' },
    { id: 'repo-other', owner_username: 'alice', name: 'design-system', slug: 'design-system', visibility: 'private', owner_id: user.id, effective_role: 'owner' },
  ];
  const reads: string[] = [];
  const snapshot = `sha256:${'1'.repeat(64)}`;
  const unexpected = await installApiFixture(page, ({ method, pathname, searchParams }) => {
    if (method !== 'GET') return undefined;
    reads.push(pathname);
    if (pathname === '/api/v1/me') return anonymous ? { status: 401, body: {} } : { body: user };
    if (pathname === '/api/v1/repositories') return { body: repositories };
    if (pathname === '/api/v1/public/users/alice') return { body: { user, repositories: repositories.filter(entry => entry.owner_username === 'alice') } };
    if (pathname === '/api/v1/public/organizations/alice') return { status: 404, body: {} };
    if (pathname.endsWith('/contributions')) return { body: { total: 0, days: [] } };
    if (pathname.endsWith('/activity')) return { body: [] };
    if (pathname === '/api/v1/public/repositories/alice/cxthub') return { body: repositories[0] };
    if (pathname === '/api/v1/repos') return { body: [{
      id: searchParams.get('repository'), default_branch: 'main',
      description: 'Git for AI context. Keep conversations, memory and team defaults together.',
      topics: ['context', 'collaboration'], remote_url: '',
    }] };
    if (/\/repositories\/[^/]+\/members$/.test(pathname)) return { body: [{ user_id: user.id, role: 'owner', user }] };
    const match = pathname.match(/^\/api\/v1\/repos\/([^/]+)\/(.+)$/);
    if (!match) return undefined;
    const [, repo, resource] = match;
    if (resource === 'refs') return { body: [{ kind: 'branch', name: 'main', target: snapshot, repo_id: repo }] };
    if (resource === 'snapshots') return { body: [{ id: snapshot, doc_hash: snapshot, repo_id: repo, branch: 'main', parents: [], provider: 'codex', message: 'Improve repository navigation', created_at: '2026-10-09T09:00:00Z', author: { name: 'Alice' } }] };
    if (['pending', 'history', 'reflog', 'unsync'].includes(resource)) return { body: [] };
    if (resource.startsWith('settings/') || resource === 'secrets') return { body: null };
    if (resource.startsWith('docs/') && resource.endsWith('/events')) return { body: { hash: snapshot, envelope: { cir_version: '2', source_provider: 'codex' }, events: [
      { seq: 0, kind: 'message', role: 'user', blocks: [{ type: 'text', text: 'Move repository switching into the header.' }] },
      { seq: 1, kind: 'message', role: 'assistant', blocks: [{ type: 'text', text: 'Search repositories without leaving your context.' }] },
    ], total: 2, offset: 0, next: -1, inherited: 0 } };
    return undefined;
  });
  return { repositories, reads, unexpected };
}

test('repository name opens searchable switching with keyboard, history and outside dismissal', async ({ page }, testInfo) => {
  const errors = capturePageErrors(page);
  const fixture = await repositoryFixture(page);
  await page.goto('/alice/cxthub');
  await expect(page.locator('.commit-row').first()).toContainText('Improve repository navigation');
  await expect(page.locator('.app-side-left, .repository-list')).toHaveCount(0);
  const trigger = page.getByRole('button', { name: 'Switch repository', exact: true });
  await expect(trigger).toContainText('cxthub');
  await trigger.click();
  const search = page.getByRole('combobox', { name: 'Search repositories' });
  const menu = page.getByRole('dialog', { name: 'Switch repository' });
  await expect(search).toBeFocused();
  await expect(menu.getByRole('option', { selected: true })).toHaveAttribute('title', 'alice/cxthub · Public');
  await page.screenshot({ path: testInfo.outputPath('repository-switcher.png'), fullPage: true });
  await search.fill('missing');
  await expect(menu.getByRole('option')).toHaveCount(0);
  await search.press('ArrowDown');
  await search.press('Enter');
  await expect(page).toHaveURL(/\/alice\/cxthub$/);
  await search.fill('acme/cxt');
  await expect(menu.getByRole('option')).toHaveCount(1);
  await search.press('Enter');
  await expect(page).toHaveURL(/\/acme\/cxthub$/);
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await page.goBack();
  await expect(page).toHaveURL(/\/alice\/cxthub$/);
  await trigger.click();
  await search.press('ArrowDown');
  await search.press('ArrowDown');
  await search.press('ArrowUp');
  await search.press('Enter');
  await expect(page).toHaveURL(/\/acme\/cxthub$/);
  await trigger.click();
  await search.press('Escape');
  await expect(trigger).toBeFocused();
  await expect(trigger).toHaveAttribute('aria-expanded', 'false');
  await trigger.click();
  await page.locator('.repository-head h2').click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await trigger.click();
  await page.getByRole('option', { name: 'design-system alice' }).click();
  await expect(page).toHaveURL(/\/alice\/design-system$/);
  await page.reload();
  await expect(trigger).toContainText('design-system');
  expect(errors).toEqual([]);
  expect(fixture.unexpected).toEqual([]);
});

test('profile plus menu opens a dedicated repository page and preserves creation failures', async ({ page }, testInfo) => {
  const errors = capturePageErrors(page);
  const fixture = await repositoryFixture(page);
  await page.route('**/api/v1/repositories', async (route) => {
    if (route.request().method() !== 'POST') return route.fallback();
    const { name } = route.request().postDataJSON();
    if (name === 'taken') return route.fulfill({ status: 409, json: { error: { message: 'Repository name already taken' } } });
    const created = { ...fixture.repositories[0], id: 'created', name, slug: name };
    fixture.repositories.push(created);
    await route.fulfill({ json: created });
  });
  await page.goto('/alice');
  await page.getByRole('button', { name: 'Create new', exact: true }).click();
  await page.screenshot({ path: testInfo.outputPath('profile-create-menu.png'), fullPage: true });
  await page.getByRole('menuitem', { name: 'New repository', exact: true }).click();
  await expect(page).toHaveURL(/\/settings\/repositories\/new$/);
  await expect(page.getByRole('heading', { name: 'New repository' })).toBeVisible();
  await page.reload();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  const input = page.getByRole('textbox', { name: 'New repository name', exact: true });
  const submit = page.getByRole('button', { name: 'Create', exact: true });
  await expect(input).toHaveAttribute('maxlength', '64');
  await expect(submit).toBeDisabled();
  await input.fill('bad/name');
  await expect(submit).toBeDisabled();
  await input.fill('taken');
  await submit.click();
  await expect(page.getByRole('alert')).toContainText('already taken');
  await expect(input).toHaveValue('taken');
  await input.fill('new-project');
  await submit.click();
  await expect(page).toHaveURL(/\/alice\/new-project$/);
  await expect(page.getByRole('button', { name: 'Switch repository' })).toContainText('new-project');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await page.getByRole('button', { name: 'Switch repository' }).click();
  await expect(page.locator('.crumb-menu form, .crumb-create')).toHaveCount(0);
  await page.getByRole('combobox', { name: 'Search repositories' }).press('Escape');
  await page.getByRole('button', { name: 'alice', exact: true }).click();
  await expect(page.locator('.repository-card-name').filter({ hasText: 'new-project' })).toBeVisible();
  expect(errors).toEqual([]);
  expect(fixture.unexpected).toEqual([]);
});

test('right details are collapsible without resetting context and persist on reload', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1440, height: 1000 });
  const errors = capturePageErrors(page);
  const fixture = await repositoryFixture(page);
  await page.goto('/alice/cxthub');
  const panel = page.getByRole('complementary', { name: 'Repository details' });
  await expect(panel).toContainText('Team defaults');
  await expect(panel).toContainText('.cxtsecrets');
  await expect(panel).toContainText('Save locally');
  await expect(page.locator('.ctx-side .about, .ctx-side .settings-row')).toHaveCount(0);
  await page.locator('.commit-row').first().click();
  await expect(page.locator('.viewer')).toContainText('Move repository switching');
  const main = page.locator('main');
  const before = await main.boundingBox();
  const side = await panel.boundingBox();
  expect(before!.x).toBe(0);
  expect(side!.x).toBeGreaterThanOrEqual(before!.x + before!.width - 1);
  await page.screenshot({ path: testInfo.outputPath('repository-details.png'), fullPage: true });
  const reads = fixture.reads.filter(path => path.endsWith('/refs')).length;
  await panel.getByRole('button', { name: 'Close', exact: true }).click();
  await expect(panel).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Show repository details' })).toBeFocused();
  expect((await main.boundingBox())!.width).toBeGreaterThan(before!.width + 200);
  await expect(page.locator('.viewer')).toContainText('Move repository switching');
  expect(fixture.reads.filter(path => path.endsWith('/refs')).length).toBe(reads);
  await page.reload();
  await expect(page.getByRole('button', { name: 'Show repository details' })).toBeVisible();
  await expect(panel).toHaveCount(0);
  await page.getByRole('button', { name: 'Show repository details' }).click();
  await panel.getByRole('button', { name: 'Close', exact: true }).focus();
  await page.keyboard.press('Escape');
  await expect(panel).toHaveCount(0);
  await page.getByRole('button', { name: 'Show repository details' }).click();
  await page.getByRole('button', { name: 'On Hold', exact: true }).click();
  await expect(panel).toBeVisible();
  await expect(page.locator('.tab.on')).toHaveText('On Hold');
  expect(errors).toEqual([]);
  expect(fixture.unexpected).toEqual([]);
});

test('signed-in public nonmembers retain accessible mobile header controls', async ({ page }) => {
  const fixture = await repositoryFixture(page);
  fixture.repositories.splice(0, 1);
  await page.route('**/api/v1/public/repositories/alice/cxthub', route => route.fulfill({ json: {
    id: 'public-other', owner_username: 'alice', name: 'cxthub', slug: 'cxthub', visibility: 'public',
  } }));
  for (const width of [375, 390]) {
    await page.setViewportSize({ width, height: 844 });
    await page.goto('/alice/cxthub');
    const header = page.locator('.public-topbar');
    await expect(header).toBeVisible();
    const navigation = (await header.locator('.topbar-left').boundingBox())!;
    const controls = (await header.locator('.who').boundingBox())!;
    expect(controls.y).toBeGreaterThanOrEqual(navigation.y + navigation.height);
    const trigger = header.getByRole('button', { name: 'Switch repository', exact: true });
    expect((await trigger.boundingBox())!.width).toBeGreaterThan(40);
    await trigger.click();
    await expect(page.getByRole('combobox', { name: 'Search repositories' })).toBeFocused();
    await page.keyboard.press('Escape');
    await header.getByRole('button', { name: 'Create new', exact: true }).click();
    await expect(page.getByRole('menuitem', { name: 'New repository', exact: true })).toBeVisible();
    await page.keyboard.press('Escape');
    await header.getByRole('button', { name: 'Profile menu', exact: true }).click();
    await expect(page.getByRole('link', { name: 'Account settings', exact: true })).toBeVisible();
    await page.keyboard.press('Escape');
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
  }
  expect(fixture.unexpected).toEqual([]);
});

test('public guests see only About and cannot list private repositories or team assets', async ({ page }) => {
  const fixture = await repositoryFixture(page, true);
  await page.goto('/alice/cxthub');
  const panel = page.getByRole('complementary', { name: 'Repository details' });
  await expect(panel).toContainText('About');
  await expect(panel).not.toContainText('Team defaults');
  await expect(panel).not.toContainText('.cxtsecrets');
  await expect(page.getByRole('button', { name: 'Switch repository' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Create new', exact: true })).toHaveCount(0);
  expect(fixture.reads).not.toContain('/api/v1/repositories');
  expect(fixture.reads.some(path => /\/(settings\/|secrets$)/.test(path))).toBe(false);
  expect(fixture.unexpected).toEqual([]);
});

for (const view of ['context', 'onhold']) {
  test(`${view} centers a bounded reading column through viewport and details changes`, async ({ page }, testInfo) => {
    const errors = capturePageErrors(page);
    const fixture = await repositoryFixture(page);
    await page.setViewportSize({ width: 2560, height: 1000 });
    await page.goto(`/alice/cxthub${view === 'onhold' ? '?tab=onhold' : ''}`);
    const center = page.locator('.ctx-main');
    const columns = page.locator('.ctx-cols');
    const graph = page.locator('.ctx-side');
    await expect(center).toBeVisible();
    for (const width of [2560, 1920, 1440, 1280, 1024, 900, 390]) {
      await page.setViewportSize({ width, height: 1000 });
      for (const detailsOpen of [true, false]) {
        await expect(page.locator('#repository-details-toggle')).toHaveAttribute('aria-expanded', String(detailsOpen));
        const parentBounds = (await columns.boundingBox())!;
        const centerBounds = (await center.boundingBox())!;
        const graphBounds = (await graph.boundingBox())!;
        const sideBySide = width > 1000 && parentBounds.width > 600;
        const expectedWidth = Math.min(1024, parentBounds.width - (sideBySide ? 304 : 0));
        expect(centerBounds.width).toBeCloseTo(expectedWidth, 0);
        expect(centerBounds.x).toBeCloseTo(parentBounds.x + (parentBounds.width - expectedWidth - (sideBySide ? 304 : 0)) / 2, 0);
        if (sideBySide) {
          expect(graphBounds.width).toBeCloseTo(280, 0);
          expect(graphBounds.x - centerBounds.x - centerBounds.width).toBeCloseTo(24, 0);
        } else {
          expect(graphBounds.y).toBeGreaterThanOrEqual(centerBounds.y + centerBounds.height);
        }
        expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
        if (view === 'context' && detailsOpen && [2560, 1440, 390].includes(width)) {
          await page.screenshot({ path: testInfo.outputPath(`context-width-${width}.png`), fullPage: true });
        }
        await page.locator('#repository-details-toggle').click();
      }
    }
    expect(errors).toEqual([]);
    expect(fixture.unexpected).toEqual([]);
  });
}

for (const width of [390, 900]) {
  test(`repository switcher and details fit ${width}px without page overflow`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: 844 });
    const fixture = await repositoryFixture(page);
    fixture.repositories[0].name = 'cxthub-repository-with-a-very-long-display-name';
    await page.goto('/alice/cxthub');
    await page.getByRole('button', { name: 'Switch repository' }).click();
    const bounds = await page.getByRole('dialog', { name: 'Switch repository' }).boundingBox();
    expect(bounds!.x).toBeGreaterThanOrEqual(0);
    expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(width);
    await page.screenshot({ path: testInfo.outputPath(`repository-menu-${width}.png`), fullPage: true });
    await page.getByRole('combobox', { name: 'Search repositories' }).press('Escape');
    await page.getByRole('button', { name: 'Hide repository details' }).click();
    await expect(page.locator('main')).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
    expect(fixture.unexpected).toEqual([]);
  });
}
