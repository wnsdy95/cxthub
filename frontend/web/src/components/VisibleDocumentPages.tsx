import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { ApiError } from '../api';
import { DocumentProjectionError, documentVisibleKey, visibleContextWitness, type DocumentSelection, type DocVisibleFold, type DocVisiblePage, type DocVisibleUnit } from '../documentVisible';
import { useDocumentFold, type DocumentVisibleQuery } from '../useDocumentVisible';
import { useT } from '../i18n';
import { DocumentEventRow, EventStream } from './EventStream';
import { DocumentPageWindow, documentScrollRoot } from './DocumentPageWindow';
import { DocumentPresentation, useDocumentPresentation } from './DocumentPresentation';
import { documentPageBodyOptions, type DocumentPageRecord } from '../documentPageLedger';
import { useDocumentFindRegistration } from './DocumentFind';

type SelectionProps = { selection: DocumentSelection; restart: string; onRestart: () => void; targetIndex?: number };

const AUTOMATIC_EMPTY_PAGES = 2;

function DocumentError({ error, retry, onRestart }: { error: Error; retry: () => void; onRestart: () => void }) {
  const translate = useT();
  const stale = error instanceof DocumentProjectionError || error instanceof ApiError && error.status === 409;
  return <p role="alert" className="err doc-load-error">{error.message}{' '}
    <button onClick={stale ? onRestart : retry}>{translate(stale ? 'context.restartDocument' : 'context.retryRead')}</button>
  </p>;
}

export function VisibleDocumentPages({ selection, restart, query, onRestart, children, targetIndex }: SelectionProps & {
  query: DocumentVisibleQuery; children?: ReactNode;
}) {
  const translate = useT();
  const trigger = useRef<HTMLDivElement>(null);
  const [visiblePages, setVisiblePages] = useState(1);
  const [following, setFollowing] = useState(false);
  const [continuedAfter, setContinuedAfter] = useState(0);
  const pages = query.data?.pages;
  const first = pages?.[0];
  const last = pages?.[Math.min(visiblePages, pages.length) - 1];
  const identity = useId();
  const selectionIdentity = JSON.stringify(documentVisibleKey(selection, restart));
  const registration = useMemo(() => first?.kind === 'units' && last?.kind === 'units' ? {
    id: identity, scope: documentVisibleKey(selection, restart), onRestart,
    source: first.replay ? { id: identity, selection: { ...selection }, context: visibleContextWitness(first),
      replay: first.replay, from: first.from, to: last.to } : undefined,
  } : undefined, [identity, selectionIdentity, first, last]);
  useDocumentFindRegistration(registration);
  const cachedNext = visiblePages < (pages?.length ?? 0);
  const hasNext = cachedNext || query.hasNextPage;
  const more = Boolean(first && (hasNext || children && !following));
  const emptyPages = useMemo(() => {
    let count = 0;
    for (let index = Math.min(visiblePages, pages?.length ?? 0) - 1; index >= 0; index--) {
      const page = pages![index];
      if (page.kind !== 'units' || page.hasContent) break;
      count++;
    }
    return count;
  }, [pages, visiblePages]);
  const emptyLimited = hasNext && Math.min(emptyPages, visiblePages - continuedAfter) >= AUTOMATIC_EMPTY_PAGES;

  useEffect(() => {
    const node = trigger.current;
    if (!node || !more || query.isError || emptyLimited) return;
    const root = documentScrollRoot(node);
    const observer = new IntersectionObserver(entries => {
      if (!entries.some(entry => entry.isIntersecting)) return;
      observer.disconnect();
      if (cachedNext) setVisiblePages(count => count + 1);
      else if (query.hasNextPage && !query.isFetching) void query.fetchNextPage({ cancelRefetch: false });
      else if (!query.hasNextPage && !query.isFetching) setFollowing(true);
    }, { root, rootMargin: cachedNext || !query.hasNextPage ? '0px' : '0px 0px 320px 0px' });
    observer.observe(node);
    return () => observer.disconnect();
  }, [more, cachedNext, visiblePages, query.hasNextPage, query.isFetching, query.isError, query.fetchNextPage, emptyLimited]);

  return <DocumentPresentation><div className="doc-events" aria-busy={query.isFetching} data-document-projection="visible-v1"
    data-document-from={first?.kind === 'units' ? first.from : undefined} data-document-to={last?.kind === 'units' ? last.to : undefined}>
    {query.isLoading && <div className="skel" style={{ height: 60 }} />}
    {pages?.slice(0, visiblePages).map(page => page.kind === 'units' ?
      <DocumentPageWindow key={page.from} identity={`units:${page.from}`}>{() => <DocumentPageBody record={page} onRestart={onRestart}>
        {body => body.units.map(unit => <VisibleUnit key={unit.from} unit={unit} context={body} selection={selection} restart={restart} onRestart={onRestart} targetIndex={targetIndex} />)}
      </DocumentPageBody>}</DocumentPageWindow> : null)}
    {query.isError && <DocumentError error={query.error} onRestart={onRestart} retry={() => {
      if (query.isFetchNextPageError) void query.fetchNextPage({ cancelRefetch: false });
      else void query.refetch();
    }} />}
    {more && !query.isError && <div className="doc-load-trigger" data-document-sentinel ref={trigger}>
      {query.isFetching && <span role="status">{translate('context.loadingEvents')}</span>}
      {emptyLimited && !query.isFetching && <button onClick={() => setContinuedAfter(visiblePages)}>{translate('context.loadMoreEvents')}</button>}
    </div>}
    {first && !hasNext && !query.isError && following && children}
  </div></DocumentPresentation>;
}

