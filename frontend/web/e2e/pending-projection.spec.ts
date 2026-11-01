import { expect, test, type Page } from '@playwright/test';
import type { QueryClient } from '@tanstack/react-query';
import type { PendingStateView, RepositoryRevision } from '../src/types';
import { fullFixture, pendingFixture, pendingStateFixture, wireFixture } from '../tests/pendingViewFixture';

async function pendingQueryState(page: Page) {
  return page.evaluate(() => {
    const client = (window as unknown as { documentQueryClient: QueryClient }).documentQueryClient;
    return client?.getQueryState<PendingStateView>(['pending-state', 'repo', 'on-hold-v1']);
  });
}

async function prepareOnHold(page: Page) {
  await page.clock.install({ time: new Date('2026-10-18T00:00:00Z') });
  await page.clock.pauseAt(new Date('2026-10-18T00:00:01Z'));
  await page.addInitScript(() => {
    localStorage.setItem('cxt.locale', 'en');
    Math.random = () => 0;
    const sources = new Set<EventTarget>();
    class Source extends EventTarget {
      constructor() { super(); sources.add(this); }
      close() { sources.delete(this); }
    }
    Object.assign(window, { EventSource: Source, emitRepositoryRevision: (revision: unknown) => {
      for (const source of sources) source.dispatchEvent(new MessageEvent('revision', { data: JSON.stringify(revision) }));
    } });
  });
  await page.route('**/__pending-projection-harness*', route => route.fulfill({
    contentType: 'text/html', body: `<!doctype html><html><body><div id="root"></div><script type="module">
import RefreshRuntime from '/@react-refresh';
RefreshRuntime.injectIntoGlobalHook(window);
window.$RefreshReg$ = () => {};
window.$RefreshSig$ = () => type => type;
window.__vite_plugin_react_preamble_installed__ = true;
await import('/e2e/document-paging-harness.ts');
</script></body></html>`,
  }));
}

