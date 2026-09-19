import type { ContextSemantics, HistoryEvent, Snapshot } from './types';
import { GraphIndex } from './graphIndex';

export type { BranchLineage } from './types';

export { conversationParents } from './graphIndex';

/** Reject a torn/unsupported generation before replacing the query cache. */
export function validateContextSemantics(history: HistoryEvent[], semantics?: ContextSemantics) {
  if (!semantics) return; // rolling upgrade compatibility
  if (semantics.version !== 1) throw new Error('Unsupported context semantics version');
  const events = new Map(history.map(e => [e.id, e]));
  for (const fact of semantics.merges) {
    if (!events.get(fact.event_id)?.pr || (fact.birth_id && !events.has(fact.birth_id))) {
      throw new Error('Incomplete context semantics generation');
    }
  }
}

/** Server-issued completion is an immutable operation fact. Current placement
 * and conversation lineage may change independently after a valid Join. */
export function completedBranchEvidence(_snapshots: Snapshot[], history: HistoryEvent[], _index?: GraphIndex, semantics?: ContextSemantics) {
  if (semantics) {
    validateContextSemantics(history, semantics);
    const events = new Map(history.map(e => [e.id, e]));
    return semantics.merges.map(fact => {
      const merge = events.get(fact.event_id)!;
      return { merge, birth: fact.birth_id ? events.get(fact.birth_id) : undefined, lineage: fact.lineage,
        completed: fact.completed, placementIntact: fact.placement_intact, sourceAvailable: fact.source_available };
    }).sort((a, b) => Date.parse(b.merge.created_at) - Date.parse(a.merge.created_at) || a.merge.id.localeCompare(b.merge.id));
  }
  return [];
}
