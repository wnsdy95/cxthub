/**
 * application/use-cases/list-sessions: Snapshot/ref list retrieval use-case.
 *
 * SPINE inbound ListSessions / MCP session_list response.
 * Used in screen 3 (Snapshot timeline) for reverse createdAt order / DAG parent graph construction.
 *
 * Dependencies: SessionGateway (port), dto.ts, domain types.
 */

import type { SessionGateway } from '../ports/session-gateway.js';
import type { ListInput, ListOutput } from '../dto.js';

/** Inbound port: Interface for use-case called by presentation. */
export interface ListSessionsUseCase {
  execute(input: ListInput): Promise<ListOutput>;
}

/**
 * Interactor: Retrieves snapshot+ref list via SessionGateway.listSessions().
 * Future enhancements:
 *   - snapshots: reverse createdAt order sorting.
 *   - Timeline/graph construction based on DAG parent links (app layer responsibility).
 *   - Mapping ref badges (head/branch/tag) to each snapshot.
 */
export class ListSessionsInteractor implements ListSessionsUseCase {
  constructor(private readonly gateway: SessionGateway) {}

  execute(input: ListInput): Promise<ListOutput> {
    return this.gateway.listSessions(input);
  }
}
