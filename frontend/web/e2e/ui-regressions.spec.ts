import { expect, test, type Page } from '@playwright/test';
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

function publicWorkspaceApi(snapshots: unknown[], refs: unknown[], pending: unknown[] = [], unsync: unknown[] = []) {
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
    if (pathname.startsWith(`/api/v1/repos/${repoId}/docs/`)) {
      return { body: sessionDoc(decodeURIComponent(pathname.split('/').at(-1) ?? '')) };
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

test('pricing publishes the storage-only contract and calculates GitHub-aligned overage', async ({ page }) => {
  const pageErrors = capturePageErrors(page);
  const unexpected = await installApiFixture(page, ({ method, pathname }) => {
    if (method === 'GET' && pathname === '/api/v1/me') {
      return { status: 401, body: { error: { message: 'anonymous fixture' } } };
    }
    return undefined;
  });

  await page.goto('/pricing');
  await expect(page).toHaveURL(/\/pricing$/);
  await expect(page.locator('.pricing-hero h1')).toHaveText('Pay for context, not headcount.');
  await expect(page.locator('.pricing-free')).toContainText('10 GiB');
  await expect(page.locator('.pricing-overage')).toContainText('$0.07');
  await expect(page.locator('.pricing-includes')).toContainText('No per-seat fee');
  await expect(page.locator('.pricing-notice')).toContainText('No overage is charged');

  const input = page.locator('#pricing-storage');
  await input.fill('25');
  await expect(page.locator('.pricing-calc-lines .total')).toContainText('$1.05');
  await input.fill('10');
  await expect(page.locator('.pricing-calc-lines .total')).toContainText('$0.00');
  await expect(page.locator('.pricing-source-links a')).toHaveCount(2);

  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.locator('.pricing-card')).toBeVisible();
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

  await page.goto('/alice/cxthub/members');
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

  await page.goto('/alice/cxthub/settings');
  await expect(page.locator('.permission-controls')).toBeVisible();
  await expect(page.locator('.role-capabilities')).toHaveCount(0);
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
    if (pathname.startsWith(`/api/v1/repos/${repoId}/settings/`)) return { body: null };
    if (pathname === `/api/v1/repos/${repoId}/secrets`) return { body: null };
    if (pathname.startsWith(`/api/v1/repos/${repoId}/docs/`)) {
      return { body: sessionDoc(decodeURIComponent(pathname.split('/').at(-1) ?? '')) };
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

  await page.goto('/alice/cxthub/settings');
  await expect(page.locator('.access-denied')).toContainText('Workspace settings require owner access');
  await expect(page.locator('.ws-settings-form')).toHaveCount(0);
  expect(pageErrors).toEqual([]);
  expect(unexpected).toEqual([]);
});

test('anonymous public settings URL renders access denial instead of workspace context', async ({ page }) => {
  const pageErrors = capturePageErrors(page);
  const unexpected = await installApiFixture(page, publicWorkspaceApi([], []));

  await page.goto('/alice/cxthub/settings');
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

  await page.goto('/alice/cxthub/settings');
  await expect(page).toHaveURL(/\/alice\/cxthub\/settings$/);
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

test('archived branches stay discoverable even when their history is shared with an active branch', async ({ page }) => {
  const archivedHead = id('9');
  const snapshots = [
    {
      id: pushedHead,
      repo_id: repoId,
      branch: 'main',
      parents: [],
      doc_hash: pushedHead,
      provider: 'codex',
      fidelity: 'full',
      message: 'active main head',
      created_at: '2026-08-31T04:00:00Z',
    },
    {
      id: archivedHead,
      repo_id: repoId,
      branch: 'feature/archived-only',
      parents: [],
      doc_hash: archivedHead,
      provider: 'claude',
      fidelity: 'full',
      message: 'archived-only history',
      created_at: '2026-08-31T03:00:00Z',
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
    lifecycleRef('feature/shared-history', pushedHead, 1),
    lifecycleRef('feature/archived-only', archivedHead, 2),
  ];
  const { pageErrors, unexpected } = await openGraph(page, publicWorkspaceApi(snapshots, refs));

  const panel = page.locator('.graph-archive-panel');
  await expect(panel.locator('summary')).toContainText('Archived branches 2');
  await panel.locator('summary').click();
  const sharedEntry = panel.locator('.graph-archive-entry').filter({ hasText: 'feature/shared-history' });
  const uniqueEntry = panel.locator('.graph-archive-entry').filter({ hasText: 'feature/archived-only' });
  await expect(sharedEntry).toContainText('Shared with an active lineage');
  await expect(uniqueEntry).toContainText('1 unique commit');
  await expect(page.locator('.graph-row')).toHaveCount(1);

  await uniqueEntry.click();
  await expect(page.locator('.graph-row')).toHaveCount(2);
  await expect(page.locator('.graph-row.on')).toHaveAttribute('aria-label', /^archived-only history/);
  const hide = panel.locator('.graph-archive-toggle');
  await expect(hide).toContainText('Hide 1 archived');
  await hide.click();
  await expect(page.locator('.graph-row')).toHaveCount(1);
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
