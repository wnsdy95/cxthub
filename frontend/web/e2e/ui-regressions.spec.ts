import { expect, test } from '@playwright/test';
import { createHash } from 'node:crypto';
import { capturePageErrors, installApiFixture, type ApiRequest, type ApiResponse } from './api-fixture';

const repoId = 'repo-1';
const repositoryId = 'repository-1';

/** A public repository as someone who is not signed in reads it. It has no session yet. */
function publicRepositoryApi() {
  return ({ method, pathname, searchParams }: ApiRequest): ApiResponse | undefined => {
    if (method !== 'GET') return undefined;
    if (pathname === '/api/v1/me') {
      return { status: 401, body: { error: { message: 'anonymous fixture' } } };
    }
    if (pathname === '/api/v1/public/repositories/alice/cxthub') {
      return {
        body: {
          id: repositoryId,
          name: 'cxthub',
          slug: 'cxthub',
          owner_username: 'alice',
          visibility: 'public',
          public_role: 'viewer',
          created_at: '2026-08-01T00:00:00Z',
        },
      };
    }
    if (pathname === '/api/v1/repos' && searchParams.get('repository') === repositoryId) {
      return {
        body: [
          {
            id: repoId,
            remote_url: 'https://github.com/wnsdy95/cxthub.git',
            default_branch: 'main',
            description: 'Browser E2E fixture',
            topics: ['context'],
          },
        ],
      };
    }
    return undefined;
  };
}

