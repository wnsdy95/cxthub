import { expect, test, type Page, type TestInfo } from '@playwright/test';
import { build } from 'esbuild';
import { serverGraphWireFixture } from '../tests/serverGraphFixture';
import type { RepositoryView } from '../src/types';

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
        return <output data-testid="view">{repo}:{q.graphError?'error':q.graphLoading?'loading':'ready'}</output>;
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

type Options = { public?: boolean; denied?: boolean; revisionStatus?: number; aRevision?: () => string; holdA?: { at: number; promise: Promise<void> } };
async function fixture(page: Page, options: Options = {}) {
  const unexpected: string[] = [], errors: string[] = [];
  const requests: string[] = [];
  let aReads = 0;
  page.on('pageerror', e => errors.push(e.message));
  await page.addInitScript(() => {
    localStorage.setItem('cxt.locale', 'en');
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
  });
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
    const send = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
    if (p === '/api/v1/me') { await send(options.public ? { error: { message: 'Unauthorized' } } : { id: 'reader', username: 'reader', name: 'Reader', locale: 'en' }, options.public ? 401 : 200); return; }
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
        await send(options.revisionStatus ? { error: { message: 'Denied' } } : { ...revision, graph: id === 'a' ? options.aRevision?.() ?? '1' : '1' }, options.revisionStatus ?? 200); return;
      }
      if (endpoint === 'view') {
        expect(url.searchParams.get('graph_encoding')).toBe('indexed-v2');
        if (id === 'a' && ++aReads === options.holdA?.at) await options.holdA.promise;
        await send(wires.get(id)); return;
      }
      if (endpoint.startsWith('docs/') && endpoint.endsWith('/events')) {
        await send({ hash: endpoint.split('/')[1], envelope: { cir_version: '2', source_provider: 'codex', captured_at: '2026-09-18T00:00:00Z', git_branch: 'main' },
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
  await expect(page.getByTestId('view')).toHaveText('a:ready'); await settle(page);
  expect(f.requests.filter(p => p === '/api/v1/repos/a/view')).toHaveLength(1);
  expect(f.requests.filter(p => p === '/api/v1/repos/a/revision')).toHaveLength(1);
  f.check();
});
