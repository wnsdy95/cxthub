/**
 * application/use-cases/list-branches: Branch list retrieval use-case.
 *
 * Enhancement use-case for frontend screen 2 (Branch list).
 * No direct correspondence to SPINE §6.2 inbound port (Open Question §7.2),
 * but derived from SessionGateway.listBranches() and SYNC-PROTOCOL GET /api/v1/repos/{repoId}/branches.
 *
 * Dependencies: SessionGateway (port), domain type.
 */

import type { Branch } from '../../domain/index.js';
import type { SessionGateway } from '../ports/session-gateway.js';

/** Branch list retrieval input. */
export interface ListBranchesInput {
  repoId: string;
}

/** Inbound port: Interface for use-case called by presentation. */
export interface ListBranchesUseCase {
  execute(input: ListBranchesInput): Promise<Branch[]>;
}

/**
 * Interactor: Retrieves branch list for a specific repo via SessionGateway.
 * Additional processing logic (sorting, default branch at top, etc.) will be added in subsequent implementations.
 */
export class ListBranchesInteractor implements ListBranchesUseCase {
  constructor(private readonly gateway: SessionGateway) {}

  execute(_input: ListBranchesInput): Promise<Branch[]> {
    throw new Error('not implemented');
  }
}
