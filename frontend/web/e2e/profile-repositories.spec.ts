import { expect, test, type Page } from '@playwright/test';
import { capturePageErrors, installApiFixture } from './api-fixture';
import type { PublicRepository } from '../src/types';

const user = { id: 'profile-member', username: 'bob', name: 'Bob', email: 'bob@example.test', locale: 'en' };
const own: PublicRepository = { id: 'own', owner_username: 'bob', name: 'project', slug: 'project', visibility: 'private', created_at: '2026-10-01T00:00:00Z' };
const invited: PublicRepository = { ...own, id: 'invited', owner_username: 'alice' };
const team: PublicRepository = { ...own, id: 'team', owner_username: 'acme' };
const publicRepo: PublicRepository = { ...own, id: 'public', owner_username: 'alice', name: 'public-project', slug: 'public-project', visibility: 'public' };

async function profileFixture(page: Page, options: { anonymous?: boolean; failList?: boolean; repositories?: PublicRepository[] } = {}) {
  const state = { failList: options.failList ?? false, repositories: options.repositories ?? [own, invited, team] };
  const reads: string[] = [];
  const unexpected = await installApiFixture(page, ({ method, pathname }) => {
    if (method !== 'GET') return undefined;
    reads.push(pathname);
    if (pathname === '/api/v1/me') return options.anonymous ? { status: 401, body: {} } : { body: user };
    if (pathname === '/api/v1/repositories') return state.failList
      ? { status: 403, body: { error: { message: 'Cannot load repositories' } } }
      : { body: state.repositories };
    if (pathname === '/api/v1/public/users/bob') return { body: { user, repositories: state.repositories.filter(repository => repository.id === own.id) } };
    if (pathname === '/api/v1/public/users/alice') return { body: { user: { ...user, id: 'alice', username: 'alice', name: 'Alice' }, repositories: [publicRepo] } };
    if (pathname.startsWith('/api/v1/public/organizations/')) return { status: 404, body: {} };
    if (pathname.endsWith('/contributions')) return { body: { total: 0, days: [] } };
    if (pathname.endsWith('/activity')) return { body: { months: [] } };
    return undefined;
  });
  return { state, reads, unexpected };
}

test('own profile lists owned, invited and team repositories with owner-qualified names', async ({ page }, testInfo) => {
  const errors = capturePageErrors(page);
  const fixture = await profileFixture(page);
  await page.goto('/bob');
  await expect(page.locator('.repository-card-name')).toHaveText(['bob/project', 'alice/project', 'acme/project']);
  await expect(page.locator('.profile-section-head .count-badge')).toHaveText('3');
  await page.screenshot({ path: testInfo.outputPath('profile-repositories-desktop.png'), fullPage: true });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.locator('.repository-card-name')).toHaveText(['bob/project', 'alice/project', 'acme/project']);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
  await page.screenshot({ path: testInfo.outputPath('profile-repositories-mobile.png'), fullPage: true });
  expect(fixture.reads.filter(pathname => pathname === '/api/v1/repositories')).toHaveLength(1);
  expect(fixture.unexpected).toEqual([]);
  expect(errors).toEqual([]);
});

test('a member with no owned repositories still sees invited repositories and the canonical destination', async ({ page }) => {
  const fixture = await profileFixture(page, { repositories: [invited] });
  await page.goto('/bob');
  await expect(page.locator('.repository-card-name')).toHaveText('alice/project');
  await expect(page.getByText('No repositories yet.', { exact: true })).toHaveCount(0);
  await page.route('**/api/v1/repos?**', route => route.fulfill({ json: [] }));
  await page.route('**/api/v1/repositories/invited/members', route => route.fulfill({ json: [] }));
  await page.locator('.repository-card').click();
  await expect(page).toHaveURL(/\/alice\/project$/);
  expect(fixture.unexpected).toEqual([]);
});

for (const anonymous of [false, true]) {
  test(`${anonymous ? 'anonymous' : 'signed-in'} visitors only see the public profile repositories`, async ({ page }) => {
    const fixture = await profileFixture(page, { anonymous });
    await page.goto('/alice');
    await expect(page.locator('.repository-card-name')).toHaveText('alice/public-project');
    expect(fixture.reads).not.toContain('/api/v1/repositories');
    expect(fixture.unexpected).toEqual([]);
  });
}

test('switching to another profile never renders the cached private membership list', async ({ page }) => {
  const fixture = await profileFixture(page);
  await page.goto('/bob');
  await expect(page.locator('.repository-card')).toHaveCount(3);
  await page.evaluate(() => {
    history.pushState({}, '', '/alice');
    window.dispatchEvent(new PopStateEvent('popstate'));
  });
  await expect(page.locator('.profile-handle')).toHaveText('alice');
  await expect(page.locator('.repository-card-name')).toHaveText('alice/public-project');
  expect(fixture.unexpected).toEqual([]);
});

test('repository loading is not displayed as an empty membership list', async ({ page }) => {
  await profileFixture(page, { repositories: [invited] });
  let release!: () => void;
  const pending = new Promise<void>(resolve => { release = resolve; });
  await page.route('**/api/v1/repositories', async route => {
    await pending;
    await route.fulfill({ json: [invited] });
  });
  try {
    await page.goto('/bob');
    await expect(page.locator('.profile-main').getByRole('status')).toBeVisible();
    await expect(page.getByText('No repositories yet.', { exact: true })).toHaveCount(0);
    await expect(page.locator('.profile-section-head .count-badge')).toHaveCount(0);
  } finally {
    release();
  }
  await expect(page.locator('.repository-card-name')).toHaveText('alice/project');
});

test('repository lookup failures are explicit and retryable rather than silently showing only owned repositories', async ({ page }) => {
  const fixture = await profileFixture(page, { failList: true });
  await page.goto('/bob');
  await expect(page.getByRole('alert')).toContainText('Could not load your repositories.', { timeout: 15_000 });
  await expect(page.locator('.repository-card')).toHaveCount(0);
  await expect(page.locator('.profile-section-head .count-badge')).toHaveCount(0);
  fixture.state.failList = false;
  await page.getByRole('button', { name: 'Retry repositories' }).click();
  await expect(page.locator('.repository-card-name')).toHaveText(['bob/project', 'alice/project', 'acme/project']);
  await expect(page.getByRole('alert')).toHaveCount(0);
  expect(fixture.unexpected).toEqual([]);
});
