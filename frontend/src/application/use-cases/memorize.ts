/**
 * application/use-cases/memorize: Active session distillation → MemoryDigest attachment use-case.
 *
 * [CLI only — not available on web]
 * RECONCILIATION §D Memorize / MCP memorize / `cxt memorize` handling.
 * POST /api/v1/repos/{repoId}/memorize is not defined in openapi.yaml·backend http, so
 * it is not exposed as a web endpoint. MemorizeInteractor.execute() always throws an error.
 *
 * Dependencies: SessionGateway (port), dto.ts.
 */

import type { SessionGateway } from '../ports/session-gateway.js';
import type { MemorizeInput, MemorizeOutput } from '../dto.js';

/** Inbound port: Interface for use-case called by presentation. */
export interface MemorizeUseCase {
  execute(input: MemorizeInput): Promise<MemorizeOutput>;
}

/**
 * Interactor: Executes session distillation through SessionGateway.memorize().
 * Additional logic to be implemented later:
 *   - Report native memory ingestion in the result.
 *   - Indicate successful MemoryDigest attachment (attached).
 *   - Store the contentHash of the distillation result in local cache.
 */
export class MemorizeInteractor implements MemorizeUseCase {
  constructor(private readonly gateway: SessionGateway) {}

  execute(_input: MemorizeInput): Promise<MemorizeOutput> {
    // Future implementation: this.gateway.memorize(input) → MemorizeOutput.
    void this.gateway;
    throw new Error('not implemented');
  }
}
