import { DocumentProjectionError, type DocumentSelection, type DocVisibleContext } from './documentVisible';

const DOCUMENT_BYTES = 512 * 1024 * 1024;
const PREPARED_PAGES = Math.ceil(DOCUMENT_BYTES / 128);
const FIND_DISPLAY_BYTES = 512 * 1024;
const FIND_FIELDS = 128 * 1024;
const FIND_MATCHES = 20;
const FIND_QUERY_RUNES = 256;
const FIND_PREVIEW_CONTEXT_RUNES = 32;
const FIND_PREVIEW_RUNES = FIND_QUERY_RUNES + 2 * FIND_PREVIEW_CONTEXT_RUNES;
const FIND_CURSOR_CHARACTERS = 5462;
const LEGACY_FIND_CURSOR_CHARACTERS = 2731;
const VISIBLE_CURSOR_CHARACTERS = 2731;

export interface DocFindPreparedProgress {
  page: number;
  generation: string;
}

export interface DocFindMatch {
  prepared_page?: number;
  event_index: number;
  field: number;
  byte_offset: number;
  byte_length: number;
  raw: boolean;
  preview: string;
  preview_byte_offset: number;
  reveal_cursor: string;
  source_range?: { from: number; to: number };
}

export interface DocFindPage {
  projection: 'find-v1' | 'find-legacy-v1' | 'find-display-v1' | 'find-legacy-display-v1' |
    'find-display-prepared-v1' | 'find-legacy-display-prepared-v1';
  prepared?: DocFindPreparedProgress;
  hash: string;
  generation: string;
  from: number;
  to: number;
  event_index: number;
  field: number;
  byte_offset: number;
  matches: DocFindMatch[];
  next_cursor: string | null;
}

type DocumentFindSourceRange = {
  id: string;
  representation?: 'display-v1' | 'display-prepared-v1';
  selection: DocumentSelection;
  from: number;
  to: number;
};

export type RootDocumentFindSource = DocumentFindSourceRange & {
  protocol?: 'visible-v1';
  context: DocVisibleContext;
  replay: string;
};

export type LegacyDocumentFindSource = DocumentFindSourceRange & {
  protocol: 'legacy';
  context: Pick<DocVisibleContext, 'hash' | 'base' | 'view' | 'start' | 'total' | 'inherited'>;
};

export type DocumentFindSource = RootDocumentFindSource | LegacyDocumentFindSource;

export type DocumentFindPosition = {
  source: number;
  cursor?: string;
  frontier?: { index: number; field: number; byte: number; prepared?: DocFindPreparedProgress };
};

type FindFrontier = NonNullable<DocumentFindPosition['frontier']>;
type FindCoordinates = Pick<FindFrontier, 'index' | 'field' | 'byte'> & { prepared?: Pick<DocFindPreparedProgress, 'page'> };

const PAGE_KEYS = ['projection', 'hash', 'generation', 'from', 'to', 'event_index', 'field', 'byte_offset', 'matches', 'next_cursor'];
const MATCH_KEYS = ['event_index', 'field', 'byte_offset', 'byte_length', 'raw', 'preview', 'preview_byte_offset', 'reveal_cursor'];

function requireValid(condition: unknown): asserts condition {
  if (!condition) throw new DocumentProjectionError();
}

