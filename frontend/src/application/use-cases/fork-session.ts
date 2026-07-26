/**
 * application/use-cases/fork-session: Fork a new branch from a snapshot use-case.
 *
 * SPINE inbound ForkSession / MCP session_fork handling.
 * Used in screen 5 (Fork action). "Fork this session branch to start a new task line from this point" (SPINE §1.2).
 *
 * fork meaning (DATA-MODEL §2.4):
 *   - Create a new branch ref with tip from fromSnapshot — no data copy (pointer copy).
 *   - Include fromSnapshot in the parents of the first snapshot created in the new branch → preserve ancestor reachability.
 *   - The original branch/snapshot is never modified (invariant F1).
 *
 * Dependencies: SessionGateway (port), dto.ts.
 */

import type { SessionGateway } from '../ports/session-gateway.js';
import type { ForkInput, ForkOutput } from '../dto.js';

/** Inbound port: Interface for use-case called by presentation. */
export interface ForkSessionUseCase {
  execute(input: ForkInput): Promise<ForkOutput>;
}

/**
 * Interactor: Forks a new branch from a snapshot via SessionGateway.fork().
 * Additional validation checks to be added in subsequent implementations (app layer responsibility):
 *   - Pre-check for duplicate newBranch name (F2).
 *   - Notify local state update upon successful fork.
 */
export class ForkSessionInteractor implements ForkSessionUseCase {
  constructor(private readonly gateway: SessionGateway) {}

  execute(_input: ForkInput): Promise<ForkOutput> {
    throw new Error('not implemented');
  }
}
