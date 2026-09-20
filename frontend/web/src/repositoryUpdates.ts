import { validateGraphState } from './graphState';
import type { PendingView, RepositoryRevision, RepositoryView } from './types';

// Verified Git evidence affects branch inclusion as well as memory. A compact
// pending-view response refreshes that projection without fetching all records.
export function revisionCovers(current: RepositoryRevision, wanted: RepositoryRevision) {
  return BigInt(current.graph) >= BigInt(wanted.graph) && BigInt(current.pending) >= BigInt(wanted.pending) && BigInt(current.evidence ?? '0') >= BigInt(wanted.evidence ?? '0');
}
export function parseRevision(value: unknown): RepositoryRevision | null {
  const r = value as RepositoryRevision | null;
  return r && typeof r.graph === 'string' && /^\d+$/.test(r.graph) && typeof r.pending === 'string' && /^\d+$/.test(r.pending) && (r.evidence === undefined || (typeof r.evidence === 'string' && /^\d+$/.test(r.evidence))) ? r : null;
}
export function pendingViewNeedsFull(view: RepositoryView, pending: PendingView): boolean {
  if (!view.revision || view.revision.graph !== pending.revision.graph) return true;
  validateGraphState(pending.graph,pending.revision);
  const existing = new Set(view.snapshots.map(s => s.id));
  const known = new Set([...existing, ...pending.snapshots.map(s => s.id)]);
  if (pending.graph.snapshot_ids.some(id => !known.has(id))) return true;
  return pending.snapshots.some(s => !existing.has(s.id) &&
    [...(s.parents ?? []), ...(s.graft_parents ?? [])].some(id => !known.has(id)));
}
/** Replace only the live projection. Retained history and referenced snapshots
 * remain untouched; stale, unreferenced sliding captures do not accumulate. */
export function mergePendingView(view: RepositoryView, pending: PendingView): RepositoryView {
  if (!view.revision || view.revision.graph !== pending.revision.graph || BigInt(view.revision.pending) > BigInt(pending.revision.pending) || BigInt(view.revision.evidence ?? '0') > BigInt(pending.revision.evidence ?? '0')) return view;
  validateGraphState(pending.graph,pending.revision);
  const keep = new Set(pending.graph.snapshot_ids);
  const memberships = new Map(view.snapshots.map(s => [s.id,s.branches]));
  const snapshots = new Map(view.snapshots.filter(s => keep.has(s.id)).map(s => [s.id,s]));
  for (const s of pending.snapshots) snapshots.set(s.id,{...s,branches:memberships.get(s.id)});
  return {...view,graph:pending.graph,revision:pending.revision,pending:pending.pending,snapshots:[...snapshots.values()]};
}

type Listener = { changed: (r: RepositoryRevision) => void; failed: () => void };
const streams = new Map<string, { source: EventSource; listeners: Set<Listener> }>();
export function subscribeRepository(url: string, listener: Listener) {
  let stream = streams.get(url);
  if (!stream) {
    const source = new EventSource(url, {withCredentials: true});
    stream = {source, listeners: new Set()};
    const current = stream;
    source.addEventListener('revision', event => {
      try {
        const r = parseRevision(JSON.parse((event as MessageEvent).data));
        if (!r) throw new Error('Invalid repository revision');
        for (const l of current.listeners) l.changed(r);
      }
      catch { for (const l of current.listeners) l.failed(); }
    });
    source.onerror = () => { for (const l of current.listeners) l.failed(); };
    streams.set(url, stream);
  }
  stream.listeners.add(listener);
  return () => { stream.listeners.delete(listener); if (!stream.listeners.size) { stream.source.close(); streams.delete(url); } };
}
