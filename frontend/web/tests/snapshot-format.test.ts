import assert from 'node:assert/strict';
import { when } from '../src/snapshotFormat';

for (const value of [undefined, '', '0001-01-01T00:00:00Z', 'not a timestamp']) {
  assert.equal(when(value), '');
  assert.equal(when(value, true), '');
}
const recorded = new Date(2026, 9, 11, 9, 7).toISOString();
assert.equal(when(recorded), '2026-10-11 09:07');
assert.equal(when(recorded, true), '10-11 09:07');
