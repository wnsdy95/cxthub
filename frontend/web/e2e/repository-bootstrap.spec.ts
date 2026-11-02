import { expect, test, type Page, type TestInfo } from '@playwright/test';
import { build } from 'esbuild';
import { serverGraphWireFixture, serverPendingStateFixture } from '../tests/serverGraphFixture';
import type { RepositoryView } from '../src/types';
import { en } from '../src/i18n/locales/en';
import { ko } from '../src/i18n/locales/ko';

type Read = { path: string; startMs: number; headersMs?: number; bodyMs?: number; bytes?: number; abortedMs?: number };
type Metrics = { reads: Read[]; firstGraphVisibleMs?: number; firstDocumentVisibleMs?: number };
declare global { interface Window { bootstrapMetrics: Metrics } }

let script: string, styles: string;
const wires = new Map<string, ReturnType<typeof serverGraphWireFixture>>();
const revision = { graph: '1', pending: '1', evidence: '0' };

test.beforeAll(async () => {
  // Compile the actual app/hooks once, outside browser timing. No test server,
  // credentials, live API or duplicated graph encoder is needed by this spec.
  const bundle = await build({
    stdin: { resolveDir: process.cwd(), loader: 'tsx', contents: `
      import {useState} from 'react';
      import {createRoot} from 'react-dom/client';
      import {QueryClient,QueryClientProvider,useQuery} from '@tanstack/react-query';
      import {App} from './src/App';
      import {I18nProvider} from './src/i18n';
      import {useRepoView} from './src/hooks';
      import {api} from './src/api';
      import './src/styles.css';
      const qc=new QueryClient({defaultOptions:{queries:{staleTime:30000,refetchOnWindowFocus:false}}});
      function View({repo}) {
        const q=useRepoView(repo);
        return <output data-testid="view" data-read-state={q.graphReadStatus.state} data-snapshot-count={q.snapshots.length}>
          {repo}:{q.graphError?'error':q.graphLoading?'loading':'ready'}</output>;
      }
      function Retained() {
        const q=useQuery({queryKey:['repo-view','a'],queryFn:({signal})=>api.repositoryView('a',signal),retry:false});
        return <output data-testid="retained">{q.isSuccess?'a:ready':'a:loading'}</output>;
      }
      function Observers() {
        const [repo,setRepo]=useState('a');
        const [retained,setRetained]=useState(new URLSearchParams(location.search).has('retain'));
        return <><button onClick={()=>setRepo('b')}>Navigate to B</button>
          <button onClick={()=>setRetained(false)}>Remove retained observer</button>
          <View repo={repo}/>{retained&&<Retained/>}</>;
      }
      createRoot(document.getElementById('root')).render(<I18nProvider>{location.pathname==='/observers'
        ?<QueryClientProvider client={qc}><Observers/></QueryClientProvider>:<App/>}</I18nProvider>);
    ` },
    bundle: true, write: false, outdir: 'bootstrap-fixture', platform: 'browser', format: 'esm', jsx: 'automatic',
    define: { 'import.meta.env': '{}', 'process.env.NODE_ENV': '"production"' },
    loader: { '.png': 'dataurl', '.svg': 'dataurl', '.webp': 'dataurl' },
  });
  script = bundle.outputFiles.find(f => f.path.endsWith('.js'))!.text;
  styles = bundle.outputFiles.find(f => f.path.endsWith('.css'))!.text;
  for (const repo of ['a', 'b', 'empty']) {
    const snapshots: RepositoryView['snapshots'] = repo === 'empty' ? [] : Array.from({ length: 120 }, (_, i) => ({
      id: `${repo}-${i}`, repo_id: repo, doc_hash: `${repo}-${i}`, branch: 'main', parents: i ? [`${repo}-${i-1}`] : [],
      provider: 'codex', fidelity: 'full', message: `${repo} snapshot ${i}`, created_at: new Date(1750000000000+i*1000).toISOString(),
    })).reverse();
    const wire = serverGraphWireFixture({ revision, snapshots, refs: snapshots.length
      ? [{ repo_id: repo, kind: 'branch', name: 'main', target: snapshots[0].id }] : [] }, '', 2);
    expect(wire.graph.encoding).toBe('indexed-v2');
    wires.set(repo, wire);
  }
});

function gate() {
  let release!: () => void;
  const promise = new Promise<void>(resolve => { release = resolve; });
  return { promise, release };
}

