import { expect, test, type Page } from '@playwright/test';
import type { QueryClient } from '@tanstack/react-query';
import type { GraphReadGeneration, RepositoryRevision, RepositoryView } from '../src/types';
import { fullFixture, pendingFixture, pendingStateFixture, wireFixture } from '../tests/pendingViewFixture';

const incarnation = '12345678-abcd-4321-9876-123456789abc';
const counters = { graph: '1', pending: '1', evidence: '1' };
function generation(applied = 1, source = 2, digest = 'a', epoch = incarnation): GraphReadGeneration {
  return { generation: `sha256:${digest.repeat(64)}`, applied_epoch: `${epoch}:${applied}`, source_epoch: `${epoch}:${source}`,
    rules: 2, complete: true, current: applied === source };
}
function notice(read: GraphReadGeneration): RepositoryRevision {
  return { ...counters, graph_projection: { ...read, state: read.current ? 'ready' : 'catching_up' } };
}
function bundle(read: GraphReadGeneration, target: string, empty = false): RepositoryView {
  const full = fullFixture(pendingFixture(target, counters));
  const snapshotIDs = empty ? [] : [target];
  return { ...full, read_generation: read, pending: [], snapshots: empty ? [] : full.snapshots.map(snapshot => ({ ...snapshot, message: target })),
    refs: empty ? [] : [{ repo_id: 'repo', kind: 'branch', name: 'main', target }],
    graph: { ...full.graph, read_generation: read, snapshot_ids: snapshotIDs, graph_ids: snapshotIDs, committed_ids: snapshotIDs,
      shared_ids: snapshotIDs, pushed_ids: snapshotIDs, uncommitted_ids: [], orphan_sessions: [], hold_counts: {},
      branch_snapshots: empty ? {} : { main: snapshotIDs } },
  };
}
function historicalBundle(read: GraphReadGeneration): RepositoryView {
  const full = bundle(read, 'historical-snapshot');
  const event = { id: 'position', repo_id: 'repo', branch: 'main', branch_id: 'main', kind: 'position' as const,
    target: 'historical-snapshot', created_at: '2026-10-19T00:00:00Z' };
  return { ...full, history: [event], graph: { ...full.graph, positions: [{ event_id: event.id, branch: event.branch,
    branch_id: event.branch_id, snapshot: event.target, archived: false, created_at: event.created_at }] } };
}
function encoded(view: RepositoryView) {
  const wire = wireFixture(view);
  return { ...wire, graph: { ...wire.graph, branch_snapshots: Object.fromEntries(Object.entries(view.graph.branch_snapshots)
    .map(([branch, snapshots]) => [branch, snapshots.map(snapshot => wire.graph.dictionary.indexOf(snapshot))])) } };
}
async function emit(page: Page, revision: RepositoryRevision) {
  await page.evaluate(value => {
    (window as unknown as { emitGraphNotice(revision: unknown): void }).emitGraphNotice(value);
  }, revision);
  await page.clock.runFor(10);
}
async function prepare(page: Page, empty = false) {
  await page.clock.install({ time: new Date('2026-10-19T00:00:00Z') });
  await page.clock.pauseAt(new Date('2026-10-19T00:00:01Z'));
  await page.addInitScript(() => {
    localStorage.setItem('cxt.locale', 'en');
    Math.random = () => 0;
    const streams = new Set<EventTarget>();
    class Source extends EventTarget {
      constructor() { super(); streams.add(this); }
      close() { streams.delete(this); }
    }
    Object.assign(window, { EventSource: Source, emitGraphNotice: (revision: unknown) => {
      for (const stream of streams) stream.dispatchEvent(new MessageEvent('revision', { data: JSON.stringify(revision) }));
    } });
  });
  await page.route('**/__graph-generation-harness*', route => route.fulfill({ contentType: 'text/html', body: `<!doctype html><html><body><div id="root"></div><script type="module">
import RefreshRuntime from '/@react-refresh';
RefreshRuntime.injectIntoGlobalHook(window);
window.$RefreshReg$ = () => {};
window.$RefreshSig$ = () => type => type;
window.__vite_plugin_react_preamble_installed__ = true;
await import('/e2e/graph-generation-harness.ts');
</script></body></html>` }));
  let releaseNotice!: () => void;
  const delayed = new Promise<void>(resolve => { releaseNotice = resolve; });
  const state = { full: bundle(generation(), 'old-graph', empty), revision: notice(generation()), fullReads: 0, pendingReads: 0,
    denied: false, pendingFailure: false, pending: pendingStateFixture(pendingFixture('independent-pending', counters)),
    historical: null as RepositoryView | null, historicalReads: 0, historicalPending: 0 };
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/revision')) {
      await delayed;
      return route.fulfill(state.denied ? { status: 403, json: { error: { code: 'forbidden', message: 'Access denied' } } } : { json: state.revision });
    }
    if (url.pathname === '/api/v1/repos/repo/view') {
      state.fullReads++;
      return route.fulfill({ json: encoded(state.full) });
    }
    if (url.pathname === '/api/v1/repos/repo/graph-state') {
      state.historicalReads++;
      if (state.historicalPending-- > 0) return route.fulfill({ status: 503, headers: { 'Retry-After': '1' },
        json: { error: { code: 'graph_projection_pending', message: 'Graph preparing' } } });
      return route.fulfill({ json: { ...encoded(state.historical ?? state.full).graph, position_event: url.searchParams.get('position') } });
    }
    if (url.pathname.endsWith('/pending-view') && url.searchParams.get('projection') === 'on-hold-v1') {
      state.pendingReads++;
      return route.fulfill(state.pendingFailure
        ? { status: 503, json: { error: { code: 'pending_unavailable', message: 'Pending read unavailable' } } }
        : { json: state.pending });
    }
    if (url.pathname === '/api/v1/me') return route.fulfill({ status: 401, json: {} });
    if (url.pathname.endsWith('/events')) return route.fulfill({ json: {
      hash: url.pathname.split('/').at(-2), envelope: { cir_version: '2', source_provider: 'codex' },
      events: [{ kind: 'message', role: 'user', seq: 0, event_index: 0, deferred: false, blocks: [{ type: 'text', text: 'Independent conversation' }] }],
      offset: 0, total: 1, inherited: 0, next: -1,
    } });
    return route.fulfill({ status: 404, json: {} });
  });
  return { state, errors, releaseNotice };
}

