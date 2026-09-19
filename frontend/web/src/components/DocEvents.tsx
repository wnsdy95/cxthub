import { useDocPages } from '../hooks';
import { EventStream, type ViewMode } from './EventStream';
import { useT } from '../i18n';

// Only mounted when a section is visible. Parent/tail comparisons happen on
// the server's event index; the browser never downloads a hidden full parent.
export function DocEvents({ repoId, hash, base, start = -1, end, mode }: {
  repoId: string; hash: string; base?: string; start?: number; end?: number; mode: ViewMode;
}) {
  const q = useDocPages(repoId, hash, base, start);
  const t = useT();
  const first = q.data?.pages[0];
  const events = q.data?.pages.flatMap(page => page.events) ?? [];
  const count = end === undefined ? events.length : Math.max(0, Math.min(events.length, end - (first?.offset ?? 0)));
  return <>
    {q.isLoading && <div className="skel" style={{ height: 60 }} />}
    {q.isError && <p role="alert" className="err">{q.error.message} <button onClick={() => void q.refetch()}>{t('context.retryRead')}</button></p>}
    {first && <EventStream events={events.slice(0, count)} offset={first.offset} mode={mode} />}
    {q.hasNextPage && (end === undefined || (first?.offset ?? 0) + events.length < end) &&
      <button className="doc-load-more" disabled={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()}>
        {q.isFetchingNextPage ? t('context.loadingEvents') : t('context.moreEvents')}
      </button>}
  </>;
}
