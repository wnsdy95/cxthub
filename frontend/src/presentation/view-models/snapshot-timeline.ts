/**
 * presentation/view-models/snapshot-timeline: Snapshot timeline view-model.
 *
 * Screen 3 — Corresponds to GitHub "commit history/DAG". Displays snapshots of a specific branch in a time-order/DAG structure, rendering ref badges (HEAD/branch/tag) along with them.
 * Framework-independent. Defines only the (state, actions) shape.
 *
 * Dependencies: application inbound port interfaces, domain types.
 */

import type { Snapshot, Ref } from '../../domain/index.js';
import type { ListSessionsUseCase } from '../../application/index.js';

/** Snapshot timeline screen state. */
export interface SnapshotTimelineState {
  loading: boolean;
  error: string | null;
/** Snapshot list (sorted by createdAt in reverse order + DAG structure for display). */
  snapshots: Snapshot[];
/** HEAD/branch/tag ref badge (mappable to each snapshot). */
  refs: Ref[];
/** ID of the currently selected snapshot (target for diff/fork/load actions). null if no selection. */
  selectedId: string | null;
}

/** Snapshot timeline screen view-model. */
export interface SnapshotTimelineViewModel {
  state: SnapshotTimelineState;
/** Loads the timeline for repoId/branch. */
  load(repoId: string, branch: string): Promise<void>;
/** Selects a snapshot (for diff/fork/load action triggers). */
  select(snapshotId: string): void;
/** Unselects the current item. */
  deselect(): void;
}

/**
 * Snapshot timeline view-model factory stub.
 * State machine/subscription implementation will be added after the framework is determined.
 */
export function createSnapshotTimelineViewModel(
  listSessions: ListSessionsUseCase,
): SnapshotTimelineViewModel {
  const state: SnapshotTimelineState = {
    loading: false,
    error: null,
    snapshots: [],
    refs: [],
    selectedId: null,
  };
  return {
    state,
    async load(repoId: string, branch: string): Promise<void> {
      state.loading = true;
      state.error = null;
      try {
        const out = await listSessions.execute({ repoId, branch });
        state.snapshots = out.snapshots;
        state.refs = out.refs;
      } catch (e) {
        state.error = e instanceof Error ? e.message : String(e);
      } finally {
        state.loading = false;
      }
    },
    select(snapshotId: string): void {
      state.selectedId = snapshotId;
    },
    deselect(): void {
      state.selectedId = null;
    },
  };
}