test('historical graph cache follows complete generations at identical counters and retries preparation', async ({ page }) => {
  const fixture = await prepare(page);
  fixture.state.full = historicalBundle(generation());
  await page.goto('/__graph-generation-harness?context');
  await expect.poll(async () => { await page.clock.runFor(1); return page.locator('.graph-row').count(); }).toBe(1);
  fixture.releaseNotice();
  await page.locator('.graph-history-scope select').selectOption('position');
  await expect.poll(async () => { await page.clock.runFor(1); return fixture.state.historicalReads; }).toBe(1);
  await page.clock.runFor(1);
  await expect(page.locator('.graph-row')).toHaveCount(1);
  const next = generation(2, 2, 'b');
  fixture.state.full = historicalBundle(next);
  fixture.state.revision = notice(next);
  fixture.state.historicalPending = 1;
  await emit(page, fixture.state.revision);
  await expect.poll(async () => { await page.clock.runFor(1); return fixture.state.historicalReads; }).toBe(2);
  await page.clock.runFor(1);
  await expect(page.locator('.graph-row')).toHaveCount(0);
  await page.clock.runFor(1000);
  await expect.poll(async () => { await page.clock.runFor(1); return page.locator('.graph-row').count(); }).toBe(1);
  expect(fixture.state.historicalReads).toBe(3);
  expect(fixture.state.fullReads).toBe(2);
  for (let source = 3; source <= 8; source++) await emit(page, notice(generation(2, source, 'b')));
  expect(fixture.state.historicalReads).toBe(3);
  expect(fixture.state.fullReads).toBe(2);
  const cached = await page.evaluate(() => (window as unknown as { graphQueryClient: QueryClient }).graphQueryClient
    .getQueriesData<{ read_generation: GraphReadGeneration }>({ queryKey: ['graph-position', 'repo', 'position'] })
    .flatMap(([, graph]) => graph ? [graph.read_generation.generation] : []));
  expect(cached.sort()).toEqual([generation().generation, next.generation]);
  expect(fixture.errors).toEqual([]);
});

