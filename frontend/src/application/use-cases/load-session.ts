/**
 * application/use-cases/load-session: Restore use-case targeting the snapshot as the provider session.
 *
 * REST LoadSession action.
 * Used in the screen (Load action — secondary). Corresponds to "checkout" (domain model).
 *
 * Two recovery modes (data model / domain model):
 *   full-context  : Full script restoration (high fidelity).
 *   memory-form   : Inject distillation summary into provider memory (CLAUDE.md/AGENTS.md).
 *
 * Dependencies: SessionGateway (port), dto.ts.
 */

import type { SessionGateway } from '../ports/session-gateway.js';
import type { LoadInput, LoadOutput } from '../dto.js';

/** Inbound port: Interface for use-case called by presentation. */
export interface LoadSessionUseCase {
  execute(input: LoadInput): Promise<LoadOutput>;
}

/**
 * Interactor: Restores the snapshot to the target provider session via SessionGateway.load().
 * Additional pure processing to be added upon subsequent implementation:
 *   - Fidelity downgrade notice for cross-provider mode.
 *   - Path abbreviation for writtenPath display.
 */
export class LoadSessionInteractor implements LoadSessionUseCase {
  constructor(private readonly gateway: SessionGateway) {}

  execute(_input: LoadInput): Promise<LoadOutput> {
    throw new Error('not implemented');
  }
}