for (const recovery of ['visibility', 'retry'] as const) test(`cheap notices gate LIVE and body reads; revision denial fences the selected viewer until authorized ${recovery}`, async ({ page }) => {
  await prepareOnHold(page);
  const revision = { graph: '1', pending: '1' };
  const epoch = '12345678-abcd-4321-9876-123456789abc:1';
  const generation = `sha256:${'a'.repeat(64)}`;
  const ready: PendingStateView = { ...pendingStateFixture(pendingFixture('cached-document', revision)), read_generation: {
    generation, source_epoch: epoch, applied_epoch: epoch, rules: 1, complete: true,
  } };
  ready.pending[0].activity_at = '2026-10-18T00:00:00Z';
  let notice: RepositoryRevision = { ...revision, pending_projection: { state: 'ready', source_epoch: epoch, generation, rules: 1 } };
  let denied = false, pendingReads = 0, fullReads = 0, revisionReads = 0;
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname === '/api/v1/repos/repo/revision') {
      revisionReads++;
      return route.fulfill(denied ? { status: 403, json: { error: { code: 'forbidden', message: 'Access denied' } } } : { json: notice });
    }
    if (url.pathname === '/api/v1/repos/repo/pending-view' && url.searchParams.get('projection') === 'on-hold-v1') {
      pendingReads++;
      return route.fulfill({ json: ready });
    }
    if (url.pathname === '/api/v1/repos/repo/view') {
      fullReads++;
      return route.fulfill({ json: wireFixture(fullFixture(pendingFixture('cached-document', revision))) });
    }
    if (url.pathname === '/api/v1/me') return route.fulfill({ status: 401, json: {} });
    if (url.pathname.endsWith('/events')) return route.fulfill({ json: {
      hash: 'cached-document', envelope: { cir_version: '2', source_provider: 'codex' },
      events: [{ kind: 'message', role: 'user', seq: 0, event_index: 0, deferred: false,
        blocks: [{ type: 'text', text: 'Authorized cached conversation' }] }],
      offset: 0, total: 1, inherited: 0, next: -1,
    } });
    return route.fulfill({ status: 404, json: {} });
  });
  const emit = () => page.evaluate(value => {
    (window as unknown as { emitRepositoryRevision(revision: unknown): void }).emitRepositoryRevision(value);
  }, notice);
  await page.goto('/__pending-projection-harness?onhold');
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return page.locator('.pending-sessions[data-live="true"] .commit-row').count();
  }).toBe(1);
  await page.locator('.pending-sessions .commit-row').click();
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return page.getByText('Authorized cached conversation', { exact: true }).isVisible();
  }).toBe(true);
  const initialPendingReads = pendingReads, initialFullReads = fullReads;
  notice = { ...revision, pending_projection: { state: 'catching_up', source_epoch: epoch, rules: 1 } };
  await emit();
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return page.locator('.pending-sessions[data-live="true"]').count();
  }).toBe(0);
  await expect(page.locator('.live-viewer-status')).not.toContainText('LIVE');
  await page.clock.runFor(3000);
  expect(pendingReads).toBe(initialPendingReads);
  notice = { ...revision, pending_projection: { state: 'needs_attention', source_epoch: epoch, rules: 1 } };
  await emit();
  await page.clock.runFor(3000);
  expect(pendingReads).toBe(initialPendingReads);
  await expect(page.locator('.ctx-main').getByRole('alert')).toContainText('requires server attention');
  await expect(page.locator('.ctx-main').getByRole('status').filter({ hasText: 'Refreshing On Hold' })).toHaveCount(0);
  await expect(page.locator('.pending-sessions[data-live="true"]')).toHaveCount(0);
  notice = { ...revision, pending_projection: { state: 'ready', source_epoch: epoch, generation, rules: 1 } };
  await emit();
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return page.locator('.pending-sessions[data-live="true"]').count();
  }).toBe(1);
  expect(pendingReads).toBe(initialPendingReads);
  denied = true;
  await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return page.evaluate(() => (window as unknown as { documentQueryClient: QueryClient }).documentQueryClient
      .getQueryState(['repository-notice', 'repo'])?.error?.message);
  }).toContain('Access denied');
  await page.clock.runFor(1);
  await expect(page.locator('.ctx-main').getByRole('alert')).toContainText('Access denied');
  await expect(page.locator('.viewer')).toHaveCount(0);
  await expect(page.locator('.pending-sessions .commit-row')).toHaveCount(0);
  await emit();
  await page.clock.runFor(1000);
  await expect(page.locator('.viewer')).toHaveCount(0);
  await expect(page.locator('.ctx-main').getByRole('alert')).toContainText('Access denied');
  expect(pendingReads).toBe(initialPendingReads);
  expect(fullReads).toBe(initialFullReads);
  denied = false;
  const beforeAuthorization = revisionReads;
  if (recovery === 'retry') await page.locator('.ctx-main').getByRole('alert').getByRole('button', { name: 'Retry', exact: true }).click();
  else await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return page.locator('.pending-sessions[data-live="true"] .commit-row').count();
  }).toBe(1);
  expect(revisionReads).toBe(beforeAuthorization + 1);
  expect(pendingReads).toBe(initialPendingReads + 1);
  expect(fullReads).toBe(initialFullReads + 1);
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return page.getByText('Authorized cached conversation', { exact: true }).isVisible();
  }).toBe(true);
  expect(errors).toEqual([]);
});

