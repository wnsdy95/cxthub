import type { HistoryEvent, Snapshot } from './types';
import { GraphIndex } from './graphIndex';

export type BranchLineage = 'natural' | 'unchanged' | 'graft' | 'disconnected' | 'missing' | 'unknown' | 'orphan';

export { conversationParents } from './graphIndex';

/** Server-issued completion is an immutable operation fact. Current placement
 * and conversation lineage may change independently after a valid Join. */
export function completedBranchEvidence(snapshots: Snapshot[], history: HistoryEvent[], index = new GraphIndex(snapshots)) {
  if (index.issues.length) return [];
  const { byId } = index;
  const birthsByBranch = new Map<string, HistoryEvent[]>();
  for (const h of history) if (h.kind === 'birth' || h.kind === 'orphan') {
    const births = birthsByBranch.get(h.branch_id) ?? [];
    births.push(h); birthsByBranch.set(h.branch_id, births);
  }
  return history.filter(h => h.kind === 'pr-merge' && h.pr_completed && h.pr).map(merge => {
    const births = birthsByBranch.get(merge.source_branch_id ?? '') ?? [];
    const birth = births.length === 1 ? births[0] : undefined;
    const sourceAvailable = Boolean(merge.source && byId.has(merge.source));
    const placementIntact = Boolean(sourceAvailable && merge.target && merge.shared_target && byId.has(merge.target)
      && byId.has(merge.shared_target) && index.reaches(merge.target, merge.source!)
      && index.reaches(merge.target, merge.shared_target));
    let lineage: BranchLineage;
    if (!sourceAvailable) lineage = 'missing';
    else if (!birth) lineage = 'unknown';
    else if (birth.kind === 'orphan') lineage = 'orphan';
    else if (!birth.source || !byId.has(birth.source)) lineage = 'missing';
    else if (birth.source === merge.source) lineage = 'unchanged';
    else if (index.reaches(merge.source!, birth.source, 'conversation')) lineage = 'natural';
    else if (!index.closure(merge.source!, 'conversation').complete) lineage = 'missing';
    else if (index.reaches(merge.source!, birth.source)) lineage = 'graft';
    else lineage = index.closure(merge.source!).complete ? 'disconnected' : 'missing';
    return { merge, birth, lineage, completed: true as const, placementIntact, sourceAvailable };
  }).sort((a, b) => Date.parse(b.merge.created_at) - Date.parse(a.merge.created_at) || a.merge.id.localeCompare(b.merge.id));
}
