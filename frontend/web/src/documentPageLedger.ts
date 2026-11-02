import { queryOptions, type QueryClient, type QueryKey } from '@tanstack/react-query';
import { historyCursors, type CursorHistory } from './documentCursorHistory';
import { documentBodyKey, installDocumentBodyCache } from './documentPageCache';
import { documentReadRetry } from './readRetry';
import { api } from './api';
import { DocumentProjectionError, documentStreamCheckpoint, validateVisibleUnits, validateVisibleFold, visibleContext, visibleContextWitness, visiblePageCheckpoint, visibleParameters,
  type DocumentSelection, type DocumentStreamCheckpoint, type DocVisibleCheckpoint, type DocVisibleContext, type DocVisibleFold, type DocVisiblePage } from './documentVisible';
import type { LegacyPageRecord } from './documentLegacyLedger';
import { readDocumentFold, readDocumentStream } from './documentVisibleReads';

export type StreamPosition = { previous?: DocumentStreamCheckpoint; history?: CursorHistory };
export type FoldPosition = { previous?: DocVisibleCheckpoint; renderedBefore: number; history?: CursorHistory };

type PageRequest = { kind: 'units'; selection: DocumentSelection; position: StreamPosition } |
  { kind: 'fold'; selection: DocumentSelection; context: DocVisibleContext; fold: DocVisibleFold; position: FoldPosition };

type DocumentHeader = Pick<DocVisiblePage['envelope'], 'source_provider' | 'source_model' | 'source_models' | 'context_tokens' | 'output_tokens'>;

export interface DocumentPageRecord extends DocVisibleCheckpoint {
  readonly from: number;
  readonly renderedRows: number;
  readonly hasContent: boolean;
  readonly key: QueryKey;
  readonly request: PageRequest;
  readonly replay?: string;
  readonly header?: DocumentHeader;
  readonly layout: string;
}

export type DocumentStreamRecord = DocumentPageRecord | LegacyPageRecord;

export function recordDocumentPage(client: QueryClient, owner: QueryKey, page: DocVisiblePage, request: PageRequest): DocumentPageRecord {
  installDocumentBodyCache(client);
  const checkpoint = visiblePageCheckpoint(page);
  const selection = { ...request.selection };
  const position: StreamPosition = { ...request.position, previous: request.position.previous ? documentStreamCheckpoint(request.position.previous) : undefined };
  const frozenRequest: PageRequest = request.kind === 'units'
    ? { kind: 'units', selection, position }
    : { kind: 'fold', selection, context: visibleContextWitness(request.context),
      fold: { from: request.fold.from, to: request.fold.to, rows: request.fold.rows, cursor: request.fold.cursor },
      position: { ...request.position, previous: request.position.previous ? visiblePageCheckpoint(request.position.previous) : undefined } };
  const first = request.kind === 'units' && !request.position.previous;
  const { source_provider, source_model, source_models, context_tokens, output_tokens } = page.envelope;
  const record: DocumentPageRecord = { ...checkpoint, from: page.from,
    renderedRows: page.events.filter(event => !event.empty_reasoning).length,
    hasContent: page.units.some(unit => unit.event || (unit.fold?.rows ?? 0) > 0),
    key: documentBodyKey(owner, page.kind, ...visibleContext(page), page.from, page.to, page.fold?.from ?? null, page.fold?.to ?? null),
    request: frozenRequest, replay: page.replay_cursor, layout: visiblePageLayout(page),
    ...(first ? { header: { source_provider, source_model, source_models: source_models?.slice(), context_tokens, output_tokens } } : {}) };
  client.getQueryCache().build<DocVisiblePage, Error, DocVisiblePage, QueryKey>(client, documentPageBodyOptions(record)).setData(page);
  return record;
}

export function documentPageBodyOptions(record: DocumentPageRecord) {
  return queryOptions({
    queryKey: record.key,
    queryFn: ({ signal }) => restoreDocumentPage(record, signal),
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

export async function restoreDocumentPage(record: DocumentPageRecord, signal: AbortSignal): Promise<DocVisiblePage> {
  const request = record.request;
  const page = record.replay ? await replayDocumentPage(record, signal) : request.kind === 'units'
    ? request.position.previous
      ? await readDocumentStream(request.selection, signal, request.position.previous, historyCursors(request.position.history))
      : validateVisibleUnits(await api.getDocVisibleEvents(request.selection.repoId, request.selection.hash,
        visibleParameters(request.selection), signal), request.selection)
    : await readDocumentFold(request.selection, signal, request.context, request.fold, request.position.previous,
      request.position.renderedBefore, historyCursors(request.position.history));
  if (page.kind === 'legacy' || page.from !== record.from ||
    record.replay !== undefined && page.replay_cursor !== record.replay ||
    visiblePageLayout(page) !== record.layout ||
    JSON.stringify(visiblePageCheckpoint(page)) !== JSON.stringify(visiblePageCheckpoint(record)) ||
    page.events.filter(event => !event.empty_reasoning).length !== record.renderedRows ||
    page.units.some(unit => unit.event || (unit.fold?.rows ?? 0) > 0) !== record.hasContent) throw new DocumentProjectionError();
  return page;
}

async function replayDocumentPage(record: DocumentPageRecord, signal: AbortSignal): Promise<DocVisiblePage> {
  const request = record.request;
  const value = await api.getDocVisibleEvents(request.selection.repoId, request.selection.hash,
    visibleParameters(request.selection, { kind: request.kind, cursor: record.replay! }), signal);
  const history = historyCursors(request.position.history);
  if (request.kind === 'units') {
    const previous = request.position.previous;
    if (previous?.kind === 'legacy') throw new DocumentProjectionError();
    return validateVisibleUnits(value, request.selection, previous, history);
  }
  return validateVisibleFold(value, request.selection, request.context, request.fold, request.position.previous,
    request.position.renderedBefore, replayHistory(record.replay!, history));
}

function* replayHistory(cursor: string, history: Iterable<string>) {
  yield cursor;
  yield* history;
}

function visiblePageLayout(page: DocVisiblePage): string {
  const event = (value: DocVisiblePage['events'][number] | null | undefined) => value
    ? [value.event_index, value.kind, value.role ?? null, value.seed_summary, value.empty_reasoning,
      value.deferred ?? false, value.event_bytes ?? null, value.compact_summary ?? false] : null;
  return JSON.stringify(page.kind === 'units'
    ? page.units.map(unit => [unit.from, unit.to, unit.turn_index, event(unit.event),
      unit.fold ? [unit.fold.from, unit.fold.to, unit.fold.rows, unit.fold.cursor] : null])
    : page.events.map(event));
}
