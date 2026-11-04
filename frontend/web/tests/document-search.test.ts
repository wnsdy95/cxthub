import assert from 'node:assert/strict';
import { test } from 'node:test';
import { searchEventPosition } from '../src/documentSearch';
import type { SearchHit } from '../src/types';

const hit: SearchHit = { snapshot_id: 'snapshot', branch: 'main', kind: 'event', snippet: 'Matched text', created_at: '2026-10-20T00:00:00Z' };

test('search navigation uses archive ordinals including zero and never treats CIR sequence as a position', () => {
  assert.equal(searchEventPosition({ ...hit, event_index: 0, seq: 900 }), 0);
  assert.equal(searchEventPosition({ ...hit, event_index: 42, seq: 0 }), 42);
  assert.equal(searchEventPosition({ ...hit, seq: 42 }), undefined);
  assert.equal(searchEventPosition({ ...hit, kind: 'commit', event_index: 42 }), undefined);
  for (const event_index of [-1, 1.5, Infinity, NaN, Number.MAX_SAFE_INTEGER + 1, null, '42']) {
    assert.equal(searchEventPosition({ ...hit, event_index } as SearchHit), undefined);
  }
});
