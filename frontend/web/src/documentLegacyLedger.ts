import { queryOptions, type QueryClient, type QueryKey } from '@tanstack/react-query';
import { api } from './api';
import { documentEventIsPrompt, documentEventIsRendered, documentEventIsSummary } from './documentEventClassification';
import { documentBodyKey, installDocumentBodyCache } from './documentPageCache';
import { DocumentProjectionError, validateLegacyDocument,
  type DocumentSelection, type LegacyDocumentCheckpoint, type LegacyDocumentPage } from './documentVisible';
import { documentReadRetry } from './readRetry';
import type { DocViewEvent } from './types';

export interface LegacyRow {
  readonly index: number;
  readonly kind: string;
  readonly role?: string;
  readonly prompt: boolean;
  readonly rendered: boolean;
  readonly message: boolean;
}

type LegacyHeader = Pick<LegacyDocumentPage['envelope'],
  'source_provider' | 'source_model' | 'source_models' | 'context_tokens' | 'output_tokens'>;
type LegacyEvent = DocViewEvent & { seed_summary?: boolean; empty_reasoning?: boolean };

export interface LegacyPageRecord extends LegacyDocumentCheckpoint {
  readonly offset: number;
  readonly inherited: number;
  readonly rows: readonly LegacyRow[];
  readonly header?: LegacyHeader;
  readonly key: QueryKey;
  readonly request: Readonly<{ selection: Readonly<DocumentSelection>; previous?: LegacyDocumentCheckpoint }>;
  readonly layout: string;
}

export function recordLegacyDocumentPage(client: QueryClient, owner: QueryKey, page: LegacyDocumentPage,
  selection: DocumentSelection, previous?: LegacyDocumentCheckpoint): LegacyPageRecord {
  const frozenSelection = copySelection(selection);
  const checkpoint = copyCheckpoint(previous);
  requireValid(owner.every(value => value == null || ['string', 'boolean', 'undefined'].includes(typeof value) ||
    typeof value === 'number' && Number.isFinite(value)));
  const { kind, ...value } = page;
  requireValid(kind === 'legacy');
  const validated = validateLegacyDocument(value, frozenSelection, checkpoint?.next ?? frozenSelection.start, checkpoint);
  const metadata = legacyMetadata(validated);
  const record: LegacyPageRecord = Object.freeze({ kind: 'legacy', offset: validated.offset, next: validated.next,
    total: validated.total, inherited: validated.inherited, rows: metadata.rows, layout: metadata.layout,
    ...(validated.doc_identity !== undefined ? { doc_identity: validated.doc_identity } : {}),
    key: Object.freeze(documentBodyKey(owner, 'legacy', validated.offset)),
    request: Object.freeze({ selection: frozenSelection, ...(checkpoint ? { previous: checkpoint } : {}) }),
    ...(!checkpoint ? { header: copyHeader(validated) } : {}) });
  installDocumentBodyCache(client);
  client.getQueryCache().build<LegacyDocumentPage, Error, LegacyDocumentPage, QueryKey>(client, legacyDocumentBodyOptions(record)).setData(validated);
  return record;
}