type ReadResponse = { body: unknown; status?: number; headers?: Record<string, string> };
type ReadOverride = () => ReadResponse | 'abort' | undefined;
type Options = {
  public?: boolean; denied?: boolean; revisionStatus?: number | (() => number); aRevision?: () => string;
  holdA?: { at: number; promise: Promise<void> };
  locale?: 'en' | 'ko';
  aView?: ReadOverride;
  aDoc?: ReadOverride;
  aPending?: ReadOverride;
  aPosition?: () => ReadResponse;
};
async function fixture(page: Page, options: Options = {}) {
  const unexpected: string[] = [], errors: string[] = [];
  const requests: string[] = [];
  let aReads = 0;
  page.on('pageerror', e => errors.push(e.message));
  await page.addInitScript(locale => {
    localStorage.setItem('cxt.locale', locale);
    const metrics: Metrics = { reads: [] };
    window.bootstrapMetrics = metrics;
    const fetch = window.fetch.bind(window);
    window.fetch = async (input, init) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, location.href);
      const read: Read = { path: url.pathname, startMs: performance.now() };
      metrics.reads.push(read);
      init?.signal?.addEventListener('abort', () => { read.abortedMs = performance.now(); }, { once: true });
      const response = await fetch(input, init);
      read.headersMs = performance.now();
      const text = response.text.bind(response);
      response.text = async () => {
        const body = await text();
        read.bodyMs = performance.now(); read.bytes = new TextEncoder().encode(body).byteLength;
        return body;
      };
      return response;
    };
    // Visible DOM availability, not a browser paint or cloud latency claim.
    const observe = () => requestAnimationFrame(() => {
      if (metrics.firstGraphVisibleMs === undefined && document.querySelector('.graph-row')?.getClientRects().length) metrics.firstGraphVisibleMs = performance.now();
      if (metrics.firstDocumentVisibleMs === undefined && document.querySelector('.doc-events')?.textContent?.includes('BOOTSTRAP_BODY_')) metrics.firstDocumentVisibleMs = performance.now();
    });
    new MutationObserver(observe).observe(document, { childList: true, subtree: true });
  }, options.locale ?? 'en');
  await page.route('**/*', async route => {
    const req = route.request(), url = new URL(req.url()), p = url.pathname;
    if (url.origin !== 'http://bootstrap.test' || req.method() !== 'GET') {
      unexpected.push(`${req.method()} ${req.url()}`); await route.abort(); return;
    }
    if (p === '/fixture.js') { await route.fulfill({ contentType: 'text/javascript', body: script }); return; }
    if (p === '/fixture.css') { await route.fulfill({ contentType: 'text/css', body: styles }); return; }
    if (req.isNavigationRequest()) {
      await route.fulfill({ contentType: 'text/html', body: '<!doctype html><html><head><link rel="stylesheet" href="/fixture.css"></head><body><div id="root"></div><script type="module" src="/fixture.js"></script></body></html>' }); return;
    }
    requests.push(p);
    const send = (body: unknown, status = 200, headers?: Record<string, string>) => route.fulfill({ status, headers, contentType: 'application/json', body: JSON.stringify(body) });
    if (p === '/api/v1/me') { await send(options.public ? { error: { message: 'Unauthorized' } } : { id: 'reader', username: 'reader', name: 'Reader', locale: options.locale ?? 'en' }, options.public ? 401 : 200); return; }
    const repositories = ['a', 'b', 'empty'].map(id => ({ id: `repository-${id}`, name: id, slug: id, owner_username: 'alice', owner_id: 'owner', effective_role: 'viewer', visibility: 'public', public_role: 'viewer' }));
    if (p === '/api/v1/repositories') { await send(repositories); return; }
    if (p.startsWith('/api/v1/public/repositories/alice/')) { await send(options.denied ? { error: { message: 'Unavailable' } } : repositories.find(r => r.slug === p.split('/').at(-1)), options.denied ? 404 : 200); return; }
    if (p === '/api/v1/repos') {
      const id = url.searchParams.get('repository')?.replace('repository-', '');
      await send([{ id, default_branch: 'main', remote_url: `https://example.test/${id}.git` }]); return;
    }
    if (['/api/v1/organizations', '/api/v1/me/invitations'].includes(p) || /^\/api\/v1\/repositories\/[^/]+\/(members|notifications)$/.test(p) || p.endsWith('/prs/promotions')) { await send([]); return; }
    const match = p.match(/^\/api\/v1\/repos\/(a|b|empty)\/(.*)$/);
    if (match) {
      const [, id, endpoint] = match;
      if (endpoint === 'changes') { await route.fulfill({ status: 204, body: '' }); return; } // Stop reconnects; revision fallback remains real.
      if (endpoint === 'revision') {
        const status = (typeof options.revisionStatus === 'function' ? options.revisionStatus() : options.revisionStatus) ?? 200;
        await send(status === 200 ? { ...revision, graph: id === 'a' ? options.aRevision?.() ?? '1' : '1' } : { error: { message: 'Denied' } }, status); return;
      }
      if (endpoint === 'view') {
        expect(url.searchParams.get('graph_encoding')).toBe('indexed-v2');
        if (id === 'a' && ++aReads === options.holdA?.at) await options.holdA.promise;
        const response = id === 'a' ? options.aView?.() : undefined;
        if (response === 'abort') { await route.abort('failed'); return; }
        if (response) { await send(response.body, response.status, response.headers); return; }
        await send(wires.get(id)); return;
      }
      if (endpoint === 'graph-state' && id === 'a' && options.aPosition) {
        expect(url.searchParams.get('graph_encoding')).toBe('indexed-v2');
        const response = options.aPosition();
        await send(response.body, response.status, response.headers); return;
      }
      if (endpoint === 'pending-view') {
        expect(url.searchParams.get('projection')).toBe('on-hold-v1');
        const response = id === 'a' ? options.aPending?.() : undefined;
        if (response === 'abort') { await route.abort('failed'); return; }
        if (response) { await send(response.body, response.status, response.headers); return; }
        const {graph: _graph, ...view} = wires.get(id)!;
        await send(serverPendingStateFixture({...view, revision: {...revision, graph: id === 'a' ? options.aRevision?.() ?? '1' : '1'}})); return;
      }
      if (endpoint.startsWith('docs/') && endpoint.endsWith('/events')) {
        const response = id === 'a' ? options.aDoc?.() : undefined;
        if (response === 'abort') { await route.abort('failed'); return; }
        if (response) { await send(response.body, response.status, response.headers); return; }
        await send({ hash: decodeURIComponent(endpoint.split('/')[1]), envelope: { cir_version: '2', source_provider: 'codex', captured_at: '2026-09-18T00:00:00Z', git_branch: 'main' },
          events: [{ kind: 'message', role: 'user', seq: 1, blocks: [{ type: 'text', text: `BOOTSTRAP_BODY_${id}` }] }], total: 1, offset: 0, inherited: 0, next: -1 }); return;
      }
    }
    unexpected.push(p); await send({ error: { message: `Unhandled ${p}` } }, 501);
  });
  return { requests, check: () => { expect(unexpected).toEqual([]); expect(errors).toEqual([]); } };
}

