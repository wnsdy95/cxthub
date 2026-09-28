import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react';
import { anchoredScrollTop, mountedRowIndices, rowAtOffset, rowOffsets, visibleRowWindow } from '../graphWindow';

const OVERSCAN = 6;
const VIRTUAL_THRESHOLD = 80;
// Static renderers use the initial bounded window; browser measurement happens
// before paint after mounting. No DOM access is needed to render static markup.
const useViewportEffect = typeof window === 'undefined' ? useEffect : useLayoutEffect;

// This hook owns viewport/focus state only. It never filters graph evidence,
// classifies publication or changes the complete lane layout passed by callers.
export function useGraphWindow(ids: string[], heights: number[], selection: string | null, identity: string, pinnedId: string | null, headerHeight: number, nodeHeight: number) {
  const viewportRef = useRef<HTMLDivElement>(null);
  const offsets = useMemo(() => rowOffsets(heights), [heights]);
  const indices = useMemo(() => new Map(ids.map((id, index) => [id, index])), [ids]);
  const [viewport, setViewport] = useState({ top: 0, height: 344 });
  const [focusedId, setFocusedId] = useState<string | null>(null);
  const [requestedFocus, setRequestedFocus] = useState<string | null>(null);
  const previous = useRef({ ids, offsets, identity });
  const selected = useRef<string | null>(null);
  const measure = useCallback(() => {
    const element = viewportRef.current;
    if (!element) return;
    const next = { top: element.scrollTop, height: element.clientHeight };
    setViewport(current => current.top === next.top && current.height === next.height ? current : next);
  }, []);
  useViewportEffect(() => {
    const element = viewportRef.current;
    if (!element) return;
    let frame = 0;
    const schedule = () => { cancelAnimationFrame(frame); frame = requestAnimationFrame(measure); };
    const observer = new ResizeObserver(schedule);
    observer.observe(element);
    element.addEventListener('scroll', schedule, { passive: true });
    measure();
    return () => { observer.disconnect(); element.removeEventListener('scroll', schedule); cancelAnimationFrame(frame); };
  }, [measure]);

  const reveal = useCallback((index: number) => {
    const element = viewportRef.current;
    if (!element || index < 0 || index >= ids.length) return;
    const available = Math.max(nodeHeight, element.clientHeight - headerHeight);
    const top = offsets[index], bottom = top + nodeHeight;
    if (top < element.scrollTop) element.scrollTop = top;
    else if (bottom > element.scrollTop + available) element.scrollTop = bottom - available;
    measure();
  }, [ids.length, offsets, headerHeight, nodeHeight, measure]);

  useViewportEffect(() => {
    const element = viewportRef.current;
    if (!element) return;
    const old = previous.current;
    if (old.identity !== identity) {
      element.scrollTop = 0;
      selected.current = null;
      setFocusedId(null);
      setRequestedFocus(null);
    } else if (old.ids !== ids || old.offsets !== offsets) {
      element.scrollTop = anchoredScrollTop(old.ids, old.offsets, indices, offsets, element.scrollTop);
    }
    if (old.identity === identity && focusedId && !indices.has(focusedId)) {
      const replacement = Math.max(0, Math.min(ids.length - 1, old.ids.indexOf(focusedId)));
      setFocusedId(null);
      setRequestedFocus(ids[replacement] ?? null);
    }
    previous.current = { ids, offsets, identity };
    // Live updates of the same selection must not drag a reader back to it.
    // A selection that arrives before its folded rows stays pending until found.
    if (!selection) selected.current = null;
    const index = selection ? indices.get(selection) : undefined;
    if (selection !== selected.current && index !== undefined) {
      selected.current = selection;
      reveal(index);
    }
    measure();
  }, [ids, offsets, indices, identity, selection, focusedId, reveal, measure]);

  const visible = visibleRowWindow(offsets, viewport.top, viewport.height - headerHeight);
  const virtualized = ids.length > VIRTUAL_THRESHOLD;
  const mounted = virtualized ? mountedRowIndices(ids.length, visible, OVERSCAN,
    [focusedId, requestedFocus, pinnedId].flatMap(id => id && indices.has(id) ? [indices.get(id)!] : [])) : ids.map((_, index) => index);
  useViewportEffect(() => {
    if (!requestedFocus) return;
    const element = [...(viewportRef.current?.querySelectorAll<HTMLButtonElement>('[data-graph-id]') ?? [])]
      .find(row => row.dataset.graphId === requestedFocus);
    if (element) { element.focus({ preventScroll: true }); setRequestedFocus(null); }
  }, [requestedFocus, mounted]);

  function onKeyDown(event: KeyboardEvent<HTMLButtonElement>, index: number) {
    let next = index;
    switch (event.key) {
      case 'ArrowUp': next--; break;
      case 'ArrowDown': next++; break;
      case 'Home': next = 0; break;
      case 'End': next = ids.length - 1; break;
      case 'PageUp': next = rowAtOffset(offsets, offsets[index] - Math.max(nodeHeight, viewport.height - headerHeight)); break;
      case 'PageDown': next = rowAtOffset(offsets, offsets[index] + Math.max(nodeHeight, viewport.height - headerHeight)); break;
      default: return;
    }
    event.preventDefault();
    next = Math.max(0, Math.min(ids.length - 1, next));
    reveal(next);
    setRequestedFocus(ids[next]);
  }
  const tabStop = focusedId && indices.has(focusedId) ? focusedId
    : selection && mounted.includes(indices.get(selection) ?? -1) ? selection : ids[visible.start];
  return { viewportRef, offsets, visible, mounted, virtualized, onKeyDown, setFocusedId, tabStop };
}
