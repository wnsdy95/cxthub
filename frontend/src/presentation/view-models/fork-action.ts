/**
 * presentation/view-models/fork-action: Fork action screen view-model.
 *
 * Screen 5 — GitHub "Fork" button response. Create a new branch from the selected snapshot.
 * Fork meaning(data model): Pointer (ref) copy — no data duplication, cost O(1).
 * Framework-independent. Defines only the (state, actions) shape.
 *
 * Dependencies: application inbound port interface, domain types.
 */

import type { ContentHash, TeamIdentity } from '../../domain/index.js';
import type { ForkSessionUseCase, ForkOutput } from '../../application/index.js';

/** Fork action screen state. */
export interface ForkActionState {
/** Form submission in progress. */
  submitting: boolean;
  error: string | null;
/** Last fork result (on success). null if not executed. */
  result: ForkOutput | null;
}

/** Fork action screen view-model. */
export interface ForkActionViewModel {
  state: ForkActionState;
/**
 * Executes branch creation from fromSnapshot to newBranch.
 * Invariants F1 (original unchanged) / F2 (newBranch unique) are guaranteed by the backend.
 */
  fork(
    repoId: string,
    fromSnapshot: ContentHash,
    newBranch: string,
    author: TeamIdentity,
  ): Promise<void>;
}

/**
 * Factory stub for the fork-action view model.
 */
export function createForkActionViewModel(
  _forkSession: ForkSessionUseCase,
): ForkActionViewModel {
  throw new Error('not implemented');
}
