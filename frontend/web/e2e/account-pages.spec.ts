import { expect, test, type Page } from '@playwright/test';
import { capturePageErrors, installApiFixture } from './api-fixture';
import { ko } from '../src/i18n/locales/ko';

async function accountFixture(page: Page, options: { empty?: boolean; anonymous?: boolean; failList?: boolean; populated?: boolean; locale?: 'en' | 'ko' } = {}) {
  let user = { id: 'account-user', username: 'alice', name: 'Alice', nickname: '', email: 'alice@example.test', locale: options.locale ?? 'en', created_at: '2026-01-01T00:00:00Z' };
  const organization = { id: 'org-1', namespace_id: 'ns-1', name: 'Acme Engineering', slug: 'acme', effective_role: 'owner', created_by: user.id, created_at: user.created_at };
  const enterprise = { id: 'enterprise-1', name: 'Acme Group', slug: 'acme-group', created_by: user.id, created_at: user.created_at, policy: { repository_creation: 'members', allow_public_repositories: true, allow_break_glass: true } };
  let authenticated = !options.anonymous;
  let failList = Boolean(options.failList);
  const writes: Array<{ pathname: string; body: unknown }> = [];
  const reads: string[] = [];
  const unexpected = await installApiFixture(page, ({ method, pathname }) => {
    if (method === 'GET') reads.push(pathname);
    if (pathname === '/api/v1/me') return authenticated ? { body: user } : { status: 401, body: { error: { message: 'Sign in required' } } };
    if (pathname === '/api/v1/auth/session' && method === 'DELETE') { authenticated = false; return { body: { status: 'ok' } }; }
    if (pathname === '/api/v1/organizations') return failList ? { status: 403, body: { error: { message: 'Cannot load organizations' } } } : { body: options.empty ? [] : [organization] };
    if (pathname === '/api/v1/enterprises') return { body: options.empty ? [] : [enterprise] };
    if (pathname === '/api/v1/github/connections') return { body: { enabled: false, identity: null, owners: [] } };
    if (options.populated) {
      const credential = { suffix: 'test-device', label: 'Alice’s development laptop', created_at: user.created_at, expires_at: '2027-01-01T00:00:00Z' };
      if (pathname === '/api/v1/me/cli-tokens') return { body: [credential] };
      if (pathname === '/api/v1/me/sessions') return { body: [{ ...credential, current: true }] };
      if (pathname === '/api/v1/me/mcp-applications') return { body: [{ client_id: 'test-client-' + 'a'.repeat(80), name: 'Coding assistant', scope: 'mcp:read', created_at: user.created_at, expires_at: credential.expires_at }] };
    }
    if (pathname === `/api/v1/public/users/${user.username}`) return { body: { user, repositories: [] } };
    if (pathname.endsWith('/contributions')) return { body: { total: 0, days: [] } };
    if (pathname.endsWith('/activity')) return { body: [] };
    if (pathname.startsWith('/api/v1/public/users/')) return { status: 404, body: { error: { message: 'Not found' } } };
    if (pathname === `/api/v1/public/organizations/${organization.slug}`) return { body: { ...organization, repositories: [] } };
    if (pathname.startsWith('/api/v1/public/organizations/')) return { status: 404, body: { error: { message: 'Not found' } } };
    if (pathname === `/api/v1/organizations/${organization.id}`) return { body: organization };
    if (pathname === `/api/v1/organizations/${organization.id}/members`) return { body: [{ organization_id: organization.id, user_id: user.id, role: 'owner', user }] };
    if (pathname === `/api/v1/organizations/${organization.id}/policy`) return { body: { organization_id: organization.id, repository_creation: 'admins', default_repository_visibility: 'private', default_repository_role: '', allow_public_repositories: true } };
    if (pathname === `/api/v1/enterprises/${enterprise.slug}`) return { body: enterprise };
    if (pathname === `/api/v1/enterprises/${enterprise.id}/members`) return { body: [{ enterprise_id: enterprise.id, user_id: user.id, role: 'owner', user }] };
    if (pathname === `/api/v1/enterprises/${enterprise.id}/organizations` || pathname === `/api/v1/enterprises/${enterprise.id}/github-connections`) return { body: [] };
    if (['/api/v1/repositories', `/api/v1/organizations/${organization.id}/repositories`, '/api/v1/me/cli-tokens', '/api/v1/me/sessions', '/api/v1/me/mcp-applications', '/api/v1/me/audit'].includes(pathname)) return { body: [] };
    return undefined;
  });
  await page.route('**/api/v1/**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (request.method() === 'PATCH' && pathname === '/api/v1/me') {
      const body = request.postDataJSON();
      writes.push({ pathname, body });
      if (body.username === 'taken') {
        await route.fulfill({ status: 409, json: { error: { message: 'conflict' } } });
      } else {
        user = { ...user, ...body };
        user.username = user.username.toLowerCase();
        await route.fulfill({ json: user });
      }
      return;
    }
    if (request.method() === 'POST' && (pathname === '/api/v1/organizations' || pathname === '/api/v1/enterprises')) {
      const body = request.postDataJSON();
      writes.push({ pathname, body });
      const space = pathname === '/api/v1/organizations' ? organization : enterprise;
      Object.assign(space, body);
      await route.fulfill({ json: space });
      return;
    }
    await route.fallback();
  });
  return { unexpected, writes, reads, recover: () => { failList = false; } };
}

