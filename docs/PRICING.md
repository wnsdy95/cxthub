# Pricing status and draft plans

## Current decision — 2026-09-16

CXTHub will operate free during its initial phase. No subscription, seat fee,
storage overage charge, or payment enrollment is active. Usage collected during
this phase will not be retroactively billed. The pricing page describes this
current policy; it does not publish a billable calculator or fixed allowances.

Keep new and existing namespaces in the default **metering** state (empty
`StoragePolicy.Plan`). Do not provision paid entitlements with `cxt-admin` in
production. Storage accounting, reconciliation, request/payload limits, access
controls, and operational monitoring remain in place. Free operation is not a
promise of unlimited infrastructure or a relaxation of repository permissions.

## Draft only — not a commercial offer

The intended plan names remain **Free**, **Team**, and **Enterprise**. Prices,
allowances, and the billing metric will be decided after observing real customer
usage. Prefer aggregate operational measurements and customer feedback over
collecting conversation content for pricing research. This decision does not
introduce a new behavioral tracking SDK or analytics data collection.

Prior proposals are retained here solely as design history:

- Storage-based pricing rather than charging for AI agent seats.
- An earlier public page proposed 10 GiB included and USD 0.07 per additional
  GiB-month, using GitHub LFS as a reference. These are **withdrawn draft values**,
  not current CXTHub allowances or prices; GitHub's current prices are not asserted.
- Enterprise: unlimited repositories, with 50 GiB included and excess storage
  metered for possible pay-as-you-go billing. The storage-policy foundation
  supports this candidate; it must remain unprovisioned during free operation.
- Free and Team included storage, base prices, and Enterprise base price are
  undecided. No paid plan is sold or automatically assigned.

## Evidence for a later decision

Review retained-byte growth and deduplication, active repositories, collaboration
patterns, read/search traffic and cost, retention needs, and direct customer
feedback. Separate ordinary use from short test spikes. Establish an observation
window and a review date once customers are using the product; no deadline or
price change is scheduled by this document.

Before enabling payment: approve the plan contract, revise the public page,
provide usage/budget controls and notices, obtain payment consent, and implement
payment-provider lifecycle and settlement. Keep the free-period measurements
separate from any future billable period. Existing policy and excess-byte-hour
code is a technical foundation, not permission to charge customers.
