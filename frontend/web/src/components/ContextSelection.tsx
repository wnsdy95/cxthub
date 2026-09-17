import { useLayoutEffect, useRef, useState } from 'react';
import type { GraphEvent } from '../graphProjection';
import { useT } from '../i18n';

/** User activation reveals the viewer, including another event at the same
 * content hash. Polls and pagination updates must not steal the reading position. */
export function useContextSelection(repoId: string, selectedId: string | null, select: (id: string) => void) {
  const viewerRef = useRef<HTMLDivElement>(null);
  const [request, setRequest] = useState<{ repo: string; snapshot: string; event?: GraphEvent } | null>(null);
  const active = request?.repo === repoId && request.snapshot === selectedId ? request : null;
  useLayoutEffect(() => {
    const viewer = viewerRef.current;
    if (!active || !viewer) return;
    const main = viewer.closest<HTMLElement>('.ctx-main');
    if (main && getComputedStyle(main).overflowY === 'auto') {
      // A newly selected document first renders a short loading placeholder.
      // Its scroll range may clamp the initial reveal. Retry as content arrives,
      // but stop as soon as it is aligned or the user starts reading manually.
      let cancelled = false;
      const stop = () => { cancelled = true; observer.disconnect(); };
      const reveal = () => {
        if (cancelled) return;
        main.scrollTo({ top: main.scrollTop + viewer.getBoundingClientRect().top - main.getBoundingClientRect().top, behavior: 'instant' });
        if (Math.abs(viewer.getBoundingClientRect().top - main.getBoundingClientRect().top) < 2) stop();
      };
      const observer = new ResizeObserver(reveal);
      observer.observe(viewer);
      observer.observe(main);
      const inputs = ['wheel', 'touchstart', 'pointerdown', 'keydown'] as const;
      for (const input of inputs) main.addEventListener(input, stop, { passive: true });
      reveal();
      return () => {
        stop();
        for (const input of inputs) main.removeEventListener(input, stop);
      };
    } else {
      viewer.scrollIntoView({ block: 'start', behavior: 'instant' });
    }
  }, [active]);
  function openSnapshot(snapshot: string, event?: GraphEvent) {
    select(snapshot);
    setRequest({ repo: repoId, snapshot, event });
  }
  return { viewerRef, openSnapshot, selectedEvent: active?.event };
}

export function ContextSelectionNotice({ event }: { event?: GraphEvent }) {
  const t = useT();
  if (!event) return null;
  return <div className="context-selection-notice" role="status">
    <strong>{event.kind === 'birth' ? `${event.branch} · ${t(event.orphan ? 'graph.orphanBirth' : 'graph.branchBorn')}`
      : `${event.sourceBranch} → ${event.branch}${event.prNumber ? ` · PR #${event.prNumber}` : ''}`}</strong>
    <span>{t(event.kind === 'birth' ? 'graph.viewingBirthContext' : 'graph.viewingMergeContext')}</span>
  </div>;
}
