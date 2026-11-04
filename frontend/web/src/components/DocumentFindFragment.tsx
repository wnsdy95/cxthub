import { useId, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api, ApiError } from '../api';
import { documentFindFragmentParameters, validateDocumentFindFragment, type DocFindFragmentPage, type DocFindMatch } from '../documentFind';
import { documentBodyKey } from '../documentPageCache';
import { useDocumentReadScope } from '../documentReadScope';
import { DOCUMENT_FRAGMENT_BYTES } from '../documentReads';
import { DocumentProjectionError } from '../documentVisible';
import { documentReadRetry } from '../readRetry';
import { useT } from '../i18n';
import type { DocumentFindResult } from '../useDocumentFind';

export function DocumentFindFragment({ result, onRestart }: { result: DocumentFindResult; onRestart: () => void }) {
  const translate = useT();
  const identity = useId();
  const owner = useDocumentReadScope();
  const { source, match } = result;
  const [offset, setOffset] = useState(match.source_range!.from);
  const parameters = documentFindFragmentParameters(source, match, offset, DOCUMENT_FRAGMENT_BYTES);
  const key = ['find-fragment', identity, source.id, match.event_index, match.reveal_cursor, offset];
  const query = useQuery({
    queryKey: owner ? documentBodyKey(owner, ...key) : key,
    queryFn: async ({ signal }) => validateDocumentFindFragment(await api.getDocFindFragment(
      source.selection.repoId, source.selection.hash, parameters, signal), source, match, offset, DOCUMENT_FRAGMENT_BYTES),
    gcTime: 0,
    staleTime: 0,
    structuralSharing: false,
    meta: { documentBody: Boolean(owner), documentEphemeral: true },
    ...documentReadRetry,
    retryOnMount: true,
    refetchOnMount: 'always',
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
  const page = query.isFetching || query.isError ? undefined : query.data;
  const stale = query.error instanceof DocumentProjectionError || query.error instanceof ApiError && query.error.status === 409;
  return <div className="document-find-reveal" aria-busy={query.isFetching}>
    {query.isFetching && <p role="status">{translate('context.loadingEvents')}</p>}
    {query.isError && <p role="alert" className="err">{query.error.message}{' '}
      <button onClick={stale ? onRestart : () => { void query.refetch(); }}>{translate(stale ? 'context.restartDocument' : 'context.retryRead')}</button>
    </p>}
    {page && <>
      <p className="muted">{translate('context.eventFragment', { start: page.byte_offset,
        end: page.next === -1 ? page.total_bytes : page.next, total: page.total_bytes })}</p>
      <pre className="document-find-source" tabIndex={0}><MatchedFragment page={page} match={match} /></pre>
    </>}
    <div className="document-find-controls">
      <button aria-disabled={!page || page.previous === -1} onClick={() => {
        if (page && page.previous !== -1) setOffset(page.previous);
      }}>{translate('context.previousFragment')}</button>
      <button aria-disabled={!page || page.next === -1} onClick={() => {
        if (page && page.next !== -1) setOffset(page.next);
      }}>{translate('context.nextFragment')}</button>
    </div>
  </div>;
}

function MatchedFragment({ page, match }: { page: DocFindFragmentPage; match: DocFindMatch }) {
  const bytes = new TextEncoder().encode(page.json_fragment);
  const start = Math.max(0, match.source_range!.from - page.byte_offset);
  const end = Math.min(bytes.length, match.source_range!.to - page.byte_offset);
  if (start >= end) return <>{page.json_fragment}</>;
  const decoder = new TextDecoder('utf-8', { fatal: true });
  return <>{decoder.decode(bytes.slice(0, start))}<mark>{decoder.decode(bytes.slice(start, end))}</mark>{decoder.decode(bytes.slice(end))}</>;
}
