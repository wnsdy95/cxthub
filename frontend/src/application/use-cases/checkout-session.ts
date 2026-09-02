/**
 * application/use-cases/checkout-session: fork (optional) + load integration checkout use-case.
 *
 * Client checkout action.
 * `cxt checkout <ref>` (simple load) or `cxt checkout -b <new> [--from <ref>]`
 * (fork then load) both forms are handled.
 *
 * Logic summary (to be detailed upon subsequent implementation):
 *   - input.newBranch != "" → SessionGateway.fork followed by SessionGateway.load sequential composition.
 *   - input.newBranch == "" → SessionGateway.load only.
 *   - Return value includes branch, head, writtenPath, resumeCmd, fidelity.
 *
 * Dependencies: SessionGateway (port), dto.ts.
 */

import type { SessionGateway } from '../ports/session-gateway.js';
import type { CheckoutInput, CheckoutOutput } from '../dto.js';

/** Inbound port: use-case interface called by presentation. */
export interface CheckoutSessionUseCase {
  execute(input: CheckoutInput): Promise<CheckoutOutput>;
}

/**
 * Interactor: sequentially composes fork (optional) + load to perform checkout.
 * Additional logic to be implemented:
 *   - If newBranch is not empty, perform fork → load composition.
 *   - In full mode, pass resumeCmd to the caller.
 *   - Emit warning event on fidelity downgrade.
 */
export class CheckoutSessionInteractor implements CheckoutSessionUseCase {
  constructor(private readonly gateway: SessionGateway) {}

  execute(_input: CheckoutInput): Promise<CheckoutOutput> {
    // Implementation details:
    //   if (input.newBranch) → this.gateway.fork({ repoId, fromSnapshot: input.from, newBranch, author })
    //                          → this.gateway.load({ repoId, ref: newBranch, targetProvider, mode, cwd })
    //   else → this.gateway.load({ repoId, ref: input.from, targetProvider, mode, cwd })
    void this.gateway;
    throw new Error('not implemented');
  }
}
