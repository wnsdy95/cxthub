import type { DocViewEventPage } from './api';
import type { DocViewEvent, DocViewMode } from './types';
import { DOCUMENT_VISIBLE_MESSAGES, validateDocViewPage } from './documentPaging';
import { containsCursor } from './documentCursorHistory';

const DOCUMENT_FOLD_EVENTS = 128;

export interface DocumentSelection {
  repoId: string;
  hash: string;
  base?: string;
  start: number;
  mode: DocViewMode;
}

export interface DocVisibleEvent extends DocViewEvent {
  event_index: number;
  seed_summary: boolean;
  empty_reasoning: boolean;
}

export interface DocVisibleFold {
  from: number;
  to: number;
  rows: number;
  cursor: string;
}

export interface DocVisibleUnit {
  from: number;
  to: number;
  turn_index: number;
  event?: DocVisibleEvent | null;
  fold?: DocVisibleFold | null;
}

export interface DocVisiblePage {
  projection: 'visible-v1';
  kind: 'units' | 'fold';
  hash: string;
  base: string;
  generation: string;
  rules: number;
  view: DocViewMode;
  envelope: DocViewEventPage['envelope'];
  total: number;
  inherited: number;
  start: number;
  from: number;
  to: number;
  units: DocVisibleUnit[];
  fold?: Pick<DocVisibleFold, 'from' | 'to'> | null;
  events: DocVisibleEvent[];
  next_cursor: string | null;
  replay_cursor?: string;
}

export type LegacyDocumentPage = DocViewEventPage & { kind: 'legacy' };
export type DocumentStreamPage = DocVisiblePage | LegacyDocumentPage;

export type DocVisibleContext = Readonly<Pick<DocVisiblePage,
  'hash' | 'base' | 'generation' | 'rules' | 'view' | 'start' | 'total' | 'inherited'>>;

export interface DocVisibleCheckpoint extends DocVisibleContext {
  readonly kind: DocVisiblePage['kind'];
  readonly to: number;
  readonly next_cursor: string | null;
  readonly lastTurnIndex: number;
  readonly fold: Readonly<Pick<DocVisibleFold, 'from' | 'to'>> | null;
}

export type LegacyDocumentCheckpoint = Readonly<Pick<LegacyDocumentPage, 'kind' | 'next' | 'total' | 'doc_identity'>>;
export type DocumentStreamCheckpoint = DocVisibleCheckpoint | LegacyDocumentCheckpoint;

export function visibleContextWitness(page: DocVisibleContext): DocVisibleContext {
  return { hash: page.hash, base: page.base, generation: page.generation, rules: page.rules,
    view: page.view, start: page.start, total: page.total, inherited: page.inherited };
}

export function visiblePageCheckpoint(page: DocVisiblePage | DocVisibleCheckpoint): DocVisibleCheckpoint {
  return { ...visibleContextWitness(page), kind: page.kind, to: page.to, next_cursor: page.next_cursor,
    lastTurnIndex: 'units' in page ? page.units.at(-1)?.turn_index ?? -1 : page.lastTurnIndex,
    fold: page.fold ? { from: page.fold.from, to: page.fold.to } : null };
}

export function documentStreamCheckpoint(page: DocumentStreamPage | DocumentStreamCheckpoint): DocumentStreamCheckpoint {
  return page.kind === 'legacy' ? { kind: page.kind, next: page.next, total: page.total,
    ...(page.doc_identity !== undefined ? { doc_identity: page.doc_identity } : {}) } : visiblePageCheckpoint(page);
}

export class DocumentProjectionError extends Error {
  constructor() { super('Invalid or changed document projection. Restart the document to read it again.'); }
}

function requireValid(condition: unknown): asserts condition {
  if (!condition) throw new DocumentProjectionError();
}

