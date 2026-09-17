import type { HistoryEvent, Ref, Snapshot } from './types';
import { archivedBranchMarkers, parseBranchLifecycleRef } from './branchLifecycle';
import { reachableSnapshotIds, sharedReachable } from './onhold';
import { completedBranchEvidence } from './graphEvidence';
import { historyBranchHeads } from './contextHistory';

export interface GraphSnapshotStatus {
  pushed: Set<string>;
  unpushed: Set<string>;
  uncommitted: Set<string>;
  tagged: Set<string>;
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

/** Inclusion alone is not a join: an unused branch starts on main too.
 * Require an identity-bound completion, or an explicit legacy graft into
 * content originating on that branch. Unknown history stays archived. */
export function classifyBranchHistoryMarkers(
  refs: Ref[],
  snapshots: Snapshot[],
  primaryBranch?: string,
  history: HistoryEvent[] = [],
): BranchHistoryMarker[] {
  const branches = refs.filter((ref) => ref.kind === 'branch');
  const primary =
    (primaryBranch && branches.find((ref) => ref.name === primaryBranch)) ||
    branches.find((ref) => ref.name === 'main') ||
    branches.find((ref) => ref.name === 'master') ||
    branches[0];
  const primaryReachable = reachableSnapshotIds(primary ? [primary.target] : [], snapshots);
  const evidence = completedBranchEvidence(snapshots, history);
  const heads = historyBranchHeads(history);
  const byId = new Map(snapshots.map(s => [s.id, s]));
  const joined = (branch: string, target: string) => {
    const identities = [...heads.values()].filter(e => e.branch === branch);
    if (identities.length) return identities.length === 1 && identities[0].kind === 'archive'
      && evidence.some(e => e.merged && e.merge.source_branch_id === identities[0].branch_id && e.merge.source === target);
    return snapshots.some(s => primaryReachable.has(s.id) && s.branch !== branch
      && s.graft_parents?.includes(target) && byId.get(target)?.branch === branch);
  };
  return archivedBranchMarkers(refs).map((marker) => ({
    ...marker,
    kind: primaryReachable.has(marker.target) && joined(marker.branch, marker.target) ? 'joined' : 'archived',
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
  historicalIds: ReadonlySet<string> = new Set<string>(),
  history: HistoryEvent[] = [],
): GraphSnapshotStatus {
  const ids = new Set(snapshots.map((snapshot) => snapshot.id));
  const shared = sharedReachable(refs, snapshots);
  for (const id of historicalIds) if (ids.has(id)) shared.add(id);
  const historyMarkers = classifyBranchHistoryMarkers(refs, snapshots, primaryBranch, history);
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
  // A server tag preserves content without attesting a branch publication.
  // Pending takes precedence: adding a tag cannot commit a live capture.
  const tagReachable = reachableSnapshotIds(refs.filter(r => r.kind === 'tag' && !parseBranchLifecycleRef(r)).map(r => r.target), snapshots);
  const tagged = new Set([...tagReachable].filter(id => !shared.has(id) && !uncommitted.has(id)));
  const unpushed = new Set(
    snapshots
      .filter((snapshot) => !shared.has(snapshot.id) && !uncommitted.has(snapshot.id) && !tagged.has(snapshot.id))
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
    tagged,
    archivedOnly,
    archivedBranches: archived.length,
    joined,
    archived,
  };
}
