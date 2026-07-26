/**
 * presentation/view-models/diff-view: Diff view model for the view.
 *
 * Screen 4 — Corresponds to GitHub "commit diff". Displays the CIR event delta between two snapshots.
 * Select left/right snapshots → Execute DiffSnapshotsUseCase → Render changes flow.
 * Framework-independent. (state, actions) shape only defined.
 *
 * Dependencies: application inbound port interfaces, domain/dto types.
 */

import type { ContentHash } from '../../domain/index.js';
import type { DiffSnapshotsUseCase, DiffEntry } from '../../application/index.js';

/** Diff view model state. */
export interface DiffViewState {
  loading: boolean;
  error: string | null;
  repoId: string | null;
/** Comparison base (left/before) snapshot id. null if not selected. */
  leftId: ContentHash | null;
/** Comparison target (right/after) snapshot id. null if not selected. */
  rightId: ContentHash | null;
/** List of changes (sorted by seq ascending). */
  changes: DiffEntry[];
}

/** Diff view model. */
export interface DiffViewModel {
  state: DiffViewState;
/**
 * Select two snapshots and run the diff.
 * Both leftId and rightId must be specified for the diff request to be sent.
 */
  compare(repoId: string, leftId: ContentHash, rightId: ContentHash): Promise<void>;
}

/**
 * Diff view view-model factory stub.
 */
export function createDiffViewModel(
  _diffSnapshots: DiffSnapshotsUseCase,
): DiffViewModel {
  throw new Error('not implemented');
}