export function legacyDocumentBodyOptions(record: LegacyPageRecord) {
  return queryOptions({
    queryKey: record.key,
    queryFn: ({ signal }) => restoreLegacyDocumentPage(record, signal),
    staleTime: Infinity,
    gcTime: Infinity,
    structuralSharing: false,
    meta: { documentBody: true },
    ...documentReadRetry,
    enabled: query => query.state.status !== 'error',
    retryOnMount: false,
    refetchOnMount: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
}

export async function restoreLegacyDocumentPage(record: LegacyPageRecord, signal: AbortSignal): Promise<LegacyDocumentPage> {
  signal.throwIfAborted();
  const { selection, previous } = record.request;
  const value = await api.getDocViewEvents(selection.repoId, selection.hash, previous ? undefined : selection.base,
    record.offset, selection.mode, signal);
  signal.throwIfAborted();
  const page = validateLegacyDocument(value, selection, record.offset, previous);
  const metadata = legacyMetadata(page);
  requireValid(page.offset === record.offset && page.next === record.next && page.total === record.total &&
    page.inherited === record.inherited && page.doc_identity === record.doc_identity && metadata.layout === record.layout);
  return page;
}

function requireValid(condition: unknown): asserts condition {
  if (!condition) throw new DocumentProjectionError();
}

function object(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function ordinal(value: unknown): value is number {
  return Number.isSafeInteger(value) && (value as number) >= 0;
}

function copySelection(selection: DocumentSelection): Readonly<DocumentSelection> {
  requireValid(typeof selection.repoId === 'string' && selection.repoId.length > 0 &&
    typeof selection.hash === 'string' && selection.hash.length > 0 &&
    (selection.base === undefined || typeof selection.base === 'string') &&
    Number.isSafeInteger(selection.start) && selection.start >= -1 && ['all', 'chat', 'prompts'].includes(selection.mode));
  return Object.freeze({ repoId: selection.repoId, hash: selection.hash, start: selection.start, mode: selection.mode,
    ...(selection.base !== undefined ? { base: selection.base } : {}) });
}

function copyCheckpoint(previous?: LegacyDocumentCheckpoint): LegacyDocumentCheckpoint | undefined {
  if (!previous) return undefined;
  requireValid(previous.kind === 'legacy' && ordinal(previous.next) && ordinal(previous.total) && previous.next < previous.total);
  return Object.freeze({ kind: 'legacy', next: previous.next, total: previous.total,
    ...(previous.doc_identity !== undefined ? { doc_identity: previous.doc_identity } : {}) });
}

function copyHeader(page: LegacyDocumentPage): LegacyHeader {
  const envelope = page.envelope;
  const source_models = envelope.source_models?.slice();
  if (source_models) Object.freeze(source_models);
  return Object.freeze({ source_provider: envelope.source_provider ?? undefined, source_model: envelope.source_model ?? undefined,
    source_models, context_tokens: envelope.context_tokens ?? undefined, output_tokens: envelope.output_tokens ?? undefined });
}

function validateEvent(value: unknown): asserts value is LegacyEvent {
  requireValid(object(value) && typeof value.kind === 'string' && value.kind.length > 0);
  for (const field of ['role', 'tool_name', 'provider_tool_name', 'redacted_summary', 'agent_author', 'agent_recipient', 'id', 'ts', 'call_id']) {
    requireValid(value[field] === undefined || typeof value[field] === 'string');
  }
  for (const field of ['seed_summary', 'empty_reasoning', 'compact_summary', 'deferred', 'agent_message', 'replacement_complete']) {
    requireValid(value[field] === undefined || typeof value[field] === 'boolean');
  }
  requireValid(value.event_bytes === undefined || ordinal(value.event_bytes));
  requireValid(value.input === undefined || object(value.input));
  requireValid(value.blocks === undefined || Array.isArray(value.blocks) && value.blocks.every(block =>
    object(block) && typeof block.type === 'string' && (block.text === undefined || typeof block.text === 'string') &&
    (block.name === undefined || typeof block.name === 'string')));
}

function legacyMetadata(page: LegacyDocumentPage) {
  const rows: LegacyRow[] = [];
  const layout: unknown[][] = [];
  page.events.forEach((event, position) => {
    validateEvent(event);
    const index = page.offset + position;
    requireValid(event.event_index === undefined || event.event_index === index);
    const row = Object.freeze({ index, kind: event.kind, ...(event.role !== undefined ? { role: event.role } : {}),
      prompt: documentEventIsPrompt(event), rendered: documentEventIsRendered(event), message: event.kind === 'message' });
    rows.push(row);
    layout.push([row.index, row.kind, row.role ?? null, row.prompt, row.rendered, row.message,
      documentEventIsSummary(event), documentEventIsSummary({ kind: event.kind, seed_summary: event.seed_summary, blocks: event.blocks }),
      event.seed_summary ?? null, event.empty_reasoning ?? false, event.compact_summary ?? false,
      event.deferred ?? false, event.event_bytes ?? null]);
  });
  return { rows: Object.freeze(rows), layout: JSON.stringify(layout) };
}