async function reads(page: Page, path: string) { return page.evaluate(p => window.bootstrapMetrics.reads.filter(r => r.path === p), path); }
async function settle(page: Page) { await page.evaluate(() => new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve())))); }
async function report(page: Page, info: TestInfo) {
  await info.attach('bootstrap-measurements', { contentType: 'application/json', body: JSON.stringify(await page.evaluate(() => window.bootstrapMetrics), null, 2) });
}

for (const publicView of [false, true]) test(`indexed-v2 ${publicView ? 'public' : 'authenticated'} bootstrap waterfall`, async ({ page }, info) => {
  const f = await fixture(page, { public: publicView });
  await page.goto('http://bootstrap.test/alice/a');
  await expect(page.locator('.graph-row').first()).toBeVisible();
  await expect(page.getByText('BOOTSTRAP_BODY_a', { exact: true })).toBeVisible();
  await expect.poll(() => page.evaluate(() => window.bootstrapMetrics.firstDocumentVisibleMs)).toBeDefined();
  const metrics = await page.evaluate(() => window.bootstrapMetrics);
  const chain = ['/api/v1/me', publicView ? '/api/v1/public/repositories/alice/a' : '/api/v1/repositories', '/api/v1/repos', '/api/v1/repos/a/view'];
  for (let i = 0; i < chain.length; i++) {
    const current = metrics.reads.filter(r => r.path === chain[i]);
    expect(current).toHaveLength(1);
    if (i) expect(current[0].startMs).toBeGreaterThanOrEqual(metrics.reads.find(r => r.path === chain[i-1])!.headersMs!);
  }
  expect(metrics.reads.find(r => r.path.endsWith('/view'))?.bytes).toBeGreaterThan(0);
  expect(metrics.firstGraphVisibleMs).toBeDefined();
  f.check(); await report(page, info);
});

test('empty and denied routes do not invent a graph or fetch documents', async ({ page }) => {
  const f = await fixture(page, { public: true });
  await page.goto('http://bootstrap.test/alice/empty');
  await expect(page.locator('.empty-box')).toContainText('No context');
  expect(f.requests.some(p => p.includes('/docs/'))).toBe(false); f.check();
  await page.unrouteAll({ behavior: 'wait' });
  const denied = await fixture(page, { public: true, denied: true });
  await page.goto('http://bootstrap.test/alice/a');
  await expect(page.locator('input[type="email"]')).toBeVisible();
  expect(denied.requests.some(p => p.startsWith('/api/v1/repos/'))).toBe(false); denied.check();
});

test('delayed A navigation aborts the last observer without consuming its body', async ({ page }, info) => {
  const delayed = gate(), f = await fixture(page, { holdA: { at: 1, promise: delayed.promise } });
  try {
    await page.goto('http://bootstrap.test/alice/a');
    await expect.poll(() => f.requests.filter(p => p === '/api/v1/repos/a/view').length).toBe(1);
    await page.evaluate(() => { history.pushState(null, '', '/alice/b'); dispatchEvent(new PopStateEvent('popstate')); });
    await expect(page.getByText('BOOTSTRAP_BODY_b', { exact: true })).toBeVisible();
    await expect.poll(async () => (await reads(page, '/api/v1/repos/a/view'))[0]?.abortedMs).toBeDefined();
    delayed.release(); await settle(page);
    expect((await reads(page, '/api/v1/repos/a/view'))[0].bodyMs).toBeUndefined();
    expect(f.requests.filter(p => p === '/api/v1/repos/a/view')).toHaveLength(1);
    f.check(); await report(page, info);
  } finally { delayed.release(); }
});

