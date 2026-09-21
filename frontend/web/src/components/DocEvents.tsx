import { useEffect, useRef, useState, type ReactNode } from 'react';
import { useDocPages } from '../hooks';
import { EventStream, type ViewMode } from './EventStream';
import { useT } from '../i18n';

// Render a contiguous prefix. A continuation is mounted only after this
// document is complete AND its bottom is visible, never alongside an unread gap.
// Parent/tail comparisons and page bounds still belong to the server index.
export function DocEvents({ repoId, hash, base, start = -1, mode, children }: {
  repoId: string; hash: string; base?: string; start?: number; mode: ViewMode; children?: ReactNode;
}) {
  const q = useDocPages(repoId, hash, base, start);
  const t = useT();
  const trigger = useRef<HTMLDivElement>(null);
  const [following, setFollowing] = useState(false);
  const [visiblePages, setVisiblePages] = useState(1);
  const pages = q.data?.pages ?? [];
  const first = pages[0];
  const events = pages.slice(0, visiblePages).flatMap(page => page.events);
  const cachedNext = visiblePages < pages.length;
  const hasNext = cachedNext || q.hasNextPage;
  const inherited = Math.max(0, Math.min(events.length, (first?.inherited ?? 0) - (first?.offset ?? 0)));
  const more = Boolean(first && (hasNext || (children && !following)));

  useEffect(() => { setFollowing(false); setVisiblePages(1); }, [repoId, hash, base, start]);
  useEffect(() => {
    const node = trigger.current;
    if (!node || !more || q.isFetching || q.isError) return;
    // The viewport root also respects clipping by the desktop center scroller.
    // It naturally switches to page scrolling on mobile, without polling.
    const observer = new IntersectionObserver(entries => {
      if (!entries.some(entry => entry.isIntersecting)) return;
      observer.disconnect();
      if (cachedNext) setVisiblePages(count => count + 1);
      else if (q.hasNextPage) void q.fetchNextPage({ cancelRefetch: false });
      else setFollowing(true);
    });
    observer.observe(node);
    return () => observer.disconnect();
    // Re-observe after each page/mode change: a page containing only hidden
    // tools may leave the bottom visible, so continue until the viewport fills.
  }, [more, cachedNext, visiblePages, q.hasNextPage, q.isFetching, q.isError, q.fetchNextPage, events.length, mode]);

  return <div className="doc-events" aria-busy={q.isFetching}>
    {q.isLoading && <div className="skel" style={{ height: 60 }} />}
    {first && <>
      {inherited > 0 && <div className="doc-inherited">
        <EventStream events={events.slice(0, inherited)} offset={first.offset} mode={mode} />
      </div>}
      <EventStream events={events.slice(inherited)} offset={first.offset + inherited} mode={mode} />
    </>}
    {q.isError && <p role="alert" className="err doc-load-error">{q.error.message} <button onClick={() => {
      if (q.isFetchNextPageError) void q.fetchNextPage({ cancelRefetch: false });
      else void q.refetch();
    }}>{t('context.retryRead')}</button></p>}
    {more && !q.isError && <div className="doc-load-trigger" ref={trigger}>
      {q.isFetching && <span role="status">{t('context.loadingEvents')}</span>}
    </div>}
    {first && !hasNext && !q.isError && following && children}
  </div>;
}
