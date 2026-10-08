import assert from 'node:assert/strict';
import {test} from 'node:test';
import {api, ApiError, type DocEventPage} from '../src/api';
import type {SessionDoc} from '../src/types';

test('root full reads retain identity; paged reads and current failures use the same API contract', async () => {
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
  const page: DocEventPage = {
    hash, envelope: doc.cir.envelope, events: doc.cir.events,
    total: 51, offset: 50, next: 51, inherited: 12,
  };
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
    assert.deepEqual(await api.getDocEvents(repo, hash, base, 50, controller.signal), page);
    const fullURL = new URL(requests[0].url, 'https://synthetic.invalid');
    assert.equal(fullURL.pathname, `/api/v1/repos/owner%2Frepository/docs/${encodeURIComponent(hash)}`);
    const pageURL = new URL(requests[1].url, 'https://synthetic.invalid');
    assert.equal(pageURL.pathname, fullURL.pathname + '/events');
    assert.deepEqual([...pageURL.searchParams], [['offset', '50'], ['limit', '50'], ['base', base]]);
    assert.equal(requests[1].options?.signal, controller.signal);
    for (const {options} of requests) {
      assert.equal(options?.method, 'GET');
      assert.equal(options?.credentials, 'include');
      assert.equal(new Headers(options?.headers).get('X-Cxt-Doc-Identities'), 'cxt-manifest-sha256-v1');
      assert.equal(new Headers(options?.headers).has('Authorization'), false);
    }

    // A prior successful response must not mask current corruption or lost access.
    for (const [status, code] of [[409, 'doc_corrupt'], [401, 'unauthorized']] as const) {
      for (const read of [() => api.getDoc(repo, hash), () => api.getDocEvents(repo, hash, undefined, 0)]) {
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
      }
    }
  } finally {
    globalThis.fetch = originalFetch;
  }
});
