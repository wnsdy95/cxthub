/**
 * application/use-cases/sync-repo: Synchronize with central server for push/pull operations.
 *
 * SPINE inbound SyncRepo / MCP sync_push, sync_pull handling.
 * Used in Sync status (secondary) screen.
 *
 * push/pull protocol (SYNC-PROTOCOL §3):
 *   push: negotiate → objects upload → refs CAS (REST 3-step = gateway single call).
 *   pull: manifest GET → objects download → local ref merge.
 *
 * Branch preservation (SYNC-PROTOCOL §5.3):
 *   Server automatically creates "<branch>--fork-<shortid>" branch when diverged.
 *   SyncOutput.newRefs includes fork branch.
 *
 * Dependencies: SessionGateway (port), dto.ts.
 */

import type { SessionGateway } from '../ports/session-gateway.js';
import type { SyncInput, SyncOutput } from '../dto.js';

/** Inbound port: Interface for push use-case called by presentation. */
export interface SyncPushUseCase {
  execute(input: SyncInput): Promise<SyncOutput>;
}

/** Inbound port: Interface for pull use-case called by presentation. */
export interface SyncPullUseCase {
  execute(input: SyncInput): Promise<SyncOutput>;
}

/**
 * Push interactor: Uploads local snapshot/ref to server via SessionGateway.syncPush().
 * Future enhancements:
 *   - Format for summary of pushed/newRefs for display.
 *   - Message for diverged fork branch guidance.
 */
export class SyncPushInteractor implements SyncPushUseCase {
  constructor(private readonly gateway: SessionGateway) {}

  execute(_input: SyncInput): Promise<SyncOutput> {
    throw new Error('not implemented');
  }
}

/**
 * Pull interactor: Downloads server changes to local via SessionGateway.syncPull().
 * Future enhancements:
 *   - Format for summary of pulled/newRefs for display.
 *   - Status display for fast-forward vs fork occurrence.
 */
export class SyncPullInteractor implements SyncPullUseCase {
  constructor(private readonly gateway: SessionGateway) {}

  execute(_input: SyncInput): Promise<SyncOutput> {
    throw new Error('not implemented');
  }
}