for (const reset of [false, true]) {
  test(`source-only ${reset ? 'UUID reset refreshes full once' : 'sequence publication keeps full cached'} at identical counters`, async ({ page }) => {
    await prepareOnHold(page);
    const revision = { graph: '1', pending: '1' };
    let ready: PendingStateView = { ...pendingStateFixture(pendingFixture('old-document', revision)), read_generation: {
      generation: `sha256:${'a'.repeat(64)}`, source_epoch: '12345678-abcd-4321-9876-123456789abc:1',
      applied_epoch: '12345678-abcd-4321-9876-123456789abc:1', rules: 1, complete: true,
    } };
    ready.pending[0].activity_at = '2026-10-18T00:00:00Z';
    const notice = (): RepositoryRevision => ({ ...revision, pending_projection: {
      state: 'ready', generation: ready.read_generation!.generation, source_epoch: ready.read_generation!.source_epoch, rules: 1,
    } });
    let pendingReads = 0, fullReads = 0;
    const errors: string[] = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.route('**/api/v1/**', route => {
      const url = new URL(route.request().url());
      if (url.pathname === '/api/v1/repos/repo/revision') return route.fulfill({ json: notice() });
      if (url.pathname === '/api/v1/repos/repo/pending-view' && url.searchParams.get('projection') === 'on-hold-v1') {
        pendingReads++;
        return route.fulfill({ json: ready });
      }
      if (url.pathname === '/api/v1/repos/repo/view') {
        fullReads++;
        return route.fulfill({ json: wireFixture(fullFixture(pendingFixture(ready.pending[0].target, revision))) });
      }
      if (url.pathname === '/api/v1/me') return route.fulfill({ status: 401, json: {} });
      if (url.pathname.endsWith('/events')) {
        const hash = url.pathname.split('/').at(-2)!;
        return route.fulfill({ json: {
          hash, envelope: { cir_version: '2', source_provider: 'codex' },
          events: [{ kind: 'message', role: 'user', seq: 0, event_index: 0, deferred: false,
            blocks: [{ type: 'text', text: `Conversation ${hash}` }] }],
          offset: 0, total: 1, inherited: 0, next: -1,
        } });
      }
      return route.fulfill({ status: 404, json: {} });
    });
    await page.goto('/__pending-projection-harness?onhold');
    await expect.poll(async () => {
      await page.clock.runFor(1);
      return page.locator('.pending-sessions[data-live="true"] .commit-row').count();
    }).toBe(1);
    await page.locator('.pending-sessions .commit-row').click();
    await expect.poll(async () => {
      await page.clock.runFor(1);
      return page.getByText('Conversation old-document', { exact: true }).isVisible();
    }).toBe(true);
    const initialPending = pendingReads, initialFull = fullReads;
    const epoch = reset ? '22345678-abcd-4321-9876-123456789abc:0' : '12345678-abcd-4321-9876-123456789abc:2';
    ready = { ...pendingStateFixture(pendingFixture('new-document', revision)), read_generation: {
      ...ready.read_generation!, generation: `sha256:${'b'.repeat(64)}`, source_epoch: epoch, applied_epoch: epoch,
    } };
    ready.pending[0].activity_at = '2026-10-18T00:00:00Z';
    await page.evaluate(value => {
      (window as unknown as { emitRepositoryRevision(revision: unknown): void }).emitRepositoryRevision(value);
    }, notice());
    await expect.poll(async () => {
      await page.clock.runFor(1);
      return page.getByText('Conversation new-document', { exact: true }).isVisible();
    }).toBe(true);
    expect(pendingReads).toBe(initialPending + 1);
    expect(fullReads).toBe(initialFull + (reset ? 1 : 0));
    expect((await pendingQueryState(page))?.data?.read_generation).toEqual(ready.read_generation);
    await page.evaluate(value => {
      const emit = (window as unknown as { emitRepositoryRevision(revision: unknown): void }).emitRepositoryRevision;
      emit(value);
      emit(value);
      document.dispatchEvent(new Event('visibilitychange'));
    }, notice());
    await page.clock.runFor(16000);
    expect(pendingReads).toBe(initialPending + 1);
    expect(fullReads).toBe(initialFull + (reset ? 1 : 0));
    expect(errors).toEqual([]);
  });
}

