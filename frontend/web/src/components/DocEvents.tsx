import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from 'react';
import type { ViewMode } from './EventStream';
import { useT } from '../i18n';
import { documentVisibleKey, type DocumentSelection } from '../documentVisible';
import { useDocumentVisible, type DocumentVisibleQuery } from '../useDocumentVisible';
import { VisibleDocumentPages } from './VisibleDocumentPages';
import { documentScrollRoot } from './DocumentPageWindow';
import { DocumentReadScope } from '../documentReadScope';
import { DocumentPresentation, DocumentSearchTarget } from './DocumentPresentation';
import { LegacyDocumentStream } from './LegacyDocumentStream';
import { DocumentFindScope, useDocumentFindRegistration } from './DocumentFind';

type DocEventsProps = {
  repoId: string; hash: string; base?: string; start?: number; mode: ViewMode; targetIndex?: number; targetKey?: string; children?: ReactNode;
};

type DocumentReaderState = {
  selection: DocumentSelection; restart: string; query: DocumentVisibleQuery; onRestart: () => void; targetIndex?: number; targetKey?: string;
};

type DocumentReaderProps = Omit<DocEventsProps, 'children'> & {
  children: (reader: DocumentReaderState) => ReactNode;
};

export function DocEvents({ children, ...selection }: DocEventsProps) {
  return <DocumentReader {...selection}>{reader => <DocumentEvents reader={reader}>{children}</DocumentEvents>}</DocumentReader>;
}

export function DocumentReader({ repoId, hash, base, start = -1, mode, targetIndex, targetKey, children }: DocumentReaderProps) {
  const selection = { repoId, hash, base, start, mode };
  const selectionKey = JSON.stringify(documentVisibleKey(selection));
  const [readIdentity, setReadIdentity] = useState({ selectionKey, restart: '' });
  const restart = readIdentity.selectionKey === selectionKey ? readIdentity.restart : '';
  if (readIdentity.selectionKey !== selectionKey) setReadIdentity({ selectionKey, restart: '' });
  const query = useDocumentVisible(selection, restart);
  return children({ selection, restart, query, targetIndex, targetKey, onRestart: () => setReadIdentity({ selectionKey, restart: crypto.randomUUID() }) });
}

export function DocumentEvents({ reader, children }: { reader: DocumentReaderState; children?: ReactNode }) {
  const { selection, restart, query, onRestart, targetIndex, targetKey } = reader;
  const bodyKey = JSON.stringify(documentVisibleKey(selection, restart));
  return <DocumentReadScope.Provider value={documentVisibleKey(selection, restart)}>
    <DocumentFindScope key={bodyKey}>
    <DocumentSearchTarget requestId={targetKey}>
    {query.data?.pages[0]?.kind === 'legacy'
      ? <LegacyDocEventPages key={bodyKey} {...selection} restart={restart} query={query} targetIndex={targetIndex} onRestart={onRestart}>{children}</LegacyDocEventPages>
      : <VisibleDocumentPages key={bodyKey} selection={selection} restart={restart} query={query}
        onRestart={onRestart} targetIndex={targetIndex}>{children}</VisibleDocumentPages>}
    </DocumentSearchTarget>
    </DocumentFindScope>
  </DocumentReadScope.Provider>;
}

