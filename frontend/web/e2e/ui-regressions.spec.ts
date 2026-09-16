import { expect, test, type Page } from '@playwright/test';
import { createHash } from 'node:crypto';
import { capturePageErrors, installApiFixture, type ApiRequest, type ApiResponse } from './api-fixture';

const repoId = 'repo-1';
const workspaceId = 'workspace-1';

function id(char: string): string {
  return `sha256:${char.repeat(64)}`;
}

const appendedRoot = id('a');
const graftTarget = id('b');
const pushedHead = id('c');
const unpushedHead = id('d');
const uncommittedHead = id('e');

function sessionDoc(hash: string) {
  return {
    hash,
    cir: {
      envelope: {
        cir_version: '2',
        source_provider: 'codex',
        source_model: 'gpt-5.6-sol',
        captured_at: '2026-08-31T02:16:00Z',
        git_branch: 'main',
      },
      events: [
        {
          kind: 'reasoning',
          seq: 1,
          locked: { provider: 'codex', scheme: 'encrypted', blob: 'opaque-fixture' },
        },
        {
          kind: 'message',
          role: 'user',
          seq: 2,
          blocks: [{ type: 'text', text: 'Visible fixture prompt' }],
        },
      ],
    },
  };
}

function docResponse(pathname: string, searchParams = new URLSearchParams()) {
  const paged = pathname.endsWith('/events');
  const hash = decodeURIComponent(pathname.split('/').at(paged ? -2 : -1) ?? '');
  const doc = sessionDoc(hash);
  if (!paged) return { body: doc };
  const offset = Math.max(0, Number(searchParams.get('offset') ?? 0));
  return { body: { hash, envelope: doc.cir.envelope, events: doc.cir.events.slice(offset), total: doc.cir.events.length, offset, next: -1, inherited: 0 } };
}