test('retained observer keeps A alive and disposed update listener issues no follow-up', async ({ page }, info) => {
  const delayed = gate(), f = await fixture(page, { holdA: { at: 1, promise: delayed.promise }, aRevision: () => '2' });
  try {
    await page.goto('http://bootstrap.test/observers?retain');
    await expect.poll(() => f.requests.filter(p => p === '/api/v1/repos/a/revision').length).toBe(1);
    await expect.poll(() => f.requests.filter(p => p === '/api/v1/repos/a/view').length).toBe(1);
    await page.getByRole('button', { name: 'Navigate to B' }).click();
    await expect(page.getByTestId('view')).toHaveText('b:ready');
    expect((await reads(page, '/api/v1/repos/a/view'))[0].abortedMs).toBeUndefined();
    delayed.release();
    await expect(page.getByTestId('retained')).toHaveText('a:ready'); await settle(page);
    expect(f.requests.filter(p => p === '/api/v1/repos/a/view')).toHaveLength(1);
    expect((await reads(page, '/api/v1/repos/a/view'))[0].bodyMs).toBeDefined();
    await expect(page.getByTestId('view')).toHaveText('b:ready');
    f.check(); await report(page, info);
  } finally { delayed.release(); }
});

test('revision-triggered full refresh also follows last-observer cancellation', async ({ page }, info) => {
  const delayed = gate(); let graph = '1';
  const f = await fixture(page, { holdA: { at: 2, promise: delayed.promise }, aRevision: () => graph });
  try {
    await page.goto('http://bootstrap.test/observers');
    await expect(page.getByTestId('view')).toHaveText('a:ready');
    graph = '2'; await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
    await expect.poll(() => f.requests.filter(p => p === '/api/v1/repos/a/view').length).toBe(2);
    await page.getByRole('button', { name: 'Navigate to B' }).click();
    await expect(page.getByTestId('view')).toHaveText('b:ready');
    await expect.poll(async () => (await reads(page, '/api/v1/repos/a/view'))[1]?.abortedMs).toBeDefined();
    delayed.release(); await settle(page);
    expect((await reads(page, '/api/v1/repos/a/view'))[1].bodyMs).toBeUndefined();
    expect(f.requests.filter(p => p === '/api/v1/repos/a/view')).toHaveLength(2);
    f.check(); await report(page, info);
  } finally { delayed.release(); }
});

for (const status of [401, 403]) test(`revision ${status} does not trigger fallback full reads`, async ({ page }) => {
  const f = await fixture(page, { revisionStatus: status });
  await page.goto('http://bootstrap.test/observers');
  await expect(page.getByTestId('view')).toHaveText('a:error'); await settle(page);
  await expect(page.getByTestId('view')).toHaveAttribute('data-read-state', 'denied');
  await expect(page.getByTestId('view')).toHaveAttribute('data-snapshot-count', '0');
  expect(f.requests.filter(p => p === '/api/v1/repos/a/view')).toHaveLength(1);
  expect(f.requests.filter(p => p === '/api/v1/repos/a/revision')).toHaveLength(1);
  f.check();
});

const overloaded = { status: 503, body: { error: { code: 'resource_overloaded', message: 'Graph worker is busy' } } };
const refreshWarning = (page: Page) => page.locator('.graph-wrap > [role="status"]').filter({ hasText: en.graph.refreshFailed })
  .or(page.locator('.graph-wrap > [role="status"]').filter({ hasText: ko.graph.refreshFailed }));
const graphFailure = (page: Page) => page.locator('.graph-wrap > [role="alert"]').filter({ hasText: en.graph.loadFailed })
  .or(page.locator('.graph-wrap > [role="alert"]').filter({ hasText: ko.graph.loadFailed }));
const refreshRepository = (page: Page) => page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));

const capacityReads = [
  { name: 'graph', option: 'aView', path: '/api/v1/repos/a/view', route: '/alice/a' },
  { name: 'document page', option: 'aDoc', path: '/api/v1/repos/a/docs/a-119/events', route: '/alice/a' },
  { name: 'pending', option: 'aPending', path: '/api/v1/repos/a/pending-view', route: '/alice/a?tab=onhold' },
] as const;