function record(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function ordinal(value: unknown): value is number {
  return Number.isSafeInteger(value) && (value as number) >= 0;
}

function selector(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0 && value.length <= 2731;
}

export function documentVisibleKey(selection: DocumentSelection, restart = '') {
  return ['doc-visible', selection.repoId, selection.hash, selection.base ?? '', selection.start, selection.mode, restart] as const;
}

export function visibleContext(page: DocVisibleContext) {
  return [page.hash, page.base, page.generation, page.rules, page.view, page.start, page.total, page.inherited] as const;
}

export function visibleParameters(selection: DocumentSelection, continuation?: { cursor: string; kind: 'units' | 'fold' }) {
  const params = new URLSearchParams({ projection: 'visible-v1' });
  if (continuation) params.set(continuation.kind === 'fold' ? 'expand' : 'cursor', continuation.cursor);
  else {
    params.set('view', selection.mode);
    params.set('offset', String(selection.start));
    params.set('limit', String(DOCUMENT_VISIBLE_MESSAGES));
    if (selection.base) params.set('base', selection.base);
  }
  return params;
}

function validateEvent(value: unknown): asserts value is DocVisibleEvent {
  requireValid(record(value) && typeof value.kind === 'string' && value.kind.length > 0 && ordinal(value.event_index) &&
    typeof value.seed_summary === 'boolean' && typeof value.empty_reasoning === 'boolean');
  requireValid(value.deferred === undefined || typeof value.deferred === 'boolean');
  requireValid(value.compact_summary === undefined || typeof value.compact_summary === 'boolean');
  requireValid(value.event_bytes === undefined || ordinal(value.event_bytes));
  requireValid(!value.seed_summary || value.kind === 'message');
  requireValid(!value.empty_reasoning || value.kind === 'reasoning');
  for (const key of ['role', 'tool_name', 'provider_tool_name', 'redacted_summary', 'agent_author']) {
    requireValid(value[key] === undefined || typeof value[key] === 'string');
  }
  requireValid(value.input === undefined || record(value.input));
  requireValid(value.blocks === undefined || Array.isArray(value.blocks) && value.blocks.every(block =>
    record(block) && typeof block.type === 'string' && (block.text === undefined || typeof block.text === 'string') &&
    (block.name === undefined || typeof block.name === 'string')));
}

function validateEnvelopeHeader(envelope: Record<string, unknown>) {
  for (const field of ['source_provider', 'source_model']) requireValid(envelope[field] == null || typeof envelope[field] === 'string');
  requireValid(envelope.source_models == null || Array.isArray(envelope.source_models) && envelope.source_models.every(model => typeof model === 'string'));
  for (const field of ['context_tokens', 'output_tokens']) requireValid(envelope[field] == null || ordinal(envelope[field]));
}

function validatePage(value: unknown, selection: DocumentSelection, expected?: DocVisibleContext): DocVisiblePage {
  requireValid(record(value) && value.projection === 'visible-v1' && (value.kind === 'units' || value.kind === 'fold'));
  requireValid(value.hash === selection.hash && value.base === (selection.base ?? '') && value.view === selection.mode &&
    typeof value.generation === 'string' && value.generation.length > 0 && value.rules === 1 && record(value.envelope));
  validateEnvelopeHeader(value.envelope);
  requireValid(ordinal(value.total) && ordinal(value.inherited) && ordinal(value.start) && ordinal(value.from) && ordinal(value.to));
  requireValid(value.inherited <= value.total && value.start <= value.from && value.from <= value.to && value.to <= value.total &&
    (value.base !== '' || value.inherited === 0));
  requireValid(Array.isArray(value.units) && Array.isArray(value.events) && (value.next_cursor === null || selector(value.next_cursor)));
  requireValid(value.replay_cursor === undefined || selector(value.replay_cursor) && value.replay_cursor !== value.next_cursor);
  const page = value as unknown as DocVisiblePage;
  if (expected) requireValid(visibleContext(page).every((field, index) => field === visibleContext(expected)[index]));
  else requireValid(page.start === (selection.start < 0 ? page.inherited : selection.start));
  return page;
}

export function validateVisibleUnits(value: unknown, selection: DocumentSelection, previous?: DocVisiblePage | DocVisibleCheckpoint, usedCursors: Iterable<string> = []): DocVisiblePage {
  const checkpoint = previous ? visiblePageCheckpoint(previous) : undefined;
  const page = validatePage(value, selection, checkpoint);
  requireValid(page.kind === 'units' && page.events.length === 0 && page.fold == null && page.units.length <= DOCUMENT_VISIBLE_MESSAGES);
  requireValid(!checkpoint || checkpoint.kind === 'units' && checkpoint.next_cursor !== null);
  requireValid(page.from === (checkpoint?.to ?? page.start));
  requireValid(page.next_cursor === null ? page.to === page.total : page.to < page.total && !containsCursor(usedCursors, page.next_cursor));
  requireValid(page.to > page.from || page.from === page.total && page.units.length === 0);
  let next = page.from;
  let turn = checkpoint?.lastTurnIndex ?? -1;
  for (const unit of page.units) {
    requireValid(record(unit) && ordinal(unit.from) && ordinal(unit.to) && unit.from === next && unit.to > unit.from && unit.to <= page.to &&
      Number.isSafeInteger(unit.turn_index) && unit.turn_index >= turn && unit.turn_index <= unit.from);
    const anchor = unit.event;
    if (anchor != null) {
      validateEvent(anchor);
      requireValid(!anchor.empty_reasoning && anchor.event_index >= unit.from && anchor.event_index < unit.to);
      if (page.view === 'chat') requireValid(anchor.kind === 'message' && anchor.event_index === unit.to - 1);
      if (page.view === 'prompts') requireValid(anchor.kind === 'message' && anchor.role === 'user' && !anchor.seed_summary && !anchor.compact_summary &&
        anchor.event_index === unit.from && unit.turn_index === unit.from);
    }
    requireValid(unit.turn_index !== unit.from || anchor?.event_index === unit.from);
    if (page.view === 'all') requireValid(unit.to === unit.from + 1 && (anchor != null || unit.fold?.rows === 0));
    const foldFrom = unit.from + (anchor?.event_index === unit.from ? 1 : 0);
    const foldTo = unit.to - (anchor && anchor.event_index !== unit.from && anchor.event_index === unit.to - 1 ? 1 : 0);
    requireValid((unit.fold != null) === (foldFrom < foldTo));
    if (unit.fold != null) {
      const fold = unit.fold;
      requireValid(record(fold) && ordinal(fold.from) && ordinal(fold.to) && ordinal(fold.rows) && selector(fold.cursor));
      requireValid(fold.from === foldFrom && fold.to === foldTo && fold.to > fold.from && fold.rows <= fold.to - fold.from);
      requireValid(!anchor || anchor.event_index < fold.from || anchor.event_index >= fold.to);
    }
    next = unit.to;
    turn = unit.turn_index;
  }
  requireValid(next === page.to);
  return page;
}

export function validateVisibleFold(value: unknown, selection: DocumentSelection, context: DocVisibleContext, fold: DocVisibleFold,
  previous?: DocVisiblePage | DocVisibleCheckpoint, renderedBefore = 0, usedCursors: Iterable<string> = []): DocVisiblePage {
  const checkpoint = previous ? visiblePageCheckpoint(previous) : undefined;
  const page = validatePage(value, selection, context);
  requireValid(page.kind === 'fold' && page.units.length === 0 && page.events.length <= DOCUMENT_FOLD_EVENTS &&
    record(page.fold) && page.fold.from === fold.from && page.fold.to === fold.to);
  requireValid(!checkpoint || checkpoint.kind === 'fold' && checkpoint.next_cursor !== null && checkpoint.fold?.from === fold.from && checkpoint.fold.to === fold.to &&
    visibleContext(checkpoint).every((field, index) => field === visibleContext(context)[index]));
  requireValid(page.from === (checkpoint?.to ?? fold.from) && page.to > page.from && page.to <= fold.to && page.events.length === page.to - page.from);
  requireValid(page.next_cursor === null ? page.to === fold.to : page.to < fold.to && !containsCursor(usedCursors, page.next_cursor));
  let rendered = renderedBefore;
  page.events.forEach((event, index) => {
    validateEvent(event);
    requireValid(event.event_index === page.from + index);
    if (!event.empty_reasoning) rendered++;
  });
  requireValid(rendered <= fold.rows && (page.next_cursor !== null || rendered === fold.rows));
  return page;
}

export function validateLegacyDocument(value: unknown, selection: DocumentSelection, requested: number, previous?: LegacyDocumentCheckpoint): LegacyDocumentPage {
  requireValid(record(value) && !('projection' in value) && !('kind' in value) && !('units' in value) &&
    record(value.envelope) && Array.isArray(value.events) && value.events.every(event => record(event) && typeof event.kind === 'string'));
  validateEnvelopeHeader(value.envelope);
  requireValid(!('doc_identity' in value) || value.doc_identity === '' || value.doc_identity === 'cxt-manifest-sha256-v1');
  const page = validateDocViewPage(value as unknown as DocViewEventPage, selection.hash, requested);
  requireValid(!previous || previous.next === page.offset && previous.total === page.total && previous.doc_identity === page.doc_identity);
  return { ...page, kind: 'legacy' };
}
