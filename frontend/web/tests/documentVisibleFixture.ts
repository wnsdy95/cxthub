import type { DocVisibleEvent, DocVisiblePage } from '../src/documentVisible';

export const visibleSelection = { repoId: 'repo', hash: 'document', base: 'base', start: -1, mode: 'chat' as const };

export function visibleMessage(index: number, text: string, role = 'assistant'): DocVisibleEvent {
  return { kind: 'message', event_index: index, event_bytes: 100, seq: index + 700, role,
    seed_summary: false, empty_reasoning: false, blocks: [{ type: 'text', text }] };
}

export function visibleTool(index: number): DocVisibleEvent {
  return { kind: 'tool_call', event_index: index, event_bytes: 900, seq: index + 700, tool_name: 'Bash',
    seed_summary: false, empty_reasoning: false, deferred: true };
}

export function syntheticDocumentText(seed: number, length: number, label: string, lines = false): string {
  let state = Math.imul(seed + 1, 2654435761) >>> 0;
  const alphabet = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789';
  const characters = Array.from({ length }, (_character, position) => {
    state ^= state << 13;
    state ^= state >>> 17;
    state ^= state << 5;
    return lines && position % 48 === 47 ? '\n' : position % 11 === 10 ? ' ' : alphabet[(state >>> 0) % alphabet.length];
  });
  return (label + characters.join('')).slice(0, length);
}

export function heavyTextVisiblePage(from: number, pageCount: number): DocVisiblePage {
  const body = heavyVisiblePage(from, pageCount * 620);
  return { ...body, replay_cursor: `main-${from}`, units: body.units.map(unit => ({ ...unit, event: {
    ...unit.event!, event_bytes: 4608 + 256,
    blocks: [{ type: 'text', text: syntheticDocumentText(unit.from, 4608, `Synthetic anchor ${unit.from}: `) }],
  } })) };
}

export function heavyVisiblePage(from = 0, total = 1240): DocVisiblePage {
  const to = Math.min(from + 620, total);
  return { projection: 'visible-v1', kind: 'units', hash: 'document', base: 'base', generation: 'generation-one', rules: 1, view: 'chat',
    envelope: { cir_version: '2', source_provider: 'codex' }, total, inherited: 0, start: 0, from, to,
    units: Array.from({ length: (to - from) / 31 }, (_, position) => {
      const begin = from + position * 31;
      return { from: begin, to: begin + 31, turn_index: -1,
        event: visibleMessage(begin + 30, `Anchor ${begin + 30}\n\n` + 'Readable paragraph.\n\n'.repeat(3)),
        fold: { from: begin, to: begin + 30, rows: 30, cursor: `fold-${begin}` } };
    }), fold: null, events: [], next_cursor: to === total ? null : `main-${to}` };
}

export function clippedPromptPage(start = 10): DocVisiblePage {
  return { ...heavyVisiblePage(), view: 'prompts', inherited: 10, start, from: start, to: 33, total: 33, next_cursor: null,
    units: [
      { from: start, to: 30, turn_index: 0, event: null, fold: { from: start, to: 30, rows: 29 - start, cursor: 'clipped-fold' } },
      { from: 30, to: 33, turn_index: 30, event: visibleMessage(30, 'Actual prompt', 'user'), fold: { from: 31, to: 33, rows: 2, cursor: 'prompt-fold' } },
    ] };
}

export function foldPage(context: DocVisiblePage, unitIndex: number, from?: number, to?: number): DocVisiblePage {
  const fold = context.units[unitIndex].fold!;
  const begin = from ?? fold.from;
  const end = to ?? fold.to;
  return { ...context, kind: 'fold', from: begin, to: end, units: [], fold: { from: fold.from, to: fold.to },
    events: Array.from({ length: end - begin }, (_, position) => visibleTool(begin + position)),
    next_cursor: end === fold.to ? null : `fold-next-${end}` };
}

export function clippedFoldPage(context: DocVisiblePage, from: number, to: number): DocVisiblePage {
  const page = foldPage(context, 0, from, to);
  page.events = page.events.map(event => event.event_index === 11
    ? { ...event, kind: 'reasoning', empty_reasoning: true }
    : event.event_index === 10
      ? { ...visibleMessage(10, 'Non-prefix seed content', 'user'), seed_summary: true, deferred: true, blocks: undefined }
      : { ...visibleMessage(event.event_index, `Deferred answer ${event.event_index}`), deferred: true, blocks: undefined });
  return page;
}
