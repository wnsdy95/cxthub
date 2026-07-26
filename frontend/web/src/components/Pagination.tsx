// "View N at a time" pagination — Common commit list for Context·On Hold tab.
// Page size dropdown (default 5) + page navigation. Reset to first page when resetKey changes (branch/filter switch).
import { useEffect, useState } from 'react';
import { useT } from '../i18n';

export const PAGE_SIZES = [5, 10, 30, 50, 100];

export interface Paged<T> {
  visible: T[];
  size: number;
  setSize: (n: number) => void;
  page: number;
  setPage: (n: number) => void;
  pageCount: number;
  total: number;
  start: number;
}

export function usePaged<T>(items: T[], resetKey?: unknown): Paged<T> {
  const [size, setSize] = useState(5);
  const [page, setPage] = useState(0);
  // Resets to first page on size change or context switch.
  useEffect(() => {
    setPage(0);
  }, [resetKey, size]);
  const pageCount = Math.max(1, Math.ceil(items.length / size));
  const clamped = Math.min(page, pageCount - 1); // Adjusts to last page if list shrinks
  const start = clamped * size;
  return {
    visible: items.slice(start, start + size),
    size,
    setSize,
    page: clamped,
    setPage,
    pageCount,
    total: items.length,
    start,
  };
}

export function PageControl<T>({ paged }: { paged: Paged<T> }) {
  const t = useT();
  return (
    <div className="page-control">
      <select aria-label={t('common.pageSize')} value={paged.size} onChange={(e) => paged.setSize(Number(e.target.value))}>
        {PAGE_SIZES.map((n) => (
          <option key={n} value={n}>
            {t('common.itemsPerPage', { n })}
          </option>
        ))}
      </select>
      {paged.pageCount > 1 && (
        <span className="pager">
          <button type="button" className="ghost mini" aria-label={t('common.prev')} disabled={paged.page === 0} onClick={() => paged.setPage(paged.page - 1)}>
            ←
          </button>
          <span className="page-ind">
            {paged.start + 1}–{Math.min(paged.start + paged.size, paged.total)} / {paged.total}
          </span>
          <button
            type="button"
            className="ghost mini"
            aria-label={t('common.next')}
            disabled={paged.page >= paged.pageCount - 1}
            onClick={() => paged.setPage(paged.page + 1)}
          >
            →
          </button>
        </span>
      )}
    </div>
  );
}