test('On Hold retries a pending projection and renders its ready generation without a full graph', async ({ page }) => {
  await prepareOnHold(page);
  const ready: PendingStateView = {
    ...pendingStateFixture(pendingFixture('ready-document')),
    read_generation: {
      generation: `sha256:${'a'.repeat(64)}`,
      source_epoch: '12345678-abcd-4321-9876-123456789abc:9007199254740993',
      applied_epoch: '12345678-abcd-4321-9876-123456789abc:9007199254740993',
      rules: 1,
      complete: true,
    },
  };
  const errors: string[] = [];
  const pendingReads: string[] = [];
  const documents: string[] = [];
  const unexpected: string[] = [];
  let fullReads = 0;
  page.on('pageerror', error => errors.push(error.message));
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    if (url.pathname === '/api/v1/repos/repo/pending-view' && url.searchParams.get('projection') === 'on-hold-v1') {
      pendingReads.push(url.pathname + url.search);
      return route.fulfill(pendingReads.length <= 2
        ? { status: 503, headers: { 'Retry-After': '3' }, json: { error: { code: 'pending_projection_pending', message: 'Preparing On Hold' } } }
        : { json: ready });
    }
    if (url.pathname === '/api/v1/repos/repo/view') {
      fullReads++;
      return route.fulfill({ status: 503, json: { error: { code: 'graph_unavailable', message: 'Full graph unavailable' } } });
    }
    if (url.pathname === '/api/v1/repos/repo/revision') return route.fulfill({ json: ready.revision });
    if (url.pathname === '/api/v1/me') return route.fulfill({ status: 401, json: {} });
    if (url.pathname === '/api/v1/repos/repo/docs/ready-document/events') {
      documents.push(url.pathname + url.search);
      return route.fulfill({ json: {
        hash: 'ready-document', envelope: { cir_version: '2', source_provider: 'codex' },
        events: [{ kind: 'message', role: 'user', seq: 700, event_index: 0, deferred: false,
          blocks: [{ type: 'text', text: 'Ready projection conversation' }] }],
        offset: 0, total: 1, inherited: 0, next: -1,
      } });
    }
    unexpected.push(url.pathname + url.search);
    return route.fulfill({ status: 404, json: {} });
  });
  await page.goto('/__pending-projection-harness?onhold');
  for (const failureCount of [1, 2]) {
    await expect.poll(async () => {
      await page.clock.runFor(1);
      return (await pendingQueryState(page))?.fetchFailureCount;
    }).toBe(failureCount);
    await page.clock.runFor(1);
    await expect(page.locator('.ctx-main .skel')).toBeVisible();
    await expect(page.locator('.pending-sessions .commit-row')).toHaveCount(0);
    await expect(page.locator('.ctx-main .empty-box')).toHaveCount(0);
    await expect(page.locator('.ctx-main').getByRole('alert')).toHaveCount(0);
    await expect(page.getByRole('complementary').getByRole('alert')).toContainText('Full graph unavailable');
    await page.clock.runFor(2900);
    expect(pendingReads).toHaveLength(failureCount);
    await page.clock.runFor(100);
  }
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return (await pendingQueryState(page))?.status;
  }).toBe('success');
  await page.clock.runFor(1);
  await expect(page.locator('.pending-sessions .commit-row')).toHaveCount(1);
  await expect(page.locator('.pending-sessions .commit-row code')).toHaveText('ready-docu');
  await expect(page.locator('.ctx-main .skel')).toHaveCount(0);
  await expect(page.locator('.ctx-main').getByRole('alert')).toHaveCount(0);
  await expect(page.getByRole('complementary').getByRole('alert')).toContainText('Full graph unavailable');
  expect((await pendingQueryState(page))?.data?.read_generation).toEqual(ready.read_generation);
  expect(pendingReads).toHaveLength(3);
  expect(new Set(pendingReads)).toEqual(new Set(['/api/v1/repos/repo/pending-view?projection=on-hold-v1']));
  await page.locator('.pending-sessions .commit-row').click();
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return page.getByText('Ready projection conversation', { exact: true }).isVisible();
  }).toBe(true);
  expect(documents).toHaveLength(1);
  expect(await page.evaluate(() => {
    const client = (window as unknown as { documentQueryClient: QueryClient }).documentQueryClient;
    const full = client.getQueryState(['repo-view', 'repo']);
    return { status: full?.status, hasData: full?.data !== undefined };
  })).toEqual({ status: 'error', hasData: false });
  expect(fullReads).toBe(1);
  expect(unexpected).toEqual([]);
  expect(errors).toEqual([]);
});

