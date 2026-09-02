import type { Ref, Snapshot } from './types';
import { archivedBranchMarkers, parseBranchLifecycleRef } from './branchLifecycle';
import { reachableSnapshotIds, sharedReachable } from './onhold';

export interface GraphSnapshotStatus {
  pushed: Set<string>;
  unpushed: Set<string>;
  uncommitted: Set<string>;
  archivedOnly: Set<string>;
  archivedBranches: number;
  joined: BranchHistoryMarker[];
  archived: Array<{
    branch: string;
    target: string;
    uniqueCount: number;
    targetAvailable: boolean;
  }>;
}

export interface BranchHistoryMarker {
  branch: string;
  target: string;
  kind: 'joined' | 'archived';
}

/** A deleted Git ref is not necessarily archived context. If its recorded tip
 * is reachable from the primary branch, the branch was joined and remains a
 * named historical lane. Only tips outside that lineage are true archives. */
export function classifyBranchHistoryMarkers(
  refs: Ref[],
  snapshots: Snapshot[],
  primaryBranch?: string,
): BranchHistoryMarker[] {
  const branches = refs.filter((ref) => ref.kind === 'branch');
  const primary =
    (primaryBranch && branches.find((ref) => ref.name === primaryBranch)) ||
    branches.find((ref) => ref.name === 'main') ||
    branches.find((ref) => ref.name === 'master') ||
    branches[0];
  const primaryReachable = reachableSnapshotIds(primary ? [primary.target] : [], snapshots);
  return archivedBranchMarkers(refs).map((marker) => ({
    ...marker,
    kind: primaryReachable.has(marker.target) ? 'joined' : 'archived',
  }));
}

/** Classifies every rendered graph snapshot into one workflow tier. Joined
 * branch history stays in the active graph; only snapshots unique to genuinely
 * archived branches are collapsed. */
export function classifyGraphSnapshots(
  refs: Ref[],
  snapshots: Snapshot[],
  uncommittedInput: ReadonlySet<string> = new Set<string>(),
  primaryBranch?: string,
): GraphSnapshotStatus {
  const ids = new Set(snapshots.map((snapshot) => snapshot.id));
  const shared = sharedReachable(refs, snapshots);
  const historyMarkers = classifyBranchHistoryMarkers(refs, snapshots, primaryBranch);
  const joined = historyMarkers.filter((marker) => marker.kind === 'joined');
  const archivedMarkers = historyMarkers.filter((marker) => marker.kind === 'archived');
  const archivedReachable = reachableSnapshotIds(
    archivedMarkers.map((marker) => marker.target),
    snapshots,
  );
  const activeRoots = refs
    .filter(
      (ref) =>
        ref.kind === 'branch' ||
        ref.kind === 'session' ||
        (ref.kind === 'tag' && parseBranchLifecycleRef(ref) === null),
    )
    .map((ref) => ref.target);
  const activeReachable = reachableSnapshotIds(activeRoots, snapshots);
  const archivedOnly = new Set(
    [...archivedReachable].filter((id) => ids.has(id) && !activeReachable.has(id)),
  );
  const archived = archivedMarkers.map(({ branch, target }) => ({
    branch,
    target,
    uniqueCount: [...reachableSnapshotIds([target], snapshots)].filter(
      (id) => ids.has(id) && !activeReachable.has(id),
    ).length,
    targetAvailable: ids.has(target),
  }));
  const uncommitted = new Set(
    [...uncommittedInput].filter((id) => ids.has(id) && !shared.has(id)),
  );
  const unpushed = new Set(
    snapshots
      .filter((snapshot) => !shared.has(snapshot.id) && !uncommitted.has(snapshot.id))
      .map((snapshot) => snapshot.id),
  );
  const pushed = new Set(
    snapshots
      .filter(
        (snapshot) =>
          shared.has(snapshot.id) && !archivedOnly.has(snapshot.id) && !uncommitted.has(snapshot.id),
      )
      .map((snapshot) => snapshot.id),
  );
  return {
    pushed,
    unpushed,
    uncommitted,
    archivedOnly,
    archivedBranches: archived.length,
    joined,
    archived,
  };
}