async function finishCapacityRetries(page: Page, requests: string[], path: string, initial = 1) {
  await expect.poll(() => requests.filter(request => request === path).length).toBe(initial);
  for (const [attempt, backoff] of [1000, 2000, 4000, 8000].entries()) {
    await expect.poll(async () => (await reads(page, path))[initial + attempt - 1]?.headersMs).toBeDefined();
    await settle(page);
    await page.clock.fastForward(backoff + 250);
    await expect.poll(() => requests.filter(request => request === path).length).toBe(initial + attempt + 1);
  }
}

for (const resource of capacityReads) for (const header of resource.name === 'graph' ? [undefined, 'unreadable'] : [undefined]) {
  test(`initial ${resource.name} capacity failure recovers automatically (${header ?? 'absent'} Retry-After)`, async ({ page }) => {
    let attempts = 0;
    const pending = resource.name === 'pending' ? serverPendingStateFixture({ revision, default_branch: 'main',
      snapshots: [{ ...wires.get('a')!.snapshots[0], id: 'a-pending', doc_hash: 'a-pending', parents: [], session_id: 'retry-session', branch: 'retry-capture' }],
      pending: [{ repo_id: 'a', session_id: 'retry-session', branch: 'retry-capture', provider: 'codex', target: 'a-pending', updated_at: '2026-10-15T00:00:00Z' }],
    }) : undefined;
    const fixtureState = await fixture(page, { [resource.option]: () => ++attempts === 1
      ? { ...overloaded, headers: header ? { 'Retry-After': header } : undefined }
      : pending ? { body: pending } : undefined });
    await page.goto(`http://bootstrap.test${resource.route}`);
    if (pending) await expect(page.locator('.pending-sessions .commit-row')).toContainText('retry-capture');
    else await expect(page.getByText('BOOTSTRAP_BODY_a', { exact: true })).toBeVisible();
    await expect(page.locator('[data-graph-id="a-119"]')).toBeVisible();
    await expect(page.getByRole('alert')).toHaveCount(0);
    await expect(refreshWarning(page)).toHaveCount(0);
    expect(fixtureState.requests.filter(path => path === resource.path)).toHaveLength(2);
    fixtureState.check();
  });
}

for (const resource of capacityReads) {
  test(`persistent ${resource.name} capacity failure stops after five requests`, async ({ page }) => {
    await page.clock.install();
    const fixtureState = await fixture(page, { [resource.option]: () => overloaded });
    await page.goto(`http://bootstrap.test${resource.route}`);
    await finishCapacityRetries(page, fixtureState.requests, resource.path);
    await expect(page.getByRole('alert').filter({ hasText: overloaded.body.error.message })).toBeVisible();
    if (resource.name === 'graph') {
      await expect(graphFailure(page)).toBeVisible();
      await expect(page.locator('.graph-row')).toHaveCount(0);
      expect(fixtureState.requests.some(path => path.includes('/docs/'))).toBe(false);
    } else {
      await expect(page.locator('[data-graph-id="a-119"]')).toBeVisible();
      if (resource.name === 'document page') await expect(page.getByText('BOOTSTRAP_BODY_a', { exact: true })).toHaveCount(0);
      else await expect(page.locator('.pending-sessions .commit-row')).toHaveCount(0);
    }
    await page.clock.fastForward(10_000);
    await settle(page);
    expect(fixtureState.requests.filter(path => path === resource.path)).toHaveLength(5);
    const attempts = await reads(page, resource.path);
    for (const [attempt, backoff] of [1000, 2000, 4000, 8000].entries()) {
      expect(attempts[attempt + 1].startMs - attempts[attempt].headersMs!).toBeGreaterThanOrEqual(backoff);
    }
    fixtureState.check();
  });

  test(`navigation cancels a delayed ${resource.name} capacity retry`, async ({ page }) => {
    await page.clock.install();
    const fixtureState = await fixture(page, { [resource.option]: () => ({ ...overloaded, headers: { 'Retry-After': '10' } }) });
    await page.goto(`http://bootstrap.test${resource.route}`);
    await expect.poll(async () => (await reads(page, resource.path))[0]?.headersMs).toBeDefined();
    await settle(page);
    expect(fixtureState.requests.filter(path => path === resource.path)).toHaveLength(1);
    await page.evaluate(route => { history.pushState(null, '', route); dispatchEvent(new PopStateEvent('popstate')); }, resource.route.replace('/alice/a', '/alice/b'));
    await expect(page.locator('[data-graph-id="b-119"]')).toBeVisible();
    if (resource.name !== 'pending') await expect(page.getByText('BOOTSTRAP_BODY_b', { exact: true })).toBeVisible();
    expect(await reads(page, resource.path)).toHaveLength(1);
    await expect.poll(async () => (await reads(page, resource.path))[0]?.abortedMs).toBeDefined();
    await page.clock.fastForward(20_000);
    await settle(page);
    expect(fixtureState.requests.filter(path => path === resource.path)).toHaveLength(1);
    await expect(page.locator('[data-graph-id="b-119"]')).toBeVisible();
    await expect(page.locator('[data-graph-id="a-119"]')).toHaveCount(0);
    await expect(page.getByRole('alert')).toHaveCount(0);
    fixtureState.check();
  });
}

