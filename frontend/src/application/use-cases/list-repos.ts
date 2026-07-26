/**
 * application/use-cases/list-repos: use case for listing repositories.
 *
 * Supporting use case for frontend screen 1 (repository list).
 * It has no direct inbound-port counterpart in SPINE §6.2 (Open Question §7.2),
 * but follows from SessionGateway.listRepos() and GET /api/v1/repos in SYNC-PROTOCOL.
 *
 * Dependencies: the SessionGateway port and domain types.
 */

import type { Repo } from '../../domain/index.js';
import type { SessionGateway } from '../ports/session-gateway.js';

/** Inbound port: interface for use-case called by presentation. */
export interface ListReposUseCase {
  execute(): Promise<Repo[]>;
}

/**
 * Interactor: retrieves backend repo list through SessionGateway (outbound port).
 * Additional processing logic (sorting, etc.) will be added in subsequent implementations.
 */
export class ListReposInteractor implements ListReposUseCase {
  constructor(private readonly gateway: SessionGateway) {}

  execute(): Promise<Repo[]> {
    return this.gateway.listRepos();
  }
}
