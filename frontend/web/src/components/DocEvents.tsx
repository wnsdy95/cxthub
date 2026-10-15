import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { useDocPages } from '../hooks';
import { EventStream, type ViewMode } from './EventStream';
import { useT } from '../i18n';

type DocEventsProps = {
  repoId: string; hash: string; base?: string; start?: number; mode: ViewMode; children?: ReactNode;
};

export function DocEvents(props: DocEventsProps) {
  return <DocEventPages key={JSON.stringify([props.repoId, props.hash, props.base, props.start, props.mode])} {...props} />;
}

function scrollRoot(node: HTMLElement): HTMLElement | null {
  for (let parent = node.parentElement; parent; parent = parent.parentElement) {
    if (/(auto|scroll)/.test(getComputedStyle(parent).overflowY)) return parent;
  }
  return null;
}

function DocEventPages({ repoId, hash, base, start = -1, mode, children }: DocEventsProps) {
  const q = useDocPages(repoId, hash, base, start, mode);
  const t = useT();
  const trigger = useRef<HTMLDivElement>(null);
  const [following, setFollowing] = useState(false);
  const [visiblePages, setVisiblePages] = useState(1);
  const [hiddenBudget, setHiddenBudget] = useState(2);
  const pages = q.data?.pages;
  const first = pages?.[0];
  const events = useMemo(() => pages?.slice(0, visiblePages).flatMap(page => page.events) ?? [], [pages, visiblePages]);
  const cachedNext = visiblePages < (pages?.length ?? 0);
  const hasNext = cachedNext || q.hasNextPage;
  const more = Boolean(first && (hasNext || (children && !following)));
  const hiddenPages = useMemo(() => {
    if (mode === 'all' || !pages) return 0;
    let count = 0;
    for (let index = Math.min(visiblePages, pages.length) - 1; index >= 0; index--) {
      if (pages[index].events.some(event => event.kind === 'message' && !event.compact_summary && (mode === 'chat' || event.role === 'user'))) break;
      count++;
    }
    return count;
  }, [pages, visiblePages, mode]);
  const scanLimited = hiddenPages >= hiddenBudget;

  useEffect(() => { if (!hiddenPages) setHiddenBudget(2); }, [hiddenPages]);

  useEffect(() => {
    const node = trigger.current;
    if (!node || !more || cachedNext || !q.hasNextPage || q.isFetching || q.isError || scanLimited) return;
    const observer = new IntersectionObserver(entries => {
      if (!entries.some(entry => entry.isIntersecting)) return;
      observer.disconnect();
      void q.fetchNextPage({ cancelRefetch: false });
    }, { root: scrollRoot(node), rootMargin: '0px 0px 320px 0px' });
    observer.observe(node);
    return () => observer.disconnect();
  }, [more, cachedNext, q.hasNextPage, q.isFetching, q.isError, q.fetchNextPage, visiblePages, scanLimited]);

  useEffect(() => {
    const node = trigger.current;
    if (!node || !more || q.isError || (cachedNext && scanLimited)) return;
    const observer = new IntersectionObserver(entries => {
      if (!entries.some(entry => entry.isIntersecting)) return;
      if (cachedNext) {
        observer.disconnect();
        setVisiblePages(count => count + 1);
      } else if (!q.hasNextPage && !q.isFetching) {
        observer.disconnect();
        setFollowing(true);
      }
    }, { root: scrollRoot(node) });
    observer.observe(node);
    return () => observer.disconnect();
  }, [more, cachedNext, visiblePages, q.hasNextPage, q.isFetching, q.isError, scanLimited]);

  return <div className="doc-events" aria-busy={q.isFetching}>
    {q.isLoading && <div className="skel" style={{ height: 60 }} />}
    {first && <EventStream events={events} offset={first.offset} inherited={first.inherited} mode={mode} repoId={repoId} hash={hash} />}
    {q.isError && <p role="alert" className="err doc-load-error">{q.error.message} <button onClick={() => {
      if (q.isFetchNextPageError) void q.fetchNextPage({ cancelRefetch: false });
      else void q.refetch();
    }}>{t('context.retryRead')}</button></p>}
    {more && !q.isError && <div className="doc-load-trigger" ref={trigger}>
      {q.isFetching && <span role="status">{t('context.loadingEvents')}</span>}
      {scanLimited && hasNext && !q.isFetching && <button onClick={() => setHiddenBudget(hiddenPages + 2)}>{t('context.loadMoreEvents')}</button>}
    </div>}
    {first && !hasNext && !q.isError && following && children}
  </div>;
}