function LegacyDocEventPages({ repoId, hash, base, start = -1, mode, children, query, targetIndex, onRestart, restart }: DocEventsProps & { restart: string; query: DocumentVisibleQuery; onRestart: () => void }) {
  const translate = useT();
  const trigger = useRef<HTMLDivElement>(null);
  const [following, setFollowing] = useState(false);
  const [visiblePages, setVisiblePages] = useState(1);
  const [hiddenBudget, setHiddenBudget] = useState(2);
  const pages = useMemo(() => query.data?.pages.filter(page => page.kind === 'legacy'), [query.data?.pages]);
  const first = pages?.[0];
  const visible = useMemo(() => pages?.slice(0, visiblePages) ?? [], [pages, visiblePages]);
  const last = visible.at(-1);
  const identity = useId();
  const registration = useMemo(() => first ? {
    id: identity, scope: documentVisibleKey({ repoId, hash, base, start, mode }, restart), onRestart,
    source: first.doc_identity === '' ? {
      protocol: 'legacy' as const, id: identity, selection: { repoId, hash, base, start, mode },
      context: { hash, base: base ?? '', view: mode, start: first.offset, total: first.total, inherited: first.inherited },
      from: first.offset, to: last ? last.next < 0 ? last.total : last.next : first.offset,
    } : undefined,
  } : undefined, [identity, repoId, hash, base, start, mode, restart, first, last]);
  useDocumentFindRegistration(registration);
  const cachedNext = visiblePages < (pages?.length ?? 0);
  const hasNext = cachedNext || query.hasNextPage;
  const more = Boolean(first && (hasNext || (children && !following)));
  const hiddenPages = useMemo(() => {
    if (!pages) return 0;
    let count = 0;
    for (let index = Math.min(visiblePages, pages.length) - 1; index >= 0; index--) {
      if (pages[index].rows.some(row => mode === 'all' ? row.rendered : mode === 'chat' ? row.message : row.prompt)) break;
      count++;
    }
    return count;
  }, [pages, visiblePages, mode]);
  const scanLimited = hiddenPages >= hiddenBudget;

  useEffect(() => { if (!hiddenPages) setHiddenBudget(2); }, [hiddenPages]);

  useEffect(() => {
    const node = trigger.current;
    if (!node || !more || cachedNext || !query.hasNextPage || query.isFetching || query.isError || scanLimited) return;
    const observer = new IntersectionObserver(entries => {
      if (!entries.some(entry => entry.isIntersecting)) return;
      observer.disconnect();
      void query.fetchNextPage({ cancelRefetch: false });
    }, { root: documentScrollRoot(node), rootMargin: '0px 0px 320px 0px' });
    observer.observe(node);
    return () => observer.disconnect();
  }, [more, cachedNext, query.hasNextPage, query.isFetching, query.isError, query.fetchNextPage, visiblePages, scanLimited]);

  useEffect(() => {
    const node = trigger.current;
    if (!node || !more || query.isError || (cachedNext && scanLimited)) return;
    const observer = new IntersectionObserver(entries => {
      if (!entries.some(entry => entry.isIntersecting)) return;
      if (cachedNext) {
        observer.disconnect();
        setVisiblePages(count => count + 1);
      } else if (!query.hasNextPage && !query.isFetching) {
        observer.disconnect();
        setFollowing(true);
      }
    }, { root: documentScrollRoot(node) });
    observer.observe(node);
    return () => observer.disconnect();
  }, [more, cachedNext, visiblePages, query.hasNextPage, query.isFetching, query.isError, scanLimited]);

  return <DocumentPresentation><div className="doc-events" aria-busy={query.isFetching}
    data-document-from={first?.offset} data-document-to={last ? last.next < 0 ? last.total : last.next : undefined}>
    {query.isLoading && <div className="skel" style={{ height: 60 }} />}
    {first && <LegacyDocumentStream pages={visible} mode={mode} repoId={repoId} hash={hash} targetIndex={targetIndex} onRestart={onRestart} />}
    {query.isError && <p role="alert" className="err doc-load-error">{query.error.message} <button onClick={() => {
      if (query.isFetchNextPageError) void query.fetchNextPage({ cancelRefetch: false });
      else void query.refetch();
    }}>{translate('context.retryRead')}</button></p>}
    {more && !query.isError && <div className="doc-load-trigger" ref={trigger}>
      {query.isFetching && <span role="status">{translate('context.loadingEvents')}</span>}
      {scanLimited && hasNext && !query.isFetching && <button onClick={() => setHiddenBudget(hiddenPages + 2)}>{translate('context.loadMoreEvents')}</button>}
    </div>}
    {first && !hasNext && !query.isError && following && children}
  </div></DocumentPresentation>;
}