test('a mismatched historical generation stays hidden and refreshes the full bundle only once', async ({ page }) => {
  const fixture = await prepare(page);
  fixture.state.full = historicalBundle(generation());
  fixture.state.historical = historicalBundle(generation(2, 2, 'b'));
  await page.goto('/__graph-generation-harness?context');
  await expect.poll(async () => { await page.clock.runFor(1); return page.locator('.graph-row').count(); }).toBe(1);
  fixture.releaseNotice();
  await page.locator('.graph-history-scope select').selectOption('position');
  const mismatch = page.getByRole('alert').filter({ hasText: 'Historical graph generation changed' });
  await expect.poll(async () => { await page.clock.runFor(1); return mismatch.count(); }).toBe(1);
  await expect.poll(() => fixture.state.fullReads).toBe(2);
  await expect(page.locator('.graph-row')).toHaveCount(0);
  await expect(page.locator('.graph-viewport .repository-empty')).toHaveCount(0);
  for (let attempt = 0; attempt < 3; attempt++) {
    await mismatch.getByRole('button').click();
    await expect.poll(async () => { await page.clock.runFor(1); return fixture.state.historicalReads; }).toBe(attempt + 2);
  }
  expect(fixture.state.fullReads).toBe(2);
  await expect(page.locator('.graph-row')).toHaveCount(0);
  fixture.state.full = fixture.state.historical;
  fixture.state.revision = notice(fixture.state.full.read_generation!);
  await emit(page, fixture.state.revision);
  await expect.poll(async () => { await page.clock.runFor(1); return page.locator('.graph-row').count(); }).toBe(1);
  expect(fixture.state.fullReads).toBe(3);
  expect(fixture.state.historicalReads).toBe(5);
  expect(fixture.errors).toEqual([]);
});

test('On Hold retains same-incarnation lag but fences pending rows and the viewer on a known reset', async ({ page }) => {
  const fixture = await prepare(page);
  const pendingGeneration = { generation: generation().generation, source_epoch: `${incarnation}:1`, applied_epoch: `${incarnation}:1`, rules: 1 as const, complete: true as const };
  fixture.state.pending.read_generation = pendingGeneration;
  fixture.state.pending.pending[0].activity_at = '2026-10-19T00:00:00Z';
  fixture.state.revision.pending_projection = { ...pendingGeneration, state: 'ready' };
  await page.goto('/__graph-generation-harness?onhold');
  await expect.poll(async () => { await page.clock.runFor(1); return page.locator('.pending-sessions .commit-row').count(); }).toBe(1);
  fixture.releaseNotice();
  await page.locator('.pending-sessions .commit-row').click();
  await expect.poll(async () => { await page.clock.runFor(1); return page.locator('.viewer').count(); }).toBe(1);
  fixture.state.revision.pending_projection = { state: 'catching_up', source_epoch: `${incarnation}:2`, rules: 1 };
  await emit(page, fixture.state.revision);
  await expect(page.locator('.pending-sessions .commit-row')).toHaveCount(1);
  await expect(page.locator('.viewer')).toHaveCount(1);
  await expect(page.locator('.pending-sessions[data-live="true"]')).toHaveCount(0);
  const reset = { ...pendingGeneration, generation: `sha256:${'b'.repeat(64)}`,
    source_epoch: '22345678-abcd-4321-9876-123456789abc:1', applied_epoch: '22345678-abcd-4321-9876-123456789abc:1' };
  fixture.state.pendingFailure = true;
  fixture.state.revision.pending_projection = { ...reset, state: 'ready' };
  await emit(page, fixture.state.revision);
  await expect.poll(async () => { await page.clock.runFor(1); return page.locator('.pending-sessions .commit-row').count(); }).toBe(0);
  await expect(page.locator('.viewer')).toHaveCount(0);
  await expect(page.locator('.ctx-main .empty-box')).toHaveCount(0);
  await expect(page.locator('.graph-row')).toHaveCount(1);
  const failure = page.locator('.ctx-main').getByRole('alert').filter({ hasText: 'Pending read unavailable' });
  await expect.poll(async () => { await page.clock.runFor(1); return failure.count(); }).toBe(1);
  fixture.state.pendingFailure = false;
  fixture.state.pending = { ...pendingStateFixture(pendingFixture('fresh-pending', counters)), read_generation: reset };
  await failure.getByRole('button').click();
  await expect.poll(async () => { await page.clock.runFor(1); return page.locator('.pending-sessions .commit-row').count(); }).toBe(1);
  await expect(page.locator('.pending-sessions .commit-row code')).toHaveText('fresh-pend');
  expect(fixture.state.fullReads).toBe(1);
  expect(fixture.errors).toEqual([]);
});

