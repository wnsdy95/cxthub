import type { Ref, Snapshot } from './types';
import { archivedBranchMarkers, parseBranchLifecycleRef } from './branchLifecycle';
import { reachableSnapshotIds, sharedReachable } from './onhold';

export interface GraphSnapshotStatus {
  pushed: Set<string>;
  unpushed: Set<string>;
  uncommitted: Set<string>;
  archivedOnly: Set<string>;
  archivedBranches: number;
}

/** Classifies every rendered graph snapshot into one workflow tier. Archived
 * history is a fourth, orthogonal lifecycle state, but only snapshots unique
 * to archived branches are collapsed; shared ancestors and appended history
 * remain visible in the active graph. */
export function classifyGraphSnapshots(
  refs: Ref[],
  snapshots: Snapshot[],
  uncommittedInput: ReadonlySet<string> = new Set<string>(),
): GraphSnapshotStatus {
  const ids = new Set(snapshots.map((snapshot) => snapshot.id));
  const shared = sharedReachable(refs, snapshots);
  const markers = archivedBranchMarkers(refs);
  const archivedReachable = reachableSnapshotIds(markers.map((marker) => marker.target), snapshots);
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
  return { pushed, unpushed, uncommitted, archivedOnly, archivedBranches: markers.length };
}
