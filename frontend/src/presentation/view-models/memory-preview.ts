/**
 * presentation/view-models/memory-preview: Memory preview screen view-model.
 *
 * Screen 6 — cxt unique feature. Display distillation of MemoryDigest read-only.
 * Actual provider file injection (memory_load) is CLI/hook/agent action — Web UI is for preview only.
 * Framework-independent. (state, actions) shape only defined.
 *
 * Dependencies: application inbound port interfaces, domain types.
 */

import type { MemoryDigest } from '../../domain/index.js';
import type { PreviewMemoryUseCase } from '../../application/index.js';

/** Memory preview screen state. */
export interface MemoryPreviewState {
  loading: boolean;
  error: string | null;
/**
 * Loaded MemoryDigest. null if no memory in this snapshot.
 * Display: summary (markdown), keyFacts list, openTasks list.
 */
  digest: MemoryDigest | null;
}

/** Memory preview screen view-model. */
export interface MemoryPreviewViewModel {
  state: MemoryPreviewState;
/** Load MemoryDigest of the snapshot. */
  load(repoId: string, snapshotId: string): Promise<void>;
}

/**
 * Memory preview view-model factory stub.
 */
export function createMemoryPreviewViewModel(
  _previewMemory: PreviewMemoryUseCase,
): MemoryPreviewViewModel {
  throw new Error('not implemented');
}
