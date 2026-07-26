/**
 * application/use-cases/diff-snapshots: Use case for CIR event delta between two snapshots.
 *
 * domain model inbound DiffSnapshots / MCP session_diff handling.
 * Used to construct the change list in Diff view (Screen 4).
 *
 * Dependencies: SessionGateway (port), dto.ts.
 */

import type { SessionGateway } from '../ports/session-gateway.js';
import type { DiffInput, DiffOutput } from '../dto.js';

/** Inbound port: Interface for use-case called by presentation. */
export interface DiffSnapshotsUseCase {
  execute(input: DiffInput): Promise<DiffOutput>;
}

/**
 * Interactor: Retrieves CIR event delta between two snapshots via SessionGateway.diff().
 * Future enhancements:
 *   - Sort changes by seq (ascending).
 *   - Map display labels by op.
 *   - Enhance context with LCA (app layer responsibility).
 */
export class DiffSnapshotsInteractor implements DiffSnapshotsUseCase {
  constructor(private readonly gateway: SessionGateway) {}

  execute(_input: DiffInput): Promise<DiffOutput> {
    throw new Error('not implemented');
  }
}
