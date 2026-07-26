/**
 * application/index: application layer public barrel re-export.
 *
 * Importing this file grants access to all public interfaces/interactors/DTOs in the application layer.
 * Presentation and infrastructure layers reference the application through this barrel.
 *
 * Dependency direction: application → domain only. Does not import infrastructure.
 */

// ── DTO ──────────────────────────────────────────────────────────
export type {
  ListInput,
  ListOutput,
  DiffInput,
  DiffEntry,
  DiffOutput,
  ForkInput,
  ForkOutput,
  LoadInput,
  LoadOutput,
  CheckoutInput,
  CheckoutOutput,
  // MemorizeInput / MemorizeOutput: CLI exclusive — excluded from web barrel.
  SyncInput,
  SyncOutput,
} from './dto.js';

// ── Outbound port interfaces ─────────────────────────────────
export type { SessionGateway } from './ports/session-gateway.js';

// ── Inbound port interfaces (use-case) + interactor ────────────────
export type { ListReposUseCase } from './use-cases/list-repos.js';
export { ListReposInteractor } from './use-cases/list-repos.js';

export type { ListBranchesUseCase, ListBranchesInput } from './use-cases/list-branches.js';
export { ListBranchesInteractor } from './use-cases/list-branches.js';

export type { ListSessionsUseCase } from './use-cases/list-sessions.js';
export { ListSessionsInteractor } from './use-cases/list-sessions.js';

export type { DiffSnapshotsUseCase } from './use-cases/diff-snapshots.js';
export { DiffSnapshotsInteractor } from './use-cases/diff-snapshots.js';

export type { ForkSessionUseCase } from './use-cases/fork-session.js';
export { ForkSessionInteractor } from './use-cases/fork-session.js';

export type { LoadSessionUseCase } from './use-cases/load-session.js';
export { LoadSessionInteractor } from './use-cases/load-session.js';

export type { PreviewMemoryUseCase, PreviewMemoryInput } from './use-cases/preview-memory.js';
export { PreviewMemoryInteractor } from './use-cases/preview-memory.js';

export type { SyncPushUseCase, SyncPullUseCase } from './use-cases/sync-repo.js';
export { SyncPushInteractor, SyncPullInteractor } from './use-cases/sync-repo.js';

export type { CheckoutSessionUseCase } from './use-cases/checkout-session.js';
export { CheckoutSessionInteractor } from './use-cases/checkout-session.js';

// NOTE: MemorizeUseCase / MemorizeInteractor are CLI exclusive (`cxt memorize`) and
// are not re-exported in the web barrel. No POST /api/v1/repos/{repoId}/memorize endpoint.
