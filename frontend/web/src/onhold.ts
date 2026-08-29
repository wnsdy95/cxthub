// On Hold logic — ContextView (badge count) and OnHoldView (tab render) share it.
// The badge number and the number of rows shown in the tab must match, so a single definition is used.
import type { Ref, Snapshot, Pending, Unsync } from './types';
import { parseBranchLifecycleRef } from './branchLifecycle';

/** Snapshot IDs reachable from explicit roots under cxthub's immutable graph
 *  rule. Natural parents and graft overlay parents are equally valid for
 *  membership; first-parent presentation is handled separately by graph.ts. */
export function reachableSnapshotIds(targets: Iterable<string>, snapshots: Snapshot[]): Set<string> {
  const byId = new Map(snapshots.map((s) => [s.id, s]));
  const seen = new Set<string>();
  const stack = [...targets];
  while (stack.length > 0) {
    const id = stack.pop() as string;
    if (!id || seen.has(id)) continue;
    const s = byId.get(id);
    if (!s) continue;
    seen.add(id);
    for (const p of s.parents ?? []) stack.push(p);
    for (const g of s.graft_parents ?? []) stack.push(g);
  }
  return seen;
}

/** Shared timeline = a set of snapshots reachable from parent chains in server branch/session refs.
 *  Reachability is the same as on the server: parents ∪ graft_parents — after a graft (diverged append),
 *  the old head chain can only be reached via overlay edges. If not taken, the committed session
 *  will reappear as an uncommitted one, and the graph will show orphaned history before the merge. */
export function sharedReachable(refs: Ref[], snapshots: Snapshot[]): Set<string> {
  // session ref preserves residual sessions from partial merges but not git branch membership.
  const roots = refs
    .filter((r) => r.kind === 'branch' || r.kind === 'session' || parseBranchLifecycleRef(r) !== null)
    .map((r) => r.target);
  return reachableSnapshotIds(roots, snapshots);
}

/** Push wait cluster — a bunch of unpushed commits linked by chains (parent edges).
 *  On Hold is a place where multiple unmerged commits gather, so the grouping criterion is not the pointer (author·branch),
 *  but the actual ancestry connection: commits belong to exactly one cluster (no duplicates),
 *  and labels are tip pointers (multiple possible) that point to this bunch. */
export interface HoldCluster {
  /** Unsynchronized pointers (author·branch label source) that point to this bunch. */
  tips: Unsync[];
  /** Commits in the bunch (newest first). */
  chain: Snapshot[];
}

/** Unsynchronized pointers → Connectivity clusters (divide hold set before reaching shared timeline). */
export function unsyncChains(unsyncs: Unsync[], snapshots: Snapshot[], shared: Set<string>): HoldCluster[] {
  const byId = new Map(snapshots.map((s) => [s.id, s]));
  // 1) Hold set: Follow all parents from each pointer target, truncated at shared timeline.
  const hold = new Set<string>();
  for (const u of unsyncs) {
    const stack = [u.target];
    while (stack.length > 0) {
      const id = stack.pop() as string;
      if (!id || hold.has(id) || shared.has(id)) continue;
      const s = byId.get(id);
      if (!s) continue;
      hold.add(id);
      for (const p of s.parents ?? []) stack.push(p);
      for (const g of s.graft_parents ?? []) stack.push(g);
    }
  }
  // 2) Connectivity clustering (union-find, only parent edges within holds).
  const root = new Map<string, string>();
  const find = (x: string): string => {
    let r = x;
    while (root.get(r) !== r) r = root.get(r) as string;
    let c = x;
    while (root.get(c) !== r) {
      const n = root.get(c) as string;
      root.set(c, r);
      c = n;
    }
    return r;
  };
  for (const id of hold) root.set(id, id);
  for (const id of hold) {
    const snap = byId.get(id);
    for (const p of [...(snap?.parents ?? []), ...(snap?.graft_parents ?? [])]) {
      if (hold.has(p)) root.set(find(id), find(p));
    }
  }
  // 3) Cluster assembly: Commits are exactly one chunk, tip pointer is in the chunk containing the target.
  const byRoot = new Map<string, Snapshot[]>();
  for (const id of hold) {
    const r = find(id);
    const list = byRoot.get(r) ?? [];
    list.push(byId.get(id) as Snapshot);
    byRoot.set(r, list);
  }
  const out: HoldCluster[] = [];
  for (const [r, list] of byRoot) {
    const tips = unsyncs.filter((u) => hold.has(u.target) && find(u.target) === r);
    list.sort((a, b) => (a.created_at < b.created_at ? 1 : -1));
    out.push({ tips, chain: list });
  }
  return out.sort((a, b) => {
    const at = a.tips[0]?.updated_at ?? a.chain[0]?.created_at ?? '';
    const bt = b.tips[0]?.updated_at ?? b.chain[0]?.created_at ?? '';
    return at < bt ? 1 : -1;
  });
}

/** Uncommitted session = durable capture not yet on the shared timeline.
 *  Determined by target reachability, not session essence (previous method hid one tip session and marked committed sessions as uncommitted):
 *    - Target reachable from branch ref = Committed → Excluded.
 *    - Target in unsync cluster chain = On Hold push pending → Excluded.
 *    - Dismissed = User-hidden session (data preserved) → Excluded.
 *  Remaining (unreachable, unclustered, undismissed) are displayed as one line each — multiple per terminal simultaneously. */
export function orphanPendings(
  pendings: Pending[],
  refs: Ref[],
  snapshots: Snapshot[],
  clusters: HoldCluster[],
): Pending[] {
  const shared = sharedReachable(refs, snapshots);
  const inCluster = new Set<string>();
  for (const c of clusters) for (const s of c.chain) inCluster.add(s.id);
  return pendings
    .filter((p) => !p.dismissed && !shared.has(p.target) && !inCluster.has(p.target))
    .sort((a, b) => (a.updated_at < b.updated_at ? 1 : -1));
}

/** Branch-specific pending count = Number of rows displayed in the On Hold tab for that branch filter (push pending commits + orphan sessions).
 *  Clusters are counted if any tip pointer is on that branch, following the same rule. */
export function holdCounts(
  refs: Ref[],
  snapshots: Snapshot[],
  unsyncs: Unsync[],
  pendings: Pending[],
): Map<string, number> {
  const shared = sharedReachable(refs, snapshots);
  const clusters = unsyncChains(unsyncs, snapshots, shared);
  const orphans = orphanPendings(pendings, refs, snapshots, clusters);
  const m = new Map<string, number>();
  const bump = (b: string, n: number) => m.set(b, (m.get(b) ?? 0) + n);
  for (const c of clusters) {
    for (const b of new Set(c.tips.map((u) => u.branch))) bump(b, c.chain.length);
  }
  for (const p of orphans) bump(p.branch, 1);
  return m;
}
