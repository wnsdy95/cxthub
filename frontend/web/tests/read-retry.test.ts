import assert from 'node:assert/strict';
import { test } from 'node:test';
import { api, ApiError, ApiTransportError, parseRetryAfter } from '../src/api';
import { capacityRetryDelay, retryCapacityRead } from '../src/readRetry';

test('only explicit read capacity failures receive bounded automatic retries', () => {
  const busy = new ApiError('busy', 'resource_overloaded', 503, 1000);
  for (let attempt = 0; attempt < 4; attempt++) assert.equal(retryCapacityRead(attempt, busy), true);
  for (const attempt of [4, 5, 20]) assert.equal(retryCapacityRead(attempt, busy), false);
  assert.equal(retryCapacityRead(0, new ApiError('busy', 'resource_overloaded', 503)), true);
  assert.equal(retryCapacityRead(0, new ApiError('busy', 'resource_overloaded', 503, 10001)), false);
  for (const status of [400, 401, 403, 404, 409, 429, 500, 502, 504]) {
    assert.equal(retryCapacityRead(0, new ApiError('rejected', 'resource_overloaded', status, 1000)), false);
  }
  for (const error of [new ApiError('integrity', 'graph_invalid', 503, 1000), new ApiError('gateway', '', 503),
    new ApiTransportError('disconnected'), new SyntaxError('JSON'), new TypeError('invalid graph'),
    new DOMException('cancelled', 'AbortError'), {status: 503, code: 'resource_overloaded'}, undefined]) {
    assert.equal(retryCapacityRead(0, error), false);
  }
});

test('capacity retry delay honors server hints and adds bounded exponential jitter', () => {
  for (let attempt = 0; attempt < 4; attempt++) {
    const backoff = 1000 * 2 ** attempt;
    for (const hint of [undefined, 0, 1000, 5000, 10000]) {
      const delay = capacityRetryDelay(attempt, new ApiError('busy', 'resource_overloaded', 503, hint));
      const expected = Math.max(backoff, hint ?? 0);
      assert.ok(delay >= expected && delay <= expected + 250);
    }
  }
});

test('Retry-After parses seconds and HTTP dates without guessing malformed numbers', () => {
  const now = Date.UTC(2026, 9, 15, 20);
  assert.equal(parseRetryAfter('1', now), 1000);
  assert.equal(parseRetryAfter(' 0 ', now), 0);
  assert.equal(parseRetryAfter(new Date(now + 5000).toUTCString(), now), 5000);
  assert.equal(parseRetryAfter(new Date(now - 5000).toUTCString(), now), 0);
  for (const value of [null, '', ' ', '-1', '1.5', '1e2', 'Infinity', 'tomorrow', '999999999999999999999999']) {
    assert.equal(parseRetryAfter(value, now), undefined);
  }
});

test('API preserves Retry-After without retrying mutations in the transport', async () => {
  const original = globalThis.fetch;
  let requests = 0;
  try {
    globalThis.fetch = async () => {
      requests++;
      return new Response(JSON.stringify({error: {code: 'resource_overloaded', message: 'busy'}}), {
        status: 503, headers: {'Retry-After': '2', 'Content-Type': 'application/json'},
      });
    };
    await assert.rejects(() => api.setSessionArchived('repo', 'snapshot', true), error => {
      assert.ok(error instanceof ApiError);
      assert.equal(error.retryAfterMs, 2000);
      return true;
    });
    assert.equal(requests, 1);
  } finally { globalThis.fetch = original; }
});
