/**
 * presentation/view-models/sync-status: Sync status screen view-model (backup).
 *
 * Corresponds to GitHub "push/pull badges". Displays push/pull results (upload count, download count, fork branch).
 * Framework-independent. Defines only the (state, actions) shape.
 *
 * Dependencies: application inbound port interfaces, domain types.
 */

import type { Ref } from '../../domain/index.js';
import type { SyncPushUseCase, SyncPullUseCase } from '../../application/index.js';

/**
 * Summary of sync operation results.
 * newRefs may include fork branches ("<branch>--fork-<shortid>") that were preserved.
 */
export interface SyncResult {
  pushed: number;
  pulled: number;
  newRefs: Ref[];
}

/** Sync status screen state. */
export interface SyncStatusState {
/** Whether a push is in progress. */
  pushing: boolean;
/** Whether a pull is in progress. */
  pulling: boolean;
  error: string | null;
/** Last sync result. null if not executed. */
  lastResult: SyncResult | null;
}

/** Sync status screen view-model. */
export interface SyncStatusViewModel {
  state: SyncStatusState;
/** Pushes local snapshot/ref to central server. */
  push(repoId: string): Promise<void>;
/** Pulls central server changes to local. */
  pull(repoId: string): Promise<void>;
}

/**
 * Sync status view-model factory stub.
 */
export function createSyncStatusViewModel(
  _syncPush: SyncPushUseCase,
  _syncPull: SyncPullUseCase,
): SyncStatusViewModel {
  throw new Error('not implemented');
}
