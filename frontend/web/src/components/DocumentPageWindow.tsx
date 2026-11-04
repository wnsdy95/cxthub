import { useId, useLayoutEffect, useRef, useState, type ReactNode } from 'react';
import { flushSync } from 'react-dom';

type DocumentTabTarget = { node: HTMLElement; loading?: HTMLElement };
type DocumentPageBoundary = { reveal: () => void; wait: (backward: boolean, loading: HTMLElement) => void };

const documentPageBoundaries = new Map<HTMLElement, DocumentPageBoundary>();

function lastDocumentDescendant(node: Node): Node {
  let descendant = node;
  while (descendant.lastChild) descendant = descendant.lastChild;
  return descendant;
}

function documentTabVisible(node: HTMLElement): boolean {
  if (node.closest('[inert]') || !node.getClientRects().length) return false;
  const visibility = getComputedStyle(node).visibility;
  if (visibility === 'hidden' || visibility === 'collapse') return false;
  for (let parent = node.parentElement; parent; parent = parent.parentElement) {
    if (parent instanceof HTMLDetailsElement && !parent.open && !parent.querySelector('summary')?.contains(node)) return false;
  }
  return true;
}

function documentTabStop(node: HTMLElement): boolean {
  return !node.matches(':disabled') && (node.tabIndex >= 0 ||
    node.isContentEditable && !node.hasAttribute('tabindex') && !node.parentElement?.isContentEditable);
}

function documentTabTarget(source: Node, backward: boolean, entering = false): DocumentTabTarget | undefined {
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_ELEMENT);
  walker.currentNode = backward && entering ? lastDocumentDescendant(source) : source;
  let candidate = backward && entering ? walker.currentNode : backward ? walker.previousNode() : walker.nextNode();
  while (candidate) {
    if (candidate instanceof HTMLElement) {
      const boundary = documentPageBoundaries.get(candidate);
      if (boundary && candidate.dataset.pageMounted === 'false' && documentTabVisible(candidate)) {
        flushSync(boundary.reveal);
        if (!candidate.isConnected) return;
        if (backward) {
          const descendant = lastDocumentDescendant(candidate);
          if (descendant !== candidate) {
            candidate = descendant;
            walker.currentNode = candidate;
            continue;
          }
        }
      }
      if (candidate.hasAttribute('data-document-page-loading') && documentTabVisible(candidate)) {
        const controls = Array.from(candidate.querySelectorAll<HTMLElement>('*')).filter(control =>
          documentTabStop(control) && documentTabVisible(control));
        const control = backward ? controls.at(-1) : controls[0];
        if (control) return { node: control };
        const page = candidate.closest<HTMLElement>('[data-document-page]');
        if (page && documentPageBoundaries.has(page)) return { node: page, loading: candidate };
      }
      if (documentTabStop(candidate) && documentTabVisible(candidate)) return { node: candidate };
    }
    candidate = backward ? walker.previousNode() : walker.nextNode();
  }
}

function revealDocumentTabBoundary(event: KeyboardEvent) {
  if (event.key !== 'Tab' || event.defaultPrevented || event.altKey || event.ctrlKey || event.metaKey) return;
  const active = document.activeElement;
  if (!(active instanceof HTMLElement)) return;
  const source = !event.shiftKey && documentPageBoundaries.has(active) ? lastDocumentDescendant(active) : active;
  const target = documentTabTarget(source, event.shiftKey, event.shiftKey && active === document.body);
  if (!target?.loading) return;
  event.preventDefault();
  documentPageBoundaries.get(target.node)?.wait(event.shiftKey, target.loading);
}

export function documentScrollRoot(node: HTMLElement): HTMLElement | null {
  for (let parent = node.parentElement; parent; parent = parent.parentElement) {
    if (/(auto|scroll)/.test(getComputedStyle(parent).overflowY)) return parent;
  }
  return null;
}

