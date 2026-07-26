/**
 * presentation/view-models/branch-list: Branch list view-model for the screen.
 *
 * Screen 2 — Corresponds to GitHub "Branch List". Lists all session lines for a specific repo.
 * Framework-independent. Defines only the (state, actions) shape.
 *
 * Dependencies: application inbound port interfaces, domain types.
 */

import type { Branch } from '../../domain/index.js';
import type { ListBranchesUseCase } from '../../application/index.js';

/** Branch list screen state. */
export interface BranchListState {
  loading: boolean;
  error: string | null;
/** Loaded repo ID. */
  repoId: string | null;
/** Branch list (default branch at the top, subsequent branches sorted alphabetically recommended). */
  branches: Branch[];
}

/** Branch list screen view-model. */
export interface BranchListViewModel {
  state: BranchListState;
/** Loads the branch list for a specific repo. */
  load(repoId: string): Promise<void>;
}

/**
 * Factory stub for the branch-list view model.
 */
export function createBranchListViewModel(
  _listBranches: ListBranchesUseCase,
): BranchListViewModel {
  throw new Error('not implemented');
}
