/**
 * presentation/view-models/repo-list: Repo list screen view-model.
 *
 * Screen 1 — Entry screen for team session browser corresponding to GitHub "Repository List".
 * Framework-independent (no React/Vue/Svelte). Defines only (state, actions) shape.
 * Actual state machine/subscription implementation will be added in subsequent turns after framework decision.
 *
 * Dependencies: application inbound port interfaces, domain types.
 */

import type { Repo } from '../../domain/index.js';
import type { ListReposUseCase } from '../../application/index.js';

/** Repo list screen state. */
export interface RepoListState {
  loading: boolean;
  error: string | null;
/** Team visibility repo list (id-based sorting recommended). */
  repos: Repo[];
}

/** Repo list screen view-model. (state + actions) */
export interface RepoListViewModel {
  state: RepoListState;
/** Loads repo list from server. */
  load(): Promise<void>;
}

/**
 * Repo list view-model factory stub.
 * Injects ListReposUseCase into composition root.
 */
export function createRepoListViewModel(
  listRepos: ListReposUseCase,
): RepoListViewModel {
  const state: RepoListState = { loading: false, error: null, repos: [] };
  return {
    state,
    async load(): Promise<void> {
      state.loading = true;
      state.error = null;
      try {
        state.repos = await listRepos.execute();
      } catch (e) {
        state.error = e instanceof Error ? e.message : String(e);
      } finally {
        state.loading = false;
      }
    },
  };
}
