import { serverGraphWireFixture, serverMemoryPositionsFixture } from '../tests/serverGraphFixture';
import type { Page } from '@playwright/test';

export interface ApiRequest {
  method: string;
  pathname: string;
  searchParams: URLSearchParams;
}

export interface ApiResponse {
  body: unknown;
  status?: number;
  contentType?: string;
}

export type ApiResponder = (request: ApiRequest) => ApiResponse | undefined;

// Existing fixtures describe each component. Assemble one server response at
// request time, preserving failures; production never performs this fan-out.
function graphViewFixture(request: ApiRequest, responder: ApiResponder): ApiResponse | undefined {
  if (request.method !== 'GET' || !/^\/api\/v1\/repos\/[^/]+\/view$/.test(request.pathname)) return undefined;
  const body: Record<string, unknown> = {};
  for (const key of ['refs', 'snapshots', 'reflog', 'history', 'pending', 'unsync']) {
    const response = responder({ ...request, pathname: request.pathname.replace(/view$/, key) });
    if (!response) return undefined;
    if ((response.status ?? 200) >= 400) return response;
    body[key] = response.body;
  }
  return { body };
}

export async function installApiFixture(page: Page, responder: ApiResponder, options: {graphRevision?: () => string} = {}): Promise<string[]> {
  const unexpected: string[] = [];
  const states = new Map<string, {graph: number; pending: number; g: string; p: string}>();
  const projected = (input: ApiRequest): ApiResponse | undefined => {
    if (input.method !== 'GET' || !/\/(view|revision|changes|pending-view|graph-state)$/.test(input.pathname)) return undefined;
    // Contract regressions can supply actual, distinct serialized responses.
    // Do not silently replace them with an enriched full-view fixture.
    if (/\/(pending-view|revision|changes)$/.test(input.pathname)) {
      const direct = responder(input);
      if (direct) return direct;
    }
    const base = input.pathname.replace(/[^/]+$/, 'view');
    const viewRequest = {...input, pathname: base};
    const response = responder(viewRequest) ?? graphViewFixture(viewRequest, responder);
    if (!response || (response.status ?? 200) >= 400) return response;
    const body = response.body as Record<string, any>;
    const pendingIDs = new Set((body.pending ?? []).map((p: any) => p.target));
    const roots = new Set((body.refs ?? []).map((r: any) => r.target));
    const g = JSON.stringify({...body, revision: undefined, pending: undefined, snapshots: body.snapshots?.filter((s: any) => !pendingIDs.has(s.id) || roots.has(s.id))});
    const p = JSON.stringify([body.pending, body.snapshots?.filter((s: any) => pendingIDs.has(s.id))]);
    const old = states.get(base);
    const next = {graph: (old?.graph ?? 0) + (g !== old?.g ? 1 : 0), pending: (old?.pending ?? 0) + (p !== old?.p ? 1 : 0), g, p}; states.set(base,next);
    const revision = {graph: options.graphRevision?.() ?? String(next.graph), pending: String(next.pending), ...(body.revision ?? {})};
    if (input.pathname.endsWith('/revision')) return {body: revision};
    if (input.pathname.endsWith('/changes')) return {contentType: 'text/event-stream', body: `event: revision\ndata: ${JSON.stringify(revision)}\n\n`};
    try {
      const full=serverGraphWireFixture({...body,revision},input.pathname.endsWith('/graph-state')?input.searchParams.get('position') ?? '':'');
      if(input.pathname.endsWith('/graph-state')) return {body:full.graph};
      if(input.pathname.endsWith('/pending-view')) return {body:{revision,graph:full.graph,pending:full.pending,
        snapshots:full.snapshots.filter(s=>pendingIDs.has(s.id)).map(({branches:_membership,...s})=>s)}};
      return {...response,body:full};
    } catch(error) {
      const cause=error as Error & {stderr?:string};
      return {status:500,body:{error:{message:String(cause.stderr || cause.message)}}};
    }
  };
  await page.route('**/api/v1/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const key = `${request.method()} ${url.pathname}${url.search}`;
    const input = {
      method: request.method(),
      pathname: url.pathname,
      searchParams: url.searchParams,
    };
    const memoryPositions = (): ApiResponse | undefined => {
      if (input.method !== 'GET' || !input.pathname.endsWith('/effective-memory/positions')) return undefined;
      const history = responder({...input, pathname: input.pathname.replace(/effective-memory\/positions$/, 'history')})?.body;
      return {body: serverMemoryPositionsFixture(input.searchParams.get('snapshot_id') ?? '', input.searchParams.get('event_id') ?? '', Array.isArray(history) ? history : [])};
    };
    const response = projected(input) ?? responder(input) ?? graphViewFixture(input, responder) ?? memoryPositions()
      // Graph-only fixtures contain no effective memory items. Memory behavior
      // tests supply their own response and consume Go's real position resolver.
      ?? (input.method === 'GET' && input.pathname.endsWith('/effective-memory') ? {body:{selection:{snapshot_id:input.searchParams.get('snapshot_id'),code_commit:input.searchParams.get('code_commit')},revision:{graph:'1',pending:'1'},state_hash:'fixture',lineage_hash:'fixture',items:[],total:0,next_cursor:''}} : undefined)
      ?? (request.method() === 'GET' && /^\/api\/v1\/workspaces\/[^/]+\/notifications$/.test(url.pathname) ? { body: [] } : undefined)
      ?? (request.method() === 'GET' && /^\/api\/v1\/repos\/[^/]+\/prs\/promotions$/.test(url.pathname) ? { body: [] } : undefined);
    if (!response) {
      unexpected.push(key);
      await route.fulfill({
        status: 501,
        contentType: 'application/json',
        body: JSON.stringify({ error: { message: `Unhandled E2E API request: ${key}` } }),
      });
      return;
    }
    await route.fulfill({
      status: response.status ?? 200,
      contentType: response.contentType ?? 'application/json',
      body: response.contentType === 'text/event-stream' ? String(response.body) : JSON.stringify(response.body),
    });
  });
  return unexpected;
}

export function capturePageErrors(page: Page): Error[] {
  const errors: Error[] = [];
  page.on('pageerror', (error) => errors.push(error));
  return errors;
}
