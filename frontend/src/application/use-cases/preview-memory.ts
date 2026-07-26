/**
 * application/use-cases/preview-memory: MemoryDigest preview use-case.
 *
 * Screen 6 (Memory Preview) — cxt unique feature. Display distilled memory summary read-only.
 * The web UI supports read-only previews, unlike MCP memory_load, which injects an actual provider memory file.
 * SPINE §7 MCP memory_save/memory_load read aspect (Open Question §7.5).
 *
 * Dependencies: SessionGateway (port), domain type.
 */

import type { MemoryDigest } from '../../domain/index.js';
import type { SessionGateway } from '../ports/session-gateway.js';

/** Memory preview input. */
export interface PreviewMemoryInput {
  repoId: string;
  snapshotId: string;
}

/** Inbound port: interface called by presentation use-case. */
export interface PreviewMemoryUseCase {
  execute(input: PreviewMemoryInput): Promise<MemoryDigest>;
}

/**
 * Interactor: previews MemoryDigest from snapshot via SessionGateway.getMemory().
 * Future implementation additions:
 *   - summary markdown parsing / section separation.
 *   - keyFacts / openTasks display processing.
 *   - handling informational status for non-existent memory.
 */
export class PreviewMemoryInteractor implements PreviewMemoryUseCase {
  constructor(private readonly gateway: SessionGateway) {}

  execute(_input: PreviewMemoryInput): Promise<MemoryDigest> {
    throw new Error('not implemented');
  }
}