for (const format of ['numeric', 'date'] as const) {
  test(`initial graph capacity retry honors a readable ${format} Retry-After`, async ({ page }) => {
    const now = new Date('2026-10-15T12:00:00Z');
    await page.clock.install({ time: now });
    let attempts = 0;
    const hint = format === 'numeric' ? '5' : new Date(now.getTime() + 5000).toUTCString();
    const fixtureState = await fixture(page, { aView: () => ++attempts === 1 ? { ...overloaded, headers: { 'Retry-After': hint } } : undefined });
    await page.goto('http://bootstrap.test/alice/a');
    await expect.poll(async () => (await reads(page, '/api/v1/repos/a/view'))[0]?.headersMs).toBeDefined();
    await settle(page);
    await page.clock.fastForward(2000);
    await settle(page);
    expect(fixtureState.requests.filter(path => path === '/api/v1/repos/a/view')).toHaveLength(1);
    await expect(page.locator('.graph-row')).toHaveCount(0);
    await expect(graphFailure(page)).toHaveCount(0);
    await page.clock.fastForward(3500);
    await expect(page.getByText('BOOTSTRAP_BODY_a', { exact: true })).toBeVisible();
    expect(fixtureState.requests.filter(path => path === '/api/v1/repos/a/view')).toHaveLength(2);
    fixtureState.check();
  });

  test(`initial graph capacity failure does not retry ${format} Retry-After above ten seconds`, async ({ page }) => {
    const now = new Date('2026-10-15T12:00:00Z');
    await page.clock.install({ time: now });
    const hint = format === 'numeric' ? '11' : new Date(now.getTime() + 20_000).toUTCString();
    const fixtureState = await fixture(page, { revisionStatus: 403, aView: () => ({ ...overloaded, headers: { 'Retry-After': hint } }) });
    await page.goto('http://bootstrap.test/alice/a');
    await expect(graphFailure(page)).toBeVisible();
    await page.clock.fastForward(25_000);
    await settle(page);
    expect(fixtureState.requests.filter(path => path === '/api/v1/repos/a/view')).toHaveLength(1);
    await expect(page.locator('.graph-row')).toHaveCount(0);
    expect(fixtureState.requests.some(path => path.includes('/docs/'))).toBe(false);
    fixtureState.check();
  });
}

for (const failure of ['401', '403', '404', '429', '500', 'other-code', 'missing-code', 'contract-invalid', 'contract-typeerror', 'graph-revision', 'transport']) {
  test(`initial graph ${failure} failure does not automatically retry`, async ({ page }) => {
    await page.clock.install();
    const invalid = structuredClone(wires.get('a')!);
    if (failure === 'contract-invalid') invalid.graph.version = 2 as 1;
    if (failure === 'contract-typeerror') Object.assign(invalid.graph, { branch_contexts: { main: null } });
    if (failure === 'graph-revision') invalid.graph.revision.graph = '99';
    const response = failure === 'transport' ? 'abort'
      : ['contract-invalid', 'contract-typeerror', 'graph-revision'].includes(failure) ? { body: invalid }
      : { status: /^\d+$/.test(failure) ? Number(failure) : 503,
        body: { error: { code: failure === 'other-code' ? 'service_unavailable' : failure === 'missing-code' ? undefined : 'resource_overloaded', message: 'Rejected read' } } };
    const fixtureState = await fixture(page, { revisionStatus: 403, aView: () => response });
    await page.goto('http://bootstrap.test/alice/a');
    await expect(graphFailure(page)).toBeVisible();
    await page.clock.fastForward(20_000);
    await settle(page);
    expect(fixtureState.requests.filter(path => path === '/api/v1/repos/a/view')).toHaveLength(1);
    await expect(page.locator('.graph-row')).toHaveCount(0);
    expect(fixtureState.requests.some(path => path.includes('/docs/'))).toBe(false);
    fixtureState.check();
  });
}

function nextGraph() {
  const previous = wires.get('a')!;
  const snapshot = { ...previous.snapshots[0], id: 'a-120', doc_hash: 'a-120', parents: ['a-119'], message: 'Recovered generation', created_at: '2026-10-15T00:00:00Z' };
  return serverGraphWireFixture({ ...previous, graph: undefined, revision: { graph: '2', pending: '2', evidence: '1' },
    snapshots: [snapshot, ...previous.snapshots], refs: [{ repo_id: 'a', kind: 'branch', name: 'main', target: snapshot.id }] }, '', 2);
}

