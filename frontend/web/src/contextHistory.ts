import type { HistoryEvent, Ref, RefLogEntry, Snapshot } from './types';
import { reachableSnapshotIds } from './onhold';

/** Resolve recorded identity heads without relying on event order or clocks.
 * A birth's dependency releases a name owned by another identity, so only
 * same-identity rename/archive dependencies supersede that identity's head. */
export function historyBranchHeads(history: HistoryEvent[]): Map<string, HistoryEvent> {
  const bindings = history.filter(e => ['birth', 'orphan', 'rename', 'archive'].includes(e.kind));
  const byEvent = new Map(bindings.map(e => [e.id, e]));
  const superseded = new Set(bindings.filter(e => (e.kind === 'rename' || e.kind === 'archive')
    && byEvent.get(e.binding_parent ?? '')?.branch_id === e.branch_id).map(e => e.binding_parent));
  const heads = new Map<string, HistoryEvent>();
  const ambiguous = new Set<string>();
  for (const event of bindings) if (!superseded.has(event.id)) {
    if (heads.has(event.branch_id)) ambiguous.add(event.branch_id);
    heads.set(event.branch_id, event);
  }
  for (const id of ambiguous) heads.delete(id);
  return heads;
}

export interface PreviousProgressGroup {
  key: string;
  branch: string;
  before: string;
  after: string;
  createdAt: string;
  snapshotIds: Set<string>;
  collapsibleIds: Set<string>;
}

/** Publication survives a later ref movement. Observation/retention events
 * (especially position and memory_source) do not attest a committed timeline. */
export function historicalSnapshotIds(entries: RefLogEntry[], snapshots: Snapshot[], history: HistoryEvent[] = []): Set<string> {
  return reachableSnapshotIds(
    [...entries.filter((entry) => entry.kind === 'branch').flatMap((entry) => [entry.old, entry.new]),
      ...history.flatMap((event) => event.kind === 'advance' ? [event.source, event.target]
        : event.kind === 'publish' ? [event.target]
        : event.kind === 'pr-merge' && event.pr_completed ? [event.source, event.target, event.shared_target] : [])
        .filter((id): id is string => Boolean(id))],
    snapshots,
  );
}

/** Resolve current names through stable IDs. A released/reused name is never
 * sufficient to assign an old snapshot or a legacy ref to an identity. */
export function graphBranchBindings(refs: Ref[], history: HistoryEvent[]) {
  const heads = historyBranchHeads(history);
  const active = new Map([...heads.values()].filter(e => e.kind !== 'archive').map(e => [e.branch, e.branch_id]));
  const claims = new Map<string, Set<string>>();
  for (const e of history) {
    const ids = claims.get(e.branch) ?? new Set<string>();
    ids.add(e.branch_id);
    claims.set(e.branch, ids);
  }
  const legacyKey = (name: string) => {
    const ids = claims.get(name);
    return ids?.size === 1 ? [...ids][0] : `legacy:${name}`;
  };
  const refKey = (ref: Ref) => ref.branch_id ?? active.get(ref.name) ?? legacyKey(ref.name);
  const snapshotKey = (name: string) => {
    if ((claims.get(name)?.size ?? 0) > 1) return undefined;
    return claims.has(name) ? legacyKey(name) : refs.find(r => r.kind === 'branch' && r.name === name)?.branch_id ?? legacyKey(name);
  };
  const label = (key: string, fallback: string) => heads.get(key)?.branch ?? refs.find(r => r.kind === 'branch' && refKey(r) === key)?.name ?? fallback;
  return { refKey, snapshotKey, label };
}

/** Fold only paths supported by a recorded movement away from their tip.
 * Reflog does not identify the cause (reset, restore, etc.), so do not infer one.
 * Unknown/missing ancestry stays visible rather than manufacturing a fork. */
