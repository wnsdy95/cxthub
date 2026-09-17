import type { HistoryEvent, Pending, Ref, Snapshot, Unsync } from './types';
import { reachableSnapshotIds, unsyncChains } from './onhold';

/** Display closure is independent of publication. Keep intermediate captures
 * needed to connect a pending/unsync/tag/history root without promoting them. */
export function repositoryGraph(snapshots: Snapshot[], refs: Ref[], history: HistoryEvent[],
  shared: ReadonlySet<string>, pendings: Pending[], unsyncs: Unsync[]) {
  const inCluster = new Set(unsyncChains(unsyncs, snapshots, new Set(shared)).flatMap(c => c.chain.map(s => s.id)));
  const pendingRoots = pendings.filter(p => !p.dismissed && !shared.has(p.target) && !inCluster.has(p.target)).map(p => p.target);
  const pendingPath = reachableSnapshotIds(pendingRoots, snapshots);
  const uncommittedIds = new Set(snapshots.filter(s => pendingPath.has(s.id) && !shared.has(s.id)
    && !inCluster.has(s.id) && s.message?.startsWith('hook: ')).map(s => s.id));
  const roots = [
    ...snapshots.filter(s => !s.message?.startsWith('hook: ')).map(s => s.id),
    ...shared, ...inCluster, ...pendingRoots, ...refs.filter(r => r.kind === 'tag').map(r => r.target),
    ...history.flatMap(e => [e.source, e.target, e.shared_target, e.memory_source].filter((id): id is string => Boolean(id))),
  ];
  const visible = reachableSnapshotIds(roots, snapshots);
  return { uncommittedIds, graphSnapshots: snapshots.filter(s => visible.has(s.id)) };
}
