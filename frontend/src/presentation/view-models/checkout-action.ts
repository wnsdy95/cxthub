/**
 * presentation/view-models/checkout-action: Checkout action screen view-model.
 *
 * RECONCILIATION §D CheckoutSession corresponding UI state + action definitions.
 * Handles both `cxt checkout <ref>` and `cxt checkout -b <new> [--from <ref>]` forms.
 * If newBranch exists, forks and loads; otherwise, simply loads.
 *
 * Distinction from fork-action.ts's fork-specific VM:
 *   - checkout-action: fork (optional) + load integration — context restoration included.
 *   - fork-action: pointer (ref) replication only — no actual session restoration.
 *
 * Framework-independent. (state, actions) shape only defined.
 *
 * Dependencies: application inbound port interfaces, domain types.
 */

import type { ProviderKind, FidelityTier } from '../../domain/index.js';
import type { CheckoutSessionUseCase, CheckoutOutput } from '../../application/index.js';

/** Checkout action screen state. */
export interface CheckoutActionState {
/** Request processing in progress. */
  submitting: boolean;
  error: string | null;
/** Last checkout result (success). null if not executed. */
  result: CheckoutOutput | null;
}

/** Checkout action screen view-model. */
export interface CheckoutActionViewModel {
  state: CheckoutActionState;
/**
 * Executes checkout.
 *
 * @param repoId       Repository ID.
 * @param from         Branch/Snapshot ID/HEAD reference.
 * @param newBranch    New branch name. Empty string for simple load (no fork).
 * @param targetProvider Restoration target provider.
 * @param mode         Restoration fidelity mode.
 * @param cwd          Working directory.
 */
  checkout(
    repoId: string,
    from: string,
    newBranch: string,
    targetProvider: ProviderKind,
    mode: FidelityTier,
    cwd: string,
  ): Promise<void>;
}

/**
 * Checkout action view-model factory stub.
 * Creates it by injecting CheckoutSessionInteractor into the composition root.
 */
export function createCheckoutActionViewModel(
  _checkoutSession: CheckoutSessionUseCase,
): CheckoutActionViewModel {
  throw new Error('not implemented');
}
