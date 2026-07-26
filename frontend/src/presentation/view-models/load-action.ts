/**
 * presentation/view-models/load-action: Load action screen view-model (helper).
 *
 * Responds to GitHub "checkout". Restores snapshot to target provider session file (Claude/Codex).
 * Two modes: full-context (transcript) / memory-form (distillation summary injection).
 * Framework-independent. Defines only (state, actions) shape.
 *
 * Dependencies: application inbound port interface, domain types.
 */

import type { ProviderKind, FidelityTier } from '../../domain/index.js';
import type { LoadSessionUseCase, LoadOutput } from '../../application/index.js';

/** Load action screen state. */
export interface LoadActionState {
  submitting: boolean;
  error: string | null;
/** Last load result (success case). null if not executed. */
  result: LoadOutput | null;
}

/** Load action screen view-model. */
export interface LoadActionViewModel {
  state: LoadActionState;
/**
 * Restores snapshot to target provider session file.
 * mode="full"/"reconstructed" → transcript restoration.
 * mode="memory"               → inject MemoryDigest to CLAUDE.md/AGENTS.md.
 * Cross-provider load may downgrade fidelity to "reconstructed".
 */
  load(
    repoId: string,
    ref: string,
    targetProvider: ProviderKind,
    mode: FidelityTier,
    cwd: string,
  ): Promise<void>;
}

/** Load action view-model factory stub. */
export function createLoadActionViewModel(
  _loadSession: LoadSessionUseCase,
): LoadActionViewModel {
  throw new Error('not implemented');
}
