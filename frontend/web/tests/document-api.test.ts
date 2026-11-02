import assert from 'node:assert/strict';
import {test} from 'node:test';
import {api, ApiError, type DocViewEventPage} from '../src/api';
import {readDocumentStream} from '../src/documentVisibleReads';
import type {SessionDoc} from '../src/types';

test('source range capability is explicit and leaves old-server find responses readable', async () => {
  const originalFetch = globalThis.fetch;
  const requests: RequestInit[] = [];
  globalThis.fetch = async (_url, options) => {
    requests.push(options!);
    return new Response(JSON.stringify({ matches: [] }));
  };
  try {
    assert.deepEqual(await api.getDocFindPage('repo', 'hash', new URLSearchParams({ q: 'needle' })), { matches: [] });
    assert.equal(new Headers(requests[0].headers).get('Accept'), 'application/json, application/vnd.cxthub.find-source-ranges-v1+json');
    await api.getDocFindFragment('repo', 'hash', new URLSearchParams());
    assert.equal(new Headers(requests[1].headers).has('Accept'), false);
    assert.equal(requests.every(request => request.credentials === 'include'), true);
  } finally { globalThis.fetch = originalFetch; }
});

test('root exports and active view readers preserve transport identity and current failures', async () => {
  const originalFetch = globalThis.fetch;
  const repo = 'owner/repository';
  const hash = `sha256:${'a'.repeat(64)}`;
  const base = `sha256:${'b'.repeat(64)}`;
  const doc: SessionDoc = {
    hash,
    identity: 'cxt-manifest-sha256-v1',
    cir: {
      envelope: {cir_version: '2', source_provider: 'codex', git_branch: 'main'},
      events: [{kind: 'message', role: 'user', seq: 0, blocks: [{type: 'text', text: 'stored root conversation'}]}],
    },
  };
  const page: DocViewEventPage = {
    hash, envelope: doc.cir.envelope, events: doc.cir.events,
    total: 51, offset: 50, next: -1, inherited: 12,
  };
  const indexedPage = { ...page, events: page.events.map((event, index) => ({ ...event, event_index: page.offset + index })) };
  const selection = { repoId: repo, hash, base, start: 50, mode: 'chat' as const };
  const requests: {url: string; options?: RequestInit}[] = [];
  let response = new Response(JSON.stringify(doc));
  globalThis.fetch = async (url, options) => {
    requests.push({url: String(url), options});
    return response;
  };
  try {
    const full = await api.getDoc(repo, hash);
    assert.deepEqual(full, doc);
    response = new Response(JSON.stringify(page));
    const controller = new AbortController();
    const viewed = await api.getDocViewEvents(repo, hash, base, 50, 'chat', controller.signal);
    assert.deepEqual(viewed, indexedPage);
    response = new Response(JSON.stringify(page));
    const negotiated = await readDocumentStream(selection, controller.signal);
    assert.deepEqual(negotiated, { ...indexedPage, kind: 'legacy' });
    assert.equal(requests.length, 3, 'a valid untagged initial response is reused without another read');
    const fullURL = new URL(requests[0].url, 'https://synthetic.invalid');
    assert.equal(fullURL.pathname, `/api/v1/repos/owner%2Frepository/docs/${encodeURIComponent(hash)}`);
    assert.deepEqual([...fullURL.searchParams], []);
    for (const [index, parameters] of [
      { view: 'chat', offset: '50', limit: '20', base },
      { projection: 'visible-v1', view: 'chat', offset: '50', limit: '20', base },
    ].entries()) {
      const request = requests[index + 1];
      const pageURL = new URL(request.url, 'https://synthetic.invalid');
      assert.equal(pageURL.pathname, fullURL.pathname + '/events');
      assert.deepEqual([...pageURL.searchParams], Object.entries(parameters));
      assert.equal(request.options?.signal, controller.signal);
    }
    for (const {options} of requests) {
      assert.equal(options?.method, 'GET');
      assert.equal(options?.credentials, 'include');
      assert.equal(new Headers(options?.headers).get('X-Cxt-Doc-Identities'), 'cxt-manifest-sha256-v1');
      assert.equal(new Headers(options?.headers).has('Authorization'), false);
    }

    for (const [status, code] of [[409, 'doc_corrupt'], [401, 'unauthorized'], [403, 'forbidden'],
      [409, 'doc_visible_generation_stale'], [503, 'doc_range_index_pending']] as const) {
      for (const read of [() => api.getDoc(repo, hash), () => api.getDocViewEvents(repo, hash, undefined, 0, 'chat', controller.signal),
        () => readDocumentStream(selection, controller.signal)]) {
        response = new Response(JSON.stringify({error: {code, message: 'current read rejected'}}), {status});
        const before = requests.length;
        await assert.rejects(read, (error: unknown) => {
          assert.ok(error instanceof ApiError);
          assert.equal(error.status, status);
          assert.equal(error.code, code);
          assert.equal(error.message, 'current read rejected');
          return true;
        });
        assert.equal(requests.length, before + 1, 'no legacy fallback or implicit retry');
        assert.deepEqual(full, doc, 'the previous response remains an unchanged historical value');
        assert.deepEqual(viewed, indexedPage);
        assert.deepEqual(negotiated, { ...indexedPage, kind: 'legacy' });
      }
    }
  } finally {
    globalThis.fetch = originalFetch;
  }
});