function publicWorkspaceApi(snapshots: unknown[], refs: unknown[], pending: unknown[] = [], unsync: unknown[] = [], reflog: unknown[] = [], history: unknown[] = []) {
  return ({ method, pathname, searchParams }: ApiRequest): ApiResponse | undefined => {
    if (method !== 'GET') return undefined;
    if (pathname === '/api/v1/me') {
      return { status: 401, body: { error: { message: 'anonymous fixture' } } };
    }
    if (pathname === '/api/v1/public/workspaces/alice/cxthub') {
      return {
        body: {
          id: workspaceId,
          name: 'cxthub',
          slug: 'cxthub',
          owner_username: 'alice',
          visibility: 'public',
          public_role: 'viewer',
          created_at: '2026-08-01T00:00:00Z',
        },
      };
    }
    if (pathname === '/api/v1/repos' && searchParams.get('workspace') === workspaceId) {
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
    if (pathname === `/api/v1/repos/${repoId}/refs`) return { body: refs };
    if (pathname === `/api/v1/repos/${repoId}/snapshots`) return { body: snapshots };
    if (pathname === `/api/v1/repos/${repoId}/pending`) return { body: pending };
    if (pathname === `/api/v1/repos/${repoId}/unsync`) return { body: unsync };
    if (pathname === `/api/v1/repos/${repoId}/reflog`) return { body: reflog };
    if (pathname === `/api/v1/repos/${repoId}/history`) return { body: history };
    if (pathname.startsWith(`/api/v1/repos/${repoId}/docs/`)) {
      return docResponse(pathname, searchParams);
    }
    if (pathname.startsWith(`/api/v1/repos/${repoId}/memories/`)) {
      return {
        body: {
          snapshot_id: graftTarget,
          summary: 'fixture memory',
          key_facts: [],
          open_tasks: [],
          provider: 'codex',
        },
      };
    }
    return undefined;
  };
}

async function openGraph(page: Page, responder: ReturnType<typeof publicWorkspaceApi>) {
  const pageErrors = capturePageErrors(page);
  const unexpected = await installApiFixture(page, responder);
  await page.goto('/alice/cxthub');
  await expect(page.locator('.graph-row').first()).toBeVisible();
  return { pageErrors, unexpected };
}

test('repository route keeps the selected context DAG across navigation and reload', async ({ page }) => {
  const pageErrors = capturePageErrors(page);
  const repositoryIds = { backend: 'repo-backend', frontend: 'repo-frontend' } as const;
  const responder = ({ method, pathname, searchParams }: ApiRequest): ApiResponse | undefined => {
    if (method !== 'GET') return undefined;
    if (pathname === '/api/v1/me') return { status: 401, body: { error: { message: 'anonymous fixture' } } };
    if (pathname === '/api/v1/public/workspaces/alice/cxthub') {
      return {
        body: {
          id: workspaceId,
          name: 'cxthub',
          slug: 'cxthub',
          owner_username: 'alice',
          visibility: 'public',
          public_role: 'viewer',
          created_at: '2026-08-01T00:00:00Z',
        },
      };
    }
    if (pathname === '/api/v1/repos' && searchParams.get('workspace') === workspaceId) {
      return {
        body: Object.entries(repositoryIds).map(([name, id]) => ({
          id,
          remote_url: `https://cxthub.com/alice/cxthub/${name}`,
          default_branch: 'main',
        })),
      };
    }
    for (const [name, repositoryId] of Object.entries(repositoryIds)) {
      const snapshotId = name === 'backend' ? id('6') : id('7');
      if (pathname === `/api/v1/repos/${repositoryId}/refs`) {
        return { body: [{ kind: 'branch', name: 'main', repo_id: repositoryId, target: snapshotId }] };
      }
      if (pathname === `/api/v1/repos/${repositoryId}/snapshots`) {
        return {
          body: [{
            id: snapshotId,
            repo_id: repositoryId,
            branch: 'main',
            parents: [],
            doc_hash: snapshotId,
            provider: 'codex',
            fidelity: 'full',
            message: `${name} context`,
            created_at: '2026-09-03T00:00:00Z',
          }],
        };
      }
      if (pathname === `/api/v1/repos/${repositoryId}/pending` || pathname === `/api/v1/repos/${repositoryId}/unsync` || pathname === `/api/v1/repos/${repositoryId}/reflog` || pathname === `/api/v1/repos/${repositoryId}/history`) {
        return { body: [] };
      }
      if (pathname.startsWith(`/api/v1/repos/${repositoryId}/docs/`)) {
        return docResponse(pathname, searchParams);
      }
    }
    return undefined;
  };
  const unexpected = await installApiFixture(page, responder);

  await page.goto('/alice/cxthub/frontend');
  await expect(page.locator('.crumb-repo')).toHaveText('frontend');
  await expect(page.locator('.commit-msg')).toContainText('frontend context');

  await page.locator('.pub-repos button').filter({ hasText: 'backend' }).click();
  await expect(page).toHaveURL(/\/alice\/cxthub\/backend$/);
  await expect(page.locator('.crumb-repo')).toHaveText('backend');
  await expect(page.locator('.commit-msg')).toContainText('backend context');

  await page.reload();
  await expect(page.locator('.crumb-repo')).toHaveText('backend');
  await expect(page.locator('.commit-msg')).toContainText('backend context');
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

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
    if (pathname === '/api/v1/workspaces') {
      return {
        body: [{
          id: workspaceId,
          name: 'cxthub',
          slug: 'cxthub',
          owner_id: 'user-1',
          owner_username: 'alice',
          visibility: 'private',
          public_role: 'viewer',
          created_at: '2026-08-01T00:00:00Z',
        }],
      };
    }
    if (pathname === `/api/v1/workspaces/${workspaceId}/members`) {
      return {
        body: [{
          workspace_id: workspaceId,
          user_id: 'user-1',
          role: 'owner',
          user: { id: 'user-1', name: 'Alice', nickname: 'Alice', email: 'alice@example.test' },
        }],
      };
    }
    if (pathname === `/api/v1/workspaces/${workspaceId}/invites`) return { body: [] };
    if (pathname === '/api/v1/repos' && searchParams.get('workspace') === workspaceId) return { body: [] };
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
  await page.locator('.tabs').getByRole('button', { name: 'Context', exact: true }).click();
  await expect(page).toHaveURL(/\/alice\/cxthub$/);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('public workspace management controls deny non-maintainers without opening dialogs', async ({ page }) => {
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
    if (pathname === '/api/v1/workspaces') {
      return {
        body: [{
          id: workspaceId,
          name: 'cxthub',
          slug: 'cxthub',
          owner_id: 'owner-1',
          owner_username: 'alice',
          visibility: 'public',
          public_role: 'viewer',
          created_at: '2026-08-01T00:00:00Z',
        }],
      };
    }
    if (pathname === `/api/v1/workspaces/${workspaceId}/members`) {
      return {
        body: [{
          workspace_id: workspaceId,
          user_id: 'member-1',
          role: 'member',
          user: { id: 'member-1', name: 'Member', nickname: 'Member', email: 'member@example.test' },
        }],
      };
    }
    if (pathname === '/api/v1/repos' && searchParams.get('workspace') === workspaceId) {
      return {
        body: [{
          id: repoId,
          remote_url: 'https://github.com/wnsdy95/cxthub.git',
          default_branch: 'main',
        }],
      };
    }
    if (pathname === `/api/v1/repos/${repoId}/refs`) {
      return { body: [{ kind: 'branch', name: 'main', repo_id: repoId, target: pushedHead }] };
    }
    if (pathname === `/api/v1/repos/${repoId}/snapshots`) {
      return {
        body: [{
          id: pushedHead,
          repo_id: repoId,
          parents: [],
          graft_parents: [],
          doc_hash: pushedHead,
          message: 'shared main head',
          author: 'Alice',
          session_id: 'session-1',
          provider: 'codex',
          models: ['gpt-5.6-sol'],
          created_at: '2026-08-31T03:00:00Z',
        }],
      };
    }
    if (pathname === `/api/v1/repos/${repoId}/pending`) return { body: [] };
    if (pathname === `/api/v1/repos/${repoId}/unsync`) return { body: [] };
    if (pathname === `/api/v1/repos/${repoId}/reflog` || pathname === `/api/v1/repos/${repoId}/history`) return { body: [] };
    if (pathname.startsWith(`/api/v1/repos/${repoId}/settings/`)) return { body: null };
    if (pathname === `/api/v1/repos/${repoId}/secrets`) return { body: null };
    if (pathname.startsWith(`/api/v1/repos/${repoId}/docs/`)) {
      return docResponse(pathname, searchParams);
    }
    return undefined;
  });

  await page.goto('/alice/cxthub');
  const settingsTab = page.locator('nav.tabs').getByRole('button', { name: 'Settings', exact: true });
  await expect(settingsTab).toBeVisible();
  await settingsTab.click();
  await expect(page.getByRole('alert')).toContainText('Workspace settings require owner access');
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
  await expect(page.locator('.access-denied')).toContainText('Workspace settings require owner access');
  await expect(page.locator('.ws-settings-form')).toHaveCount(0);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('anonymous public settings URL renders access denial instead of workspace context', async ({ page }) => {
  const pageErrors = capturePageErrors(page);
  const unexpected = await installApiFixture(page, publicWorkspaceApi([], []));

  await page.goto('/alice/cxthub?tab=settings');
  await expect(page.locator('.access-denied')).toContainText('Workspace settings require owner access');
  await expect(page.locator('.ctx-layout')).toHaveCount(0);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('signed-in non-member stays on the public workspace route and gets settings denial', async ({ page }) => {
  const pageErrors = capturePageErrors(page);
  const publicApi = publicWorkspaceApi([], []);
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
    if (method === 'GET' && pathname === '/api/v1/workspaces') return { body: [] };
    return publicApi(request);
  });

  await page.goto('/alice/cxthub?tab=settings');
  await expect(page).toHaveURL(/\/alice\/cxthub\?tab=settings$/);
  await expect(page.getByText('Public view', { exact: true })).toBeVisible();
  await expect(page.locator('.access-denied')).toContainText('Workspace settings require owner access');
  await expect(page.locator('.app-side')).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Sign in' })).toHaveCount(0);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('profile survives nullable activity arrays and keeps the legend inside the calendar', async ({ page }) => {
  const pageErrors = capturePageErrors(page);
  const unexpected = await installApiFixture(page, ({ method, pathname }) => {
    if (method !== 'GET') return undefined;
    if (pathname === '/api/v1/me') {
      return {
        body: {
          id: 'user-1',
          email: 'alice@example.test',
          name: 'JY Min',
          username: 'alice',
          nickname: 'JY Min',
          locale: 'ko',
        },
      };
    }
    if (pathname === '/api/v1/public/users/alice') {
      return {
        body: {
          user: {
            name: 'JY Min',
            username: 'alice',
            nickname: 'JY Min',
            created_at: '2026-01-01T00:00:00Z',
          },
          workspaces: [
            {
              id: workspaceId,
              name: 'cxthub',
              slug: 'cxthub',
              owner_username: 'alice',
              visibility: 'public',
              created_at: '2026-08-01T00:00:00Z',
            },
          ],
        },
      };
    }
    if (pathname === '/api/v1/public/enterprises/alice') {
      return { status: 404, body: { error: { message: 'not an Enterprise namespace' } } };
    }
    if (pathname === '/api/v1/enterprises') return { body: [] };
    if (pathname === '/api/v1/public/users/alice/contributions') {
      return { body: { total: 0, days: [] } };
    }
    if (pathname === '/api/v1/public/users/alice/activity') {
      return {
        body: {
          months: [
            {
              month: '2026-08',
              commit_total: 1,
              commit_repos: null,
              created: null,
            },
          ],
        },
      };
    }
    return undefined;
  });

  await page.goto('/alice');
  await expect(page.locator('.profile-name')).toHaveText('JY Min');
  await expect(page.locator('.act-month')).toHaveCount(1);
  await expect(page.locator('.contrib-legend')).toBeVisible();

  const bounds = await page.locator('.contrib-cal').evaluate((calendar) => {
    const legend = calendar.querySelector<HTMLElement>('.contrib-legend');
    if (!legend) throw new Error('contribution legend is not nested in the calendar');
    const calendarBox = calendar.getBoundingClientRect();
    const legendBox = legend.getBoundingClientRect();
    return { calendarRight: calendarBox.right, legendRight: legendBox.right };
  });
  expect(bounds.legendRight).toBeLessThanOrEqual(bounds.calendarRight + 0.5);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('Enterprise profile keeps administration separate from Workspace context and opens audited emergency read', async ({ page }) => {
  const pageErrors = capturePageErrors(page);
  const enterpriseId = 'ent_11111111111111111111111111111111';
  const enterpriseWorkspaceId = 'ws_11111111111111111111111111111111';
  const enterprise = {
    id: enterpriseId,
    namespace_id: 'ns_11111111111111111111111111111111',
    name: 'Acme Engineering',
    slug: 'acme',
    created_by: 'owner-1',
    created_at: '2026-09-03T00:00:00Z',
  };
  const workspace = {
    id: enterpriseWorkspaceId,
    name: 'Platform',
    slug: 'platform',
    owner_id: 'admin-1',
    owner_namespace_id: enterprise.namespace_id,
    owner_username: 'acme',
    visibility: 'private',
    created_at: '2026-09-03T00:00:00Z',
  };
  let usageReads = 0;
  let reconciled = false;
  const unexpected = await installApiFixture(page, ({ method, pathname, searchParams }) => {
    if (pathname === `/api/v1/namespaces/${enterprise.namespace_id}/storage` && method === 'GET') {
      usageReads++;
      return { body: { namespace_id: enterprise.namespace_id, policy: { plan: 'enterprise', included_bytes: 50 * 2 ** 30, pay_as_you_go: true, max_bytes: 55 * 2 ** 30, grace_bytes: 0, grace_until: null }, policy_revision: 1,
        current_bytes: (reconciled ? 49 : 56) * 2 ** 30, excess_bytes: (reconciled ? 0 : 6) * 2 ** 30, state: reconciled ? 'active' : 'read_only', metered_since: '2026-09-01T00:00:00Z', period_start: '2026-09-01T00:00:00Z', period_end: '2026-09-16T00:00:00Z', overage_byte_hours: '1073741824', entries: [] } };
    }
    if (pathname === `/api/v1/namespaces/${enterprise.namespace_id}/storage/reconcile` && method === 'POST') { reconciled = true; return { body: { reconciled: true } }; }
    if (method === 'GET' && pathname === '/api/v1/me') {
      return { body: { id: 'owner-1', email: 'owner@acme.test', name: 'Owner', username: 'owner', locale: 'en' } };
    }
    if (method === 'GET' && pathname === '/api/v1/public/users/acme') {
      return { status: 404, body: { error: { message: 'not a user namespace' } } };
    }
    if (method === 'GET' && pathname === '/api/v1/public/enterprises/acme') {
      return { body: { ...enterprise, workspaces: [] } };
    }
    if (method === 'GET' && pathname === '/api/v1/enterprises') return { body: [enterprise] };
    if (method === 'GET' && pathname === `/api/v1/enterprises/${enterpriseId}`) return { body: enterprise };
    if (method === 'GET' && pathname === `/api/v1/enterprises/${enterpriseId}/members`) {
      return {
        body: [{
          enterprise_id: enterpriseId,
          user_id: 'owner-1',
          role: 'owner',
          user: { id: 'owner-1', email: 'owner@acme.test', name: 'Owner', username: 'owner' },
          created_at: '2026-09-03T00:00:00Z',
        }],
      };
    }
    if (method === 'GET' && pathname === `/api/v1/enterprises/${enterpriseId}/policy`) {
      return {
        body: {
          enterprise_id: enterpriseId,
          workspace_creation: 'admins',
          default_workspace_visibility: 'private',
          allow_public_workspaces: true,
          break_glass_enabled: true,
          break_glass_max_minutes: 60,
          updated_at: '2026-09-03T00:00:00Z',
        },
      };
    }
    if (method === 'GET' && pathname === `/api/v1/enterprises/${enterpriseId}/workspaces`) return { body: [workspace] };
    if (method === 'GET' && pathname === '/api/v1/workspaces') return { body: [] };
    if (method === 'POST' && pathname === `/api/v1/enterprises/${enterpriseId}/break-glass`) {
      return {
        body: {
          id: 'bg_11111111111111111111111111111111',
          enterprise_id: enterpriseId,
          workspace_id: enterpriseWorkspaceId,
          user_id: 'owner-1',
          reason: 'production incident',
          created_at: '2026-09-03T00:00:00Z',
          expires_at: '2026-09-03T00:15:00Z',
        },
      };
    }
    if (method === 'GET' && pathname === '/api/v1/public/workspaces/acme/platform') return { body: workspace };
    if (method === 'GET' && pathname === '/api/v1/repos' && searchParams.get('workspace') === enterpriseWorkspaceId) {
      return { body: [] };
    }
    return undefined;
  });

  await page.goto('/acme');
  await expect(page.locator('.profile-name')).toHaveText('Acme Engineering');
  await expect(page.locator('.enterprise-access-note')).toContainText('explicit role on each Workspace');
  await expect(page.getByRole('tab', { name: 'People' })).toBeVisible();
  await expect(page.getByRole('tab', { name: 'Audit log' })).toBeVisible();
  await expect(page.locator('.ws-card')).toBeDisabled();
  await expect(page.locator('.ws-card-access')).toContainText('explicit Workspace role');

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
	const lastOwnerRole = page.getByRole('combobox', { name: "Change Owner's Enterprise role" });
	await expect(lastOwnerRole).toBeDisabled();
	await expect(lastOwnerRole).toHaveAttribute('title', 'An Enterprise must always retain at least one Owner.');
	await expect(page.locator('.enterprise-inline-form select option')).toHaveCount(3);
	await page.getByRole('tab', { name: 'Workspaces' }).click();

  await page.getByRole('button', { name: 'Emergency read access' }).click();
  await page.getByPlaceholder('Specific reason recorded in the audit log').fill('production incident');
  await page.getByRole('button', { name: 'Grant time-limited read access' }).click();
  await expect(page).toHaveURL(/\/acme\/platform$/);
  await expect(page.getByText('Emergency read-only', { exact: true })).toBeVisible();
  await expect(page.locator('.warn-red')).toContainText('every use is audited');
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('reverse-time graft stays ordered and its same-lane SVG connector renders', async ({ page }) => {
  const snapshots = [
    {
      id: graftTarget,
      repo_id: repoId,
      branch: 'main',
      parents: [],
      doc_hash: graftTarget,
      memory_hash: id('f'),
      provider: 'codex',
      fidelity: 'full',
      message: 'ship desktop app sessions and clear graph states',
      author: { name: 'Alice', email: 'alice@example.test', team: '' },
      models: ['gpt-5.6-sol'],
      created_at: '2026-08-31T04:00:00Z',
    },
    {
      id: appendedRoot,
      repo_id: repoId,
      branch: 'main',
      parents: [],
      graft_parents: [graftTarget],
      grafted: true,
      doc_hash: appendedRoot,
      provider: 'codex',
      fidelity: 'full',
      message: 'older appended session root',
      author: { name: 'Alice', email: 'alice@example.test', team: '' },
      models: ['gpt-5.6-sol'],
      created_at: '2026-08-31T02:00:00Z',
    },
  ];
  const refs = [{ kind: 'branch', name: 'main', repo_id: repoId, target: appendedRoot }];
  const { pageErrors, unexpected } = await openGraph(page, publicWorkspaceApi(snapshots, refs));

  const rows = page.locator('.graph-row');
  await expect(rows).toHaveCount(2);
  await expect(rows.nth(0)).toHaveAttribute('aria-label', /^older appended session root/);
  await expect(rows.nth(1)).toHaveAttribute('aria-label', /^ship desktop app sessions/);

  const child = rows.nth(0).locator('svg');
  const parent = rows.nth(1).locator('svg');
  await expect(child.locator('[id^="seam-out-"]')).toHaveAttribute('gradientUnits', 'userSpaceOnUse');
  await expect(child.locator('line[stroke^="url(#seam-out-"]')).toHaveCount(1);
  await expect(parent.locator('[id^="seam-in-"]')).toHaveAttribute('gradientUnits', 'userSpaceOnUse');
  await expect(parent.locator('line[stroke^="url(#seam-in-"]')).toHaveCount(1);

  const rowGap = await rows.evaluateAll((elements) => {
    const first = elements[0]?.getBoundingClientRect();
    const second = elements[1]?.getBoundingClientRect();
    if (!first || !second) throw new Error('graph rows missing');
    return second.top - first.bottom;
  });
  expect(Math.abs(rowGap)).toBeLessThanOrEqual(0.5);

  const offMainline = page.locator('.commit-row.off-mainline').filter({ hasText: 'ship desktop app sessions' });
  await expect(offMainline).toBeVisible();
  const rowBox = await offMainline.boundingBox();
  expect(rowBox?.height).toBeLessThanOrEqual(40);
  await expect(page.locator('.viewer')).toContainText('Visible fixture prompt');
  await expect(page.locator('.viewer')).not.toContainText('[reasoning]');
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('graph exposes pushed, unpushed, and uncommitted as three browser-visible tiers', async ({ page }) => {
  const snapshots = [
    {
      id: uncommittedHead,
      repo_id: repoId,
      branch: 'main',
      parents: [unpushedHead],
      doc_hash: uncommittedHead,
      provider: 'codex',
      fidelity: 'full',
      message: 'hook: active desktop session',
      created_at: '2026-08-31T05:00:00Z',
    },
    {
      id: unpushedHead,
      repo_id: repoId,
      branch: 'main',
      parents: [pushedHead],
      doc_hash: unpushedHead,
      provider: 'codex',
      fidelity: 'full',
      message: 'local commit before push',
      created_at: '2026-08-31T04:00:00Z',
    },
    {
      id: pushedHead,
      repo_id: repoId,
      branch: 'main',
      parents: [],
      doc_hash: pushedHead,
      provider: 'codex',
      fidelity: 'full',
      message: 'shared main head',
      created_at: '2026-08-31T03:00:00Z',
    },
  ];
  const refs = [{ kind: 'branch', name: 'main', repo_id: repoId, target: pushedHead }];
  const pending = [
    {
      repo_id: repoId,
      session_id: 'desktop-session',
      branch: 'main',
      provider: 'codex',
      target: uncommittedHead,
      updated_at: '2026-08-31T05:00:00Z',
    },
  ];
  const unsync = [
    {
      repo_id: repoId,
      user: 'alice',
      branch: 'main',
      target: unpushedHead,
      updated_at: '2026-08-31T04:00:00Z',
    },
  ];
  const { pageErrors, unexpected } = await openGraph(
    page,
    publicWorkspaceApi(snapshots, refs, pending, unsync),
  );

  await expect(page.locator('.graph-status-item.pushed')).toContainText('1');
  await expect(page.locator('.graph-status-item.unpushed')).toContainText('1');
  await expect(page.locator('.graph-status-item.uncommitted')).toContainText('1');
  await expect(page.locator('.uncommitted-node')).toHaveCount(1);
  await expect(page.locator('.uncommitted-divider')).toHaveCount(1);
  await expect(page.locator('.unpushed-divider')).toHaveCount(1);

  const opacity = await page
    .locator('.graph-row[aria-label^="hook: active desktop session"] svg')
    .getAttribute('opacity');
  expect(opacity).toBe('0.42');
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('branch labels exist only for visible graph lanes and share horizontal scroll with the lines', async ({ page }) => {
  const hash = (value: number): string => `sha256:${value.toString(16).padStart(64, '0')}`;
  const mainSnapshots = Array.from({ length: 24 }, (_, index) => ({
    id: hash(100 + index),
    repo_id: repoId,
    branch: 'main',
    parents: index === 23 ? [] : [hash(101 + index)],
    doc_hash: hash(100 + index),
    provider: 'codex',
    fidelity: 'full',
    message: `main row ${index}`,
    created_at: new Date(Date.UTC(2026, 7, 31, 8, 0 - index)).toISOString(),
  }));
  const shortBranch = [
    {
      id: hash(10),
      repo_id: repoId,
      branch: 'short-lived',
      parents: [hash(11)],
      doc_hash: hash(10),
      provider: 'codex',
      fidelity: 'full',
      message: 'short branch head',
      created_at: '2026-08-31T10:00:00Z',
    },
    {
      id: hash(11),
      repo_id: repoId,
      branch: 'short-lived',
      parents: [],
      doc_hash: hash(11),
      provider: 'codex',
      fidelity: 'full',
      message: 'short branch root',
      created_at: '2026-08-31T09:59:00Z',
    },
  ];
  const oldBranch = [
    {
      id: hash(12),
      repo_id: repoId,
      branch: 'old-branch',
      parents: [hash(13)],
      doc_hash: hash(12),
      provider: 'codex',
      fidelity: 'full',
      message: 'old branch head',
      created_at: '2026-08-31T07:45:30Z',
    },
    {
      id: hash(13),
      repo_id: repoId,
      branch: 'old-branch',
      parents: [],
      doc_hash: hash(13),
      provider: 'codex',
      fidelity: 'full',
      message: 'old branch root',
      created_at: '2026-08-31T07:45:15Z',
    },
  ];
  const verticalRefs = [
    { kind: 'branch', name: 'main', repo_id: repoId, target: mainSnapshots[0].id },
    { kind: 'branch', name: 'short-lived', repo_id: repoId, target: shortBranch[0].id },
    { kind: 'branch', name: 'old-branch', repo_id: repoId, target: oldBranch[0].id },
  ];
  const first = await openGraph(
    page,
    publicWorkspaceApi([...shortBranch, ...mainSnapshots, ...oldBranch], verticalRefs),
  );
  const viewport = page.locator('.graph-viewport');
  const shortLabel = page.locator('[data-graph-lane="1"]');
  await expect(shortLabel).toBeVisible();
  await shortLabel.hover();
  const fullLabel = page.locator('.graph-lane-tip');
  await expect(fullLabel).toHaveText('short-lived');
  await expect(fullLabel).toBeVisible();
  await expect(viewport.locator('.graph-lane-tip')).toHaveCount(0);
  const fullLabelBounds = await fullLabel.evaluate((element) => {
    const rect = element.getBoundingClientRect();
    return { left: rect.left, right: rect.right, viewportWidth: window.innerWidth };
  });
  expect(fullLabelBounds.left).toBeGreaterThanOrEqual(0);
  expect(fullLabelBounds.right).toBeLessThanOrEqual(fullLabelBounds.viewportWidth);

  await viewport.evaluate((element) => {
    element.scrollTop = 100;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect(fullLabel).toHaveCount(0);
  await expect(page.locator('[data-graph-lane="1"]')).toHaveCount(0);
  await expect(page.locator('[data-graph-lane="0"]')).toBeVisible();

  await viewport.evaluate((element) => {
    element.scrollTop = element.scrollHeight;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect(page.locator('[data-graph-lane="1"]')).toContainText('old-br');

  await viewport.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect(page.locator('[data-graph-lane="1"]')).toBeVisible();
  expect(first.pageErrors).toEqual([]);
  expect(first.unexpected).toEqual([]);

  const wideHeads = Array.from({ length: 14 }, (_, index) => ({
    id: hash(200 + index),
    repo_id: repoId,
    branch: `feature-${index}`,
    parents: [hash(300 + index)],
    doc_hash: hash(200 + index),
    provider: 'codex',
    fidelity: 'full',
    message: `feature ${index} head`,
    created_at: new Date(Date.UTC(2026, 7, 31, 12, 0 - index)).toISOString(),
  }));
  const wideRoots = Array.from({ length: 14 }, (_, index) => ({
    id: hash(300 + index),
    repo_id: repoId,
    branch: `feature-${index}`,
    parents: [],
    doc_hash: hash(300 + index),
    provider: 'codex',
    fidelity: 'full',
    message: `feature ${index} root`,
    created_at: new Date(Date.UTC(2026, 7, 31, 6, 0 - index)).toISOString(),
  }));
  const wideRefs = [
    { kind: 'branch', name: 'main', repo_id: repoId, target: mainSnapshots[0].id },
    ...wideHeads.map((snapshot, index) => ({
      kind: 'branch',
      name: `feature-${index}`,
      repo_id: repoId,
      target: snapshot.id,
    })),
  ];
  const second = await openGraph(
    page,
    publicWorkspaceApi([...wideHeads, ...mainSnapshots, ...wideRoots], wideRefs),
  );
  const wideViewport = page.locator('.graph-viewport');
  const movement = await wideViewport.evaluate((element) => {
    const measure = () => {
      const label = element.querySelector<HTMLElement>('[data-graph-lane="4"]');
      const row = element.querySelector<HTMLElement>('[data-graph-node-lane="4"]');
      const svg = row?.querySelector<SVGSVGElement>('svg');
      const node = svg?.querySelector<SVGCircleElement>('circle:last-of-type');
      const head = label?.parentElement;
      if (!label || !svg || !node || !head) throw new Error('lane 4 label/node missing');
      return {
        labelAnchor: head.getBoundingClientRect().left + label.offsetLeft,
        nodeAnchor: svg.getBoundingClientRect().left + node.cx.baseVal.value,
      };
    };
    const before = measure();
    const maxScroll = element.scrollWidth - element.clientWidth;
    element.scrollLeft = Math.min(66, maxScroll);
    element.dispatchEvent(new Event('scroll'));
    const after = measure();
    return { before, after, scrollLeft: element.scrollLeft, maxScroll };
  });
  expect(movement.maxScroll).toBeGreaterThan(0);
  expect(movement.scrollLeft).toBeGreaterThan(0);
  expect(movement.after.labelAnchor - movement.before.labelAnchor).toBeCloseTo(-movement.scrollLeft, 0);
  expect(movement.after.nodeAnchor - movement.before.nodeAnchor).toBeCloseTo(-movement.scrollLeft, 0);
  expect(
    (movement.after.labelAnchor - movement.after.nodeAnchor) -
      (movement.before.labelAnchor - movement.before.nodeAnchor),
  ).toBeCloseTo(0, 1);
  expect(second.pageErrors).toEqual([]);
  expect(second.unexpected).toEqual([]);
});

test('previous progress folds independently and preserves shared paths and sync status', async ({ page }, testInfo) => {
  const root = id('1'), shared = id('2'), previous = id('3'), current = id('4'), other = id('5');
  const snapshots = [
    { id: current, parents: [root], message: 'current work' },
    { id: previous, parents: [shared], message: 'hook: first previous tip' },
    { id: other, parents: [shared], message: 'second previous tip' },
    { id: shared, parents: [root], message: 'shared previous ancestor' },
    { id: root, parents: [], message: 'common root' },
  ].map((snapshot, index) => ({
    ...snapshot, repo_id: repoId, doc_hash: snapshot.id, branch: snapshot.id === shared ? '(stash)' : 'main', provider: 'codex', fidelity: 'full',
    created_at: `2026-09-15T0${5 - index}:00:00Z`,
  }));
  const refs = [{ kind: 'branch', name: 'main', repo_id: repoId, target: current }];
  const reflog = [
    { kind: 'branch', name: 'main', old: previous, new: root, created_at: '2026-09-15T03:00:00Z' },
    { kind: 'branch', name: 'main', old: other, new: root, created_at: '2026-09-15T04:00:00Z' },
  ];
  const { pageErrors, unexpected } = await openGraph(page, publicWorkspaceApi(snapshots, refs, [], [], reflog));
  const panel = page.locator('.graph-history-panel');
  await expect(panel).toContainText('3 saved snapshots');
  await expect(page.locator('.graph-row')).toHaveCount(2);
  await expect(page.locator('.graph-status-item.pushed')).toHaveText('Pushed 5');
  await expect(page.locator('.graph-status-item.unpushed')).toHaveText('Not pushed 0');
  const first = panel.locator('.graph-history-toggle').nth(0);
  const second = panel.locator('.graph-history-toggle').nth(1);
  await first.focus();
  await page.keyboard.press('Enter');
  await expect(first).toHaveAttribute('aria-expanded', 'true');
  await expect(page.locator('.graph-row')).toHaveCount(4);
  await expect(page.locator('.graph-row[aria-label^="shared previous ancestor"]')).toBeVisible();
  await expect(page.locator('.graph-row[aria-label^="second previous tip"]')).toHaveCount(0);
  await second.click();
  await expect(page.locator('.graph-row')).toHaveCount(5);
  await page.screenshot({ path: testInfo.outputPath('previous-progress.png'), fullPage: true });
  await first.click();
  await expect(page.locator('.graph-row')).toHaveCount(4);
  await expect(page.locator('.graph-row[aria-label^="shared previous ancestor"]')).toBeVisible();
  await panel.locator('.graph-history-view').nth(1).click();
  await expect(page.locator('.graph-row.on')).toHaveAttribute('aria-label', /^second previous tip/);
  await second.focus();
  await page.keyboard.press('Space');
  await expect(page.locator('.graph-row')).toHaveCount(2);
  await expect(page.locator('.graph-row.on')).toHaveAttribute('aria-label', /^current work/);
  // Reading a folded tip reveals it; folding remains a local display operation.
  await panel.locator('.graph-history-view').nth(0).click();
  await expect(page.locator('.graph-row')).toHaveCount(4);
  await expect(page.locator('.graph-row.on')).toHaveAttribute('aria-label', /^hook: first previous tip/);
  await first.click();
  await expect(page.locator('.graph-row')).toHaveCount(2);
  await expect(page.locator('.graph-row[aria-label^="common root"]')).toBeVisible();
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]); // fixture permits GET only: no ref/pending writes.
});

test('previous progress used by a teammate remains visible', async ({ page }) => {
  const root = id('1'), previous = id('2'), current = id('3');
  const snapshots = [
    { id: current, parents: [root], message: 'current main' },
    { id: previous, parents: [root], message: 'teammate work' },
    { id: root, parents: [], message: 'shared root' },
  ].map((snapshot) => ({
    ...snapshot, repo_id: repoId, doc_hash: snapshot.id, branch: 'main', provider: 'codex', fidelity: 'full',
    created_at: '2026-09-15T00:00:00Z',
  }));
  const refs = [
    { kind: 'branch', name: 'main', repo_id: repoId, target: current },
    { kind: 'branch', name: 'teammate', repo_id: repoId, target: previous },
  ];
  const reflog = [{ kind: 'branch', name: 'main', old: previous, new: root, created_at: '2026-09-15T01:00:00Z' }];
  const { pageErrors, unexpected } = await openGraph(page, publicWorkspaceApi(snapshots, refs, [], [], reflog));
  await expect(page.locator('.graph-history-panel')).toContainText('On an active path');
  await expect(page.locator('.graph-history-toggle')).toHaveCount(0);
  await expect(page.locator('.graph-row')).toHaveCount(3);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('server history exposes births and explicit past positions without write requests', async ({ page }, testInfo) => {
  const root = id('1'), later = id('2'), current = id('3');
  const snapshots = [
    { id: current, parents: [root], message: 'current continuation' },
    { id: later, parents: [root], message: 'retained later work' },
    { id: root, parents: [], message: 'selected old code' },
  ].map((snapshot) => ({ ...snapshot, doc_hash: snapshot.id, repo_id: repoId, branch: 'main', provider: 'codex', fidelity: 'full', created_at: '2026-09-15T00:00:00Z' }));
  const refs = [{ kind: 'branch', name: 'main', repo_id: repoId, target: current },
    { kind: 'tag', name: 'cxt/history/v1/advance/source', repo_id: repoId, target: later }];
  const common = { repo_id: repoId, branch: 'main', branch_id: 'main-identity', created_at: '2026-09-15T01:00:00Z' };
  const history = [
    { ...common, id: 'born', branch: 'feature/new', branch_id: 'new-identity', kind: 'birth', source: root, target: root },
    { ...common, id: 'position', kind: 'position', source: later, target: root, git_after: 'a'.repeat(40), worktree_id: 'b'.repeat(32) },
    { ...common, id: 'advance', kind: 'advance', source: later, target: current },
  ];
  const { pageErrors, unexpected } = await openGraph(page, publicWorkspaceApi(snapshots, refs, [], [], [], history));
  await expect(page.locator('.graph-row:not([data-graph-event])')).toHaveCount(2);
  await expect(page.locator('.graph-status-item.pushed')).toHaveText('Pushed 3');
  await page.locator('.graph-births summary').click();
  await expect(page.locator('.graph-births')).toContainText('feature/new');
  await page.getByLabel('View from', { exact: true }).selectOption('position');
  await expect(page.locator('.graph-row:not([data-graph-event])')).toHaveCount(1);
  await expect(page.locator('.graph-row.on')).toHaveAttribute('aria-label', /^selected old code/);
  await expect(page.locator('.graph-history-scope')).toContainText('Browsing only');
  await page.screenshot({ path: testInfo.outputPath('recorded-context-position.png'), fullPage: true });
  for (const button of await page.locator('.graph-history-toggle').all()) await button.click();
  await expect(page.locator('.graph-row:not([data-graph-event])')).toHaveCount(3);
  await page.getByLabel('View from', { exact: true }).selectOption('');
  await expect(page.locator('.graph-row:not([data-graph-event])')).toHaveCount(2);
  await expect(page.locator('.graph-row.on')).toHaveAttribute('aria-label', /^current continuation/);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('renamed history stays with its identity when the old name is reused', async ({ page }) => {
  const root = id('1'), later = id('2'), current = id('3');
  const snapshots = [
    { id: current, parents: [root], message: 'current continuation' },
    { id: later, parents: [root], message: 'retained old-name work' },
    { id: root, parents: [], message: 'original code' },
  ].map(s => ({ ...s, doc_hash: s.id, repo_id: repoId, branch: 'old', provider: 'codex', fidelity: 'full', created_at: '2026-09-15T00:00:00Z' }));
  const refs = [{ kind: 'branch', name: 'main', repo_id: repoId, target: current }, { kind: 'branch', name: 'old', repo_id: repoId, target: root }];
  const common = { repo_id: repoId, branch: 'old', branch_id: 'original-task', created_at: '2026-09-15T01:00:00Z' };
  const history = [
    { ...common, id: 'reuse', kind: 'birth', branch_id: 'new-task', source: root, target: root, binding_parent: 'rename' },
    { ...common, id: 'rename', kind: 'rename', branch: 'main', previous_branch: 'old', binding_parent: 'birth', source: current, target: current },
    { ...common, id: 'position', kind: 'position', source: root, target: root },
    { ...common, id: 'advance', kind: 'advance', source: later, target: current },
    { ...common, id: 'birth', kind: 'birth', source: root, target: root },
  ];
  const { pageErrors, unexpected } = await openGraph(page, publicWorkspaceApi(snapshots, refs, [], [], [], history));
  await expect(page.locator('.graph-row:not([data-graph-event])')).toHaveCount(2);
  await expect(page.locator('section.graph-history-panel .graph-history-branch')).toHaveText('main');
  await page.getByLabel('View from', { exact: true }).selectOption('position');
  await expect(page.locator('.graph-row:not([data-graph-event])')).toHaveCount(1);
  for (const button of await page.locator('.graph-history-toggle').all()) await button.click();
  await expect(page.locator('.graph-row:not([data-graph-event])')).toHaveCount(3);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('PR-joined branch lanes keep their name while truly deleted branches stay archived', async ({ page }) => {
  const root = id('8');
  const joinedHead = id('9');
  const archivedHead = id('7');
  const snapshots = [
    {
      id: pushedHead,
      repo_id: repoId,
      branch: 'main',
      parents: [root],
      graft_parents: [joinedHead],
      grafted: true,
      doc_hash: pushedHead,
      provider: 'codex',
      fidelity: 'full',
      message: 'active main head',
      created_at: '2026-08-31T04:00:00Z',
    },
    {
      id: joinedHead,
      repo_id: repoId,
      branch: 'feature/merged',
      parents: [root],
      doc_hash: joinedHead,
      provider: 'claude',
      fidelity: 'full',
      message: 'merged branch history',
      created_at: '2026-08-31T03:30:00Z',
    },
    {
      id: archivedHead,
      repo_id: repoId,
      branch: 'feature/abandoned',
      parents: [root],
      doc_hash: archivedHead,
      provider: 'claude',
      fidelity: 'full',
      message: 'archived-only history',
      created_at: '2026-08-31T03:00:00Z',
    },
    {
      id: root,
      repo_id: repoId,
      branch: 'main',
      parents: [],
      doc_hash: root,
      provider: 'codex',
      fidelity: 'full',
      message: 'shared root',
      created_at: '2026-08-31T02:00:00Z',
    },
  ];
  const lifecycleRef = (branch: string, target: string, generation: number) => ({
    kind: 'tag',
    name: `cxt/branch-state/v1/${String(generation).padStart(20, '0')}/archived/${target.replace(/^sha256:/, '')}/${branch}`,
    repo_id: repoId,
    target,
  });
  const refs = [
    { kind: 'branch', name: 'main', repo_id: repoId, target: pushedHead },
    lifecycleRef('feature/merged', joinedHead, 1),
    lifecycleRef('feature/abandoned', archivedHead, 2),
  ];
  const { pageErrors, unexpected } = await openGraph(page, publicWorkspaceApi(snapshots, refs));

  await expect(page.locator('.graph-row')).toHaveCount(3);
  const joinedRow = page.locator('.graph-row[aria-label^="merged branch history"]');
  await expect(joinedRow).toBeVisible();
  const joinedLane = page.locator('.graph-lane-label[aria-label="feature/merged"]');
  await expect(joinedLane).toBeVisible();
  await expect(joinedLane).not.toHaveClass(/archived/);
  await joinedRow.hover();
  await expect(page.locator('.graph-tip .tip-badges')).toContainText('joined · feature/merged');

  const panel = page.locator('.graph-archive-panel');
  await expect(panel.locator('summary')).toContainText('Archived branches 1');
  await panel.locator('summary').click();
  await expect(panel).not.toContainText('feature/merged');
  const uniqueEntry = panel.locator('.graph-archive-entry').filter({ hasText: 'feature/abandoned' });
  await expect(uniqueEntry).toContainText('1 unique commit');

  await uniqueEntry.click();
  await expect(page.locator('.graph-row')).toHaveCount(4);
  await expect(page.locator('.graph-row.on')).toHaveAttribute('aria-label', /^archived-only history/);
  const hide = panel.locator('.graph-archive-toggle');
  await expect(hide).toContainText('Hide 1 archived');
  await hide.click();
  await expect(page.locator('.graph-row')).toHaveCount(3);
  await expect(page.locator('.graph-row.on')).toHaveAttribute('aria-label', /^active main head/);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('real cxtd wire renders a newly created public profile', async ({ page }, testInfo) => {
  test.skip(!process.env.CXT_E2E_FULLSTACK, 'full-stack smoke runs in CI or with CXT_E2E_FULLSTACK=1');

  const pageErrors = capturePageErrors(page);
  const api = page.context().request;
  const origin = 'http://127.0.0.1:4174';
  const mutationHeaders = { Origin: origin, 'X-Cxt-CSRF': '1' };
  const workspaceName = `Browser_E2E_${testInfo.retry}`;

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

  const create = await api.post('/api/v1/workspaces', {
    headers: mutationHeaders,
    data: { name: workspaceName },
  });
  expect(create.ok()).toBe(true);
  const workspace = (await create.json()) as { id: string };

  const publish = await api.patch(`/api/v1/workspaces/${encodeURIComponent(workspace.id)}`, {
    headers: mutationHeaders,
    data: { visibility: 'public' },
  });
  expect(publish.ok()).toBe(true);

  const logout = await api.delete('/api/v1/auth/session', { headers: mutationHeaders });
  expect(logout.ok()).toBe(true);

  await page.goto(`/${encodeURIComponent(me.username)}`);
  await expect(page.locator('.profile-name')).toHaveText('Browser E2E');
  await expect(page.locator('.ws-card-name').filter({ hasText: workspaceName })).toHaveCount(1);
  await expect(page.locator('.contrib-legend')).toBeVisible();
  expect(pageErrors).toEqual([]);
});

test('real cxtd completes remote MCP OAuth consent, PKCE, read-only call, and revocation', async ({ page }) => {
  test.skip(!process.env.CXT_E2E_FULLSTACK, 'full-stack smoke runs in CI or with CXT_E2E_FULLSTACK=1');

  const pageErrors = capturePageErrors(page);
  const api = page.context().request;
  const origin = 'http://127.0.0.1:4174';
  const callback = `${origin}/e2e-mcp-callback`;
  const verifier = 'v'.repeat(64);
  const challenge = createHash('sha256').update(verifier).digest('base64url');

  const login = await api.post('/api/v1/auth/session', {
    headers: { Origin: origin, 'X-Cxt-CSRF': '1', Authorization: 'Bearer dev:mcp-e2e@example.test:MCP E2E' },
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

  const revoke = await api.post('/oauth/revoke', {
    form: { client_id: client.client_id, token: tokens.access_token, token_type_hint: 'access_token' },
  });
  expect(revoke.ok()).toBe(true);
  const afterRevoke = await api.post('/mcp', {
    headers: { Authorization: `Bearer ${tokens.access_token}`, Accept: 'application/json, text/event-stream' },
    data: { jsonrpc: '2.0', id: 2, method: 'ping' },
  });
  expect(afterRevoke.status()).toBe(401);
  expect(pageErrors).toEqual([]);
});

test('repository history protection is explicit, survives reload, and reports upgrade conflicts', async ({ page }) => {
  let protocol = 0;
  let attempts = 0;
  const errors = capturePageErrors(page);
  const unexpected = await installApiFixture(page, ({ method, pathname }) => {
    if (method === 'POST' && pathname === `/api/v1/repos/${repoId}/context-protocol`) {
      attempts += 1;
      if (attempts === 1) return { status: 409, body: { error: { message: 'Reused branch needs identity-aware sync before upgrade' } } };
      protocol = 1;
      return { body: { context_protocol: 1 } };
    }
    if (method !== 'GET') return undefined;
    if (pathname === `/api/v1/repos/${repoId}/secrets`) return { status: 404, body: { error: { message: 'No secrets configured' } } };
    if (pathname === '/api/v1/me') return { body: { id: 'owner', username: 'alice', name: 'Alice', locale: 'en' } };
    if (pathname === '/api/v1/workspaces') return { body: [{ id: workspaceId, owner_id: 'owner', owner_username: 'alice', name: 'cxthub', slug: 'cxthub', visibility: 'private' }] };
    if (pathname === `/api/v1/workspaces/${workspaceId}/members`) return { body: [{ workspace_id: workspaceId, user_id: 'owner', role: 'owner' }] };
    if (pathname === '/api/v1/repos') return { body: [{ id: repoId, default_branch: 'main', remote_url: 'https://cxthub.com/alice/cxthub/repo', context_protocol: protocol }] };
    if (pathname === `/api/v1/repos/${repoId}/refs`) return { body: [{ kind: 'branch', name: 'main', repo_id: repoId, target: pushedHead, ...(protocol ? { branch_id: 'main-identity' } : {}) }] };
    return undefined;
  });
  await page.goto('/alice/cxthub/settings');
  const panel = page.locator('.repo-history-protection');
  await expect(panel).toContainText('Update every CLI');
  expect(attempts).toBe(0);
  await panel.getByRole('button', { name: 'Enable history protection' }).click();
  await expect(panel.locator('.err')).toContainText('Reused branch needs');
  await panel.getByRole('button', { name: 'Enable history protection' }).click();
  await expect(panel).toContainText('Enabled.');
  await expect(panel.getByRole('button')).toHaveCount(0);
  await page.reload();
  await expect(panel).toContainText('Enabled.');
  expect(attempts).toBe(2);
  expect(errors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('context loads bounded pages and only reads inherited events when expanded', async ({ page }) => {
  const snapshots = [
    { id: pushedHead, repo_id: repoId, branch: 'main', parents: [graftTarget], doc_hash: pushedHead, provider: 'codex', fidelity: 'full', message: 'paged head', created_at: '2026-09-16T00:01:00Z' },
    { id: graftTarget, repo_id: repoId, branch: 'main', parents: [], doc_hash: graftTarget, provider: 'codex', fidelity: 'full', message: 'parent', created_at: '2026-09-16T00:00:00Z' },
  ];
  const base = publicWorkspaceApi(snapshots, [{ repo_id: repoId, kind: 'branch', name: 'main', target: pushedHead }]);
  const reads: string[] = [];
  const responder = (request: ApiRequest): ApiResponse | undefined => {
    if (request.pathname.includes('/docs/')) {
      reads.push(request.pathname + '?' + request.searchParams);
      if (!request.pathname.endsWith('/events')) return { status: 500, body: { error: { message: 'whole document read is forbidden in viewer' } } };
      const offset = Number(request.searchParams.get('offset')) === -1 ? 100 : Number(request.searchParams.get('offset'));
      const end = Math.min(offset + 50, 220);
      return { body: { hash: pushedHead, envelope: sessionDoc(pushedHead).cir.envelope, events: Array.from({ length: end - offset }, (_, n) => ({ seq: offset+n, kind: 'message', role: 'user', blocks: [{ type: 'text', text: `Event ${offset+n} — ${offset < 100 ? 'inherited' : 'current'}` }] })), total: 220, inherited: 100, offset, next: end === 220 ? -1 : end } };
    }
    return base(request);
  };
  const { pageErrors, unexpected } = await openGraph(page, responder);
  await expect(page.getByText('Event 100 — current', { exact: true })).toBeVisible();
  await expect(page.getByText('Event 0 — inherited', { exact: true })).toHaveCount(0);
  expect(reads).toHaveLength(1);
  expect(reads[0]).toContain('base=');
  await page.locator('.doc-load-more').click();
  await expect(page.getByText('Event 199 — current', { exact: true })).toBeVisible();
  await page.locator('.doc-load-more').click();
  await expect(page.getByText('Event 219 — current', { exact: true })).toBeVisible();
  await expect(page.locator('.doc-load-more')).toHaveCount(0);
  await page.locator('.inherited-block > summary').click();
  await expect(page.getByText('Event 0 — inherited', { exact: true })).toBeVisible();
  expect(reads.every(url => url.includes('/events?'))).toBe(true);
  expect(reads.some(url => url.includes(`/docs/${encodeURIComponent(graftTarget)}/events`))).toBe(false);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('branch birth and completed join keep feature off the main lane', async ({ page }, testInfo) => {
  const at = (n: number) => `2026-09-16T00:00:0${n}Z`;
  const make = (hash: string, branch: string, parents: string[], n: number, graft: string[] = []) => ({ id: hash, repo_id: repoId, branch, parents, graft_parents: graft, grafted: graft.length > 0, doc_hash: hash, provider: 'codex', fidelity: 'full', message: `archive-${branch}-${n}`, created_at: at(n) });
  const snapshots = [make(appendedRoot, 'main', [], 0), make(graftTarget, 'main', [appendedRoot], 2), make(unpushedHead, 'feature/login', [appendedRoot], 3, [graftTarget]), make(pushedHead, 'feature/login', [unpushedHead], 4)];
  const refs = [{ repo_id: repoId, kind: 'branch', name: 'main', target: pushedHead }, { repo_id: repoId, kind: 'branch', name: 'feature/login', target: pushedHead }];
  const birth = { id: 'birth', repo_id: repoId, branch_id: 'feature-identity', branch: 'feature/login', kind: 'birth', source: appendedRoot, target: appendedRoot, created_at: at(1) };
  const advance = { ...birth, id: 'advance', kind: 'advance', target: unpushedHead, created_at: at(3) };
  const reflog = [{ kind: 'branch', name: 'main', old: graftTarget, new: pushedHead, created_at: at(5) }];
  const { pageErrors, unexpected } = await openGraph(page, publicWorkspaceApi(snapshots, refs, [], [], reflog, [birth, advance]));
  const merge = page.locator('[data-graph-event="merge"]');
  const born = page.locator('[data-graph-event="birth"]');
  await expect(merge).toHaveAttribute('data-graph-node-lane', '0');
  await expect(born).not.toHaveAttribute('data-graph-node-lane', '0');
  const featureLane = await born.getAttribute('data-graph-node-lane');
  await expect(page.locator(`.graph-lane-label[data-graph-lane="${featureLane}"]`)).toHaveAttribute('aria-label', 'feature/login');
  await page.locator('.graph-wrap').screenshot({ path: testInfo.outputPath('branch-events.png') });
  await expect(page.getByRole('button', { name: /archive-feature\/login-4 ·/ })).toHaveAttribute('data-graph-node-lane', featureLane!);
  await merge.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByText('Visible fixture prompt', { exact: true })).toBeVisible();
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});


test('same-snapshot PR completion closes the branch without a ref movement', async ({ page }) => {
  const snapshots = [{id: pushedHead, repo_id: repoId, branch: 'main', parents: [], doc_hash: pushedHead, provider: 'codex', fidelity: 'full', message: 'shared archive', created_at: '2026-09-16T00:00:00Z'}];
  const refs = ['main', 'feature/no-context-change'].map(name => ({repo_id: repoId, kind: 'branch', name, target: pushedHead}));
  const birth = {id: 'birth', repo_id: repoId, branch_id: 'feature-id', branch: 'feature/no-context-change', kind: 'birth', source: pushedHead, target: pushedHead, created_at: '2026-09-16T00:00:01Z'};
  const completed = {...birth, id: 'completed', kind: 'pr-merge', branch: 'main', branch_id: 'main-id', source_branch_id: 'feature-id', shared_target: pushedHead, pr_completed: true,
    pr: {number: 1, base_branch: 'main', head_branch: birth.branch, head_sha: 'a'.repeat(40), merge_sha: 'b'.repeat(40)}, created_at: '2026-09-16T00:00:02Z'};
  const {pageErrors, unexpected} = await openGraph(page, publicWorkspaceApi(snapshots, refs, [], [], [], [birth, completed]));
  await expect(page.locator('[data-graph-event="merge"]')).toHaveCount(1);
  await expect(page.locator('[data-graph-event="merge"]')).toHaveAttribute('data-graph-node-lane', '0');
  await expect(page.locator('[data-graph-event="birth"]')).not.toHaveAttribute('data-graph-node-lane', '0');
  const lane = await page.locator('[data-graph-event="birth"]').getAttribute('data-graph-node-lane');
  await expect(page.locator(`.graph-lane-label[data-graph-lane="${lane}"]`)).toHaveAttribute('aria-label', birth.branch);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('PR delivery distinguishes waiting and completed jobs without viewer retry authority', async ({ page }) => {
  const snapshot = { id: pushedHead, repo_id: repoId, doc_hash: pushedHead, branch: 'main', parents: [], provider: 'codex', created_at: '2026-09-16T01:00:00Z' };
  const base = publicWorkspaceApi([snapshot], [{ kind: 'branch', name: 'main', target: pushedHead }]);
  const state = { id: 'job-1', repo_id: repoId, pr: { number: 42, base_branch: 'main', head_branch: 'feature/late' }, state: 'waiting', reason: 'source_context_pending', attempts: 1 };
  const { pageErrors, unexpected } = await openGraph(page, request => request.pathname.endsWith('/prs/promotions') ? { body: [state] } : base(request));
  await expect(page.locator('.pr-promotions > summary')).toContainText('1 pending');
  await page.locator('.pr-promotions > summary').click();
  await expect(page.locator('.pr-promotions')).toContainText('Waiting for the exact source context');
  await expect(page.locator('.pr-promotions button')).toHaveCount(0);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('context has one lazy memory toggle, scrollable badges and independent center/graph scrolling', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 800 });
  const snapshots = Array.from({ length: 40 }, (_, n) => ({
    id: `sha256:${n.toString(16).padStart(64, '0')}`, repo_id: repoId, branch: 'main',
    parents: n === 39 ? [] : [`sha256:${(n + 1).toString(16).padStart(64, '0')}`],
    doc_hash: `sha256:${n.toString(16).padStart(64, '0')}`, memory_hash: id('f'),
    provider: 'codex', fidelity: 'full', message: `Context fixture ${n}`, models: ['gpt-5.6-sol'],
    author: { name: 'Alice', email: 'alice@example.test', team: '' }, created_at: '2026-09-16T04:00:00Z',
  }));
  const refs = ['main', ...Array.from({ length: 18 }, (_, n) => `feature/long-branch-name-${n}`)].map(name => ({ kind: 'branch', name, repo_id: repoId, target: snapshots[0].id }));
  const base = publicWorkspaceApi(snapshots, refs);
  let memoryReads = 0;
  const { pageErrors, unexpected } = await openGraph(page, request => {
    if (request.pathname.includes('/memories/')) memoryReads++;
    if (request.pathname.endsWith('/events')) return { body: {
      hash: snapshots[0].doc_hash, envelope: sessionDoc(snapshots[0].id).cir.envelope,
      events: Array.from({ length: 40 }, (_, seq) => ({ kind: 'message', role: 'user', seq, blocks: [{ type: 'text', text: `Long conversation ${seq}\n` + 'Archived conversation line.\n'.repeat(8) }] })),
      total: 40, offset: 0, next: -1, inherited: 0,
    } };
    return base(request);
  });
  const center = page.locator('.ctx-main');
  const graph = page.locator('.graph-viewport');
  const row = page.locator('.commit-row').first();
  const scrolling = row.locator('.commit-scroll');
  await expect.poll(() => scrolling.evaluate(e => e.scrollWidth > e.clientWidth)).toBe(true);
  const metaBefore = await row.locator('.commit-meta').boundingBox();
  await row.focus();
  await page.keyboard.press('ArrowRight');
  await expect.poll(() => scrolling.evaluate(e => e.scrollLeft)).toBeGreaterThan(0);
  expect((await row.locator('.commit-meta').boundingBox())?.x).toBe(metaBefore?.x);
  await expect(row.locator('time')).toBeVisible();
  await expect(row.locator('.commit-meta')).toContainText('Alice');
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);

  const outerBefore = await page.evaluate(() => window.scrollY);
  await center.hover();
  await page.mouse.wheel(0, 280);
  await expect.poll(() => center.evaluate(e => e.scrollTop)).toBeGreaterThan(0);
  expect(await page.evaluate(() => window.scrollY)).toBe(outerBefore);
  expect(await graph.evaluate(e => e.scrollTop)).toBe(0);
  const centerBefore = await center.evaluate(e => e.scrollTop);
  await graph.hover();
  await page.mouse.wheel(0, 240);
  await expect.poll(() => graph.evaluate(e => e.scrollTop)).toBeGreaterThan(0);
  expect(await center.evaluate(e => e.scrollTop)).toBe(centerBefore);
  await page.locator('.ctx-side').hover({ position: { x: 12, y: 12 } });
  await page.mouse.wheel(0, 200);
  await expect.poll(() => page.evaluate(() => window.scrollY)).toBeGreaterThan(outerBefore);
  expect(await center.evaluate(e => e.scrollTop)).toBe(centerBefore);

  expect(memoryReads).toBe(0);
  const memory = page.locator('.memory-box');
  await expect(memory).toHaveCount(1);
  expect(await memory.evaluate(e => e.parentElement?.closest('details') === null)).toBe(true);
  await memory.locator('summary').click();
  await expect(memory).toContainText('fixture memory');
  expect(memoryReads).toBe(1);
  await memory.locator('summary').click();
  await memory.locator('summary').click();
  await expect(memory).toContainText('fixture memory');
  expect(memoryReads).toBe(1);

  await page.setViewportSize({ width: 390, height: 844 });
  await expect.poll(() => center.evaluate(e => getComputedStyle(e).overflowY)).toBe('visible');
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});