for (const screen of ['context', 'onhold']) {
  test(`${screen}: lag labels, source-only ticks, complete publication, epoch reset and authorization`, async ({ page }) => {
    const fixture = await prepare(page);
    await page.goto(`/__graph-generation-harness?${screen}`);
    await expect.poll(async () => {
      await page.clock.runFor(1);
      return page.locator('[data-graph-read-state="lagging"]').count();
    }).toBe(1);
    await expect(page.getByRole('status').filter({ hasText: 'Showing the last complete graph' })).toBeVisible();
    await expect(page.locator('.graph-row')).toHaveCount(1);
    expect(fixture.state.fullReads).toBe(1);
    if (screen === 'onhold') await expect(page.locator('.pending-sessions .commit-row')).toHaveCount(1);
    fixture.releaseNotice();
    await expect.poll(() => page.evaluate(() => (window as unknown as { graphQueryClient: QueryClient }).graphQueryClient
      .getQueryData<RepositoryRevision>(['repository-notice', 'repo'])?.graph_projection?.source_epoch)).toBe(generation().source_epoch);
    for (let sequence = 3; sequence <= 12; sequence++) {
      fixture.state.revision = notice(generation(1, sequence));
      await emit(page, fixture.state.revision);
    }
    expect(fixture.state.fullReads).toBe(1);
    await expect(page.locator('.graph-row')).toHaveAttribute('data-graph-snapshot', 'old-graph');
    const next = generation(12, 12, 'b');
    fixture.state.full = bundle(next, 'new-graph');
    fixture.state.revision = notice(next);
    await emit(page, fixture.state.revision);
    await expect.poll(async () => {
      await page.clock.runFor(1);
      return page.locator('.graph-row').getAttribute('data-graph-snapshot');
    }).toBe('new-graph');
    expect(fixture.state.fullReads).toBe(2);
    await expect(page.locator('[data-graph-read-state]')).toHaveCount(0);
    const reset = generation(0, 0, 'c', '22345678-abcd-4321-9876-123456789abc');
    fixture.state.revision = { ...counters, graph_projection: {
      state: 'catching_up', source_epoch: reset.source_epoch, rules: 2, complete: false, current: false,
    } };
    await emit(page, fixture.state.revision);
    await expect(page.locator('[data-graph-read-state="pending"]')).toBeVisible();
    await expect(page.locator('.graph-row')).toHaveCount(0);
    await expect(page.locator('.ctx-side .graph-viewport .repository-empty')).toHaveCount(0);
    await expect(page.locator('.ctx-main > .commits .repository-empty')).toHaveCount(0);
    expect(fixture.state.fullReads).toBe(2);
    if (screen === 'onhold') {
      await expect(page.locator('.pending-sessions .commit-row')).toHaveCount(1);
      await page.locator('.pending-sessions .commit-row').click();
      await expect.poll(async () => {
        await page.clock.runFor(1);
        return page.getByText('Independent conversation', { exact: true }).isVisible();
      }).toBe(true);
    } else await expect(page.locator('.ctx-main .commit-row')).toHaveCount(0);
    fixture.state.full = bundle(reset, 'reset-graph');
    fixture.state.revision = notice(reset);
    await emit(page, fixture.state.revision);
    await expect.poll(async () => {
      await page.clock.runFor(1);
      return page.locator('.graph-row').getAttribute('data-graph-snapshot');
    }).toBe('reset-graph');
    expect(fixture.state.fullReads).toBe(3);
    fixture.state.denied = true;
    await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
    await expect.poll(async () => {
      await page.clock.runFor(1);
      return page.evaluate(() => (window as unknown as { graphQueryClient: QueryClient }).graphQueryClient
        .getQueryState(['repository-notice', 'repo'])?.error?.message);
    }).toContain('Access denied');
    await page.clock.runFor(1);
    await expect(page.getByRole('complementary').getByRole('alert').filter({ hasText: 'Access denied' })).toBeVisible();
    await expect(page.locator('.graph-row')).toHaveCount(0);
    await expect(page.locator('.ctx-side .graph-viewport .repository-empty')).toHaveCount(0);
    await expect(page.locator('.pending-sessions .commit-row')).toHaveCount(0);
    await expect(page.locator('.viewer')).toHaveCount(0);
    expect(fixture.state.fullReads).toBe(3);
    expect(fixture.errors).toEqual([]);
  });

  test(`${screen}: an empty lagged bundle displays its banner, never a false current-empty state`, async ({ page }) => {
    const fixture = await prepare(page, true);
    await page.goto(`/__graph-generation-harness?${screen}`);
    await expect.poll(async () => {
      await page.clock.runFor(1);
      return page.locator('[data-graph-read-state="lagging"]').count();
    }).toBe(1);
    await expect(page.locator('.empty-box, .repository-empty')).toHaveCount(0);
    expect(fixture.state.fullReads).toBe(1);
    fixture.releaseNotice();
    const next = generation(2, 2, 'b');
    fixture.state.full = bundle(next, 'empty', true);
    fixture.state.revision = notice(next);
    await emit(page, fixture.state.revision);
    await expect.poll(async () => {
      await page.clock.runFor(1);
      return page.locator('[data-graph-read-state]').count();
    }).toBe(0);
    await expect(page.locator(screen === 'context' ? '.empty-box' : '.ctx-side .graph-viewport .repository-empty')).toHaveCount(1);
    expect(fixture.state.fullReads).toBe(2);
    expect(fixture.errors).toEqual([]);
  });
}