test('cached durable rows lose LIVE during lag, follow a new epoch and disappear on access denial', async ({ page }) => {
  await prepareOnHold(page);
  let phase = 'ready';
  let pendingReads = 0;
  const errors: string[] = [];
  const unexpected: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  let ready: PendingStateView = {
    ...pendingStateFixture(pendingFixture('cached-document', { graph: '1', pending: '1' })),
    read_generation: {
      generation: `sha256:${'a'.repeat(64)}`, source_epoch: '12345678-abcd-4321-9876-123456789abc:99',
      applied_epoch: '12345678-abcd-4321-9876-123456789abc:99', rules: 1, complete: true,
    },
  };
  ready.pending[0].activity_at = '2026-10-18T00:00:00Z';
  const oldFull = wireFixture(fullFixture(pendingFixture('obsolete-full', { graph: '100', pending: '100' })));
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname === '/api/v1/repos/repo/pending-view' && url.searchParams.get('projection') === 'on-hold-v1') {
      pendingReads++;
      if (phase === 'lag') return route.fulfill({ status: 503, headers: { 'Retry-After': '3' },
        json: { error: { code: 'pending_projection_pending', message: 'On Hold projection is catching up' } } });
      if (phase === 'denied') return route.fulfill({ status: 403, json: { error: { code: 'forbidden', message: 'Access denied' } } });
      return route.fulfill({ json: ready });
    }
    if (url.pathname === '/api/v1/repos/repo/view') return route.fulfill({ json: oldFull });
    if (url.pathname === '/api/v1/repos/repo/revision') return route.fulfill({ json: ready.revision });
    if (url.pathname === '/api/v1/me') return route.fulfill({ status: 401, json: {} });
    if (url.pathname.endsWith('/events')) {
      const hash = url.pathname.split('/').at(-2)!;
      return route.fulfill({ json: {
        hash, envelope: { cir_version: '2', source_provider: 'codex' },
        events: [{ kind: 'message', role: 'user', seq: 0, event_index: 0, deferred: false,
          blocks: [{ type: 'text', text: `Conversation ${hash}` }] }],
        offset: 0, total: 1, inherited: 0, next: -1,
      } });
    }
    unexpected.push(url.pathname + url.search);
    return route.fulfill({ status: 404, json: {} });
  });
  await page.goto('/__pending-projection-harness?onhold');
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return page.locator('.pending-sessions[data-live="true"] .commit-row').count();
  }).toBe(1);
  await page.clock.runFor(1);
  await expect(page.locator('.pending-sessions .commit-row code')).toHaveText('cached-doc');
  await page.locator('.pending-sessions .commit-row').click();
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return page.getByText('Conversation cached-document', { exact: true }).isVisible();
  }).toBe(true);
  phase = 'lag';
  await page.evaluate(() => {
    (window as unknown as { emitRepositoryRevision(revision: unknown): void }).emitRepositoryRevision({ graph: '1', pending: '2' });
  });
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return (await pendingQueryState(page))?.fetchFailureCount;
  }).toBe(1);
  await page.clock.runFor(1);
  await expect(page.locator('.ctx-main').getByRole('status').filter({ hasText: 'Refreshing On Hold' })).toBeVisible();
  await expect(page.locator('.pending-sessions .commit-row')).toHaveCount(1);
  await expect(page.locator('.pending-sessions[data-live="true"]')).toHaveCount(0);
  await expect(page.locator('.live-viewer-status')).not.toContainText('LIVE');
  await expect(page.locator('.ctx-main').getByRole('alert')).toHaveCount(0);
  expect((await pendingQueryState(page))?.error).toBeNull();
  ready = {
    ...pendingStateFixture(pendingFixture('fresh-document', { graph: '1', pending: '2' })),
    read_generation: { ...ready.read_generation!, generation: `sha256:${'b'.repeat(64)}`,
      source_epoch: '22345678-abcd-4321-9876-123456789abc:1', applied_epoch: '22345678-abcd-4321-9876-123456789abc:1' },
  };
  ready.pending[0].activity_at = '2026-10-18T00:00:00Z';
  phase = 'ready';
  await page.clock.runFor(3000);
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return page.getByText('Conversation fresh-document', { exact: true }).isVisible();
  }).toBe(true);
  await expect(page.locator('.pending-sessions .commit-row code')).toHaveText('fresh-docu');
  await expect(page.locator('.pending-sessions[data-live="true"] .commit-row')).toHaveCount(1);
  await expect(page.locator('.ctx-main').getByRole('status').filter({ hasText: 'Refreshing On Hold' })).toHaveCount(0);
  expect((await pendingQueryState(page))?.data?.read_generation).toEqual(ready.read_generation);
  phase = 'denied';
  await page.evaluate(() => {
    (window as unknown as { emitRepositoryRevision(revision: unknown): void }).emitRepositoryRevision({ graph: '1', pending: '3' });
  });
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return (await pendingQueryState(page))?.status;
  }).toBe('error');
  await page.clock.runFor(1);
  await expect(page.locator('.ctx-main').getByRole('alert')).toContainText('Access denied');
  await expect(page.locator('.pending-sessions .commit-row')).toHaveCount(0);
  await expect(page.locator('.viewer')).toHaveCount(0);
  await expect(page.getByText('Conversation fresh-document', { exact: true })).toHaveCount(0);
  expect((await pendingQueryState(page))?.data?.pending[0].target).toBe('fresh-document');
  const rejectedReads = pendingReads;
  await page.clock.runFor(10_000);
  expect(pendingReads).toBe(rejectedReads);
  expect(unexpected).toEqual([]);
  expect(errors).toEqual([]);
});

