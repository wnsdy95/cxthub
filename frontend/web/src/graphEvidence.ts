import type { HistoryEvent, Snapshot } from './types';

export type BranchLineage = 'natural' | 'unchanged' | 'graft' | 'disconnected' | 'missing' | 'unknown' | 'orphan';

/** Old destructive grafts put the append seam in parents[0]. Like overlay
 * parents, that edge proves inclusion but not native conversation continuity. */
export function conversationParents(snapshot: Snapshot): string[] {
  const parents = snapshot.parents ?? [];
  return snapshot.grafted && !snapshot.graft_parents?.length ? parents.slice(1) : parents;
}

/** A PR completion proves inclusion, not the path taken after branch creation.
 * Classify those facts separately; an append of previous main cannot prove
 * that the incoming conversation started at that branch's birth. */
export function completedBranchEvidence(snapshots: Snapshot[], history: HistoryEvent[]) {
  const byId = new Map(snapshots.map(s => [s.id, s]));
  const cache = new Map<string, { ids: Set<string>; complete: boolean }>();
  function closure(root: string, graft: boolean) {
    const key = `${graft}:${root}`;
    const cached = cache.get(key);
    if (cached) return cached;
    const ids = new Set<string>();
    const stack = [root];
    let complete = true;
    while (stack.length) {
      const id = stack.pop()!;
      if (ids.has(id)) continue;
      ids.add(id);
      const s = byId.get(id);
      if (!s) { complete = false; continue; }
      stack.push(...(graft ? [...(s.parents ?? []), ...(s.graft_parents ?? [])] : conversationParents(s)));
    }
    const result = { ids, complete };
    cache.set(key, result);
    return result;
  }
  return history.filter(h => h.kind === 'pr-merge' && h.pr_completed && h.pr).map(merge => {
    const births = history.filter(h => (h.kind === 'birth' || h.kind === 'orphan') && h.branch_id === merge.source_branch_id);
    const birth = births.length === 1 ? births[0] : undefined;
    const sourceAvailable = Boolean(merge.source && byId.has(merge.source));
    const merged = Boolean(sourceAvailable && merge.target && merge.shared_target && byId.has(merge.target)
      && byId.has(merge.shared_target) && closure(merge.target, true).ids.has(merge.source!)
      && closure(merge.target, true).ids.has(merge.shared_target));
    let lineage: BranchLineage;
    if (!sourceAvailable) lineage = 'missing';
    else if (!birth) lineage = 'unknown';
    else if (birth.kind === 'orphan') lineage = 'orphan';
    else if (!birth.source || !byId.has(birth.source)) lineage = 'missing';
    else if (birth.source === merge.source) lineage = 'unchanged';
    else if (closure(merge.source!, false).ids.has(birth.source)) lineage = 'natural';
    else if (!closure(merge.source!, false).complete) lineage = 'missing';
    else if (closure(merge.source!, true).ids.has(birth.source)) lineage = 'graft';
    else lineage = closure(merge.source!, true).complete ? 'disconnected' : 'missing';
    return { merge, birth, lineage, merged, sourceAvailable };
  }).sort((a, b) => Date.parse(b.merge.created_at) - Date.parse(a.merge.created_at) || a.merge.id.localeCompare(b.merge.id));
}