for (const pendingProjection of ['absent', 'disabled'] as const) {
  test(`On Hold drops an old-incarnation full fallback after reset with ${pendingProjection} pending notices and a failed pending read`, async ({ page }) => {
    const fixture = await prepare(page);
    const pending = pendingFixture('old-fallback', counters);
    const read = generation();
    fixture.state.full = { ...fullFixture(pending), read_generation: read, graph: { ...pending.graph, read_generation: read } };
    fixture.state.pendingFailure = true;
    await page.goto('/__graph-generation-harness?onhold');
    await expect.poll(async () => {
      await page.clock.runFor(1);
      return page.locator('.pending-sessions .commit-row').count();
    }).toBe(1);
    await expect(page.locator('.pending-sessions .commit-row code')).toHaveText('old-fallba');
    const source = '22345678-abcd-4321-9876-123456789abc:0';
    fixture.state.revision = { ...counters, graph_projection: {
      state: 'catching_up', source_epoch: source, rules: 2, complete: false, current: false,
    }, ...(pendingProjection === 'disabled' ? { pending_projection: { state: 'disabled' as const, source_epoch: source, rules: 1 as const } } : {}) };
    fixture.releaseNotice();
    await emit(page, fixture.state.revision);
    await expect.poll(async () => {
      await page.clock.runFor(1);
      return page.locator('.pending-sessions .commit-row').count();
    }).toBe(0);
    await expect(page.locator('[data-graph-read-state="pending"]')).toBeVisible();
    await expect(page.locator('.ctx-main').getByRole('alert')).toContainText('Pending read unavailable');
    await expect(page.locator('.ctx-main .empty-box')).toHaveCount(0);
    await expect(page.locator('.graph-row')).toHaveCount(0);
    fixture.state.pendingFailure = false;
    await page.locator('.ctx-main').getByRole('alert').getByRole('button').click();
    await expect.poll(async () => {
      await page.clock.runFor(1);
      return page.locator('.pending-sessions .commit-row').count();
    }).toBe(1);
    await expect(page.locator('.pending-sessions .commit-row code')).toHaveText('independen');
    await expect(page.locator('[data-graph-read-state="pending"]')).toBeVisible();
    await expect(page.locator('.graph-row')).toHaveCount(0);
    expect(fixture.state.fullReads).toBe(1);
    expect(fixture.errors).toEqual([]);
  });
}
