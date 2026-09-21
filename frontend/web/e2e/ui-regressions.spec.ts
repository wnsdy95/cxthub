import { serverGraphWireFixture } from '../tests/serverGraphFixture';
import { ko } from '../src/i18n/locales/ko';
import { expect, test, type Page } from '@playwright/test';
import { createHash } from 'node:crypto';
import { capturePageErrors, installApiFixture, type ApiRequest, type ApiResponse } from './api-fixture';
import { expectRenderedGraphPath } from './graph-paths';

const auditSnapshot = (hash: string, branch: string, parents: string[], message: string, n: number) => ({
  id: hash, repo_id: repoId, doc_hash: hash, branch, parents, message, provider: 'codex', fidelity: 'full',
  created_at: new Date(Date.UTC(2026, 8, 18, 0, 0, n)).toISOString(),
});

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
    if (pathname.startsWith(`/api/v1/repos/${repoId}/memories/`) || pathname.startsWith(`/api/v1/repos/${repoId}/memory-objects/`)) {
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
          author: {name:'Alice',email:'alice@example.test',team:''},
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
  await expect(page.locator('.uncommitted-divider')).toHaveCount(0);
  await expect(page.locator('.graph-uncommitted-badge')).toHaveText('Uncommitted');
  await expect(page.locator('.graph-row').filter({ has: page.locator('.graph-uncommitted-badge') })).toHaveAttribute('data-graph-snapshot', uncommittedHead);
  await expect(page.locator('.unpushed-divider')).toHaveCount(1);

  const opacity = await page
    .locator('.graph-row[aria-label^="hook: active desktop session"] svg')
    .getAttribute('opacity');
  expect(opacity).toBe('1'); // Published through-lines are not dimmed by a pending node.
  await expectRenderedGraphPath(page, uncommittedHead, unpushedHead);
  await expectRenderedGraphPath(page, unpushedHead, pushedHead);
  // Regression control: the old divider gap must fail the same path check.
  await page.locator('.graph-status-divider svg').first().evaluate(el=>el.remove());
  await expect(expectRenderedGraphPath(page, unpushedHead, pushedHead)).rejects.toThrow('Rendered path');
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

test('sidebar keeps graph first and groups collapsed history and diagnostics at the bottom', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1440, height: 1000 });
  const root = id('1'), source = id('2'), current = id('3'), previous = id('4'), archived = id('5');
  const snapshots = [
    auditSnapshot(current, 'main', [source], 'Current main context', 5),
    auditSnapshot(source, 'feature/context-sidebar', [root], 'Integrated feature context', 4),
    auditSnapshot(previous, 'main', [root], 'Retained previous progress', 3),
    auditSnapshot(archived, 'feature/archived', [root], 'Archived context', 2),
    auditSnapshot(root, 'main', [], 'Project origin', 1),
  ];
  const refs = [
    { kind: 'branch', name: 'main', branch_id: 'main-id', target: current },
    { kind: 'tag', name: 'cxt/history/v1/sidebar-advance/source', target: previous },
    { kind: 'tag', name: `cxt/branch-state/v1/00000000000000000001/archived/${archived.slice(7)}/feature/archived`, target: archived },
  ];
  const common = { repo_id: repoId, branch_id: 'main-id', branch: 'main', created_at: '2026-09-21T00:00:00Z' };
  const history = [
    { ...common, id: 'sidebar-birth', kind: 'birth', branch_id: 'topic-id', branch: 'feature/context-sidebar', source: root, target: root },
    { ...common, id: 'sidebar-merge', kind: 'pr-merge', source_branch_id: 'topic-id', source, target: source, shared_target: root, pr_completed: true,
      pr: { number: 42, head_branch: 'feature/context-sidebar', base_branch: 'main', head_sha: 'a'.repeat(40), merge_sha: 'b'.repeat(40) } },
    { ...common, id: 'sidebar-position', kind: 'position', source: previous, target: root, git_after: 'c'.repeat(40) },
    { ...common, id: 'sidebar-advance', kind: 'advance', source: previous, target: current },
  ];
  const base = publicWorkspaceApi(snapshots, refs, [], [], [], history);
  const { pageErrors, unexpected } = await openGraph(page, request => request.pathname.endsWith('/prs/promotions')
    ? { body: [{ id: 'waiting', repo_id: repoId, pr: { number: 43, base_branch: 'main', head_branch: 'feature/pending' }, state: 'waiting', reason: 'source_context_pending', attempts: 1 }] }
    : base(request));
  const details = page.locator('.graph-details');
  await expect(details.getByRole('heading', { name: 'History & sync' })).toBeVisible();
  const panels = ['.graph-merge-records', '.graph-births', '.graph-previous', '.graph-archive-panel', '.pr-promotions', '.code-applicability', '.git-scans', '.git-changes', '.reflog'];
  for (const selector of panels) {
    await expect(details.locator(selector)).toHaveCount(1);
    await expect(details.locator(selector)).not.toHaveAttribute('open');
  }
  await expect(details.locator('.graph-history-scope')).toHaveCount(1);
  await expect(details.locator('.graph-status')).toHaveCount(1);
  await expect(details.locator('.pr-promotions > summary')).toContainText('1 pending');
  const assertOrder = async () => {
    const bounds = await page.locator('.ctx-side').evaluate(side => {
      const graph = side.querySelector('.graph-viewport')!.getBoundingClientRect();
      const ai = side.querySelector('.aibar')!.getBoundingClientRect();
      const footer = side.querySelector('.graph-details')!.getBoundingClientRect();
      return { graphBottom: graph.bottom, aiTop: ai.top, aiBottom: ai.bottom, footerTop: footer.top,
        overflow: document.documentElement.scrollWidth > window.innerWidth };
    });
    expect(bounds.graphBottom).toBeLessThanOrEqual(bounds.aiTop);
    expect(bounds.aiBottom).toBeLessThanOrEqual(bounds.footerTop);
    expect(bounds.overflow).toBe(false);
  };
  await assertOrder();
  await page.locator('.ctx-side').screenshot({ path: testInfo.outputPath('sidebar-desktop.png') });
  const nodes = await page.locator('.graph-row').count();
  await details.locator('.graph-previous > summary').focus();
  await page.keyboard.press('Enter');
  await expect(details.locator('.graph-previous .graph-history-view').first()).toBeVisible();
  await expect(page.locator('.graph-row')).toHaveCount(nodes);
  await details.locator('.graph-previous > summary').click();
  await page.setViewportSize({ width: 390, height: 844 });
  await assertOrder();
  await page.locator('.ctx-side').screenshot({ path: testInfo.outputPath('sidebar-mobile.png') });
  await page.getByLabel('Language', { exact: true }).selectOption('ko');
  await expect(details.getByRole('heading', { name: ko.graph.detailsTitle })).toBeVisible();
  await assertOrder();
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.locator('.ctx-side').screenshot({ path: testInfo.outputPath('sidebar-korean.png') });
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
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
  const panel = page.locator('.graph-previous');
  await panel.locator(':scope > summary').click();
  await expect(panel).toContainText('3 saved snapshots');
  await expect(page.locator('.graph-row')).toHaveCount(2);
  await expect(page.locator('.graph-status-item.pushed')).toHaveText('Pushed 5');
  await expect(page.locator('.graph-status-item.unpushed')).toHaveText('Not pushed 0');
  const firstRow=panel.locator('li').filter({has:page.locator('code',{hasText:previous.slice(7,14)})});
  const secondRow=panel.locator('li').filter({has:page.locator('code',{hasText:other.slice(7,14)})});
  const first = firstRow.locator('.graph-history-toggle');
  const second = secondRow.locator('.graph-history-toggle');
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
  await secondRow.locator('.graph-history-view').click();
  await expect(page.locator('.graph-row.on')).toHaveAttribute('aria-label', /^second previous tip/);
  await second.focus();
  await page.keyboard.press('Space');
  await expect(page.locator('.graph-row')).toHaveCount(2);
  await expect(page.locator('.graph-row.on')).toHaveAttribute('aria-label', /^current work/);
  // Reading a folded tip reveals it; folding remains a local display operation.
  await firstRow.locator('.graph-history-view').click();
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
  await page.locator('.graph-previous > summary').click();
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
  await expect(page.locator('.graph-previous .graph-history-branch')).toHaveText('main');
  await page.getByLabel('View from', { exact: true }).selectOption('position');
  await expect(page.locator('.graph-row:not([data-graph-event])')).toHaveCount(1);
  await page.locator('.graph-previous > summary').click();
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
  await expect(page.locator('.doc-load-more')).toHaveCount(0);
  await page.locator('.doc-load-trigger').scrollIntoViewIfNeeded();
  await expect(page.getByText('Event 199 — current', { exact: true })).toBeAttached();
  await page.locator('.doc-load-trigger').scrollIntoViewIfNeeded();
  await expect(page.getByText('Event 219 — current', { exact: true })).toBeAttached();
  await expect(page.locator('.doc-load-trigger')).toHaveCount(0);
  await expect(page.locator('.doc-load-more')).toHaveCount(0);
  await page.locator('.inherited-toggle').click();
  await expect(page.getByText('Event 0 — inherited', { exact: true })).toBeVisible();
  await expect(page.getByText('Event 100 — current', { exact: true })).toHaveCount(0);
  await page.locator('.doc-load-trigger').scrollIntoViewIfNeeded();
  await expect(page.getByText('Event 99 — inherited', { exact: true })).toBeAttached();
  await expect(page.getByText('Event 100 — current', { exact: true })).toHaveCount(0);
  await page.locator('.doc-load-trigger').scrollIntoViewIfNeeded();
  await expect(page.getByText('Event 100 — current', { exact: true })).toBeAttached();
  expect(reads.every(url => url.includes('/events?'))).toBe(true);
  expect(reads.some(url => url.includes(`/docs/${encodeURIComponent(graftTarget)}/events`))).toBe(false);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

