import assert from 'node:assert/strict';
import { test } from 'node:test';
import { api, ApiError, ApiTransportError, parseRetryAfter } from '../src/api';
import { capacityReadRetry, capacityRetryDelay, documentReadRetry, pendingStateReadRetry, retryCapacityRead, retryDocumentRead, retryPendingStateRead } from '../src/readRetry';

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

test('only durable document preparation extends the retry budget, never graph reads', () => {
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
      () => api.getDocViewEvents('repo', 'document', 'base', -1, 'chat'),
      () => api.getDocEvent('repo', 'document', 7),
      () => api.getDocEventFragment('repo', 'document', 7, 65536),
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

test('only durable pending projection preparation extends On Hold retries while capacity stays at four', () => {
  const pending = new ApiError('preparing', 'pending_projection_pending', 503, 3000);
  for (let failureCount = 0; failureCount < 12; failureCount++) {
    assert.equal(retryPendingStateRead(failureCount, pending), failureCount < 9);
    assert.equal(pendingStateReadRetry.retry(failureCount, pending), failureCount < 9);
    const busy = new ApiError('busy', 'resource_overloaded', 503, 3000);
    assert.equal(retryPendingStateRead(failureCount, busy), failureCount < 4);
    assert.equal(pendingStateReadRetry.retry(failureCount, busy), capacityReadRetry.retry(failureCount, busy));
  }
  for (const hint of [undefined, 0, 3000, 10_000]) {
    assert.equal(retryPendingStateRead(0, new ApiError('preparing', 'pending_projection_pending', 503, hint)), true);
  }
  for (const hint of [10_001, Infinity, NaN]) {
    for (const code of ['pending_projection_pending', 'resource_overloaded']) {
      assert.equal(retryPendingStateRead(0, new ApiError('preparing', code, 503, hint)), false);
    }
  }
});

test('pending, document and full graph retry policies never borrow another preparation contract', () => {
  const pending = new ApiError('preparing', 'pending_projection_pending', 503, 3000);
  const document = new ApiError('preparing', 'doc_range_index_pending', 503, 3000);
  for (let failureCount = 0; failureCount < 12; failureCount++) {
    assert.equal(capacityReadRetry.retry(failureCount, pending), false);
    assert.equal(capacityReadRetry.retry(failureCount, document), false);
    assert.equal(documentReadRetry.retry(failureCount, pending), false);
    assert.equal(pendingStateReadRetry.retry(failureCount, document), false);
  }
});

test('pending projection retries reject terminal statuses, codes, transport failures and lookalike errors', () => {
  for (const status of [200, 202, 400, 401, 403, 404, 409, 422, 429, 500, 502, 504]) {
    assert.equal(retryPendingStateRead(0, new ApiError('rejected', 'pending_projection_pending', status, 3000)), false);
  }
  for (const code of ['pending_projection_unavailable', 'doc_range_index_unavailable', 'integrity', 'not_found', 'unavailable', 'graph_invalid', '']) {
    assert.equal(retryPendingStateRead(0, new ApiError('terminal', code, 503, 3000)), false);
  }
  for (const error of [new ApiTransportError('disconnected'), new SyntaxError('JSON'), new TypeError('invalid projection'),
    new Error('Invalid On Hold read generation'), new DOMException('cancelled', 'AbortError'),
    { status: 503, code: 'pending_projection_pending', retryAfterMs: 3000 }, null, undefined]) {
    assert.equal(retryPendingStateRead(0, error), false);
  }
});

test('pending projection retries honor hints and bounded jitter with no more than 90 seconds of waits', context => {
  for (const random of [0, 0.5, 0.999]) {
    context.mock.method(Math, 'random', () => random);
    for (const hint of [undefined, 0, 3000, 5000, 10_000]) {
      const pending = new ApiError('preparing', 'pending_projection_pending', 503, hint);
      const delays = Array.from({ length: 9 }, (_, attempt) => pendingStateReadRetry.retryDelay(attempt, pending));
      assert.deepEqual(delays, [1000, 2000, 4000, 8000, 8000, 8000, 8000, 8000, 8000]
        .map(backoff => Math.min(10_000, Math.max(backoff, hint ?? 0) + Math.floor(random * 251))));
      assert.ok(delays.every(delay => delay >= (hint ?? 0) && delay <= 10_000));
      assert.ok(delays.reduce((total, delay) => total + delay, 0) <= 90_000);
    }
    context.mock.restoreAll();
  }
});

test('pending read transport preserves preparation hints but performs only one request per call', async () => {
  const original = globalThis.fetch;
  const requests: string[] = [];
  try {
    globalThis.fetch = async url => {
      requests.push(String(url));
      return new Response(JSON.stringify({ error: { code: 'pending_projection_pending', message: 'Preparing On Hold' } }), {
        status: 503, headers: { 'Retry-After': '3', 'Content-Type': 'application/json' },
      });
    };
    await assert.rejects(() => api.pendingState('repo/with space'), error => {
      assert.ok(error instanceof ApiError);
      assert.equal(error.status, 503);
      assert.equal(error.code, 'pending_projection_pending');
      assert.equal(error.retryAfterMs, 3000);
      assert.equal(retryPendingStateRead(0, error), true);
      assert.equal(retryDocumentRead(0, error), false);
      assert.equal(retryCapacityRead(0, error), false);
      return true;
    });
    assert.deepEqual(requests, ['/api/v1/repos/repo%2Fwith%20space/pending-view?projection=on-hold-v1']);
  } finally { globalThis.fetch = original; }
});

test('pending API transport, cancellation and JSON failures are terminal without hidden retries', async () => {
  const original = globalThis.fetch;
  let requests = 0;
  try {
    for (const failure of ['transport', 'abort', 'json'] as const) {
      globalThis.fetch = async () => {
        requests++;
        if (failure === 'transport') throw new TypeError('disconnected');
        if (failure === 'abort') throw new DOMException('cancelled', 'AbortError');
        return new Response('{not-json');
      };
      const before = requests;
      await assert.rejects(() => api.pendingState('repo'), error => {
        if (failure === 'transport') assert.ok(error instanceof ApiTransportError);
        else if (failure === 'abort') assert.ok(error instanceof DOMException && error.name === 'AbortError');
        else assert.ok(error instanceof SyntaxError);
        assert.equal(retryPendingStateRead(0, error), false);
        assert.equal(retryDocumentRead(0, error), false);
        assert.equal(retryCapacityRead(0, error), false);
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
    await assert.rejects(() => api.setSessionArchived('repo', 'snapshot', true), error => {
      assert.ok(error instanceof ApiError);
      assert.equal(error.retryAfterMs, 2000);
      return true;
    });
    assert.equal(requests, 1);
  } finally { globalThis.fetch = original; }
});
