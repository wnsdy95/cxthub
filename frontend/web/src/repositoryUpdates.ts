import type { PendingView, RepositoryRevision, RepositoryView } from './types';

export function revisionCovers(current: RepositoryRevision, wanted: RepositoryRevision) {
  return BigInt(current.graph) >= BigInt(wanted.graph) && BigInt(current.pending) >= BigInt(wanted.pending);
}
export function parseRevision(value: unknown): RepositoryRevision | null {
  const r = value as RepositoryRevision | null;
  return r && typeof r.graph === 'string' && /^\d+$/.test(r.graph) && typeof r.pending === 'string' && /^\d+$/.test(r.pending) ? r : null;
}
export function pendingViewNeedsFull(view: RepositoryView, pending: PendingView): boolean {
  if (!view.revision || view.revision.graph !== pending.revision.graph) return true;
  const existing = new Set(view.snapshots.map(s => s.id));
  const known = new Set([...existing, ...pending.snapshots.map(s => s.id)]);
  return pending.snapshots.some(s => !existing.has(s.id) &&
    [...(s.parents ?? []), ...(s.graft_parents ?? [])].some(id => !known.has(id)));
}
/** Replace only the live projection. Retained history and referenced snapshots
 * remain untouched; stale, unreferenced sliding captures do not accumulate. */
export function mergePendingView(view: RepositoryView, pending: PendingView): RepositoryView {
  if (!view.revision || view.revision.graph !== pending.revision.graph || BigInt(view.revision.pending) > BigInt(pending.revision.pending)) return view;
  const retained = new Set(view.refs.map(r => r.target));
  for (const e of view.history) for (const id of [e.source, e.target, e.shared_target]) if (id) retained.add(id);
  for (const e of view.reflog) { retained.add(e.old); retained.add(e.new); }
  for (const s of [...view.snapshots, ...pending.snapshots]) for (const id of [...(s.parents ?? []), ...(s.graft_parents ?? [])]) retained.add(id);
  const oldTargets = new Set(view.pending.map(p => p.target));
  const memberships = new Map(view.snapshots.map(s => [s.id, s.branches]));
  const snapshots = new Map(view.snapshots.filter(s => !oldTargets.has(s.id) || retained.has(s.id)).map(s => [s.id, s]));
  for (const s of pending.snapshots) {
    // /pending-view does not project branch membership. It must neither erase
    // nor manufacture graph-owned fields at the same graph revision.
    snapshots.set(s.id, {...s, branches: memberships.get(s.id)});
  }
  return {...view, revision: pending.revision, pending: pending.pending, snapshots: [...snapshots.values()]};
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
