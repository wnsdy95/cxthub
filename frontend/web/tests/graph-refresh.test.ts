import assert from 'node:assert/strict';
import { test } from 'node:test';
import { api, ApiError, ApiTransportError } from '../src/api';
import { isRetryableGraphRefreshError } from '../src/graphRefresh';

test('graph refresh retention uses a narrow transport and capacity allowlist', () => {
  assert.equal(isRetryableGraphRefreshError(new ApiTransportError('connection lost')), true);
  assert.equal(isRetryableGraphRefreshError(new ApiError('busy', 'resource_overloaded', 503)), true);
  for (const status of [408, 429, 502, 503, 504]) {
    assert.equal(isRetryableGraphRefreshError(new ApiError('gateway failure', '', status)), true);
    assert.equal(isRetryableGraphRefreshError(new ApiError('invalid', 'graph_invalid', status)), false);
  }
  for (const status of [400, 401, 403, 404, 409, 413, 422, 500, 501]) {
    for (const code of ['', 'resource_overloaded']) {
      assert.equal(isRetryableGraphRefreshError(new ApiError('rejected', code, status)), false);
    }
  }
  for (const error of [null, undefined, new Error('503 resource_overloaded'), new TypeError('Failed to fetch'),
    new SyntaxError('Invalid JSON'), new DOMException('Cancelled', 'AbortError'),
    { status: 503, code: 'resource_overloaded' }]) {
    assert.equal(isRetryableGraphRefreshError(error), false);
  }
});

test('only fetch and response-body transport failures receive the transport error type', async () => {
  const originalFetch = globalThis.fetch;
  const failure = new TypeError('connection interrupted');
  try {
    globalThis.fetch = async () => { throw failure; };
    await assert.rejects(() => api.repositoryView('repo'), error => {
      assert.ok(error instanceof ApiTransportError);
      assert.equal(error.cause, failure);
      return isRetryableGraphRefreshError(error);
    });
    globalThis.fetch = async () => {
      const response = new Response();
      response.text = async () => { throw failure; };
      return response;
    };
    await assert.rejects(() => api.repositoryView('repo'), ApiTransportError);
    globalThis.fetch = async () => { throw new DOMException('Cancelled', 'AbortError'); };
    await assert.rejects(() => api.repositoryView('repo'), error => {
      assert.equal((error as Error).name, 'AbortError');
      return !isRetryableGraphRefreshError(error);
    });
    for (const body of ['not JSON', 'null', JSON.stringify({ graph: {
      encoding: 'indexed-v2', dictionary: [], branch_snapshots: {}, hold: [], previous: [], branch_contexts: { main: null },
    } })]) {
      globalThis.fetch = async () => new Response(body);
      await assert.rejects(() => api.repositoryView('repo'), error => {
        assert.ok(error instanceof TypeError || error instanceof SyntaxError);
        return !isRetryableGraphRefreshError(error);
      });
    }
    for (const status of [401, 403, 503]) {
      globalThis.fetch = async () => new Response(JSON.stringify({ error: { code: 'resource_overloaded', message: 'busy' } }), { status });
      await assert.rejects(() => api.repositoryView('repo'), error => {
        assert.ok(error instanceof ApiError);
        assert.equal(error.status, status);
        assert.equal(isRetryableGraphRefreshError(error), status === 503);
        return true;
      });
    }
  } finally { globalThis.fetch = originalFetch; }
});