test('landing renders every captured product view without placeholders', async ({ page }) => {
  const pageErrors = capturePageErrors(page);
  const unexpected = await installApiFixture(page, ({ method, pathname }) => {
    if (method === 'GET' && pathname === '/api/v1/me') {
      return { status: 401, body: { error: { message: 'anonymous fixture' } } };
    }
    return undefined;
  });

  await page.goto('/');
  await expect(page.locator('.shot-placeholder')).toHaveCount(0);
  await expect(page.locator('.product-shot img')).toHaveCount(6);
  await expect(page.locator('.landing-nav a')).toHaveText(['Auto-capture', 'Product', 'Security', 'Pricing']);

  const expected = ['setup.jpg', 'context.jpg', 'onhold.jpg', 'profile.jpg', 'security.jpg', 'permissions.jpg'];
  const images = page.locator('.product-shot img');
  for (let i = 0; i < expected.length; i += 1) {
    const image = images.nth(i);
    await image.scrollIntoViewIfNeeded();
    await expect(image).toHaveAttribute('src', new RegExp(`/landing/${expected[i]}$`));
    await expect.poll(() => image.evaluate((element) => ({
      complete: (element as HTMLImageElement).complete,
      width: (element as HTMLImageElement).naturalWidth,
      height: (element as HTMLImageElement).naturalHeight,
    }))).toEqual({ complete: true, width: 1200, height: 700 });
  }

  await page.locator('.landing-nav a').filter({ hasText: 'Pricing' }).click();
  await expect(page).toHaveURL(/\/pricing$/);
  await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(0);

  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('pricing offers free access without a billable allowance or calculator', async ({ page }) => {
  const pageErrors = capturePageErrors(page);
  const unexpected = await installApiFixture(page, ({ method, pathname }) => {
    if (method === 'GET' && pathname === '/api/v1/me') {
      return { status: 401, body: { error: { message: 'anonymous fixture' } } };
    }
    return undefined;
  });

  await page.goto('/pricing');
  await expect(page).toHaveURL(/\/pricing$/);
  await expect(page.locator('.pricing-hero h1')).toHaveText('Build with context. Start free.');
  await expect(page.locator('.pricing-free')).toContainText('$0');
  await expect(page.locator('.pricing-includes')).toContainText('No storage overage charges');
  await expect(page.locator('.pricing-notice')).toContainText('They do not create a bill.');
  await expect(page.locator('#future-plans-title')).toHaveText('Designed around how you work');
  await expect(page.locator('#pricing-storage')).toHaveCount(0);
  await expect(page.locator('.pricing-overage')).toHaveCount(0);
  await expect(page.getByText('$0.07', { exact: false })).toHaveCount(0);

  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.locator('.pricing-card').first()).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('members page orders invites, role capabilities, and members without page overflow', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const pageErrors = capturePageErrors(page);
  const unexpected = await installApiFixture(page, ({ method, pathname, searchParams }) => {
    if (method !== 'GET') return undefined;
    if (pathname === '/api/v1/me') {
      return {
        body: {
          id: 'user-1',
          email: 'alice@example.test',
          name: 'Alice',
          username: 'alice',
          nickname: 'Alice',
          locale: 'en',
        },
      };
    }
    if (pathname === '/api/v1/repositories') {
      return {
        body: [{
          id: repositoryId,
          name: 'cxthub',
          slug: 'cxthub',
          owner_id: 'user-1', effective_role: 'owner',
          owner_username: 'alice',
          visibility: 'private',
          public_role: 'viewer',
          created_at: '2026-08-01T00:00:00Z',
        }],
      };
    }
    if (pathname === `/api/v1/repositories/${repositoryId}/members`) {
      return {
        body: [{
          repository_id: repositoryId,
          user_id: 'user-1',
          role: 'owner',
          user: { id: 'user-1', name: 'Alice', nickname: 'Alice', email: 'alice@example.test' },
        }],
      };
    }
    if (pathname === `/api/v1/repositories/${repositoryId}/invites`) return { body: [] };
    if (pathname === '/api/v1/repos' && searchParams.get('repository') === repositoryId) return { body: [] };
    return undefined;
  });

  await page.goto('/alice/cxthub?tab=members');
  const matrix = page.locator('.role-capabilities');
  await expect(matrix).toBeVisible();
  await expect(matrix.locator('thead code')).toHaveText(['viewer', 'puller', 'member', 'maintainer', 'owner']);
  await expect(matrix.locator('tbody tr')).toHaveCount(5);
  await expect(matrix.locator('td.allowed')).toHaveCount(15);
  await expect(matrix.locator('td.denied')).toHaveCount(10);

  const correctSectionOrder = await page.locator('main.main').evaluate((main) => {
    const invites = main.querySelector('.invite-panel');
    const capabilities = main.querySelector('.role-capabilities');
    const members = main.querySelector('.members-panel');
    if (!invites || !capabilities || !members) return false;
    const follows = Node.DOCUMENT_POSITION_FOLLOWING;
    return Boolean(invites.compareDocumentPosition(capabilities) & follows)
      && Boolean(capabilities.compareDocumentPosition(members) & follows);
  });
  expect(correctSectionOrder).toBe(true);

  const overflow = await matrix.locator('.role-capabilities-scroll').evaluate((element) => ({
    client: element.clientWidth,
    scroll: element.scrollWidth,
  }));
  expect(overflow.scroll).toBeGreaterThan(overflow.client);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);

  await page.reload();
  await expect(matrix).toBeVisible();
  await page.locator('.tabs').getByRole('button', { name: /^Connected/ }).click();
  await expect(page).toHaveURL(/\/alice\/cxthub\?tab=connections$/);
  await expect(page.locator('.tab.on')).toContainText('Connected');
  await page.goBack();
  await expect(page).toHaveURL(/\/alice\/cxthub\?tab=members$/);
  await expect(matrix).toBeVisible();
  await page.goForward();
  await expect(page.locator('.tab.on')).toContainText('Connected');
  await page.locator('.tabs').getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page).toHaveURL(/\/alice\/cxthub\?tab=settings$/);
  await expect(page.locator('.permission-controls')).toBeVisible();
  await page.reload();
  await expect(page.locator('.permission-controls')).toBeVisible();
  await expect(page.locator('.role-capabilities')).toHaveCount(0);
  await page.locator('.tabs').getByRole('button', { name: /^Members/ }).click();
  await expect(page).toHaveURL(/\/alice\/cxthub\?tab=members$/);
  await expect(matrix).toBeVisible();
  await page.locator('.tabs').getByRole('button', { name: 'Sessions', exact: true }).click();
  await expect(page).toHaveURL(/\/alice\/cxthub\?tab=sessions$/);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('public repository management controls deny non-maintainers without opening dialogs', async ({ page }) => {
  const pageErrors = capturePageErrors(page);
  const unexpected = await installApiFixture(page, ({ method, pathname, searchParams }) => {
    if (method !== 'GET') return undefined;
    if (pathname === '/api/v1/me') {
      return {
        body: {
          id: 'member-1',
          email: 'member@example.test',
          name: 'Member',
          username: 'member',
          nickname: 'Member',
          locale: 'en',
        },
      };
    }
    if (pathname === '/api/v1/repositories') {
      return {
        body: [{
          id: repositoryId,
          name: 'cxthub',
          slug: 'cxthub',
          owner_id: 'owner-1', effective_role: 'member',
          owner_username: 'alice',
          visibility: 'public',
          public_role: 'viewer',
          created_at: '2026-08-01T00:00:00Z',
        }],
      };
    }
    if (pathname === `/api/v1/repositories/${repositoryId}/members`) {
      return {
        body: [{
          repository_id: repositoryId,
          user_id: 'member-1',
          role: 'member',
          user: { id: 'member-1', name: 'Member', nickname: 'Member', email: 'member@example.test' },
        }],
      };
    }
    if (pathname === '/api/v1/repos' && searchParams.get('repository') === repositoryId) {
      return {
        body: [{
          id: repoId,
          remote_url: 'https://github.com/wnsdy95/cxthub.git',
          default_branch: 'main',
        }],
      };
    }
    if (pathname.startsWith(`/api/v1/repos/${repoId}/settings/`)) return { body: null };
    if (pathname === `/api/v1/repos/${repoId}/secrets`) return { body: null };
    return undefined;
  });

  await page.goto('/alice/cxthub');
  const settingsTab = page.locator('nav.tabs').getByRole('button', { name: 'Settings', exact: true });
  await expect(settingsTab).toBeVisible();
  await settingsTab.click();
  await expect(page.getByRole('alert')).toContainText('Repository settings require owner access');
  await expect(page).toHaveURL(/\/alice\/cxthub$/);

  const teamSection = page.locator('.side-sec').filter({ hasText: 'Team defaults' });
  await teamSection.getByRole('button', { name: 'Upload team defaults' }).click();
  await expect(teamSection.getByRole('alert')).toContainText('maintainer or owner');
  await expect(page.getByRole('dialog', { name: 'Upload team defaults' })).toHaveCount(0);

  const secretsSection = page.locator('.side-sec').filter({ hasText: '.cxtsecrets' });
  await secretsSection.getByRole('button', { name: '.cxtsecrets settings' }).click();
  await expect(secretsSection.getByRole('alert')).toContainText('maintainer or owner');
  await expect(page.getByRole('dialog', { name: '.cxtsecrets settings' })).toHaveCount(0);

  await page.goto('/alice/cxthub?tab=settings');
  await expect(page.locator('.access-denied')).toContainText('Repository settings require owner access');
  await expect(page.locator('.repository-settings-form')).toHaveCount(0);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('anonymous public settings URL renders access denial instead of repository context', async ({ page }) => {
  const pageErrors = capturePageErrors(page);
  const unexpected = await installApiFixture(page, publicRepositoryApi());

  await page.goto('/alice/cxthub?tab=settings');
  await expect(page.locator('.access-denied')).toContainText('Repository settings require owner access');
  await expect(page.locator('.anchor-view')).toHaveCount(0);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('signed-in non-member stays on the public repository route and gets settings denial', async ({ page }) => {
  const pageErrors = capturePageErrors(page);
  const publicApi = publicRepositoryApi();
  const unexpected = await installApiFixture(page, (request) => {
    const { method, pathname } = request;
    if (method === 'GET' && pathname === '/api/v1/me') {
      return {
        body: {
          id: 'outsider-1',
          email: 'outsider@example.test',
          name: 'Outsider',
          username: 'outsider',
          nickname: 'Outsider',
          locale: 'en',
        },
      };
    }
    if (method === 'GET' && pathname === '/api/v1/repositories') return { body: [] };
    return publicApi(request);
  });

  await page.goto('/alice/cxthub?tab=settings');
  await expect(page).toHaveURL(/\/alice\/cxthub\?tab=settings$/);
  await expect(page.getByText('Public view', { exact: true })).toBeVisible();
  await expect(page.locator('.access-denied')).toContainText('Repository settings require owner access');
  await expect(page.locator('.app-side-left')).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Sign in' })).toHaveCount(0);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});


test('Organization owner opens repositories directly and retains audited administration', async ({ page }) => {
  const pageErrors = capturePageErrors(page);
  const organizationId = 'ent_11111111111111111111111111111111';
  const organizationRepositoryId = 'ws_11111111111111111111111111111111';
  const organization = {
    id: organizationId,
    namespace_id: 'ns_11111111111111111111111111111111',
    name: 'Acme Engineering',
    slug: 'acme',
    created_by: 'owner-1',
    created_at: '2026-09-03T00:00:00Z',
  };
  const repository = {
    id: organizationRepositoryId,
    name: 'Platform',
    slug: 'platform',
    owner_id: 'admin-1', effective_role: 'owner',
    owner_namespace_id: organization.namespace_id,
    owner_username: 'acme',
    visibility: 'private',
    created_at: '2026-09-03T00:00:00Z',
  };
  let usageReads = 0;
  let reconciled = false;
  const unexpected = await installApiFixture(page, ({ method, pathname, searchParams }) => {
    if (pathname === `/api/v1/namespaces/${organization.namespace_id}/storage` && method === 'GET') {
      usageReads++;
      return { body: { namespace_id: organization.namespace_id, policy: { plan: 'enterprise', included_bytes: 50 * 2 ** 30, pay_as_you_go: true, max_bytes: 55 * 2 ** 30, grace_bytes: 0, grace_until: null }, policy_revision: 1,
        current_bytes: (reconciled ? 49 : 56) * 2 ** 30, excess_bytes: (reconciled ? 0 : 6) * 2 ** 30, state: reconciled ? 'active' : 'read_only', metered_since: '2026-09-01T00:00:00Z', period_start: '2026-09-01T00:00:00Z', period_end: '2026-09-16T00:00:00Z', overage_byte_hours: '1073741824', entries: [] } };
    }
    if (pathname === `/api/v1/namespaces/${organization.namespace_id}/storage/reconcile` && method === 'POST') { reconciled = true; return { body: { reconciled: true } }; }
    if (method === 'GET' && pathname === '/api/v1/me') {
      return { body: { id: 'owner-1', email: 'owner@acme.test', name: 'Owner', username: 'owner', locale: 'en' } };
    }
    if (method === 'GET' && pathname === '/api/v1/public/users/acme') {
      return { status: 404, body: { error: { message: 'not a user namespace' } } };
    }
    if (method === 'GET' && pathname === '/api/v1/public/organizations/acme') {
      return { body: { ...organization, repositories: [] } };
    }
    if (method === 'GET' && pathname === '/api/v1/organizations') return { body: [organization] };
    if (method === 'GET' && pathname === `/api/v1/organizations/${organizationId}`) return { body: organization };
    if (method === 'GET' && pathname === `/api/v1/organizations/${organizationId}/members`) {
      return {
        body: [{
          organization_id: organizationId,
          user_id: 'owner-1',
          role: 'owner',
          user: { id: 'owner-1', email: 'owner@acme.test', name: 'Owner', username: 'owner' },
          created_at: '2026-09-03T00:00:00Z',
        }],
      };
    }
    if (method === 'GET' && pathname === `/api/v1/organizations/${organizationId}/policy`) {
      return {
        body: {
          organization_id: organizationId,
          repository_creation: 'admins',
          default_repository_visibility: 'private',
          allow_public_repositories: true,
          break_glass_enabled: true,
          break_glass_max_minutes: 60,
          updated_at: '2026-09-03T00:00:00Z',
        },
      };
    }
    if (method === 'GET' && pathname === `/api/v1/organizations/${organizationId}/repositories`) return { body: [repository] };
    if (method === 'GET' && pathname === '/api/v1/repositories') return { body: [repository] };
    if (method === 'GET' && pathname === `/api/v1/organizations/${organizationId}/invitations`) return {body:[]};
    if (method === 'GET' && pathname === '/api/v1/public/repositories/acme/platform') return { body: repository };
    if (method === 'GET' && pathname === '/api/v1/repos' && searchParams.get('repository') === organizationRepositoryId) {
      return { body: [] };
    }
    return undefined;
  });

  await page.goto('/acme');
  await expect(page.locator('.profile-name')).toHaveText('Acme Engineering');
  await expect(page.locator('.organization-access-note')).toContainText('owners have owner access to every repository');
  await expect(page.getByRole('tab', { name: 'People' })).toBeVisible();
  await expect(page.getByRole('tab', { name: 'Audit log' })).toBeVisible();
  await expect(page.locator('.repository-card')).toBeEnabled();
  await expect(page.getByRole('button',{name:'Emergency read access'})).toHaveCount(0);

  expect(usageReads).toBe(0);
  await page.getByRole('tab', { name: 'Storage usage' }).click();
  await expect(page.locator('.storage-state')).toHaveText('Storage limit reached');
  await expect(page.locator('.storage-totals')).toContainText('50 GiB');
  await expect(page.locator('.storage-totals')).toContainText('56 GiB');
  await expect(page.locator('.storage-usage')).toContainText('Existing history remains readable');
  await page.getByRole('button', { name: 'Reconcile from stored data' }).click();
  await expect(page.locator('.storage-state')).toHaveText('Within allowance');
  await expect(page.locator('.storage-totals')).toContainText('49 GiB');
  expect(usageReads).toBeGreaterThan(1);

	await page.getByRole('tab', { name: 'People' }).click();
	const lastOwnerRole = page.getByRole('combobox', { name: "Change Owner's Organization role" });
	await expect(lastOwnerRole).toBeDisabled();
	await expect(lastOwnerRole).toHaveAttribute('title', 'An Organization must always retain at least one Owner.');
	await expect(page.getByLabel('Email or @username')).toBeVisible();
  await expect(page.getByRole('button', {name:'Create invitation',exact:true})).toBeVisible();
	await page.getByRole('tab', { name: 'Repositories' }).click();

  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});


test('real cxtd wire renders a newly created public profile', async ({ page }, testInfo) => {
  test.skip(!process.env.CXT_E2E_FULLSTACK, 'full-stack smoke runs in CI or with CXT_E2E_FULLSTACK=1');

  const pageErrors = capturePageErrors(page);
  const api = page.context().request;
  const origin = 'http://127.0.0.1:4174';
  const mutationHeaders = { Origin: origin, 'X-Cxt-CSRF': '1' };
  const repositoryName = `Browser_E2E_${testInfo.retry}`;

  const login = await api.post('/api/v1/auth/session', {
    headers: {
      ...mutationHeaders,
      Authorization: 'Bearer dev:browser-e2e@example.test:Browser E2E',
    },
  });
  expect(login.ok()).toBe(true);

  const meResponse = await api.get('/api/v1/me');
  expect(meResponse.ok()).toBe(true);
  const me = (await meResponse.json()) as { username: string };

  const create = await api.post('/api/v1/repositories', {
    headers: mutationHeaders,
    data: { name: repositoryName },
  });
  expect(create.ok()).toBe(true);
  const repository = (await create.json()) as { id: string };

  const publish = await api.patch(`/api/v1/repositories/${encodeURIComponent(repository.id)}`, {
    headers: mutationHeaders,
    data: { visibility: 'public' },
  });
  expect(publish.ok()).toBe(true);

  const logout = await api.delete('/api/v1/auth/session', { headers: mutationHeaders });
  expect(logout.ok()).toBe(true);

  await page.goto(`/${encodeURIComponent(me.username)}`);
  await expect(page.locator('.profile-name')).toHaveText('Browser E2E');
  await expect(page.locator('.repository-card-name').filter({ hasText: repositoryName })).toHaveCount(1);
  // The profile shows repositories only: what it once counted per day were captures.
  await expect(page.locator('.contrib-legend, .act')).toHaveCount(0);
  expect(pageErrors).toEqual([]);
});

test('independent API and MCP complete remote OAuth consent, PKCE, read-only call, and revocation', async ({ page }, info) => {
  test.skip(!process.env.CXT_E2E_FULLSTACK, 'full-stack smoke runs in CI or with CXT_E2E_FULLSTACK=1');

  const pageErrors = capturePageErrors(page);
  const api = page.context().request;
  const origin = 'http://127.0.0.1:4174';
  const callback = `${origin}/e2e-mcp-callback`;
  const verifier = 'v'.repeat(64);
  const challenge = createHash('sha256').update(verifier).digest('base64url');
  const email = `mcp-e2e-${Date.now()}-${info.workerIndex}-${info.retry}@example.test`;

  const login = await api.post('/api/v1/auth/session', {
    headers: { Origin: origin, 'X-Cxt-CSRF': '1', Authorization: `Bearer dev:${email}:MCP E2E` },
  });
  expect(login.ok()).toBe(true);

  const registration = await api.post('/oauth/register', {
    data: {
      client_name: 'Codex App E2E',
      redirect_uris: [callback],
      grant_types: ['authorization_code'],
      response_types: ['code'],
      token_endpoint_auth_method: 'none',
    },
  });
  expect(registration.status()).toBe(201);
  const client = (await registration.json()) as { client_id: string };

  const authorize = new URL('/oauth/authorize', origin);
  authorize.search = new URLSearchParams({
    response_type: 'code',
    client_id: client.client_id,
    redirect_uri: callback,
    code_challenge: challenge,
    code_challenge_method: 'S256',
    resource: `${origin}/mcp`,
    scope: 'mcp:read',
    state: 'playwright-state',
  }).toString();
  await page.goto(authorize.toString());
  await expect(page).toHaveURL(/\/connect\/mcp\?request=/);
  await expect(page.getByRole('heading', { name: 'Connect read-only context to this agent?' })).toBeVisible();
  await expect(page.locator('.mcp-consent-card')).toContainText('Codex App E2E');
  await expect(page.locator('.mcp-consent-card')).toContainText(callback);
  await expect(page.locator('.mcp-consent-card')).toContainText('not verified');

  await page.route(`${callback}**`, async (route) => {
    await route.fulfill({ status: 200, contentType: 'text/html', body: '<main>OAuth callback received</main>' });
  });
  await page.getByRole('button', { name: 'Connect read-only' }).click();
  await expect(page).toHaveURL(new RegExp(`${callback.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\?`));
  const callbackURL = new URL(page.url());
  expect(callbackURL.searchParams.get('state')).toBe('playwright-state');
  const code = callbackURL.searchParams.get('code');
  expect(code).toBeTruthy();

  const tokenResponse = await api.post('/oauth/token', {
    form: {
      grant_type: 'authorization_code',
      client_id: client.client_id,
      redirect_uri: callback,
      code: code ?? '',
      code_verifier: verifier,
      resource: `${origin}/mcp`,
    },
  });
  expect(tokenResponse.ok()).toBe(true);
  const tokens = (await tokenResponse.json()) as { access_token: string; refresh_token: string; scope: string };
  expect(tokens.scope).toBe('mcp:read');

  const mcp = await api.post('/mcp', {
    headers: { Authorization: `Bearer ${tokens.access_token}`, Accept: 'application/json, text/event-stream' },
    data: { jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name: 'repository_list', arguments: {} } },
  });
  expect(mcp.ok()).toBe(true);
  const listing = await mcp.json();
  expect(listing.result.isError).not.toBe(true);
  const catalogue = JSON.parse(listing.result.content[0].text);
  expect(catalogue.repositories).toEqual([]);
  expect(catalogue.next_cursor).toBe('');

  const me = await (await api.get('/api/v1/me')).json();
  const repositoryResponse = await api.post('/api/v1/repositories', {
    headers: { Origin: origin, 'X-Cxt-CSRF': '1' }, data: { name: 'MCP_Link_Status' },
  });
  expect(repositoryResponse.ok(), await repositoryResponse.text()).toBe(true);
  const repository = await repositoryResponse.json();
  const remote = `${origin}/${me.username}/${repository.slug}`;
  const contextRepo = `sha256:${createHash('sha256').update(remote.replace(/^https?:\/\//, '').toLowerCase()).digest('hex')}`;
  const contextRepository = await api.post('/api/v1/repos', {
    headers: { Origin: origin, 'X-Cxt-CSRF': '1' },
    data: { id: contextRepo, remote_url: remote, default_branch: 'main' },
  });
  expect(contextRepository.ok(), await contextRepository.text()).toBe(true);
  // The API and the tool server answer the same thing about the new repository's sessions.
  const sessionsResponse = await api.get(`/api/v1/repos/${encodeURIComponent(contextRepo)}/anchored-sessions`);
  expect(sessionsResponse.ok()).toBe(true);
  expect(await sessionsResponse.json()).toEqual([]);
  const callTool = async (name: string) => {
    const toolResponse = await api.post('/mcp', {
      headers: { Authorization: `Bearer ${tokens.access_token}`, Accept: 'application/json, text/event-stream' },
      data: { jsonrpc: '2.0', id: name, method: 'tools/call', params: { name, arguments: { repository: contextRepo } } },
    });
    expect(toolResponse.ok()).toBe(true);
    return toolResponse.json();
  };
  const listed = await callTool('session_list');
  expect(listed.result.isError, JSON.stringify(listed)).not.toBe(true);
  const piece = JSON.parse(listed.result.content[0].text);
  expect(piece.page_complete).toBe(true);
  expect(JSON.parse(piece.json_fragment)).toEqual({ sessions: [] });
  // A tool that read captures is no longer known.
  const removedTool = await callTool('context_link_status');
  expect(removedTool.result.isError).toBe(true);
  await api.patch('/api/v1/me', {headers:{Origin:origin,'X-Cxt-CSRF':'1'},data:{locale:'en'}});
  await page.goto(`/${me.username}`);
  await page.getByRole('button', {name:'Profile menu',exact:true}).click();
  await page.getByRole('link', {name:'Account settings',exact:true}).click();
  await expect(page).toHaveURL(/\/settings\/account$/);
  await expect(page.getByText('Connected MCP applications', {exact:true})).toBeVisible();
  await expect(page.getByRole('main')).toContainText('Codex App E2E');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await page.getByRole('button', {name:'Disconnect application',exact:true}).click();
  await expect(page.getByText('No connected applications', {exact:true})).toBeVisible();
  const refresh = await api.post('/oauth/token', {form:{grant_type:'refresh_token',client_id:client.client_id,refresh_token:tokens.refresh_token,resource:`${origin}/mcp`}});
  expect(refresh.status()).toBe(400);
  const afterRevoke = await api.post('/mcp', {
    headers: { Authorization: `Bearer ${tokens.access_token}`, Accept: 'application/json, text/event-stream' },
    data: { jsonrpc: '2.0', id: 2, method: 'ping' },
  });
  expect(afterRevoke.status()).toBe(401);
  expect(pageErrors).toEqual([]);
});
