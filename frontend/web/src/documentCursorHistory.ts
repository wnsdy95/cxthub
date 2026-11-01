export interface CursorHistory {
  readonly cursor: string;
  readonly previous?: CursorHistory;
}

export function rememberCursor(cursor: string, previous?: CursorHistory): CursorHistory {
  return { cursor, previous };
}

export function* historyCursors(history?: CursorHistory): IterableIterator<string> {
  for (let node = history; node; node = node.previous) yield node.cursor;
}

export function containsCursor(history: Iterable<string>, cursor: string): boolean {
  for (const previous of history) if (previous === cursor) return true;
  return false;
}
