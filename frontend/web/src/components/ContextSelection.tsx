import { useLayoutEffect, useRef, useState } from 'react';
import type { GraphEvent } from '../graphProjection';
import { useT } from '../i18n';

/** User activation reveals the viewer, including another event at the same
 * content hash. Polls and pagination updates must not steal the reading position. */
export function useContextSelection(repoId: string, selectedId: string | null, select: (id: string) => void) {
  const viewerRef = useRef<HTMLDivElement>(null);
  const [request, setRequest] = useState<{ repo: string; snapshot: string; event?: GraphEvent; eventIndex?: number; targetKey?: string } | null>(null);
  const active = request?.repo === repoId && request.snapshot === selectedId ? request : null;
  useLayoutEffect(() => {
    const viewer = viewerRef.current;
    if (!active || !viewer) return;
    const main = viewer.closest<HTMLElement>('.ctx-main');
    const pane = main && getComputedStyle(main).overflowY === 'auto' ? main : null;
    if (pane || active.eventIndex !== undefined) {
      // A newly selected document first renders a short loading placeholder.
      // Its scroll range may clamp the initial reveal. Retry as content arrives,
      // but stop as soon as it is aligned or the user starts reading manually.
      let cancelled = false;
      const stop = () => { cancelled = true; observer.disconnect(); mutations.disconnect(); };
      const reveal = () => {
        if (cancelled) return;
        const currentPane = main && getComputedStyle(main).overflowY === 'auto' ? main : null;
        const match = active.eventIndex === undefined ? viewer : viewer.querySelector<HTMLElement>(`[data-document-target="${active.eventIndex}"]`);
        const target = match ?? viewer.querySelector<HTMLElement>(`[data-document-page="units:${active.eventIndex}"]`);
        if (!target) return;
        const stickyInset = Array.from(viewer.querySelectorAll<HTMLElement>('.viewer-head,[data-document-sticky]')).reduce((height, node) => {
          const style = getComputedStyle(node);
          return style.position === 'sticky' ? Math.max(height, node.getBoundingClientRect().height + (parseFloat(style.top) || 0)) : height;
        }, 0);
        const inset = active.eventIndex === undefined ? 0 :
          stickyInset +
          (parseFloat(getComputedStyle(target).scrollMarginTop) || 0);
        const alignedTop = (currentPane?.getBoundingClientRect().top ?? 0) + inset;
        if (currentPane) {
          currentPane.scrollTo({ top: currentPane.scrollTop + target.getBoundingClientRect().top - alignedTop, behavior: 'instant' });
        } else {
          window.scrollTo({ top: window.scrollY + target.getBoundingClientRect().top - alignedTop, behavior: 'instant' });
        }
        if (match && Math.abs(match.getBoundingClientRect().top - alignedTop) < 2) stop();
      };
      const observer = new ResizeObserver(reveal);
      const mutations = new MutationObserver(reveal);
      mutations.observe(viewer, { childList: true, subtree: true });
      observer.observe(viewer);
      if (main) observer.observe(main);
      window.addEventListener('resize', reveal);
      const inputs = ['wheel', 'touchstart', 'pointerdown', 'keydown'] as const;
      for (const input of inputs) window.addEventListener(input, stop, { passive: true });
      reveal();
      return () => {
        stop();
        window.removeEventListener('resize', reveal);
        for (const input of inputs) window.removeEventListener(input, stop);
      };
    } else {
      viewer.scrollIntoView({ block: 'start', behavior: 'instant' });
    }
  }, [active]);
  function openSnapshot(snapshot: string, event?: GraphEvent, eventIndex?: number) {
    select(snapshot);
    setRequest({ repo: repoId, snapshot, event, eventIndex, targetKey: eventIndex === undefined ? undefined : crypto.randomUUID() });
  }
  return { viewerRef, openSnapshot, selectedEvent: active?.event, selectedEventIndex: active?.eventIndex, selectedTargetKey: active?.targetKey };
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