function record(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function exactRecord(value: unknown, keys: readonly string[]): value is Record<string, unknown> {
  return record(value) && Object.keys(value).length === keys.length && keys.every(key => Object.hasOwn(value, key));
}

function ordinal(value: unknown, maximum: number): value is number {
  return Number.isSafeInteger(value) && (value as number) >= 0 && (value as number) <= maximum;
}

function token(value: unknown, maximum: number): value is string {
  return typeof value === 'string' && value.length > 0 && value.length <= maximum && /^[A-Za-z0-9_-]+$/.test(value);
}

function utf8Boundaries(value: unknown, maximumRunes: number): number[] {
  requireValid(typeof value === 'string' && value.length <= maximumRunes * 2);
  const boundaries = [0];
  let bytes = 0;
  for (const rune of value) {
    const code = rune.codePointAt(0)!;
    requireValid((code < 0xd800 || code > 0xdfff) && boundaries.length <= maximumRunes);
    bytes += code < 0x80 ? 1 : code < 0x800 ? 2 : code < 0x10000 ? 3 : 4;
    boundaries.push(bytes);
  }
  return boundaries;
}

function validateSource(source: DocumentFindSource) {
  requireValid(record(source) && record(source.selection) && record(source.context));
  requireValid(source.protocol === undefined || source.protocol === 'visible-v1' || source.protocol === 'legacy');
  requireValid(source.representation === undefined || source.representation === 'display-v1' || source.representation === 'display-prepared-v1');
  const { selection, context } = source;
  requireValid(typeof selection.hash === 'string' && selection.hash.length > 0 && context.hash === selection.hash &&
    typeof context.base === 'string' && context.base === (selection.base ?? '') &&
    context.view === selection.mode && ['all', 'chat', 'prompts'].includes(context.view));
  if (source.protocol === 'legacy') {
    requireValid(!('generation' in context) && !('rules' in context) && !('replay' in source));
  } else {
    requireValid(typeof source.context.generation === 'string' && source.context.generation.length > 0 && source.context.rules === 1 &&
      token(source.replay, VISIBLE_CURSOR_CHARACTERS));
  }
  requireValid(ordinal(context.total, DOCUMENT_BYTES) && ordinal(context.start, context.total) && ordinal(context.inherited, context.total) &&
    (context.base !== '' || context.inherited === 0) && Number.isSafeInteger(selection.start) && selection.start >= -1 &&
    context.start === (selection.start === -1 ? context.inherited : selection.start));
  requireValid(ordinal(source.from, context.total) && ordinal(source.to, context.total) && source.from >= context.start && source.to >= source.from);
}

function findCursorCharacters(source: DocumentFindSource): number {
  return source.protocol === 'legacy' ? LEGACY_FIND_CURSOR_CHARACTERS : FIND_CURSOR_CHARACTERS;
}

function validateFrontier(frontier: FindFrontier, source: DocumentFindSource) {
  const display = source.representation !== undefined;
  const prepared = source.representation === 'display-prepared-v1';
  requireValid(record(frontier) && ordinal(frontier.index, source.to) && frontier.index >= source.from &&
    ordinal(frontier.field, display ? 0 : FIND_FIELDS) && ordinal(frontier.byte, display && !prepared ? FIND_DISPLAY_BYTES : DOCUMENT_BYTES) &&
    (frontier.index !== source.to || frontier.field === 0 && frontier.byte === 0));
  if (prepared) {
    const progress = frontier.prepared;
    requireValid(exactRecord(frontier, ['index', 'field', 'byte', 'prepared']) && exactRecord(progress, ['page', 'generation']) &&
      ordinal(progress.page, PREPARED_PAGES - 1) && typeof progress.generation === 'string' &&
      (progress.generation === '' || progress.generation.length === 71 && /^sha256:[0-9a-f]{64}$/.test(progress.generation)) &&
      (progress.page === 0 || progress.generation !== '') &&
      (progress.generation !== '' || frontier.byte <= FIND_DISPLAY_BYTES) &&
      (frontier.index !== source.to || progress.page === 0 && progress.generation === ''));
  } else {
    requireValid(!('prepared' in frontier));
  }
}

function compareFrontiers(left: FindCoordinates, right: FindCoordinates): number {
  return left.index - right.index || (left.prepared?.page ?? 0) - (right.prepared?.page ?? 0) ||
    left.field - right.field || left.byte - right.byte;
}

export function documentFindParameters(query: string, source: DocumentFindSource, position?: DocumentFindPosition): URLSearchParams {
  requireValid(utf8Boundaries(query, FIND_QUERY_RUNES).length > 1);
  validateSource(source);
  if (position) {
    requireValid(ordinal(position.source, Number.MAX_SAFE_INTEGER));
    if (position.frontier !== undefined) validateFrontier(position.frontier, source);
    if (position.cursor !== undefined) {
      requireValid(token(position.cursor, findCursorCharacters(source)));
    }
  }
  let parameters: URLSearchParams;
  if (position?.cursor !== undefined) {
    parameters = new URLSearchParams(source.protocol === 'legacy' ? { q: query, legacy: '1', cursor: position.cursor } : { q: query, cursor: position.cursor });
  } else if (source.protocol === 'legacy') {
    parameters = new URLSearchParams({ q: query, legacy: '1', from: String(source.from), to: String(source.to),
      total: String(source.context.total), inherited: String(source.context.inherited) });
    if (source.context.base !== '') parameters.set('base', source.context.base);
  } else {
    parameters = new URLSearchParams({ q: query, selection: source.replay, from: String(source.from), to: String(source.to) });
  }
  if (source.representation !== undefined) parameters.set('representation', source.representation);
  return parameters;
}

function validateMatch(value: unknown, source: DocumentFindSource): DocFindMatch {
  const display = source.representation !== undefined;
  const prepared = source.representation === 'display-prepared-v1';
  const maximumBytes = display && !prepared ? FIND_DISPLAY_BYTES : DOCUMENT_BYTES;
  requireValid(record(value) && exactRecord(value, prepared ? [...MATCH_KEYS, 'prepared_page', 'source_range'] :
    Object.hasOwn(value, 'source_range') ? [...MATCH_KEYS, 'source_range'] : MATCH_KEYS) &&
    ordinal(value.event_index, source.to) && value.event_index >= source.from && value.event_index < source.to &&
    ordinal(value.field, display ? 0 : FIND_FIELDS) && ordinal(value.byte_offset, maximumBytes) && ordinal(value.byte_length, FIND_QUERY_RUNES * 4) &&
    value.byte_length > 0 && value.byte_offset <= maximumBytes - value.byte_length &&
    typeof value.raw === 'boolean' && ordinal(value.preview_byte_offset, value.byte_offset) &&
    (source.protocol === 'legacy' ? value.reveal_cursor === '' : token(value.reveal_cursor, VISIBLE_CURSOR_CHARACTERS)));
  if (display) requireValid(value.raw === false && Object.hasOwn(value, 'source_range'));
  if (prepared) requireValid(ordinal(value.prepared_page, PREPARED_PAGES - 1));
  if (Object.hasOwn(value, 'source_range')) {
    const range = value.source_range;
    requireValid(exactRecord(range, ['from', 'to']) && ordinal(range.from, DOCUMENT_BYTES) && ordinal(range.to, DOCUMENT_BYTES) && range.from < range.to);
    if (value.raw) requireValid(range.from === value.byte_offset && range.to === value.byte_offset + value.byte_length);
  }
  const boundaries = utf8Boundaries(value.preview, FIND_PREVIEW_RUNES);
  requireValid(value.preview_byte_offset <= maximumBytes - boundaries.at(-1)!);
  const begin = boundaries.indexOf(value.byte_offset - value.preview_byte_offset);
  const end = boundaries.indexOf(value.byte_offset - value.preview_byte_offset + value.byte_length);
  requireValid(begin >= 0 && end > begin && end - begin <= FIND_QUERY_RUNES &&
    begin <= FIND_PREVIEW_CONTEXT_RUNES && boundaries.length - 1 - end <= FIND_PREVIEW_CONTEXT_RUNES);
  return value as unknown as DocFindMatch;
}

export interface DocFindFragmentPage {
  projection: 'find-fragment-v1' | 'find-legacy-fragment-v1';
  hash: string;
  generation: string;
  event_index: number;
  byte_offset: number;
  total_bytes: number;
  json_fragment: string;
  next: number;
  previous: number;
}

export function documentFindFragmentParameters(source: DocumentFindSource, match: DocFindMatch, offset: number, budget: number): URLSearchParams {
  validateSource(source);
  validateMatch(match, source);
  requireValid(ordinal(offset, DOCUMENT_BYTES - 1) && ordinal(budget, 64 * 1024) && budget >= 4);
  return new URLSearchParams(source.protocol === 'legacy'
    ? { legacy: '1', index: String(match.event_index), total: String(source.context.total), offset: String(offset), budget: String(budget) }
    : { selection: match.reveal_cursor, offset: String(offset), budget: String(budget) });
}

export function validateDocumentFindFragment(value: unknown, source: DocumentFindSource, match: DocFindMatch, offset: number, budget: number): DocFindFragmentPage {
  documentFindFragmentParameters(source, match, offset, budget);
  requireValid(exactRecord(value, ['projection', 'hash', 'generation', 'event_index', 'byte_offset', 'total_bytes', 'json_fragment', 'next', 'previous']) &&
    value.projection === (source.protocol === 'legacy' ? 'find-legacy-fragment-v1' : 'find-fragment-v1') &&
    value.hash === source.context.hash && value.generation === (source.protocol === 'legacy' ? '' : source.context.generation) &&
    value.event_index === match.event_index && value.byte_offset === offset && ordinal(value.total_bytes, DOCUMENT_BYTES) &&
    value.total_bytes > offset && typeof value.json_fragment === 'string' && value.json_fragment.length <= budget);
  const boundaries = utf8Boundaries(value.json_fragment, budget);
  const length = boundaries.at(-1)!;
  requireValid(length > 0 && length <= budget && length <= value.total_bytes - offset &&
    value.next === (offset + length === value.total_bytes ? -1 : offset + length) &&
    (offset === 0 ? value.previous === -1 : ordinal(value.previous, offset - 1) && value.previous >= Math.max(0, offset - budget - 3)) &&
    (!match.source_range || match.source_range.to <= value.total_bytes));
  if (match.source_range) {
    const from = Math.max(0, match.source_range.from - offset);
    const to = Math.min(length, match.source_range.to - offset);
    if (from < to) requireValid(boundaries.includes(from) && boundaries.includes(to));
  }
  return value as unknown as DocFindFragmentPage;
}

export function validateDocumentFindPage(value: unknown, source: DocumentFindSource, previous?: FindFrontier): DocFindPage {
  validateSource(source);
  const prepared = source.representation === 'display-prepared-v1';
  const projection = source.protocol === 'legacy'
    ? (prepared ? 'find-legacy-display-prepared-v1' : source.representation === 'display-v1' ? 'find-legacy-display-v1' : 'find-legacy-v1')
    : (prepared ? 'find-display-prepared-v1' : source.representation === 'display-v1' ? 'find-display-v1' : 'find-v1');
  requireValid(exactRecord(value, prepared ? [...PAGE_KEYS, 'prepared'] : PAGE_KEYS) && value.projection === projection &&
    value.hash === source.context.hash && value.generation === (source.protocol === 'legacy' ? '' : source.context.generation) &&
    value.from === source.from && value.to === source.to &&
    Array.isArray(value.matches) && value.matches.length <= FIND_MATCHES);
  const frontier = { index: value.event_index, field: value.field, byte: value.byte_offset,
    ...(prepared ? { prepared: value.prepared } : {}) } as FindFrontier;
  validateFrontier(frontier, source);
  const start = previous ?? { index: source.from, field: 0, byte: 0,
    ...(prepared ? { prepared: { page: 0, generation: '' } } : {}) };
  validateFrontier(start, source);
  if (prepared && start.index === frontier.index && start.prepared!.generation !== '') {
    requireValid(frontier.prepared!.generation === start.prepared!.generation);
  }
  const progress = compareFrontiers(frontier, start);
  requireValid(progress >= 0 && (frontier.index === source.to ? value.next_cursor === null :
    progress > 0 && token(value.next_cursor, findCursorCharacters(source))));
  let last: FindCoordinates | undefined;
  for (const valueMatch of value.matches) {
    const match = validateMatch(valueMatch, source);
    const position = { index: match.event_index, field: match.field, byte: match.byte_offset,
      ...(prepared ? { prepared: { page: match.prepared_page! } } : {}) };
    requireValid(compareFrontiers(position, start) >= 0 && compareFrontiers(position, frontier) < 0 &&
      (!last || compareFrontiers(position, last) > 0));
    last = position;
  }
  return value as unknown as DocFindPage;
}
