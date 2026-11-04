import type { LegacyPageRecord, LegacyRow } from './documentLegacyLedger';

export interface LegacyLocatedRow extends LegacyRow {
  readonly page: LegacyPageRecord;
}

export interface LegacyTurn {
  readonly index: number;
  readonly head: LegacyLocatedRow | null;
  readonly body: LegacyLocatedRow[];
}

export function legacyDocumentTurns(pages: readonly LegacyPageRecord[]): LegacyTurn[] {
  const turns: LegacyTurn[] = [];
  let current: LegacyTurn = { index: (pages[0]?.offset ?? 0) - 1, head: null, body: [] };
  for (const page of pages) {
    for (const metadata of page.rows) {
      const row = { ...metadata, page };
      if (row.prompt) {
        if (current.head || current.body.length) turns.push(current);
        current = { index: row.index, head: row, body: [] };
      } else if (row.rendered) current.body.push(row);
    }
  }
  if (current.head || current.body.length) turns.push(current);
  return turns;
}

export function legacyResponseGroups(body: readonly LegacyLocatedRow[]) {
  const groups: { pre: LegacyLocatedRow[]; message: LegacyLocatedRow | null }[] = [];
  let pending: LegacyLocatedRow[] = [];
  for (const row of body) {
    if (row.message) {
      groups.push({ pre: pending, message: row });
      pending = [];
    } else pending.push(row);
  }
  if (pending.length) groups.push({ pre: pending, message: null });
  return groups;
}

export function legacyGroupSegments<Value>(groups: readonly Value[]) {
  const segments: Value[][] = [];
  for (let offset = 0; offset < groups.length; offset += 20) segments.push(groups.slice(offset, offset + 20));
  return segments;
}

export function legacyRowSegments(rows: readonly LegacyLocatedRow[]) {
  const segments: { page: LegacyPageRecord; rows: LegacyLocatedRow[] }[] = [];
  for (const row of rows) {
    let segment = segments.at(-1);
    if (!segment || segment.page !== row.page || segment.rows.length >= 64) {
      segment = { page: row.page, rows: [] };
      segments.push(segment);
    }
    segment.rows.push(row);
  }
  return segments;
}
