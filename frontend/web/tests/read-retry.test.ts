import assert from 'node:assert/strict';
import { test } from 'node:test';
import { api, ApiError, ApiTransportError, parseRetryAfter } from '../src/api';
import { capacityRetryDelay, documentReadRetry, retryCapacityRead, retryDocumentRead } from '../src/readRetry';

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

test('only durable document preparation extends the retry budget, never other reads', () => {
  const pending = new ApiError('preparing', 'doc_range_index_pending', 503, 3000);
  for (let failureCount = 0; failureCount < 9; failureCount++) {
    assert.equal(documentReadRetry.retry(failureCount, pending), true);
    assert.equal(retryCapacityRead(failureCount, pending), false);
  }
  for (const failureCount of [9, 10, 20]) assert.equal(retryDocumentRead(failureCount, pending), false);
  for (let failureCount = 0; failureCount < 12; failureCount++) {
    const busy = new ApiError('busy', 'resource_overloaded', 503, 3000);
    assert.equal(retryDocumentRead(failureCount, busy), retryCapacityRead(failureCount, busy));
  }
  assert.equal(retryDocumentRead(0, new ApiError('preparing', 'doc_range_index_pending', 503)), true);
  assert.equal(retryDocumentRead(0, new ApiError('preparing', 'doc_range_index_pending', 503, 10_000)), true);
  for (const hint of [10_001, Infinity, NaN]) {
    assert.equal(retryDocumentRead(0, new ApiError('preparing', 'doc_range_index_pending', 503, hint)), false);
  }
  for (const status of [200, 202, 400, 401, 403, 404, 409, 422, 429, 500, 502, 504]) {
    assert.equal(retryDocumentRead(0, new ApiError('rejected', 'doc_range_index_pending', status, 3000)), false);
  }
  for (const code of ['doc_range_index_unavailable', 'doc_find_display_unavailable', 'integrity', 'not_found', 'unavailable', 'graph_invalid', '']) {
    assert.equal(retryDocumentRead(0, new ApiError('terminal', code, 503, 3000)), false);
  }
  for (const error of [new ApiTransportError('disconnected'), new SyntaxError('JSON'), new TypeError('invalid page'),
    new DOMException('cancelled', 'AbortError'), { status: 503, code: 'doc_range_index_pending' }, undefined]) {
    assert.equal(retryDocumentRead(0, error), false);
  }
});

test('document retries reuse exponential capacity backoff with jitter and at most 90 seconds of waits', context => {
  for (const random of [0, 0.5, 0.999]) {
    context.mock.method(Math, 'random', () => random);
    for (const hint of [undefined, 0, 3000, 5000, 10_000]) {
      const pending = new ApiError('preparing', 'doc_range_index_pending', 503, hint);
      const delays = Array.from({ length: 9 }, (_, attempt) => documentReadRetry.retryDelay(attempt, pending));
      assert.deepEqual(delays, [1000, 2000, 4000, 8000, 8000, 8000, 8000, 8000, 8000]
        .map(backoff => Math.min(10_000, Math.max(backoff, hint ?? 0) + Math.floor(random * 251))));
      assert.ok(delays.reduce((total, delay) => total + delay, 0) <= 90_000);
      assert.ok(delays.every(delay => delay >= (hint ?? 0)));
    }
    context.mock.restoreAll();
  }
});

test('public-field preparation uses the bounded document retry policy only', () => {
  for (const failureCount of [0, 1, 8]) {
    const pending = new ApiError('fields preparing', 'doc_find_fields_pending', 503, 1000);
    assert.equal(retryDocumentRead(failureCount, pending), true);
    assert.equal(retryCapacityRead(failureCount, pending), false);
  }
  for (const failureCount of [9, 10]) assert.equal(retryDocumentRead(failureCount, new ApiError('preparing', 'doc_find_fields_pending', 503)), false);
  for (const status of [401, 403, 404, 409, 422, 500, 502]) {
    assert.equal(retryDocumentRead(0, new ApiError('denied', 'doc_find_fields_pending', status)), false);
  }
  assert.equal(retryDocumentRead(0, new ApiError('unavailable', 'doc_find_fields_unavailable', 503)), false);
  assert.equal(retryDocumentRead(0, new ApiError('preparing', 'doc_find_fields_pending', 503, 10_001)), false);
});

test('document read transport preserves durable pending hints without retrying independently', async () => {
  const original = globalThis.fetch;
  let requests = 0;
  try {
    globalThis.fetch = async () => {
      requests++;
      return new Response(JSON.stringify({ error: { code: 'doc_range_index_pending', message: 'Preparing document' } }), {
        status: 503, headers: { 'Retry-After': '3', 'Content-Type': 'application/json' },
      });
    };
    for (const read of [
      () => api.anchoredEvents('repo', 'claude', 'session', 0),
      () => api.anchoredEvent('repo', 'claude', 'session', 7),
      () => api.anchoredEventFragment('repo', 'claude', 'session', 7, 65536),
    ]) {
      const before = requests;
      await assert.rejects(read, error => {
        assert.ok(error instanceof ApiError);
        assert.equal(error.code, 'doc_range_index_pending');
        assert.equal(error.retryAfterMs, 3000);
        assert.equal(retryDocumentRead(0, error), true);
        return true;
      });
      assert.equal(requests, before + 1);
    }
  } finally { globalThis.fetch = original; }
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
    await assert.rejects(() => api.setAnchoredSessionArchived('repo', 'claude', 'session', true), error => {
      assert.ok(error instanceof ApiError);
      assert.equal(error.retryAfterMs, 2000);
      return true;
    });
    assert.equal(requests, 1);
  } finally { globalThis.fetch = original; }
});
