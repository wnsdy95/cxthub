import { type Query, type QueryClient, type QueryKey, type QueryState } from '@tanstack/react-query';

export const DOCUMENT_BODY_CACHE_PAGES = 8;
export const DOCUMENT_BODY_CACHE_BYTES = 8 * 1024 * 1024;

const installed = new WeakSet<QueryClient>();

export function installDocumentBodyCache(client: QueryClient) {
  if (installed.has(client)) return;
  installed.add(client);
  const retained = new Map<Query, number>();
  const failures = new Map<string, { key: QueryKey; state: Partial<QueryState<unknown, Error>> }>();
  let retainedBytes = 0;
  let trimming = false;
  const forget = (query: Query) => {
    retainedBytes -= retained.get(query) ?? 0;
    retained.delete(query);
  };
  const trim = (admitting?: Query) => {
    if (trimming) return;
    trimming = true;
    try {
      for (const query of retained.keys()) {
        if (retained.size <= DOCUMENT_BODY_CACHE_PAGES && retainedBytes <= DOCUMENT_BODY_CACHE_BYTES) break;
        if (query === admitting || query.getObserversCount() || query.state.fetchStatus !== 'idle') continue;
        forget(query);
        client.getQueryCache().remove(query);
      }
    } finally { trimming = false; }
  };
  client.getQueryCache().subscribe(event => {
    const query = event.query;
    if (event.type === 'removed') {
      forget(query);
      if (query.queryKey[0] === 'doc-visible' && query.queryKey.length === 7) {
        const owner: QueryKey = query.queryKey;
        for (const [hash, failure] of failures) {
          if (owner.every((value, index) => failure.key[index] === value)) failures.delete(hash);
        }
        client.removeQueries({ queryKey: query.queryKey });
      }
      return;
    }
    if (query.meta?.documentBody !== true) return;
    if (event.type === 'added' && query.meta?.documentEphemeral !== true) {
      const failure = failures.get(query.queryHash);
      if (failure) query.setState(failure.state);
    }
    if (event.type === 'updated' && event.action.type === 'error') {
      forget(query);
      if (query.meta?.documentEphemeral === true) {
        failures.delete(query.queryHash);
      } else {
        failures.set(query.queryHash, { key: query.queryKey, state: {
          data: undefined, dataUpdatedAt: 0, status: 'error', fetchStatus: 'idle',
          error: query.state.error, errorUpdatedAt: query.state.errorUpdatedAt, errorUpdateCount: query.state.errorUpdateCount,
          fetchFailureCount: query.state.fetchFailureCount, fetchFailureReason: query.state.fetchFailureReason,
        } });
      }
      if (query.state.data !== undefined) query.setState({ data: undefined, dataUpdatedAt: 0 });
    }
    if (query.state.status === 'error' && !query.getObserversCount() && query.state.fetchStatus === 'idle' &&
      (event.type === 'observerRemoved' || event.type === 'updated' && event.action.type === 'error')) {
      client.getQueryCache().remove(query);
      return;
    }
    if (event.type === 'updated' && event.action.type === 'success') {
      failures.delete(query.queryHash);
      forget(query);
      const size = new TextEncoder().encode(JSON.stringify(query.state.data)).byteLength;
      retained.set(query, size);
      retainedBytes += size;
      trim(query);
      return;
    }
    if (event.type === 'observerAdded' && retained.has(query)) {
      const size = retained.get(query)!;
      retained.delete(query);
      retained.set(query, size);
    }
    if (event.type === 'observerRemoved' || event.type === 'observerAdded' || event.type === 'updated') trim();
  });
}

export function documentBodyKey(owner: QueryKey, ...identity: readonly unknown[]) {
  return [...owner, 'body', ...identity] as const;
}