export function DocumentPageWindow({ identity, children }: { identity: string; children: () => ReactNode }) {
  if (import.meta.env.VITE_DOCUMENT_RENDER_WINDOW !== '1') return <>{children()}</>;
  return <MeasuredDocumentPageWindow identity={identity}>{children}</MeasuredDocumentPageWindow>;
}

function MeasuredDocumentPageWindow({ identity, children }: { identity: string; children: () => ReactNode }) {
  const container = useRef<HTMLDivElement>(null);
  const loadingDescription = useId();
  const height = useRef(0);
  const waiting = useRef<boolean | null>(null);
  const [mounted, setMounted] = useState(true);

  useLayoutEffect(() => {
    const node = container.current;
    if (!node) return;
    if (!documentPageBoundaries.size) document.addEventListener('keydown', revealDocumentTabBoundary);
    documentPageBoundaries.set(node, {
      reveal: () => setMounted(true),
      wait: (backward, loading) => {
        waiting.current = backward;
        if (!loading.id) loading.id = loadingDescription;
        node.setAttribute('aria-describedby', loading.id);
        node.focus({ preventScroll: true });
        loading.scrollIntoView({ block: backward ? 'end' : 'start' });
      },
    });
    let root: HTMLElement | null | undefined;
    let observer: IntersectionObserver | undefined;
    const observe = () => {
      const nextRoot = documentScrollRoot(node);
      if (nextRoot === root) return;
      root = nextRoot;
      observer?.disconnect();
      observer = new IntersectionObserver(entries => {
        const entry = entries[0];
        if (entry) setMounted(entry.isIntersecting || node.contains(document.activeElement));
      }, { root, rootMargin: '1000px 0px' });
      observer.observe(node);
    };
    const focusOut = (event: FocusEvent) => {
      if (event.target === node) {
        waiting.current = null;
        node.removeAttribute('aria-describedby');
      }
      if (event.relatedTarget instanceof Node && node.contains(event.relatedTarget)) return;
      const bounds = node.getBoundingClientRect();
      const viewport = root?.getBoundingClientRect();
      setMounted(bounds.bottom >= (viewport?.top ?? 0) - 1000 && bounds.top <= (viewport?.bottom ?? window.innerHeight) + 1000);
    };
    observe();
    const resize = new ResizeObserver(observe);
    resize.observe(node);
    window.addEventListener('resize', observe, { passive: true });
    node.addEventListener('focusout', focusOut);
    return () => {
      observer?.disconnect();
      resize.disconnect();
      window.removeEventListener('resize', observe);
      node.removeEventListener('focusout', focusOut);
      documentPageBoundaries.delete(node);
      if (!documentPageBoundaries.size) document.removeEventListener('keydown', revealDocumentTabBoundary);
    };
  }, [loadingDescription]);

  useLayoutEffect(() => {
    const node = container.current;
    if (!node || !mounted) return;
    const measure = () => {
      if (node.querySelector('[data-document-page-loading]')) node.style.minHeight = `${height.current}px`;
      else {
        node.style.minHeight = '';
        height.current = node.getBoundingClientRect().height;
      }
    };
    const restore = () => {
      measure();
      if (waiting.current === null || document.activeElement !== node) return;
      const backward = waiting.current;
      const target = documentTabTarget(node, backward, true);
      if (!target || target.loading && target.node === node) return;
      waiting.current = null;
      if (target.loading) documentPageBoundaries.get(target.node)?.wait(backward, target.loading);
      else target.node.focus();
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(node);
    const mutations = new MutationObserver(restore);
    mutations.observe(node, { childList: true, subtree: true, attributes: true, attributeFilter: ['data-document-page-loading'] });
    return () => { observer.disconnect(); mutations.disconnect(); };
  }, [mounted]);

  return <div ref={container} role="group" tabIndex={-1} data-document-page={identity} data-page-mounted={mounted}
    style={{ display: 'flow-root', height: mounted ? undefined : height.current }}>
    {mounted ? children() : null}
  </div>;
}
