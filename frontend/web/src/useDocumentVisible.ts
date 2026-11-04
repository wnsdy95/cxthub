import { infiniteQueryOptions, useInfiniteQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { documentReadRetry } from './readRetry';
import { documentStreamCheckpoint, documentVisibleKey, visibleContext, visibleContextWitness, visiblePageCheckpoint,
  type DocumentSelection, type DocVisibleContext, type DocVisibleFold } from './documentVisible';
import { readDocumentFold, readDocumentStream } from './documentVisibleReads';
import { historyCursors, rememberCursor } from './documentCursorHistory';
import { recordDocumentPage, type DocumentStreamRecord, type StreamPosition, type FoldPosition } from './documentPageLedger';
import { installDocumentBodyCache } from './documentPageCache';
import { recordLegacyDocumentPage } from './documentLegacyLedger';

export const documentVisibleReadOptions = {
  staleTime: Infinity,
  gcTime: 60_000,
  structuralSharing: false,
  ...documentReadRetry,
  retryOnMount: false,
  refetchOnMount: false,
  refetchOnWindowFocus: false,
  refetchOnReconnect: false,
} as const;

export function documentVisibleQueryOptions(client: QueryClient, selection: DocumentSelection, restart: string) {
  installDocumentBodyCache(client);
  const owner = documentVisibleKey(selection, restart);
  return infiniteQueryOptions({
    queryKey: owner,
    initialPageParam: {} as StreamPosition,
    queryFn: async ({ pageParam, signal }): Promise<DocumentStreamRecord> => {
      const page = await readDocumentStream(selection, signal, pageParam.previous, historyCursors(pageParam.history));
      signal.throwIfAborted();
      return page.kind === 'legacy'
        ? recordLegacyDocumentPage(client, owner, page, selection, pageParam.previous?.kind === 'legacy' ? pageParam.previous : undefined)
        : recordDocumentPage(client, owner, page, { kind: 'units', selection, position: pageParam });
    },
    getNextPageParam: (last, _pages, position): StreamPosition | undefined => {
      if (last.kind === 'legacy') return last.next < 0 ? undefined : { previous: documentStreamCheckpoint(last) };
      return last.next_cursor === null ? undefined : { previous: documentStreamCheckpoint(last), history: rememberCursor(last.next_cursor, position.history) };
    },
    enabled: query => query.state.status !== 'error',
    ...documentVisibleReadOptions,
  });
}

export function useDocumentVisible(selection: DocumentSelection, restart: string) {
  return useInfiniteQuery(documentVisibleQueryOptions(useQueryClient(), selection, restart));
}

export type DocumentVisibleQuery = ReturnType<typeof useDocumentVisible>;

export function documentFoldQueryOptions(client: QueryClient, selection: DocumentSelection, restart: string, context: DocVisibleContext, fold: DocVisibleFold) {
  const { from, to, rows, cursor } = fold;
  return foldQueryOptions(client, selection, restart, visibleContextWitness(context), { from, to, rows, cursor });
}

function foldQueryOptions(client: QueryClient, selection: DocumentSelection, restart: string, context: DocVisibleContext, fold: DocVisibleFold) {
  installDocumentBodyCache(client);
  const owner = documentVisibleKey(selection, restart);
  return infiniteQueryOptions({
    queryKey: [...documentVisibleKey(selection, restart), 'fold', ...visibleContext(context), fold.from, fold.to, fold.rows, fold.cursor],
    initialPageParam: { renderedBefore: 0 } as FoldPosition,
    queryFn: async ({ pageParam, signal }) => {
      const page = await readDocumentFold(selection, signal, context, fold, pageParam.previous, pageParam.renderedBefore, historyCursors(pageParam.history));
      signal.throwIfAborted();
      return recordDocumentPage(client, owner, page, { kind: 'fold', selection, context, fold, position: pageParam });
    },
    getNextPageParam: (last, _pages, position): FoldPosition | undefined => last.next_cursor === null ? undefined : {
      previous: visiblePageCheckpoint(last),
      renderedBefore: position.renderedBefore + last.renderedRows,
      history: rememberCursor(position.previous?.next_cursor ?? fold.cursor, position.history),
    },
    enabled: query => query.state.status !== 'error',
    ...documentVisibleReadOptions,
    gcTime: Infinity,
  });
}

export function useDocumentFold(selection: DocumentSelection, restart: string, context: DocVisibleContext, fold: DocVisibleFold) {
  return useInfiniteQuery(documentFoldQueryOptions(useQueryClient(), selection, restart, context, fold));
}