function DocumentPageBody({ record, children, onRestart }: {
  record: DocumentPageRecord; children: (page: DocVisiblePage) => ReactNode; onRestart: () => void;
}) {
  const query = useQuery(documentPageBodyOptions(record));
  const translate = useT();
  if (query.isError) return <div data-document-page-loading><DocumentError error={query.error} retry={() => { void query.refetch(); }} onRestart={onRestart} /></div>;
  if (!query.data) return <div data-document-page-loading role="status">{translate('context.loadingEvents')}</div>;
  return children(query.data);
}

function VisibleUnit({ unit, context, selection, restart, onRestart, targetIndex }: SelectionProps & { unit: DocVisibleUnit; context: DocVisiblePage }) {
  const [open, setOpen] = useDocumentPresentation(`unit:${unit.from}:open`, false);
  const translate = useT();
  const fold = unit.fold;
  const expandable = Boolean(fold && fold.rows > 0);
  if (!unit.event && !expandable) return null;
  const work = open && fold ? <VisibleFold selection={selection} restart={restart} context={context} fold={fold} onRestart={onRestart} /> : null;
  return <div className="turn" data-visible-unit={unit.from} data-turn-index={unit.turn_index}>
    {selection.mode === 'chat' && unit.event && work}
    {unit.event && <DocumentEventRow event={unit.event} index={unit.event.event_index} inherited={context.inherited}
      repoId={selection.repoId} hash={selection.hash} targeted={unit.event.event_index === targetIndex} />}
    {expandable && <button className="turn-lead" aria-expanded={open} onClick={() => setOpen(previous => !previous)}
      aria-label={translate('context.aiEvents', { count: fold!.rows })}>
      {open ? '▾' : '▸'} AI {fold!.rows}
    </button>}
    {(selection.mode !== 'chat' || !unit.event) && work}
  </div>;
}

function VisibleFold({ selection, restart, context, fold, onRestart }: SelectionProps & { context: DocVisiblePage; fold: DocVisibleFold }) {
  const query = useDocumentFold(selection, restart, context, fold);
  const translate = useT();
  return <div className="asst-work" data-document-fold={`${fold.from}:${fold.to}`} aria-busy={query.isFetching}>
    {query.data?.pages.map(page => <DocumentPageWindow key={page.from} identity={`fold:${page.from}`}>{() =>
      <DocumentPageBody record={page} onRestart={onRestart}>{body =>
        <EventStream events={body.events} inherited={context.inherited} repoId={selection.repoId} hash={selection.hash} />
      }</DocumentPageBody>
    }</DocumentPageWindow>)}
    {query.isFetching && <span role="status">{translate('context.loadingEvents')}</span>}
    {query.isError && <DocumentError error={query.error} onRestart={onRestart} retry={() => {
      if (query.isFetchNextPageError) void query.fetchNextPage({ cancelRefetch: false });
      else void query.refetch();
    }} />}
    {query.hasNextPage && !query.isError && <button disabled={query.isFetching} onClick={() => void query.fetchNextPage({ cancelRefetch: false })}>
      {translate('context.loadMoreEvents')}
    </button>}
  </div>;
}