for (const view of ['context', 'onhold']) for (const width of [1440, 390]) test(`${view} appends pages only at the conversation bottom (${width}px)`, async ({ page }) => {
  await page.setViewportSize({ width, height: 800 });
  const root = id('1'), head = id('2'), tail = id('3');
  const snapshots = [auditSnapshot(root, 'main', [], 'Root', 0),
    {...auditSnapshot(head, 'main', [root], 'Saved conversation', 1), session_id: 'active-session'},
    {...auditSnapshot(tail, 'main', [head], 'hook: live continuation', 2), session_id: 'active-session'}].reverse();
  const pending = [{repo_id:repoId,session_id:'active-session',branch:'main',target:tail,provider:'codex',updated_at:'2026-09-21T00:00:00Z'}];
  const base = publicWorkspaceApi(snapshots, [{kind:'branch',name:'main',target:view === 'context' ? head : root}], pending,
    view === 'onhold' ? [{repo_id:repoId,branch:'main',user:'alice',target:head,updated_at:'2026-09-21T00:00:00Z'}] : []);
  const reads: string[] = [];
  // Count completed transfers. React's development StrictMode may cancel the
  // first mount's request before remounting; that is not a duplicated page.
  page.on('requestfinished', request => {
    const url = new URL(request.url());
    if (!url.pathname.endsWith('/events')) return;
    const hash = decodeURIComponent(url.pathname.split('/').at(-2)!);
    if (hash === head || hash === tail) reads.push(`${hash === tail ? 'live' : 'saved'}:${url.searchParams.get('offset')}`);
  });
  const {pageErrors, unexpected} = await openGraph(page, req => {
    if (view === 'onhold') {
      if (req.pathname === '/api/v1/me') return {body:{id:'member',username:'alice',locale:'en'}};
      if (req.pathname === '/api/v1/workspaces') return {body:[{id:workspaceId,name:'cxthub',slug:'cxthub',owner_username:'alice',visibility:'private'}]};
      if (req.pathname.endsWith('/members')) return {body:[{workspace_id:workspaceId,user_id:'member',role:'member'}]};
      if (req.pathname.includes('/settings/') || req.pathname.endsWith('/secrets')) return {body:null};
    }
    if (req.pathname.endsWith('/events')) {
      const hash = decodeURIComponent(req.pathname.split('/').at(-2)!);
      const requested = Number(req.searchParams.get('offset'));
      const inherited = hash === tail ? 100 : 0;
      const total = hash === tail ? 180 : hash === head ? 100 : 0;
      const offset = requested === -1 ? inherited : requested;
      const end = Math.min(offset + 50, total);
      if (hash === tail) expect(req.searchParams.get('base')).toBe(head);
      return {body:{hash,envelope:sessionDoc(hash).cir.envelope,total,inherited,offset,next:end < total ? end : -1,
        events:Array.from({length:end-offset},(_,i)=>({seq:offset+i,kind:'message',role:i%2?'assistant':'user',
          blocks:[{type:'text',text:`${hash === tail ? 'Live' : 'Saved'} ${offset+i}\n`+'Conversation line.\n'.repeat(4)}]}))}};
    }
    return base(req);
  });
  if (view === 'onhold') {
    await page.getByRole('button', {name:'On Hold',exact:true}).click();
    await page.locator(`.graph-row[data-graph-snapshot="${head}"]`).click();
  }
  await expect(page.getByText(/^Saved 49\b/)).toBeAttached();
  expect(reads).toEqual(['saved:-1']);
  await expect(page.locator('.doc-load-more')).toHaveCount(0);
  await expect(page.locator('.doc-load-trigger')).toHaveCount(1);
  await expect(page.locator('.viewer .pending-divider')).toHaveCount(0);
  await page.locator('.doc-load-trigger').scrollIntoViewIfNeeded();
  await expect(page.getByText(/^Saved 99\b/)).toBeAttached();
  expect(reads).toEqual(['saved:-1','saved:50']);
  await expect(page.locator('.viewer .pending-divider')).toHaveCount(0);
  await page.locator('.doc-load-trigger').scrollIntoViewIfNeeded();
  await expect(page.getByText(/^Live 149\b/)).toBeAttached();
  await expect(page.locator('.viewer .pending-divider')).toHaveCount(1);
  await expect(page.locator('.doc-load-trigger')).toHaveCount(1);
  await page.locator('.doc-load-trigger').scrollIntoViewIfNeeded();
  await expect(page.getByText(/^Live 179\b/)).toBeAttached();
  await expect(page.locator('.doc-load-trigger')).toHaveCount(0);
  expect(reads).toEqual(['saved:-1','saved:50','live:-1','live:150']);
  const content = await page.locator('.viewer .msg-body').allTextContents();
  expect(content.map(text=>Number(text.match(/^(?:Saved|Live) (\d+)/)![1]))).toEqual(Array.from({length:180},(_,i)=>i));
  // Returning to a cached snapshot still displays a bounded prefix and reveals
  // cached pages at the bottom without transferring the document again.
  await page.locator(`.graph-row[data-graph-snapshot="${root}"]`).click();
  await page.locator(`.graph-row[data-graph-snapshot="${head}"]`).click();
  await expect(page.getByText(/^Saved 49\b/)).toBeAttached();
  await expect(page.getByText(/^Saved 50\b/)).toHaveCount(0);
  await expect(page.locator('.viewer .pending-divider')).toHaveCount(0);
  await page.locator('.doc-load-trigger').scrollIntoViewIfNeeded();
  await expect(page.getByText(/^Saved 99\b/)).toBeAttached();
  expect(reads).toEqual(['saved:-1','saved:50','live:-1','live:150']);
  expect(pageErrors).toEqual([]); expect(unexpected).toEqual([]);
});

test('bottom pagination keeps loaded text on failure and retries only the missing page', async ({page}) => {
  const snap = auditSnapshot(pushedHead,'main',[],'Retryable conversation',1);
  const base = publicWorkspaceApi([snap],[{kind:'branch',name:'main',target:pushedHead}]);
  const reads: number[] = [];
  let fail = true;
  const {pageErrors, unexpected} = await openGraph(page, req => {
    if (req.pathname.endsWith('/events')) {
      const requested = Number(req.searchParams.get('offset'));
      reads.push(requested);
      if (requested === 2 && fail) return {status:503,body:{error:{message:'Temporary page failure'}}};
      const offset = Math.max(0,requested);
      return {body:{hash:pushedHead,envelope:sessionDoc(pushedHead).cir.envelope,total:4,inherited:0,offset,next:offset===0?2:-1,
        events:[offset,offset+1].map(seq=>({seq,kind:'message',role:'user',blocks:[{type:'text',text:`Retained ${seq}\n`+'Long visible line.\n'.repeat(40)}]}))}};
    }
    return base(req);
  });
  await expect(page.getByText(/^Retained 1\b/)).toBeAttached();
  await page.locator('.doc-load-trigger').scrollIntoViewIfNeeded();
  const error = page.locator('.doc-load-error');
  await expect(error).toContainText('Temporary page failure',{timeout:15_000});
  await expect(page.getByText(/^Retained 0\b/)).toBeAttached();
  await expect(page.locator('.doc-load-trigger')).toHaveCount(0);
  expect(reads.filter(offset=>offset===-1)).toHaveLength(1);
  fail = false;
  await error.getByRole('button',{name:'Retry',exact:true}).click();
  await expect(page.getByText(/^Retained 3\b/)).toBeAttached();
  await expect(error).toHaveCount(0);
  expect(reads.filter(offset=>offset===-1)).toHaveLength(1);
  expect(await page.locator('.viewer .msg.user').count()).toBe(4);
  expect(pageErrors).toEqual([]); expect(unexpected).toEqual([]);
});

