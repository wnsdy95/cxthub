import { useEffect, useState } from 'react';
import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { api } from './api';
import { documentFindParameters, validateDocumentFindPage, type DocumentFindSource, type DocumentFindPosition, type DocFindMatch } from './documentFind';
import { documentReadRetry } from './readRetry';
import { DocumentProjectionError } from './documentVisible';

type FindPosition = { position: DocumentFindPosition; before: number };
type FindRun = { id: string; query: string; sources: readonly DocumentFindSource[]; history: FindPosition[]; page: number; index: number; activation: number };
export type DocumentFindResult = { source: DocumentFindSource; match: DocFindMatch };
type FindBatch = { matches: DocumentFindResult[]; next?: DocumentFindPosition };

export class DocumentFindReadError extends Error {
  constructor(public readonly sourceId: string, cause: unknown) {
    super(cause instanceof Error ? cause.message : String(cause), { cause });
  }
}

export function documentFindSourceIdentity(source: DocumentFindSource) {
  return JSON.stringify([source.id, source.selection, source.context, source.protocol === 'legacy' ? null : source.replay, source.from, source.representation ?? '']);
}

export async function readDocumentFindBatch(client: QueryClient, run: Pick<FindRun, 'id' | 'query' | 'sources'>,
  start: DocumentFindPosition, signal: AbortSignal): Promise<FindBatch> {
  let position = start;
  while (position.source < run.sources.length) {
    signal.throwIfAborted();
    const source = run.sources[position.source];
    const params = documentFindParameters(run.query, source, position);
    try {
      const value = await client.fetchQuery({
        queryKey: ['document-find-read', run.id, source.id, params.toString()],
        queryFn: ({ signal: requestSignal }) => api.getDocFindPage(source.selection.repoId, source.selection.hash, params,
          AbortSignal.any([signal, requestSignal])),
        gcTime: 0,
        staleTime: 0,
        structuralSharing: false,
        ...documentReadRetry,
      });
      signal.throwIfAborted();
      const page = validateDocumentFindPage(value, source, position.frontier);
      if (page.next_cursor === position.cursor) throw new DocumentProjectionError();
      const next: DocumentFindPosition | undefined = page.next_cursor !== null
        ? { source: position.source, cursor: page.next_cursor, frontier: { index: page.event_index, field: page.field, byte: page.byte_offset,
          ...(page.prepared ? { prepared: { ...page.prepared } } : {}) } }
        : position.source + 1 < run.sources.length ? { source: position.source + 1 } : undefined;
      if (page.matches.length || !next) return { matches: page.matches.map(match => ({ source, match })), next };
      position = next;
    } catch (error) {
      signal.throwIfAborted();
      throw new DocumentFindReadError(source.id, error);
    }
  }
  return { matches: [] };
}

export function useDocumentFind(sources: readonly DocumentFindSource[], identity: string) {
  const client = useQueryClient();
  const [run, setRun] = useState<FindRun>();
  const current = run?.history[run.page];
  const invalid = Boolean(run && run.sources.some(previous => !sources.some(source =>
    documentFindSourceIdentity(source) === documentFindSourceIdentity(previous) && source.to >= previous.to)));
  const runId = run?.id;
  useEffect(() => () => {
    if (runId) void client.cancelQueries({ queryKey: ['document-find-read', runId] });
  }, [client, runId]);
  useEffect(() => {
    if (invalid) {
      void client.cancelQueries({ queryKey: ['document-find', identity] });
      if (runId) void client.cancelQueries({ queryKey: ['document-find-read', runId] });
    }
  }, [client, identity, invalid, runId]);
  const query = useQuery({
    queryKey: ['document-find', identity, run?.id, run?.page, run?.activation],
    queryFn: ({ signal }) => readDocumentFindBatch(client, run!, current!.position, signal),
    enabled: Boolean(run && current && !invalid),
    staleTime: Infinity,
    gcTime: 0,
    structuralSharing: false,
    retry: false,
    retryOnMount: false,
    refetchOnMount: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
  const data = invalid || query.isError || query.isFetching ? undefined : query.data;
  const index = run?.index === -1 ? (data?.matches.length ?? 0) - 1 : run?.index ?? 0;
  const result = data?.matches[index];
  const expanded = Boolean(run && sources.some(source => !run.sources.some(previous =>
    documentFindSourceIdentity(source) === documentFindSourceIdentity(previous) && source.to === previous.to)));
  const stop = () => {
    void client.cancelQueries({ queryKey: ['document-find', identity] });
    if (run) void client.cancelQueries({ queryKey: ['document-find-read', run.id] });
  };
  return {
    query, invalid, result,
    number: result ? (current?.before ?? 0) + index + 1 : 0,
    started: Boolean(run), text: run?.query, expanded,
    range: run ? { records: run.sources.length, events: run.sources.reduce((count, source) => count + source.to - source.from, 0) } : undefined,
    canPrevious: Boolean(run && !invalid && (index > 0 || run.page > 0)),
    canNext: Boolean(run && !invalid && data && (index + 1 < data.matches.length || data.next)),
    start: (text: string) => {
      stop();
      setRun({ id: crypto.randomUUID(), query: text, sources: sources.map(source => ({ ...source })),
        history: [{ position: { source: 0 }, before: 0 }], page: 0, index: 0, activation: 0 });
    },
    close: () => { stop(); setRun(undefined); },
    previous: () => {
      if (!run || invalid || query.isFetching) return;
      if (index > 0) setRun({ ...run, index: index - 1 });
      else if (run.page > 0) setRun({ ...run, page: run.page - 1, index: -1, activation: run.activation + 1 });
    },
    next: () => {
      if (!run || invalid || query.isFetching || !data) return;
      if (index + 1 < data.matches.length) setRun({ ...run, index: index + 1 });
      else if (data.next) setRun({ ...run, page: run.page + 1, index: 0, activation: run.activation + 1,
        history: [...run.history.slice(0, run.page + 1), { position: data.next, before: current!.before + data.matches.length }] });
    },
  };
}