async function followMenu(page: Page, name: string) {
  await page.getByRole('button', { name: 'Profile menu', exact: true }).click();
  await page.getByRole('navigation', { name: 'Profile menu', exact: true }).getByRole('link', { name, exact: true }).click();
}

test('profile menu opens four real pages with reload and browser history', async ({ page }, testInfo) => {
  const errors = capturePageErrors(page);
  const fixture = await accountFixture(page);
  await page.goto('/alice');
  await expect(page.locator('.profile-name')).toHaveText('Alice');
  await expect(page.locator('main')).not.toContainText('Acme Engineering');
  await expect(page.locator('.organization-create-form, details.organization-create')).toHaveCount(0);
  expect(fixture.reads).not.toContain('/api/v1/enterprises');
  expect(fixture.reads).not.toContain('/api/v1/organizations');
  await followMenu(page, 'My organizations');
  await expect(page).toHaveURL(/\/settings\/organizations$/);
  await expect(page.getByRole('heading', { name: 'My organizations', exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: /Acme Engineering/ })).toBeVisible();
  await expect(page.locator('.account-create-form')).toHaveCount(0);
  await page.reload();
  await expect(page.getByRole('link', { name: /Acme Engineering/ })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath('organizations-desktop.png'), fullPage: true });
  await followMenu(page, 'My enterprises');
  await expect(page).toHaveURL(/\/settings\/enterprises$/);
  await expect(page.getByRole('heading', { name: 'My enterprises', exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: /Acme Group/ })).toBeVisible();
  await page.goBack();
  await expect(page.getByRole('heading', { name: 'My organizations', exact: true })).toBeVisible();
  await page.goForward();
  await expect(page.getByRole('heading', { name: 'My enterprises', exact: true })).toBeVisible();
  await followMenu(page, 'Account settings');
  await expect(page).toHaveURL(/\/settings\/account$/);
  await expect(page.getByRole('heading', { name: 'Account settings', exact: true })).toBeVisible();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await page.reload();
  await expect(page.getByLabel('Username — URL handle', { exact: true })).toHaveValue('alice');
  await followMenu(page, 'My profile');
  await expect(page).toHaveURL(/\/alice$/);
  await page.getByRole('link', { name: 'Edit profile', exact: true }).click();
  await expect(page).toHaveURL(/\/settings\/account$/);
  expect(errors).toEqual([]);
  expect(fixture.unexpected).toEqual([]);
});