test('bottom pagination fills the viewport when a page has no visible chat events', async ({page}) => {
  const snap = auditSnapshot(pushedHead,'main',[],'Hidden work followed by conversation',1);
  const base = publicWorkspaceApi([snap],[{kind:'branch',name:'main',target:pushedHead}]);
  const reads: number[] = [];
  const {pageErrors, unexpected} = await openGraph(page, req => {
    if (req.pathname.endsWith('/events')) {
      const offset = Number(req.searchParams.get('offset'));
      reads.push(offset);
      return {body:{hash:pushedHead,envelope:sessionDoc(pushedHead).cir.envelope,total:51,inherited:0,offset:Math.max(0,offset),next:offset===-1?50:-1,
        events:offset===-1?Array.from({length:50},(_,seq)=>({seq,kind:'reasoning',locked:{provider:'codex',scheme:'encrypted',blob:'fixture'}}))
          :[{seq:50,kind:'message',role:'user',blocks:[{type:'text',text:'Visible after hidden page'}]}]}};
    }
    return base(req);
  });
  await expect(page.getByText('Visible after hidden page',{exact:true})).toBeVisible();
  expect(reads).toEqual([-1,50]);
  await expect(page.locator('.doc-load-trigger')).toHaveCount(0);
  expect(pageErrors).toEqual([]); expect(unexpected).toEqual([]);
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

test('graph activation reveals context and distinguishes events at the same snapshot', async ({ page }) => {
  const snapshots = [
    {id:pushedHead,repo_id:repoId,branch:'main',parents:[appendedRoot],doc_hash:pushedHead,provider:'codex',created_at:'2026-09-16T02:00:00Z',message:'current'},
    {id:appendedRoot,repo_id:repoId,branch:'main',parents:[],doc_hash:appendedRoot,provider:'codex',created_at:'2026-09-16T00:00:00Z',message:'baseline'},
  ];
  const birth = {git_after:'a'.repeat(40),id:'born',repo_id:repoId,branch_id:'topic',branch:'feature/same',kind:'birth',source:appendedRoot,target:appendedRoot,created_at:'2026-09-16T00:01:00Z'};
  const done = {...birth,id:'done',kind:'pr-merge',branch_id:'main',branch:'main',source_branch_id:'topic',shared_target:appendedRoot,pr_completed:true,
    pr:{number:42,base_branch:'main',head_branch:'feature/same',head_sha:'a'.repeat(40),merge_sha:'b'.repeat(40)},created_at:'2026-09-16T01:00:00Z'};
  const base = publicWorkspaceApi(snapshots,[{repo_id:repoId,kind:'branch',name:'main',target:pushedHead}],[],[],[],[birth,done]);
  const {pageErrors,unexpected} = await openGraph(page, req => {
    if (req.pathname.endsWith('/events')) {
      const result = docResponse(req.pathname,req.searchParams).body as ReturnType<typeof sessionDoc> & { events: unknown[]; total:number };
      result.events = Array.from({length:60},(_,seq)=>({kind:'message',role:'user',seq,blocks:[{type:'text',text:`Long context message ${seq}`}]}));
      result.total = 60;
      return {body:result};
    }
    return base(req);
  });
  const main = page.locator('.ctx-main');
  const graph = page.locator('.graph-viewport');
  const born = page.locator('[data-graph-event="birth"]');
  const merged = page.locator('[data-graph-event="merge"]');
  const raw = page.locator(`[data-graph-snapshot="${appendedRoot}"]:not([data-graph-event])`);
  const reveal = async () => expect.poll(() => page.locator('.viewer').evaluate(el => {
    const main = el.closest('.ctx-main')!;
    return Math.abs(el.getBoundingClientRect().top-main.getBoundingClientRect().top);
  })).toBeLessThan(3);
  for (const target of [born, merged, merged, raw]) {
    await main.evaluate(el=>{el.scrollTop=el.scrollHeight;});
    await expect.poll(()=>main.evaluate(el=>el.scrollTop)).toBeGreaterThan(300);
    const before = await graph.evaluate(el=>el.scrollTop);
    await target.click();
    await expect(target).toHaveAttribute('aria-pressed','true');
    await expect(page.locator('.viewer-head code')).toHaveText('aaaaaaaaaa');
    await reveal();
    expect(await graph.evaluate(el=>el.scrollTop)).toBe(before);
    await expect(page.locator('.graph-row.on')).toHaveCount(1);
  }
  await expect(page.locator('.context-selection-notice')).toHaveCount(0);
  await born.click();
  await expect(page.locator('.context-selection-notice')).toContainText('feature/same');
  await expect(page.locator('.effective-memory')).toHaveCount(0);
  await merged.click();
  await expect(page.locator('.context-selection-notice')).toContainText('PR #42');
  await expect(page.locator('.effective-memory')).toHaveCount(0);
  await page.locator('.graph-merge-records summary').click();
  await expect(page.locator('[data-branch-lineage="unchanged"]')).toContainText('does not establish whether later conversation was included');
  // Narrow screens use page scrolling; the sticky application header must
  // not cover the selected context after activation from the lower graph.
  await page.setViewportSize({width:800,height:800});
  await raw.click();
  await expect(page.locator('.viewer-head code')).toBeInViewport();
  expect(await page.locator('.viewer').evaluate(el=>el.getBoundingClientRect().top)).toBeGreaterThanOrEqual(60);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('consecutive same-tip PR joins stay on main while unexplained birth gaps stay explicit', async ({ page }) => {
  const snapshot = (hash:string,parents:string[],n:number) => ({id:hash,repo_id:repoId,branch:'main',parents,doc_hash:hash,provider:'codex',message:`state-${n}`,created_at:`2026-09-16T00:00:0${n}Z`});
  const snapshots = [snapshot(appendedRoot,[],0),snapshot(graftTarget,[appendedRoot],1),snapshot(pushedHead,[graftTarget],6),
    {...snapshot(uncommittedHead,[],3.5),message:'hook: other pending session'},snapshot(unpushedHead,[],2.5)];
  const history = [1,2,3].flatMap(n=>{
    const b = {id:`birth-${n}`,repo_id:repoId,branch_id:`topic-${n}`,branch:`feature/${n}`,kind:'birth',source:graftTarget,target:graftTarget,created_at:`2026-09-16T00:00:01Z`};
    return [b,{...b,id:`merge-${n}`,kind:'pr-merge',branch:'main',branch_id:'main',source_branch_id:b.branch_id,source:appendedRoot,target:graftTarget,shared_target:graftTarget,pr_completed:true,
      pr:{number:n,base_branch:'main',head_branch:b.branch,head_sha:'a'.repeat(40),merge_sha:'b'.repeat(40)},created_at:`2026-09-16T00:00:0${n+1}Z`}];
  });
  const pending = [{repo_id:repoId,session_id:'unrelated-session',branch:'main',provider:'codex',target:uncommittedHead,updated_at:'2026-09-16T00:00:03.5Z'}];
  const unsync = [{repo_id:repoId,user:'alice',branch:'main',target:unpushedHead,updated_at:'2026-09-16T00:00:02.5Z'}];
  const {pageErrors,unexpected} = await openGraph(page,publicWorkspaceApi(snapshots,[{repo_id:repoId,kind:'branch',name:'main',target:pushedHead}],pending,unsync,[],history));
  await expect(page.locator('.graph-status-divider')).toHaveCount(1);
  await expect(page.locator('.uncommitted-divider')).toHaveCount(0);
  const captureRow = page.locator(`.graph-row[data-graph-id="${uncommittedHead}"]`);
  await expect(captureRow.locator('.graph-uncommitted-badge')).toHaveText('Uncommitted');
  await expect(page.locator('.graph-row').filter({ has: page.locator('.graph-uncommitted-badge') })).toHaveCount(1);
  // Pending is interleaved with published commits and three PRs, not a range.
  await expect(page.locator('[data-graph-event] .graph-uncommitted-badge')).toHaveCount(0);
  await expect(page.locator('.graph')).not.toContainText('above: uncommitted');
  const merges = page.locator('[data-graph-event="merge"]');
  await expect(merges).toHaveCount(3);
  for (const row of await merges.all()) await expect(row).toHaveAttribute('data-graph-node-lane','0');
  await page.locator('.graph-merge-records summary').click();
  await expect(page.locator('[data-branch-lineage="disconnected"]')).toHaveCount(3);
  await expect(page.locator('.graph-merge-records')).toContainText('no conversation ancestry is inferred');
  await expect(page.locator('.graph-lifecycle-legend')).toContainText('do not establish conversation continuity');
  for (const n of [1,2,3]) {
    await expectRenderedGraphPath(page, `graph:merge:merge-${n}`, `graph:birth:birth-${n}`, true);
    await expectRenderedGraphPath(page, `graph:birth:birth-${n}`, graftTarget);
  }
  await page.getByRole('button',{name:'View merged context',exact:true}).first().click();
  await expect(page.locator('.viewer-head code')).toHaveText('aaaaaaaaaa');
  await expect(page.locator('.context-selection-notice')).toContainText('PR #3');
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

test('completed PR survives a live graft reorder without rewriting current ancestry', async ({ page }) => {
  const p=id('1'), x=id('2'), h=id('3'), h2=id('4');
  const snapshots = [auditSnapshot(p,'main',[],'root',0), auditSnapshot(x,'main',[p],'earlier main',1),
    {...auditSnapshot(h,'main',[p],'merged context',2),graft_parents:[x],grafted:true},
    auditSnapshot(h2,'main',[h],'continued context',3)];
  const refs = [{repo_id:repoId,kind:'branch',name:'main',branch_id:'main-id',target:h2}];
  const history = [{id:'retained-completion',repo_id:repoId,branch_id:'main-id',branch:'main',kind:'pr-merge',
    source_branch_id:'topic-id',source:h,target:h,shared_target:x,pr_completed:true,
    pr:{number:42,head_branch:'topic',base_branch:'main',head_sha:'a'.repeat(40),merge_sha:'b'.repeat(40)},
    created_at:'2026-09-18T00:00:02Z'}];
  const semantics = {version:1,merges:[{event_id:'retained-completion',completed:true,
    source_available:true,placement_intact:true,lineage:'unknown'}]};
  const base = publicWorkspaceApi(snapshots,refs,[],[],[],history);
  const {pageErrors,unexpected} = await openGraph(page,request => request.pathname.endsWith('/view')
    ? {body:{snapshots,refs,history,reflog:[],pending:[],unsync:[],semantics}} : base(request));
  const merge = page.locator('[data-graph-id="graph:merge:retained-completion"]');
  await expect(merge).toHaveCount(1);
  snapshots[1] = {...snapshots[1],graft_parents:[h2],grafted:true};
  snapshots[2] = {...snapshots[2],graft_parents:[],grafted:false};
  refs[0].target = x;
  semantics.merges[0].placement_intact = false;
  await page.locator('.graph-merge-records summary').click();
  await expect(page.locator('.graph-merge-records')).toContainText('Placement has changed', {timeout:15_000});
  await expect(merge).toHaveCount(1);
  await expect(page.locator('[data-graph-issue]')).toHaveCount(0);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('memory attachment changes load the new immutable blob at the same snapshot', async ({ page }) => {
  let generation = 1;
  const reads: string[] = [];
  const snapshots = [{id:pushedHead,repo_id:repoId,doc_hash:pushedHead,branch:'main',parents:[],
    provider:'codex',created_at:'2026-09-19T00:00:00Z',memory_hash:id('8')}];
  const base = publicWorkspaceApi(snapshots,[{kind:'branch',name:'main',target:pushedHead}]);
  const {pageErrors,unexpected} = await openGraph(page, request => {
    snapshots[0].memory_hash = id(generation === 1 ? '8' : '9');
    if (/\/(memories|memory-objects)\//.test(request.pathname)) {
      const hash = decodeURIComponent(request.pathname.split('/').at(-1)!);
      reads.push(hash);
      const version = hash === id('9') ? 2 : 1;
      return {body:{snapshot_id:pushedHead,summary:`memory version ${version}`,key_facts:[],open_tasks:[]}};
    }
    return base(request);
  });
  const panel = page.locator('.memory-box');
  await panel.locator('summary').click();
  await expect(panel).toContainText('memory version 1');
  generation = 2;
  await expect(panel).toContainText('memory version 2', {timeout:15_000});
  expect(reads).toEqual([id('8'),id('9')]);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('context shows saved memory with one toggle, scrollable badges and independent center/graph scrolling', async ({ page }) => {
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
    if (request.pathname.includes('/memory-objects/')) memoryReads++;
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
  await expect(memory).not.toHaveAttribute('open');
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


test('archived automatic publications retain continuous birth, source and merge paths', async ({page}, testInfo) => {
  const at = (n:number) => `2026-09-16T00:00:0${n}Z`;
  const make = (hash:string, branch:string, parents:string[], n:number, graft:string[] = []) => ({
    id:hash, repo_id:repoId, branch, parents, graft_parents:graft, grafted:graft.length>0,
    doc_hash:hash, provider:'codex', fidelity:'full', message:`capture-${branch}`, created_at:at(n),
  });
  const middle = id('f');
  const snapshots = [make(appendedRoot,'main',[],0), make(graftTarget,'main',[appendedRoot],1),
    make(unpushedHead,'feature/left',[appendedRoot],3,[graftTarget]), make(middle,'main',[unpushedHead],5),
    make(pushedHead,'feature/right',[appendedRoot],6,[middle]), make(uncommittedHead,'main',[pushedHead],8)];
  const refs = [{repo_id:repoId,kind:'branch',name:'main',branch_id:'main-id',target:uncommittedHead}];
  const history = [];
  for (const [branch, source, before, n] of [['feature/left',unpushedHead,graftTarget,3], ['feature/right',pushedHead,middle,6]] as const) {
    const birth = {repo_id:repoId,id:`birth-${branch}`,branch_id:branch,branch,kind:'birth',source:appendedRoot,target:appendedRoot,created_at:at(n-1)};
    history.push(birth,
      {...birth,id:`position-${branch}`,kind:'position',source,target:source,created_at:at(n)},
      {...birth,id:`publish-${branch}`,kind:'publish',source,target:source,created_at:at(n)},
      {...birth,id:`merge-${branch}`,kind:'pr-merge',branch:'main',branch_id:'main-id',source_branch_id:branch,
        source,target:source,shared_target:before,pr_completed:true,created_at:at(n+1),
        pr:{number:n,head_branch:branch,base_branch:'main',head_sha:'a'.repeat(40),merge_sha:'b'.repeat(40)}},
      {...birth,id:`archive-${branch}`,kind:'archive',binding_parent:birth.id,source,target:source,created_at:at(9)});
  }
  const reflog = [
    {kind:'branch',name:'feature/left',old:appendedRoot,new:unpushedHead,created_at:at(3)},
    {kind:'branch',name:'feature/right',old:appendedRoot,new:pushedHead,created_at:at(6)},
  ];
  const {pageErrors,unexpected} = await openGraph(page, publicWorkspaceApi(snapshots,refs,[],[],reflog,history));
  await expect(page.locator('[data-graph-event="merge"]')).toHaveCount(2);
  for (const branch of ['feature/left','feature/right']) {
    const born = page.locator(`[data-graph-event="birth"][data-graph-branch="${branch}"]`);
    const source = page.getByRole('button',{name:new RegExp(`^capture-${branch} ·`)});
    const lane = await source.getAttribute('data-graph-node-lane');
    await expect(born).toHaveAttribute('data-graph-node-lane',lane!);
    await expect(born).not.toHaveAttribute('data-graph-node-lane','0');
    const start = Number(await source.getAttribute('data-graph-row-index'));
    const end = Number(await born.getAttribute('data-graph-row-index'));
    expect(end).toBeGreaterThan(start);
    // Check the actual SVG segments, not only row labels or lane assignments.
    const continuity = await page.locator('.graph-row').evaluateAll((rows,{start,end,lane}) => {
      const source = rows[start];
      const x = source.querySelector('circle')!.getAttribute('cx');
      return rows.slice(start+1,end+1).every((row,index) => {
        const last = index === end-start-1;
        return [...row.querySelectorAll('svg line')].some(line => line.getAttribute('x1') === x
          && line.getAttribute('x2') === x && line.getAttribute('y1') === '0'
          && Number(line.getAttribute('y2')) > 0)
          && (!last || row.getAttribute('data-graph-node-lane') === lane);
      });
    },{start,end,lane});
    expect(continuity).toBe(true);
    const sourceId = await source.getAttribute('data-graph-snapshot');
    await expectRenderedGraphPath(page, `graph:merge:merge-${branch}`, sourceId!);
    await expectRenderedGraphPath(page, sourceId!, `graph:birth:birth-${branch}`);
    await expectRenderedGraphPath(page, `graph:birth:birth-${branch}`, appendedRoot);
  }
  await page.locator('.graph-wrap').screenshot({path:testInfo.outputPath('published-branch-paths.png')});
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

for (const mode of ['pending', 'unsync'] as const) {
  test(`graph preserves intermediate hook parents for ${mode} without promoting observations`, async ({ page }) => {
    const snapshots = [auditSnapshot(appendedRoot, 'main', [], 'root', 0),
      auditSnapshot(graftTarget, 'main', [appendedRoot], 'hook: earlier', 1),
      auditSnapshot(pushedHead, 'main', [graftTarget], 'hook: latest', 2)];
    const refs = [{ kind: 'branch', name: 'main', repo_id: repoId, target: appendedRoot }];
    const pending = [{ repo_id: repoId, branch: 'main', session_id: 'live', provider: 'codex', target: pushedHead }];
    const unsync = mode === 'unsync' ? [{ repo_id: repoId, branch: 'main', user: 'alice', target: pushedHead }] : [];
    const history = [{ id: 'position', kind: 'position', branch_id: 'main-id', branch: 'main', source: pushedHead, target: pushedHead, created_at: '2026-09-18T00:00:03Z' }];
    const { pageErrors, unexpected } = await openGraph(page, publicWorkspaceApi(snapshots, refs, pending, unsync, [], history));
    await expect(page.locator('.graph-row')).toHaveCount(3);
    await expect(page.locator(`.graph-row[data-graph-snapshot="${pushedHead}"]`)).toHaveAttribute('aria-label', mode === 'pending' ? /Uncommitted/ : /Not pushed/);
    await expectRenderedGraphPath(page, pushedHead, graftTarget);
    await expectRenderedGraphPath(page, graftTarget, appendedRoot);
    await expect(page.locator('.graph-status-item.pushed')).toHaveText(/Pushed 1/);
    expect(pageErrors).toEqual([]);
    expect(unexpected).toEqual([]);
  });
}

test('branch selection survives ref polling and follows identity through rename', async ({ page }) => {
  await page.clock.install();
  const snapshots = [auditSnapshot(appendedRoot, 'main', [], 'root', 0), auditSnapshot(graftTarget, 'feature', [appendedRoot], 'feature', 1), auditSnapshot(pushedHead, 'main', [appendedRoot], 'next main', 2)];
  let refs = [{ kind: 'branch', name: 'main', branch_id: 'main-id', repo_id: repoId, target: appendedRoot }, { kind: 'branch', name: 'feature', branch_id: 'feature-id', repo_id: repoId, target: graftTarget }];
  const base = publicWorkspaceApi(snapshots, refs);
  const { pageErrors, unexpected } = await openGraph(page, req => req.pathname.endsWith('/refs') ? { body: refs } : base(req));
  const select = page.getByRole('combobox', { name: 'Branch', exact: true });
  await select.selectOption('feature');
  refs = [{ ...refs[0], target: pushedHead }, refs[1]];
  await page.clock.fastForward(16_000);
  await expect(page.locator('.commit-row').filter({ hasText: 'feature' })).toBeVisible();
  await expect(select).toHaveValue('feature');
  refs = [refs[0], { ...refs[1], name: 'renamed' }];
  await page.clock.fastForward(16_000);
  await expect(select).toHaveValue('renamed');
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('graph distinguishes snapshot failure from empty history and recovers on retry', async ({ page }) => {
  const pageErrors = capturePageErrors(page);
  let fail = true;
  const snapshots = [auditSnapshot(appendedRoot, 'main', [], 'root', 0)];
  const base = publicWorkspaceApi(snapshots, [{ kind: 'branch', repo_id: repoId, name: 'main', target: appendedRoot }]);
  const unexpected = await installApiFixture(page, req => req.pathname.endsWith('/snapshots') && fail
    ? { status: 503, body: { error: { message: 'snapshot outage' } } } : base(req));
  await page.goto('/alice/cxthub');
  await expect(page.locator('.graph-wrap').getByRole('alert')).toContainText('snapshot outage', { timeout: 15_000 });
  await expect(page.locator('.graph .ws-empty')).toHaveCount(0);
  fail = false;
  await page.locator('.graph-wrap').getByRole('alert').getByRole('button').click();
  await expect(page.locator('.graph-row')).toHaveCount(1);
  await expect(page.locator('.graph-wrap').getByRole('alert')).toHaveCount(0);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('graph advances only from a complete repository view', async ({ page }) => {
  await page.clock.install();
  const root = auditSnapshot(appendedRoot, 'main', [], 'root', 0);
  const tip = auditSnapshot(pushedHead, 'main', [appendedRoot], 'committed together', 1);
  let advance = false;
  let viewCalls = 0;
  const base = publicWorkspaceApi([root], [{ kind: 'branch', repo_id: repoId, name: 'main', target: appendedRoot }]);
  const { pageErrors, unexpected } = await openGraph(page, req => {
    if (req.pathname.endsWith('/view')) {
      viewCalls++;
      return { body: {
        refs: [{ kind: 'branch', repo_id: repoId, name: 'main', target: advance ? pushedHead : appendedRoot }],
        snapshots: advance ? [tip, root] : [root],
        reflog: [], history: [], pending: [], unsync: [],
      } };
    }
    // Legacy component endpoints intentionally remain at the old generation.
    return base(req);
  });
  await expect(page.locator('.graph-row')).toHaveCount(1);
  const previousCalls = viewCalls;
  advance = true;
  await page.clock.fastForward(11_000);
  await expect.poll(() => viewCalls).toBeGreaterThan(previousCalls);
  await expect(page.locator('.graph-row')).toHaveCount(2);
  await expect(page.locator(`.graph-row[data-graph-snapshot="${pushedHead}"]`)).toBeVisible();
  await expect(page.locator('.graph-wrap').getByRole('alert')).toHaveCount(0);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('tag preservation and unused branch archive do not fabricate publication or a merge', async ({ page }) => {
  const snapshots = [auditSnapshot(appendedRoot, 'main', [], 'root', 0), auditSnapshot(graftTarget, 'feature', [appendedRoot], 'tagged history', 1)];
  const refs = [{ kind: 'branch', repo_id: repoId, name: 'main', target: appendedRoot },
    { kind: 'tag', repo_id: repoId, name: 'v1', target: graftTarget },
    { kind: 'tag', repo_id: repoId, name: `cxt/branch-state/v1/00000000000000000001/archived/${appendedRoot.slice(7)}/unused`, target: appendedRoot }];
  const { pageErrors, unexpected } = await openGraph(page, publicWorkspaceApi(snapshots, refs));
  await expect(page.locator(`.graph-row[data-graph-snapshot="${graftTarget}"]`)).toHaveAttribute('aria-label', /Preserved by a server tag/);
  await expect(page.locator('.graph-status-item.tagged')).toHaveText('Tagged 1');
  await expect(page.locator('.graph-status-item.unpushed')).toHaveText('Not pushed 0');
  await page.locator('.graph-archive-panel summary').click();
  await expect(page.locator('.graph-archive-entry')).toContainText('unused');
  await expect(page.locator('.graph-row[data-graph-event="merge"]')).toHaveCount(0);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('renamed destination keeps a delayed PR completion on its main path', async ({ page }) => {
  const at = (n: number) => new Date(Date.UTC(2026, 8, 18, 0, 0, n)).toISOString();
  const snapshots = [auditSnapshot(appendedRoot, 'main', [], 'root', 0), auditSnapshot(graftTarget, 'feature', [appendedRoot], 'source', 3), auditSnapshot(pushedHead, 'trunk', [graftTarget], 'current', 2)];
  const main = { id: 'main-birth', kind: 'birth', repo_id: repoId, branch: 'main', branch_id: 'main-id', source: appendedRoot, target: appendedRoot, created_at: at(0) };
  const birth = { ...main, id: 'birth', branch: 'feature', branch_id: 'feature-id', created_at: at(1) };
  const done = { ...main, id: 'done', kind: 'pr-merge', source: graftTarget, target: graftTarget, shared_target: appendedRoot, source_branch_id: birth.branch_id, pr_completed: true,
    pr: { number: 1, base_branch: 'main', head_branch: 'feature', head_sha: 'a'.repeat(40), merge_sha: 'b'.repeat(40) }, created_at: at(5) };
  const rename = { ...main, id: 'rename', kind: 'rename', branch: 'trunk', previous_branch: 'main', binding_parent: main.id, created_at: at(6) };
  const refs = [{ kind: 'branch', repo_id: repoId, name: 'trunk', branch_id: 'main-id', target: pushedHead }];
  const base = publicWorkspaceApi(snapshots, refs, [], [], [], [main, birth, done, rename]);
  const { pageErrors, unexpected } = await openGraph(page, req => req.pathname === '/api/v1/repos'
    ? { body: [{ id: repoId, default_branch: 'trunk' }] } : base(req));
  await expectRenderedGraphPath(page, pushedHead, 'graph:merge:done');
  await expectRenderedGraphPath(page, 'graph:merge:done', graftTarget);
  await expect(page.locator('[data-graph-id="graph:merge:done"]')).toHaveAttribute('data-graph-node-lane', '0');
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});


test('pending badge stays beside its active lanes in a wide sparse graph', async ({ page }) => {
  const hash = (n: number) => `sha256:${n.toString(16).padStart(64, '0')}`;
  const roots = Array.from({ length: 20 }, (_, i) => auditSnapshot(hash(100 + i), `feature/${i}`, [], `root ${i}`, 2));
  const heads = roots.map((root, i) => auditSnapshot(hash(200 + i), root.branch, [root.id], `head ${i}`, 3));
  const snapshots = [auditSnapshot(appendedRoot, 'main', [], 'main root', 0),
    auditSnapshot(uncommittedHead, 'main', [appendedRoot], 'hook: pending', 1), ...roots, ...heads];
  const refs = [{ kind: 'branch', repo_id: repoId, name: 'main', target: appendedRoot },
    ...heads.map(s => ({ kind: 'branch', repo_id: repoId, name: s.branch, target: s.id }))];
  const pending = [{ repo_id: repoId, branch: 'main', session_id: 'pending', provider: 'codex', target: uncommittedHead }];
  const { pageErrors, unexpected } = await openGraph(page, publicWorkspaceApi(snapshots, refs, pending));
  const viewport = page.locator('.graph-viewport');
  expect(await viewport.evaluate(el => el.scrollWidth > el.clientWidth)).toBe(true);
  const node = page.locator('.uncommitted-node');
  await node.scrollIntoViewIfNeeded();
  const badge = page.locator('.graph-uncommitted-badge');
  const bounds = await badge.boundingBox();
  const visible = await viewport.boundingBox();
  const nodeBounds = await node.boundingBox();
  expect(bounds!.x).toBeGreaterThan(nodeBounds!.x + nodeBounds!.width);
  expect(bounds!.x - nodeBounds!.x - nodeBounds!.width).toBeLessThan(40);
  expect(bounds!.x).toBeGreaterThanOrEqual(visible!.x);
  expect(bounds!.x + bounds!.width).toBeLessThan(visible!.x + visible!.width);
  await expectRenderedGraphPath(page, uncommittedHead, appendedRoot);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

for (const malformed of ['duplicate-id', 'cycle'] as const) {
  test(`invalid ${malformed} is reported even on a folded branch and a fresh read recovers`, async ({ page }, testInfo) => {
    const root=id('1'), current=id('2'), hidden=id('3');
    const good=[auditSnapshot(current,'main',[root],'current readable context',2),auditSnapshot(root,'main',[],'root',0),auditSnapshot(hidden,'deleted',[root],'archived record',1)];
    const bad=malformed==='duplicate-id' ? [...good,{...good[2],message:'conflicting duplicate'}]
      : good.map(s=>s.id===hidden?{...s,graft_parents:[hidden]}:s);
    const refs=[{repo_id:repoId,kind:'branch',name:'main',target:current},
      {repo_id:repoId,kind:'tag',name:`cxt/branch-state/v1/00000000000000000001/archived/${hidden.slice(7)}/deleted`,target:hidden}];
    let stage=0;
    const badApi=publicWorkspaceApi(bad,refs), goodApi=publicWorkspaceApi(good,refs);
    const pageErrors=capturePageErrors(page);
    const unexpected=await installApiFixture(page, request=>(stage===1?badApi:goodApi)(request));
    await page.goto('/alice/cxthub');
    await page.locator('.commit-row').filter({hasText:'current readable context'}).click();
    await expect(page.getByText('Visible fixture prompt',{exact:true})).toBeVisible();
    stage=1;
    const alert=page.locator('.graph-wrap [role="alert"]');
    await expect(alert).toBeVisible({timeout:10_000});
    await expect(alert).toContainText(malformed==='duplicate-id'?'duplicate graph snapshot':'cyclic graph ancestry');
    await expect(page.locator('.graph-row')).toHaveCount(0);
    // The last coherent document view stays usable while invalid new graph data is rejected.
    await expect(page.getByText('Visible fixture prompt',{exact:true})).toBeVisible();
    await alert.screenshot({path:testInfo.outputPath(`invalid-${malformed}.png`)});
    stage=2;
    await alert.getByRole('button').click();
    await expect(alert).toHaveCount(0);
    await expect(page.locator('.graph-row')).toHaveCount(2);
    expect(pageErrors).toEqual([]);
    expect(unexpected).toEqual([]);
  });
}

test('PR evidence survives every combination of archive and overlapping progress folds', async ({page}) => {
  const root=id('1'), before=id('2'), source=id('3'), current=id('4'), previous=id('5'), archived=id('6');
  const snapshots=[auditSnapshot(root,'main',[],'root',0),auditSnapshot(before,'main',[root],'before',1),
    {...auditSnapshot(source,'topic',[root],'merged source',3),graft_parents:[before],grafted:true},
    auditSnapshot(previous,'main',[source],'previous tip',5),auditSnapshot(current,'main',[root],'current tip',6),
    auditSnapshot(archived,'deleted',[root],'archived only',7)];
  const birth={repo_id:repoId,id:'fold-birth',kind:'birth',branch:'topic',branch_id:'topic-id',source:root,target:root,created_at:'2026-09-18T00:00:02Z'};
  const history=[birth,{...birth,id:'fold-merge',kind:'pr-merge',branch:'main',branch_id:'main-id',source_branch_id:'topic-id',source,target:source,shared_target:before,pr_completed:true,created_at:'2026-09-18T00:00:04Z',pr:{number:501,head_branch:'topic',base_branch:'main',head_sha:'a'.repeat(40),merge_sha:'b'.repeat(40)}}];
  const refs=[{repo_id:repoId,kind:'branch',name:'main',branch_id:'main-id',target:current},
    {repo_id:repoId,kind:'tag',name:`cxt/branch-state/v1/00000000000000000001/archived/${archived.slice(7)}/deleted`,target:archived}];
  const reflog=[{kind:'branch',name:'main',old:previous,new:root,created_at:'2026-09-18T00:00:06Z'},
    {kind:'branch',name:'main',old:source,new:root,created_at:'2026-09-18T00:00:07Z'}];
  const {pageErrors,unexpected}=await openGraph(page,publicWorkspaceApi(snapshots,refs,[],[],reflog,history));
  await page.locator('.graph-merge-records summary').click();
  const evidence=page.locator('.graph-merge-records li');
  const evidenceText=await evidence.innerText();
  await page.locator('.graph-archive-panel summary').click();
  await page.locator('.graph-previous > summary').click();
  const controls=[page.locator('.graph-history-toggle').nth(0),page.locator('.graph-history-toggle').nth(1),page.locator('.graph-archive-toggle')];
  await expect(controls[1]).toBeVisible();
  // Gray-code traversal visits all 8 states, changing only one control each time.
  let prior=0;
  for(const mask of [0,1,3,2,6,7,5,4]) {
    const changed=mask^prior;
    if(changed) await controls[Math.log2(changed)].click();
    prior=mask;
    await expect(evidence).toHaveText(evidenceText, {useInnerText:true});
    await expect(evidence).toHaveAttribute('data-branch-lineage','natural');
    await expect(page.locator('.graph-invalid')).toHaveCount(0);
    await expect(page.locator('.graph-history-error')).toHaveCount(0);
    if(mask&3) {
      await expectRenderedGraphPath(page,'graph:merge:fold-merge',source);
      await expectRenderedGraphPath(page,source,'graph:birth:fold-birth');
    } else await expect(page.locator('[data-graph-event="merge"]')).toHaveCount(0);
  }
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});


test('Git reversal history loads on demand and preserves partial and unverified outcomes', async ({page}) => {
 const snap = auditSnapshot(pushedHead, 'main', [], 'Retained merged context', 1);
 const base = publicWorkspaceApi([snap], [{kind: 'branch', name: 'main', target: pushedHead, repo_id: repoId}]);
 let requests = 0;
 const row = (char: string, coverage: string) => ({id: char.repeat(64), request: {commit: char.repeat(40), target: 'f'.repeat(40)}, state: 'completed', version: '1', coverage, verified_paths: coverage === 'partial' ? 1 : 0, unverified_paths: 2, updated_at: '2026-09-19T01:00:00Z'});
 const {pageErrors, unexpected} = await openGraph(page, req => {
  if (req.pathname === `/api/v1/repos/${repoId}/git-changes`) {
   requests++;
   return {body: req.searchParams.get('cursor') ? {items: [row('b', 'unverified')]} : {items: [row('a', 'partial')], next_cursor: 'a'.repeat(64)}};
  }
  return base(req);
 });
 expect(requests).toBe(0);
 const panel = page.locator('.git-changes');
 await panel.locator('summary').click();
 await expect(panel.locator('li')).toHaveCount(1);
 await expect(panel).toContainText('Some paths need review');
 await expect(panel).toContainText('1 verified paths');
 await panel.getByRole('button', {name: 'Load more'}).click();
 await expect(panel.locator('li')).toHaveCount(2);
 await expect(panel).toContainText('Reversal unverified');
 await expect(panel.getByRole('button', {name: 'Retry'})).toHaveCount(0);
 await expect(page.locator(`.graph-row[data-graph-id="${pushedHead}"]`)).toHaveCount(1);
 await panel.locator('summary').click();
 await expect(panel.locator('li')).toHaveCount(0);
 expect(pageErrors).toEqual([]);
 expect(unexpected).toEqual([]);
});

test('automatic Git discovery shows deferred work without changing graph facts', async ({page}) => {
 const snap = auditSnapshot(pushedHead, 'main', [], 'Retained context', 1);
 const base = publicWorkspaceApi([snap], [{kind: 'branch', name: 'main', target: pushedHead, repo_id: repoId}]);
 let requests = 0;
 const {pageErrors, unexpected} = await openGraph(page, req => {
  if (req.pathname === `/api/v1/repos/${repoId}/git-scans`) {
   requests++;
   return {body: {items: [{id: 'a'.repeat(64), commit: 'b'.repeat(40), state: 'retrying', indexed: false, version: '2', reason: 'temporary_provider_or_storage_failure', updated_at: '2026-09-19T01:00:00Z'}]}};
  }
  return base(req);
 });
 expect(requests).toBe(0);
 const panel = page.locator('.git-scans');
 await panel.locator('summary').click();
 await expect(panel.locator('li')).toHaveCount(1);
 await expect(panel).toContainText('Waiting for complete Git objects');
 await expect(panel.getByRole('button', {name: 'Retry', exact: true})).toHaveCount(0);
 await expect(page.locator(`.graph-row[data-graph-id="${pushedHead}"]`)).toHaveCount(1);
 await panel.locator('summary').click();
 await expect(panel.locator('li')).toHaveCount(0);
 expect(requests).toBe(1);
 expect(pageErrors).toEqual([]);
 expect(unexpected).toEqual([]);
});

test('code applicability uses explicit selection and server state without moving graph history', async ({page}) => {
 const snap = auditSnapshot(pushedHead, 'main', [], 'Retained merged context', 1);
 const base = publicWorkspaceApi([snap], [{kind: 'branch', name: 'main', target: pushedHead, repo_id: repoId}]);
 const requests: string[] = [];
 const {pageErrors, unexpected} = await openGraph(page, req => {
  if (req.pathname === `/api/v1/repos/${repoId}/code-applicability`) {
   const code = req.searchParams.get('code_commit')!;
   requests.push(code);
   expect(req.searchParams.get('source_commit')).toBe('a'.repeat(40));
   expect(req.searchParams.getAll('path')).toEqual(['feature.ts']);
   return {body: {selection: {code_commit: code, source_commit: 'a'.repeat(40), paths: ['feature.ts']}, revision: {graph: '1', pending: '0'}, state_hash: id('f'), relation: code.startsWith('b') ? 'ancestor' : 'unknown', paths: [{path: 'feature.ts', state: code.startsWith('b') ? 'before' : 'unknown'}]}};
  }
  return base(req);
 });
 const panel = page.locator('.code-applicability');
 expect(requests).toHaveLength(0);
 await panel.locator('summary').click();
 expect(requests).toHaveLength(0);
 await panel.getByLabel('Selected code').fill('b'.repeat(40));
 await panel.getByLabel('Source change').fill('a'.repeat(40));
 await panel.getByLabel('Repository file path').fill('feature.ts');
 await panel.getByRole('button', {name: 'Check file state'}).click();
 await expect(panel).toContainText('Matches the state before the change');
 await panel.getByLabel('Selected code').fill('c'.repeat(40));
 await panel.getByRole('button', {name: 'Check file state'}).click();
 await expect(panel).toContainText('Needs verification');
 await expect(panel).not.toContainText('Matches the state before the change');
 await expect(page.locator(`.graph-row[data-graph-id="${pushedHead}"]`)).toHaveCount(1);
 await panel.locator('summary').click();
 await expect(panel.locator('form')).toHaveCount(0);
 expect(requests).toHaveLength(2);
 expect(pageErrors).toEqual([]);
 expect(unexpected).toEqual([]);
});


test('evidence-only revisions refresh applicability without downloading the graph', async ({page}) => {
 const snap = auditSnapshot(pushedHead, 'main', [], 'Historical completion retained', 1);
 const base = publicWorkspaceApi([snap], [{kind: 'branch', name: 'main', target: pushedHead, repo_id: repoId}]);
 let evidence = '1', fullReads = 0;
 page.on('request', req => {if (new URL(req.url()).pathname.endsWith('/view')) fullReads++;});
 const {pageErrors, unexpected} = await openGraph(page, req => {
  const revision = {graph: '1', pending: '1', evidence};
  if (req.pathname.endsWith('/view')) return {body:{refs:[{kind:'branch',name:'main',target:pushedHead,repo_id:repoId}],snapshots:[snap],pending:[],unsync:[],reflog:[],history:[],revision}};
  if (req.pathname.endsWith('/changes')) return {contentType: 'text/event-stream', body: `event: revision\ndata: ${JSON.stringify(revision)}\n\n`};
  if (req.pathname.endsWith('/revision')) return {body: revision};
  if (req.pathname.endsWith('/code-applicability')) return {body: {selection: {code_commit: 'b'.repeat(40), source_commit: 'a'.repeat(40), paths: ['feature.ts']}, revision, state_hash: id('f'), relation: 'ancestor', paths: [{path: 'feature.ts', state: evidence === '1' ? 'unknown' : 'applied'}]}};
  return base(req);
 });
 const panel = page.locator('.code-applicability');
 await panel.locator('summary').click();
 await panel.getByLabel('Selected code').fill('b'.repeat(40));
 await panel.getByLabel('Source change').fill('a'.repeat(40));
 await panel.getByLabel('Repository file path').fill('feature.ts');
 await panel.getByRole('button', {name: 'Check file state'}).click();
 await expect(panel).toContainText('Needs verification');
 const before = fullReads;
 evidence = '2';
 await expect(panel).toContainText('Matches the applied change', {timeout: 10_000});
 expect(fullReads).toBe(before);
 await expect(page.locator(`.graph-row[data-graph-id="${pushedHead}"]`)).toHaveCount(1);
 expect(pageErrors).toEqual([]); expect(unexpected).toEqual([]);
});

function memoryPublication(snapshot: string, code: string, event = 'a'.repeat(32), branch = 'main') {
 return {id:event, repo_id:repoId, branch_id:branch, branch, kind:'publish', source:snapshot, target:snapshot, git_after:code, created_at:'2026-09-20T00:00:00Z'};
}

test('context keeps original memory and leaves integrated memory to agent APIs', async ({page}) => {
 const snap={...auditSnapshot(pushedHead,'main',[],'Original saved context',1),memory_hash:id('a')};
 const base=publicWorkspaceApi([snap],[{kind:'branch',name:'main',target:pushedHead}],[],[],[],[memoryPublication(pushedHead,'a'.repeat(40))]);
 const machineReads:string[]=[];
 const {pageErrors,unexpected}=await openGraph(page,req=>{
  if(req.pathname.includes('/effective-memory')) {machineReads.push(req.pathname);return {status:500,body:{error:{message:'Unexpected automatic assessment'}}};}
  return base(req);
 });
 await expect(page.locator('.memory-box')).not.toHaveAttribute('open');
 await expect(page.getByLabel('View mode', {exact:true})).toHaveValue('chat');
 await page.locator('.memory-box > summary').click();
 await expect(page.locator('.memory-box')).toContainText('fixture memory');
 await expect(page.getByText('Visible fixture prompt',{exact:true})).toBeVisible();
 await expect(page.locator('.effective-memory')).toHaveCount(0);
 await expect(page.getByRole('button',{name:'↓ memory'})).toBeVisible();
 await page.getByLabel('View mode', {exact:true}).selectOption('all');
 await page.reload();
 await expect(page.locator('.memory-box')).not.toHaveAttribute('open');
 await expect(page.getByLabel('View mode', {exact:true})).toHaveValue('chat');
 expect(machineReads).toEqual([]);
 expect(pageErrors).toEqual([]);expect(unexpected).toEqual([]);
});

test('main includes PR conversations and evidence refreshes the graph without automatic memory reads', async ({page}) => {
 const head=pushedHead, a=appendedRoot, b=graftTarget, baseID=id('9');
 const code='3'.repeat(40);
 const snapshots=[
  {...auditSnapshot(head,'main',[baseID],'Continued main',4),memory_hash:id('8')},
  auditSnapshot(a,'feature-a',[baseID],'First integrated PR context',3),
  auditSnapshot(b,'feature-b',[baseID],'Second integrated PR context',1),
  auditSnapshot(baseID,'main',[],'Prior main context',0),
 ];
 const refs=[{kind:'branch',name:'main',branch_id:'main-id',target:head,repo_id:repoId}];
 const history=[a,b].map((source,i)=>({id:`integration-${i}`,repo_id:repoId,branch:'main',branch_id:'main-id',source_branch_id:`topic-${i}`,kind:'pr-merge',source,target:source,shared_target:baseID,pr_completed:true,
  pr:{number:i+1,base_branch:'main',head_branch:`feature-${i}`,head_sha:'a'.repeat(40),merge_sha:String(i+1).repeat(40)},created_at:`2026-01-0${3-i}T00:00:00Z`}));
 let evidence='1',fullReads=0,patchReads=0;
 const inclusion=()=>({branch_id:'main-id',snapshot_id:head,code_commit:code,reason:'selected_code',roots:evidence==='1'?[head]:[a,b,head],snapshot_ids:evidence==='1'?[head,baseID]:[head,b,a,baseID],
  merges:history.map((h,i)=>({event_id:h.id,source:h.source,before:baseID,merge_sha:h.pr.merge_sha,pr_number:i+1,state:evidence==='1'?'review':'included',reason:'verified_git_order',order:1-i}))});
 const view=()=>({snapshots,refs,history,pending:[],unsync:[],reflog:[],revision:{graph:'1',pending:'1',evidence},graph:{branch_contexts:{main:inclusion()}}});
 const base=publicWorkspaceApi(snapshots,refs,[],[],[],history);
 const {pageErrors,unexpected}=await openGraph(page,req=>{
  const v=view();
  if(req.pathname.endsWith('/view')) {fullReads++;return {body:v};}
  if(req.pathname.endsWith('/revision')) return {body:v.revision};
  if(req.pathname.endsWith('/changes')) return {contentType:'text/event-stream',body:`event: revision\ndata: ${JSON.stringify(v.revision)}\n\n`};
  if(req.pathname.endsWith('/pending-view')) {patchReads++;const full=serverGraphWireFixture(v as any);return {body:{graph:full.graph,revision:v.revision,pending:[],snapshots:[]}};}
  if(req.pathname.includes('/effective-memory')) throw new Error('Context must not assess agent memory');
  return base(req);
 });
 await expect(page.locator('.effective-memory')).toHaveCount(0);
 await expect(page.locator('.commits .commit-msg')).toHaveText(['Continued main','Prior main context']);
 const before=fullReads;
 evidence='2';
 await expect(page.locator('.commits .commit-msg')).toHaveText(['Continued main','Second integrated PR context','First integrated PR context','Prior main context']);
 await expectRenderedGraphPath(page,head,'graph:merge:integration-1');
 await expectRenderedGraphPath(page,'graph:merge:integration-1','graph:merge:integration-0');
 expect(fullReads).toBe(before);expect(patchReads).toBeGreaterThan(0);
 expect(pageErrors).toEqual([]);expect(unexpected).toEqual([]);
});

for(const fork of ['main','feature-parent'] as const) for(const continuation of ['contributor','local'] as const) test(`PR checkpoint keeps recorded ${fork} fork and ${continuation} continuation on main`,async({page})=>{
 const baseID=id('9'),a=id('a'),checkpoint=id('8'),parent=id('d'),b=id('b'),head=pushedHead;
 const origin=fork==='main'?checkpoint:parent;
 const snapshots=[auditSnapshot(head,'main',[b],'Current main',8),auditSnapshot(b,'feature-child',[origin],'Child work',6),auditSnapshot(checkpoint,'main',[continuation==='contributor'?a:baseID],'Checkpoint on main',4),auditSnapshot(parent,'feature-parent',[baseID],'Parent branch work',3),auditSnapshot(a,'feature-first',[baseID],'Earlier PR',2),auditSnapshot(baseID,'main',[],'Base',0)];
 const refs=[{kind:'branch',name:'main',branch_id:'main-id',target:head,repo_id:repoId},{kind:'branch',name:'feature-parent',branch_id:'parent-id',target:parent,repo_id:repoId}];
 const birth={id:'child-birth',repo_id:repoId,branch_id:'child-id',branch:'feature-child',kind:'birth',source:origin,target:origin,created_at:'2026-01-01T00:00:05Z'};
 const merges=[a,b].map((source,i)=>({id:`checkpoint-merge-${i}`,repo_id:repoId,branch:'main',branch_id:'main-id',source_branch_id:i?'child-id':'first-id',kind:'pr-merge',source,target:source,shared_target:i?checkpoint:baseID,pr_completed:true,
  pr:{number:i+1,base_branch:'main',head_branch:i?'feature-child':'feature-first',head_sha:'a'.repeat(40),merge_sha:String(i+1).repeat(40)},created_at:`2026-01-01T00:00:0${i?7:3}Z`}));
 const history=[birth,...merges];
 const inclusion={branch_id:'main-id',snapshot_id:head,code_commit:'2'.repeat(40),reason:'selected_code',roots:[baseID,a,checkpoint,b,head],snapshot_ids:[head,b,checkpoint,parent,a,baseID],merges:merges.map((h,i)=>({event_id:h.id,source:h.source,before:h.shared_target,merge_sha:h.pr.merge_sha,pr_number:i+1,state:'included',reason:'verified_git_order',order:1-i}))};
 const base=publicWorkspaceApi(snapshots,refs,[],[],[],history);
 const {pageErrors,unexpected}=await openGraph(page,req=>req.pathname.endsWith('/view')?{body:{snapshots,refs,history,pending:[],unsync:[],reflog:[],revision:{graph:'1',pending:'1',evidence:'1'},graph:{branch_contexts:{main:inclusion}}}}:base(req));
 for(const node of [head,'graph:merge:checkpoint-merge-1',checkpoint,'graph:merge:checkpoint-merge-0']) await expect(page.locator(`[data-graph-id="${node}"]`)).toHaveAttribute('data-graph-node-lane','0');
 await expectRenderedGraphPath(page,head,'graph:merge:checkpoint-merge-1');
 await expectRenderedGraphPath(page,'graph:merge:checkpoint-merge-1',checkpoint);
 await expectRenderedGraphPath(page,checkpoint,'graph:merge:checkpoint-merge-0');
 await expectRenderedGraphPath(page,'graph:merge:checkpoint-merge-1',b);
 await expectRenderedGraphPath(page,b,'graph:birth:child-birth');
 await expectRenderedGraphPath(page,'graph:birth:child-birth',origin);
 const childLane=await page.locator(`[data-graph-id="${b}"]`).getAttribute('data-graph-node-lane');
 expect(childLane).not.toBe('0');
 await expect(page.locator(`.graph-lane-label[data-graph-lane="${childLane}"]`)).toHaveAttribute('aria-label','feature-child');
 if(fork==='feature-parent') await expect(page.locator(`[data-graph-id="${parent}"]`)).not.toHaveAttribute('data-graph-node-lane','0');
 expect(pageErrors).toEqual([]);expect(unexpected).toEqual([]);
});