test('an empty durable cache is not shown as current while its projection is catching up', async ({ page }) => {
  await prepareOnHold(page);
  let lagging = false;
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  const empty: PendingStateView = {
    ...pendingStateFixture(), pending: [], snapshots: [], orphan_sessions: [], hold_counts: {},
    read_generation: {
      generation: `sha256:${'a'.repeat(64)}`, source_epoch: '12345678-abcd-4321-9876-123456789abc:1',
      applied_epoch: '12345678-abcd-4321-9876-123456789abc:1', rules: 1, complete: true,
    },
  };
  const unexpected: string[] = [];
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname === '/api/v1/repos/repo/pending-view' && url.searchParams.get('projection') === 'on-hold-v1') {
      return route.fulfill(lagging
        ? { status: 503, headers: { 'Retry-After': '3' }, json: { error: { code: 'pending_projection_pending', message: 'Catching up' } } }
        : { json: empty });
    }
    if (url.pathname === '/api/v1/repos/repo/view') return route.fulfill({ json: wireFixture(fullFixture()) });
    if (url.pathname === '/api/v1/repos/repo/revision') return route.fulfill({ json: empty.revision });
    if (url.pathname === '/api/v1/me') return route.fulfill({ status: 401, json: {} });
    unexpected.push(url.pathname + url.search);
    return route.fulfill({ status: 404, json: {} });
  });
  await page.goto('/__pending-projection-harness?onhold');
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return page.locator('.ctx-main .empty-box').isVisible();
  }).toBe(true);
  lagging = true;
  await page.evaluate(revision => {
    (window as unknown as { emitRepositoryRevision(revision: unknown): void }).emitRepositoryRevision(revision);
  }, { ...empty.revision, graph: '2' });
  await expect.poll(async () => {
    await page.clock.runFor(1);
    return (await pendingQueryState(page))?.fetchFailureCount;
  }).toBe(1);
  await page.clock.runFor(1);
  await expect(page.locator('.ctx-main').getByRole('status')).toContainText('Refreshing On Hold');
  await expect(page.locator('.ctx-main .empty-box')).toHaveCount(0);
  await expect(page.locator('.pending-sessions .commit-row')).toHaveCount(0);
  await expect(page.locator('.ctx-main').getByRole('alert')).toHaveCount(0);
  expect((await pendingQueryState(page))?.data?.pending).toEqual([]);
  expect(unexpected).toEqual([]);
  expect(errors).toEqual([]);
});