export function previousProgressGroups(
  refs: Ref[], snapshots: Snapshot[], entries: RefLogEntry[],
  history: HistoryEvent[] = [], position?: { branch: string; branch_id?: string; snapshot: string },
): PreviousProgressGroup[] {
  const heads = historyBranchHeads(history);
  const active = new Map([...heads.values()].filter(e => e.kind !== 'archive').map(e => [e.branch, e.branch_id]));
  const labels = new Map([...heads].map(([id, event]) => [id, event.branch]));
  const keyOf = (name: string, identity?: string) => (identity && heads.has(identity) ? identity : undefined) || active.get(name) || name;
  refs = refs.map(ref => ref.kind === 'branch' ? { ...ref, name: keyOf(ref.name) } : ref);
  // A legacy reflog lacks identity. Once a name has explicit generations, its
  // raw name alone cannot prove which logical branch moved.
  const movements = [...entries.filter(e => !active.has(e.name)), ...history.filter((event) => event.kind === 'advance' && event.source && event.target)
    .map((event) => ({ kind: 'branch', name: keyOf(event.branch, heads.has(event.branch_id) ? event.branch_id : undefined), old: event.source!, new: event.target!, created_at: event.created_at }))];
  if (position) {
    const branch = keyOf(position.branch, position.branch_id);
    // This is an explicit browsing projection, never a server ref mutation.
    const roots = new Map<string, string>();
    for (const ref of refs) if (ref.kind === 'branch' && ref.name === branch) roots.set(ref.target, '');
    for (const event of history) if (position.branch_id ? event.branch_id === position.branch_id : keyOf(event.branch, heads.has(event.branch_id) ? event.branch_id : undefined) === branch) {
      for (const id of [event.source, event.target, event.shared_target]) if (id) roots.set(id, event.created_at);
    }
    for (const [root, at] of roots) movements.push({ kind: 'branch', name: branch, old: root, new: position.snapshot, created_at: at });
    refs = [...refs.filter((ref) => ref.kind !== 'branch' || ref.name !== branch),
      { kind: 'branch', name: branch, target: position.snapshot, repo_id: refs[0]?.repo_id ?? '' }];
  }
  const byId = new Map(snapshots.map((snapshot) => [snapshot.id, snapshot]));
  const branches = new Map(refs.filter((ref) => ref.kind === 'branch').map((ref) => [ref.name, ref.target]));
  const cache = new Map<string, { ids: Set<string>; complete: boolean }>();
  function closure(root: string) {
    const cached = cache.get(root);
    if (cached) return cached;
    const ids = new Set<string>();
    const stack = [root];
    let complete = true;
    while (stack.length) {
      const id = stack.pop()!;
      if (ids.has(id)) continue;
      ids.add(id);
      const snapshot = byId.get(id);
      if (!snapshot) { complete = false; continue; }
      stack.push(...(snapshot.parents ?? []), ...(snapshot.graft_parents ?? []));
    }
    const result = { ids, complete };
    cache.set(root, result);
    return result;
  }

  const groups: PreviousProgressGroup[] = [];
  const seen = new Set<string>();
  for (const entry of movements) {
    if (entry.kind !== 'branch' || !entry.old || !entry.new || entry.old === entry.new) continue;
    const head = branches.get(entry.name);
    if (!head || !byId.has(entry.old) || !byId.has(entry.new)) continue;
    const current = closure(head);
    if (!current.complete || current.ids.has(entry.old) || !current.ids.has(entry.new)) continue;
    const after = closure(entry.new);
    const before = closure(entry.old);
    if (!after.complete || !before.complete || after.ids.has(entry.old)) continue;
    // Repeated delivery of the same transition should not duplicate a path.
    const key = JSON.stringify([entry.name, entry.old, entry.new]);
    if (seen.has(key)) continue;
    seen.add(key);
    groups.push({
      key, branch: labels.get(entry.name) ?? entry.name, before: entry.old, after: entry.new, createdAt: entry.created_at,
      snapshotIds: new Set([...before.ids].filter((id) => !current.ids.has(id))),
      collapsibleIds: new Set(),
    });
  }

  const candidates = new Set(groups.flatMap((group) => [...group.snapshotIds]));
  // Any visible child needs its entire path, including a pending/local child.
  // Branch/session roots protect a teammate using the previous path as well.
  const visibleRoots = [
    ...snapshots.filter((snapshot) => !candidates.has(snapshot.id)).map((snapshot) => snapshot.id),
    ...refs.filter((ref) => ref.kind === 'branch' || ref.kind === 'session').map((ref) => ref.target),
  ];
  const protectedIds = reachableSnapshotIds(visibleRoots, snapshots);
  for (const group of groups) {
    group.collapsibleIds = new Set([...group.snapshotIds].filter((id) => !protectedIds.has(id)));
  }
  return groups;
}

/** Expanding either of two overlapping groups also reveals their shared path. */
export function hiddenProgressIds(
  groups: PreviousProgressGroup[], expandedKeys: ReadonlySet<string>,
): Set<string> {
  const hidden = new Set(groups.flatMap((group) => [...group.collapsibleIds]));
  for (const group of groups) {
    if (expandedKeys.has(group.key)) for (const id of group.collapsibleIds) hidden.delete(id);
  }
  return hidden;
}
