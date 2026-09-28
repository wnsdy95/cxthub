import assert from 'node:assert/strict';
import { anchoredScrollTop, mountedRowIndices, rowOffsets, visibleRowWindow } from '../src/graphWindow';

const heights = Array.from({ length: 10_000 }, (_, i) => i % 17 === 0 ? 46 : 26);
const offsets = rowOffsets(heights);
assert.equal(offsets.at(-1), heights.reduce((sum, height) => sum + height, 0));
for (let top = 0; top < offsets.at(-1)!; top += 317) {
  const visible = visibleRowWindow(offsets, top, 300);
  const expected = heights.flatMap((height, i) => offsets[i] < top + 300 && offsets[i] + height > top ? [i] : []);
  assert.deepEqual(Array.from({ length: visible.end - visible.start }, (_, i) => visible.start + i), expected);
  const mounted = mountedRowIndices(heights.length, visible, 6, [0, 9999]);
  assert.ok(mounted.length <= 27, 'viewport, overscan and pinned focus/drag rows bound the DOM');
  assert.ok(expected.every(i => mounted.includes(i)), 'no visible SVG segment is dropped');
  assert.ok(mounted.includes(0) && mounted.includes(9999), 'focus and drag survive scrolling');
}
assert.deepEqual(visibleRowWindow([0], 0, 300), { start: 0, end: 0 });
assert.deepEqual(visibleRowWindow([0, 26, 72, 98], 26, 46), { start: 1, end: 2 });
assert.deepEqual(visibleRowWindow([0, 26], 26, 300), { start: 0, end: 0 });

const old = ['a', 'b', 'c'], oldOffsets = rowOffsets([26, 46, 26]);
const next = ['new', ...old], nextOffsets = rowOffsets([26, 26, 46, 26]);
const nextIndices = new Map(next.map((id, index) => [id, index]));
assert.equal(anchoredScrollTop(old, oldOffsets, nextIndices, nextOffsets, 0), 0, 'readers at the top see new work');
assert.equal(anchoredScrollTop(old, oldOffsets, nextIndices, nextOffsets, 35), 61, 'live inserts preserve the same row and pixel offset');
assert.equal(anchoredScrollTop(old, oldOffsets, new Map([['a', 0], ['c', 1]]), rowOffsets([26, 26]), 35), 26, 'folding retains a surviving successor');