for (const view of ['context', 'onhold']) {
  test(`transient ${view} graph refresh capacity failure recovers without a manual retry`, async ({ page }) => {
    const recovered = nextGraph();
    let graph = '1', attempts = 0;
    const fixtureState = await fixture(page, { aRevision: () => graph,
      aView: () => ++attempts === 2 ? overloaded : { body: graph === '1' ? wires.get('a') : recovered } });
    await page.goto(`http://bootstrap.test/alice/a${view === 'onhold' ? '?tab=onhold' : ''}`);
    await expect(page.locator('[data-graph-id="a-119"]')).toBeVisible();
    await settle(page);
    const previous = await page.locator('.graph').innerHTML();
    graph = '2';
    await refreshRepository(page);
    await expect.poll(async () => (await reads(page, '/api/v1/repos/a/view'))[1]?.headersMs).toBeDefined();
    expect(await page.locator('.graph').innerHTML()).toBe(previous);
    await expect(refreshWarning(page)).toHaveCount(0);
    await expect(graphFailure(page)).toHaveCount(0);
    await expect(page.locator('[data-graph-id="a-120"]')).toBeVisible();
    expect(fixtureState.requests.filter(path => path === '/api/v1/repos/a/view')).toHaveLength(3);
    await expect(refreshWarning(page)).toHaveCount(0);
    await expect(graphFailure(page)).toHaveCount(0);
    fixtureState.check();
  });
}

for (const view of ['context', 'onhold']) for (const locale of ['en', 'ko'] as const) {
  test(`graph refresh keeps a complete ${view} graph after 200 → 503 and recovers (${locale})`, async ({ page }) => {
    await page.clock.install();
    const delayed = gate(), recovered = nextGraph();
    let graph = '1', failing = false;
    const fixtureState = await fixture(page, { locale, aRevision: () => graph, holdA: { at: 7, promise: delayed.promise },
      aView: () => failing ? overloaded : { body: graph === '1' ? wires.get('a') : recovered } });
    const statuses: number[] = [];
    page.on('response', response => { if (new URL(response.url()).pathname === '/api/v1/repos/a/view') statuses.push(response.status()); });
    try {
      await page.goto(`http://bootstrap.test/alice/a${view === 'onhold' ? '?tab=onhold' : ''}`);
      await expect(page.locator('[data-graph-id="a-119"]')).toBeVisible();
      await settle(page);
      const previous = await page.locator('.graph').innerHTML();
      graph = '2'; failing = true;
      await refreshRepository(page);
      await finishCapacityRetries(page, fixtureState.requests, '/api/v1/repos/a/view', 2);
      await expect(refreshWarning(page)).toContainText((locale === 'ko' ? ko : en).graph.refreshFailed);
      expect(statuses).toEqual([200, 503, 503, 503, 503, 503]);
      expect(await page.locator('.graph').innerHTML()).toBe(previous);
      await expect(page.locator('[data-graph-id="a-120"]')).toHaveCount(0);
      await expect(graphFailure(page)).toHaveCount(0);
      failing = false;
      await refreshWarning(page).getByRole('button', { name: (locale === 'ko' ? ko : en).context.retryRead, exact: true }).click();
      await expect.poll(() => fixtureState.requests.filter(path => path === '/api/v1/repos/a/view').length).toBe(7);
      await expect(refreshWarning(page)).toBeVisible();
      expect(await page.locator('.graph').innerHTML()).toBe(previous);
      delayed.release();
      await expect(page.locator('[data-graph-id="a-120"]')).toBeVisible();
      await expect(refreshWarning(page)).toHaveCount(0);
      expect(statuses).toEqual([200, 503, 503, 503, 503, 503, 200]);
      fixtureState.check();
    } finally { delayed.release(); }
  });
}

test('graph refresh preserves the complete graph on a real fetch transport failure', async ({ page }) => {
  let graph = '1', failing = false;
  const fixtureState = await fixture(page, { aRevision: () => graph, aView: () => failing ? 'abort' : { body: wires.get('a') } });
  await page.goto('http://bootstrap.test/alice/a');
  await expect(page.locator('[data-graph-id="a-119"]')).toBeVisible();
  graph = '2'; failing = true;
  await refreshRepository(page);
  await expect(refreshWarning(page)).toBeVisible();
  await expect(page.locator('[data-graph-id="a-119"]')).toBeVisible();
  await expect(graphFailure(page)).toHaveCount(0);
  fixtureState.check();
});

test('graph refresh warns even when the last complete generation was empty', async ({ page }) => {
  await page.clock.install();
  let graph = '1', failing = false;
  const fixtureState = await fixture(page, { aRevision: () => graph, aView: () => failing ? overloaded : { body: wires.get('empty') } });
  await page.goto('http://bootstrap.test/alice/a');
  await expect(page.locator('.empty-box')).toContainText('No context');
  graph = '2'; failing = true;
  await refreshRepository(page);
  await finishCapacityRetries(page, fixtureState.requests, '/api/v1/repos/a/view', 2);
  await expect(refreshWarning(page)).toBeVisible();
  await expect(refreshWarning(page).getByRole('button', { name: 'Retry', exact: true })).toBeVisible();
  await expect(page.locator('.graph-row')).toHaveCount(0);
  await expect(graphFailure(page)).toHaveCount(0);
  fixtureState.check();
});

