// Presentation geometry only. All rows and edges are laid out before windowing.
export type RowWindow = { start: number; end: number };

export function rowOffsets(heights: readonly number[]): number[] {
  const offsets = [0];
  for (const height of heights) offsets.push(offsets[offsets.length - 1] + height);
  return offsets;
}

export function rowAtOffset(offsets: readonly number[], offset: number): number {
  let low = 0, high = offsets.length - 1;
  while (low < high) {
    const middle = Math.ceil((low + high) / 2);
    if (offsets[middle] <= offset) low = middle;
    else high = middle - 1;
  }
  return Math.min(low, offsets.length - 2);
}

export function visibleRowWindow(offsets: readonly number[], top: number, height: number): RowWindow {
  const count = offsets.length - 1;
  if (!count || height <= 0 || top >= offsets[count]) return { start: 0, end: 0 };
  const start = Math.max(0, rowAtOffset(offsets, Math.max(0, top)));
  const bottom = Math.min(offsets[count], top + height);
  let end = rowAtOffset(offsets, bottom);
  if (offsets[end] < bottom) end++;
  return { start, end: Math.max(start, end) };
}

export function mountedRowIndices(count: number, visible: RowWindow, overscan: number, pinned: readonly number[]): number[] {
  const indices = new Set<number>();
  for (let i = Math.max(0, visible.start - overscan); i < Math.min(count, visible.end + overscan); i++) indices.add(i);
  for (const i of pinned) if (i >= 0 && i < count) indices.add(i);
  return [...indices].sort((a, b) => a - b);
}

export function anchoredScrollTop(previousIds: readonly string[], previousOffsets: readonly number[], nextIndices: ReadonlyMap<string, number>, nextOffsets: readonly number[], top: number): number {
  if (top <= 0 || previousIds.length === 0) return 0;
  const anchor = rowAtOffset(previousOffsets, top);
  // If folding removed the anchor, retain the next surviving row. Never use
  // an unrelated row's old numerical index as its identity after a live insert.
  for (let i = anchor; i < previousIds.length; i++) {
    const next = nextIndices.get(previousIds[i]);
    if (next !== undefined) return nextOffsets[next] + (i === anchor ? Math.min(top - previousOffsets[i], nextOffsets[next + 1] - nextOffsets[next] - 1) : 0);
  }
  return Math.min(top, nextOffsets[nextOffsets.length - 1]);
}