test('account page saves without a modal and refreshes the profile and its URL', async ({ page }, testInfo) => {
  const errors = capturePageErrors(page);
  const fixture = await accountFixture(page);
  await page.goto('/alice');
  await page.getByRole('link', { name: 'Edit profile', exact: true }).click();
  const save = page.getByRole('button', { name: 'Save', exact: true });
  await expect(save).toBeDisabled();
  await page.getByLabel('Nickname — display name', { exact: true }).fill('Alice Updated');
  await page.getByLabel('Username — URL handle', { exact: true }).fill('taken');
  await save.click();
  await expect(page.getByRole('alert')).toContainText('already taken');
  await expect(page.getByLabel('Nickname — display name', { exact: true })).toHaveValue('Alice Updated');
  await page.getByLabel('Username — URL handle', { exact: true }).fill('Alice-Updated');
  await save.click();
  await expect(page.getByRole('status').filter({ hasText: 'Saved' })).toBeVisible();
  await expect(save).toBeDisabled();
  await expect(page.getByLabel('Username — URL handle', { exact: true })).toHaveValue('alice-updated');
  await expect(page).toHaveURL(/\/settings\/account$/);
  await expect(page.getByText('Connected MCP applications', { exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Issue token', exact: true })).toBeVisible();
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.screenshot({ path: testInfo.outputPath('account-settings-desktop.png'), fullPage: true });
  await followMenu(page, 'My profile');
  await expect(page).toHaveURL(/\/alice-updated$/);
  await expect(page.locator('.profile-name')).toHaveText('Alice Updated');
  await page.goBack();
  await expect(page.getByLabel('Nickname — display name', { exact: true })).toHaveValue('Alice Updated');
  expect(fixture.writes.at(-1)?.body).toEqual({ username: 'Alice-Updated', nickname: 'Alice Updated' });
  expect(errors).toEqual([]);
  expect(fixture.unexpected).toEqual([]);
});

for (const section of ['organizations', 'enterprises'] as const) {
  test(`${section} creation uses a dedicated page and the existing API`, async ({ page }) => {
    const errors = capturePageErrors(page);
    const fixture = await accountFixture(page, { empty: true });
    const isOrganization = section === 'organizations';
    await page.goto(`/settings/${section}`);
    await expect(page.locator('main .empty-box')).toBeVisible();
    await page.getByRole('link', { name: isOrganization ? 'Create an Organization' : 'Create enterprise', exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`/settings/${section}/new$`));
    await page.reload();
    const form = page.locator('.account-create-form');
    await expect(form).toBeVisible();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await form.getByRole('textbox').nth(0).fill('New Space');
    await form.getByRole('textbox').nth(1).fill('new-space');
    await form.getByRole('button', { name: 'Create', exact: true }).click();
    await expect(page).toHaveURL(new RegExp(isOrganization ? '/new-space$' : '/enterprises/new-space$'));
    await expect(page.locator('.profile-name')).toHaveText('New Space');
    expect(fixture.writes).toEqual([{ pathname: `/api/v1/${section}`, body: { name: 'New Space', slug: 'new-space' } }]);
    expect(errors).toEqual([]);
    expect(fixture.unexpected).toEqual([]);
  });
}

test('list errors stay distinct from empty membership and can retry', async ({ page }) => {
  const fixture = await accountFixture(page, { failList: true });
  await page.goto('/settings/organizations');
  await expect(page.getByRole('alert')).toHaveText('Cannot load organizations', { timeout: 15_000 });
  await expect(page.getByText('You haven’t joined any organizations yet.', { exact: true })).toHaveCount(0);
  fixture.recover();
  await page.getByRole('button', { name: 'Try again', exact: true }).click();
  await expect(page.getByRole('link', { name: /Acme Engineering/ })).toBeVisible();
  expect(fixture.unexpected).toEqual([]);
});

test('mobile navigation supports keyboard dismissal and does not overflow', async ({ page }, testInfo) => {
  const errors = capturePageErrors(page);
  const fixture = await accountFixture(page);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto('/settings/account');
  const trigger = page.getByRole('button', { name: 'Profile menu', exact: true });
  await trigger.focus();
  await page.keyboard.press('Enter');
  await expect(trigger).toHaveAttribute('aria-expanded', 'true');
  await page.keyboard.press('Tab');
  await expect(page.getByRole('navigation', { name: 'Profile menu', exact: true }).getByRole('link', { name: 'My profile', exact: true })).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(trigger).toBeFocused();
  await expect(trigger).toHaveAttribute('aria-expanded', 'false');
  await trigger.click();
  await page.locator('.account-layout').click({ position: { x: 4, y: 4 } });
  await expect(trigger).toHaveAttribute('aria-expanded', 'false');
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await page.screenshot({ path: testInfo.outputPath('account-settings-mobile.png'), fullPage: true });
  await followMenu(page, 'My organizations');
  await expect(page.getByRole('link', { name: /Acme Engineering/ })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await page.screenshot({ path: testInfo.outputPath('organizations-mobile.png'), fullPage: true });
  expect(errors).toEqual([]);
  expect(fixture.unexpected).toEqual([]);
});

test('signing out from the profile menu removes private account content', async ({ page }) => {
  const fixture = await accountFixture(page);
  await page.goto('/settings/organizations');
  await expect(page.getByRole('link', { name: /Acme Engineering/ })).toBeVisible();
  await page.getByRole('button', { name: 'Profile menu', exact: true }).click();
  await page.getByRole('navigation', { name: 'Profile menu', exact: true }).getByRole('button', { name: 'Sign out', exact: true }).click();
  await expect(page.locator('.auth-card')).toBeVisible();
  await expect(page.locator('.account-main')).toHaveCount(0);
  await expect(page).toHaveURL(/\/settings\/organizations$/);
  expect(fixture.unexpected).toEqual([]);
});

for (const width of [1280, 390]) {
  for (const locale of ['en', 'ko'] as const) {
    test(`settings layout remains usable (${width}px, ${locale})`, async ({ page }, testInfo) => {
      const errors = capturePageErrors(page);
      const fixture = await accountFixture(page, { populated: true, locale });
      await page.setViewportSize({ width, height: 960 });
      await page.goto('/settings/account');
      await expect(page.getByRole('heading', { level: 1, name: locale === 'en' ? 'Account settings' : ko.settings.account })).toBeVisible();
      await expect(page.locator('#account-profile-title')).toBeVisible();
      await expect(page.locator('#account-preferences-title')).toBeVisible();
      await expect(page.getByText('Coding assistant', { exact: true })).toBeVisible();
      await expect(page.locator('.settings-slot')).toHaveCount(2);
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      expect(await page.locator('.account-field-grid').evaluate((element) => getComputedStyle(element).gridTemplateColumns.split(' ').length)).toBe(width > 700 ? 2 : 1);
      const help = page.locator('.account-context-help');
      await expect(help).not.toHaveAttribute('open');
      await help.locator('summary').click();
      await expect(help.locator('p')).toContainText('cxt config load.mode');
      await help.locator('summary').click();
      expect(fixture.reads.some((url) => url.includes('/storage'))).toBe(false);
      await page.evaluate(() => window.scrollTo(0, 0));
      await page.screenshot({ path: testInfo.outputPath(`account-polished-${locale}-${width}.png`), fullPage: true });
      if (width === 1280 && locale === 'ko') await page.screenshot({ path: testInfo.outputPath('account-preview.png') });
      await page.getByRole('link', { name: locale === 'en' ? 'Manage connection' : ko.accountUI.manageGitHub }).click();
      await expect(page).toHaveURL(/\/connect\/github$/);
      expect(errors).toEqual([]);
      expect(fixture.unexpected).toEqual([]);
    });
  }
}

for (const path of ['/settings/account', '/settings/organizations', '/settings/enterprises', '/settings/organizations/new', '/settings/enterprises/new', '/settings/repositories/new']) {
  test(`anonymous visits to ${path} require login without losing the URL`, async ({ page }) => {
    const fixture = await accountFixture(page, { anonymous: true });
    await page.goto(path);
    await expect(page.locator('.auth-card')).toBeVisible();
    await expect(page).toHaveURL(new RegExp(`${path}$`));
    await expect(page.locator('.account-main')).toHaveCount(0);
    await expect(page.locator('.repository-create-page')).toHaveCount(0);
    expect(fixture.reads).toEqual(['/api/v1/me']);
    expect(fixture.unexpected).toEqual([]);
  });
}

for (const width of [1280, 390]) {
  test(`profile create menu links to supported pages and supports keyboard dismissal (${width}px)`, async ({ page }, testInfo) => {
    const errors = capturePageErrors(page);
    const fixture = await accountFixture(page);
    await page.setViewportSize({ width, height: 844 });
    await page.goto('/alice');
    const trigger = page.getByRole('button', { name: 'Create new', exact: true });
    const menu = page.getByRole('menu', { name: 'Create new', exact: true });
    await trigger.focus();
    await trigger.press('ArrowDown');
    await expect(menu.getByRole('menuitem', { name: 'New repository' })).toBeFocused();
    await page.keyboard.press('Shift+Tab');
    await expect(trigger).toBeFocused();
    await trigger.press('ArrowDown');
    await expect(menu.getByRole('menuitem', { name: 'New repository' })).toBeFocused();
    await expect(menu.getByRole('menuitem')).toHaveCount(3);
    await page.keyboard.press('ArrowDown');
    await expect(menu.getByRole('menuitem', { name: 'New organization' })).toBeFocused();
    await page.keyboard.press('End');
    await expect(menu.getByRole('menuitem', { name: 'New enterprise' })).toBeFocused();
    await page.keyboard.press('Home');
    await expect(menu.getByRole('menuitem', { name: 'New repository' })).toBeFocused();
    await page.keyboard.press('Escape');
    await expect(trigger).toBeFocused();
    await expect(menu).toHaveCount(0);
    await trigger.press('ArrowUp');
    await expect(menu.getByRole('menuitem', { name: 'New enterprise' })).toBeFocused();
    const bounds = await menu.boundingBox();
    expect(bounds!.x).toBeGreaterThanOrEqual(0);
    expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(width);
    await page.screenshot({ path: testInfo.outputPath(`profile-plus-${width}.png`) });
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(/\/settings\/enterprises\/new$/);
    await expect(page.getByRole('heading', { name: 'Create enterprise' })).toBeVisible();
    await page.goBack();
    await trigger.click();
    await menu.getByRole('menuitem', { name: 'New organization' }).click();
    await expect(page).toHaveURL(/\/settings\/organizations\/new$/);
    await expect(page.getByRole('heading', { name: 'Create an Organization' })).toBeVisible();
    await page.goBack();
    await trigger.click();
    await page.getByRole('button', { name: 'Profile menu', exact: true }).click();
    await expect(menu).toHaveCount(0);
    await expect(page.getByRole('navigation', { name: 'Profile menu' })).toBeVisible();
    await trigger.click();
    await expect(page.getByRole('navigation', { name: 'Profile menu' })).toHaveCount(0);
    await page.locator('.profile-name').click();
    await expect(menu).toHaveCount(0);
    await trigger.click();
    await page.keyboard.press('Tab');
    await expect(menu).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Profile menu', exact: true })).toBeFocused();
    await trigger.click();
    await page.keyboard.press('Space');
    await expect(page).toHaveURL(/\/settings\/repositories\/new$/);
    await expect(page.getByRole('heading', { name: 'New repository' })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath(`new-repository-${width}.png`) });
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
    await page.getByRole('link', { name: 'Cancel', exact: true }).click();
    await expect(page).toHaveURL(/\/alice$/);
    expect(errors).toEqual([]);
    expect(fixture.unexpected).toEqual([]);
  });
}