for (const failure of ['401', '403', 'contract-invalid', 'contract-typeerror', 'graph-revision', 'pending-revision', 'evidence-revision']) {
  test(`graph refresh fails closed for ${failure} after a complete 200`, async ({ page }) => {
    const recovered = nextGraph();
    let graph = '1', failing = false, revisionStatus = 200;
    const invalid = structuredClone(recovered);
    if (failure === 'contract-invalid') invalid.graph.version = 2 as 1;
    if (failure === 'contract-typeerror') Object.assign(invalid.graph, { branch_contexts: { main: null } });
    if (failure.endsWith('-revision')) Object.assign(invalid.graph.revision, { [failure.split('-')[0]]: '99' });
    const rejected = failure === '401' || failure === '403'
      ? { status: Number(failure), body: { error: { code: 'resource_overloaded', message: 'Access denied' } } }
      : { body: invalid };
    const fixtureState = await fixture(page, { aRevision: () => graph, revisionStatus: () => revisionStatus,
      aView: () => failing ? rejected : { body: graph === '1' ? wires.get('a') : recovered } });
    await page.goto('http://bootstrap.test/alice/a');
    await expect(page.locator('[data-graph-id="a-119"]')).toBeVisible();
    graph = '2'; failing = true;
    await refreshRepository(page);
    await expect(graphFailure(page)).toBeVisible();
    await expect(page.locator('.graph-row')).toHaveCount(0);
    await expect(refreshWarning(page)).toHaveCount(0);
    if (failure === '401' || failure === '403') {
      revisionStatus = Number(failure);
      const beforeRetry = fixtureState.requests.length;
      await graphFailure(page).getByRole('button', { name: 'Retry', exact: true }).click();
      await expect.poll(() => fixtureState.requests.slice(beforeRetry).filter(path => path.endsWith('/revision')).length).toBe(1);
      await expect(graphFailure(page)).toContainText('Denied');
      await settle(page);
      expect(fixtureState.requests.slice(beforeRetry).filter(path => path !== '/api/v1/repos/a/changes')).toEqual(['/api/v1/repos/a/revision']);
      await expect(page.locator('.graph-row')).toHaveCount(0);
      await expect(page.getByText('BOOTSTRAP_BODY_a', { exact: true })).toHaveCount(0);
      revisionStatus = 200;
    }
    failing = false;
    const beforeRecovery = fixtureState.requests.length;
    await graphFailure(page).getByRole('button', { name: 'Retry', exact: true }).click();
    await expect(page.locator('[data-graph-id="a-120"]')).toBeVisible();
    if (failure === '401' || failure === '403') {
      const recovery = fixtureState.requests.slice(beforeRecovery).filter(path => path !== '/api/v1/repos/a/changes');
      expect(recovery[0]).toBe('/api/v1/repos/a/revision');
      expect(recovery.filter(path => path === '/api/v1/repos/a/view')).toHaveLength(1);
    }
    await expect(graphFailure(page)).toHaveCount(0);
    fixtureState.check();
  });
}

test('graph refresh retention does not bypass selected code-position revision checks', async ({ page }) => {
  await page.clock.install();
  const original = wires.get('a')!;
  const positionInput: Partial<RepositoryView> = { ...original, graph: undefined, history: [{ id: 'recorded-position', repo_id: 'a', kind: 'position',
    branch: 'main', branch_id: '', target: 'a-119', git_after: 'a'.repeat(40), created_at: '2026-10-15T00:00:00Z' }] };
  const positioned = serverGraphWireFixture(positionInput, '', 2);
  const mismatched = serverGraphWireFixture({ ...positionInput, revision: { ...revision, evidence: '1' } }, 'recorded-position', 2);
  let graph = '1', failing = false;
  const fixtureState = await fixture(page, { aRevision: () => graph, aView: () => failing ? overloaded : { body: positioned },
    aPosition: () => ({ body: mismatched.graph }) });
  await page.goto('http://bootstrap.test/alice/a');
  await expect(page.locator('[data-graph-id="a-119"]')).toBeVisible();
  graph = '2'; failing = true;
  await refreshRepository(page);
  await finishCapacityRetries(page, fixtureState.requests, '/api/v1/repos/a/view', 2);
  await expect(refreshWarning(page)).toBeVisible();
  const positions = page.locator('.graph-history-scope select');
  await positions.selectOption('recorded-position');
  await expect.poll(() => fixtureState.requests.filter(path => path === '/api/v1/repos/a/graph-state').length).toBe(1);
  await expect.poll(() => fixtureState.requests.filter(path => path === '/api/v1/repos/a/view').length).toBe(7);
  await expect(page.locator('.graph-row')).toHaveCount(0);
  await positions.selectOption('');
  await expect(page.locator('[data-graph-id="a-119"]')).toBeVisible();
  await expect(refreshWarning(page)).toBeVisible();
  fixtureState.check();
});
