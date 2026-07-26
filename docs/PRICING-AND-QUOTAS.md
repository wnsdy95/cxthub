# cxthub v1 Pricing and Workspace Quota Contract

> Status: **Product policy finalized; implementation pending**
> Effective date: 2026-07-23
> Scope: cxthub Hosting Service. Not applicable to self-hosted `cxtd`.

This document defines the cxthub v1 pricing, seat, storage, and transfer
contract. It is deliberately conservative to bound operational cost and abuse.
It remains the product-policy source of truth until billing and quota
enforcement are implemented. The domain, database, OpenAPI contract, CLI, and
web UI must then conform to it.

The core principles are as follows.

- The **workspace** is the billing and usage boundary.
- Seats are based on accepted member roles, not monthly activity.
- Repository, snapshot, and capture counts are not primary billing units.
- Storage uses actual unique object bytes after compression and
  content-addressed deduplication.
- Reaching a quota never deletes existing context automatically.
- There is no automatic overage billing; only prepaid add-on capacity applies.

---

## 1. Launch Pricing

The pricing is in USD and taxes are not included. Initial release will only offer monthly payments. Annual discounts will be determined after measuring the actual usage and cost for a minimum of three months.

| Plan | Monthly Price | Contributor Seats | Free Viewers | Private Repos | Storage | Monthly Transfer |
|---|---:|---:|---:|---:|---:|---:|
| **Free** | $0 | 1 owner | 2 viewers | 1 repo | 250MB | 1GB |
| **Solo** | $12/workspace | 1 seat | 5 viewers | 5 repos | 2GB | 5GB |
| **Team** | $49/workspace | 3 seats included | 25 viewers | 25 repos | 5GB base + 2GB per contributor | 10GB base + 5GB per contributor |
| **Team Additional Seat** | $15/seat | +1 seat | — | — | +2GB | +5GB |

For example, a Team workspace with 10 contributors costs
`$49 + ($15 × 7) = $154` per month and shares 25GB of storage and 60GB of
monthly transfer across the workspace.

### 1.1 Deferred Plans

- Business and Enterprise are not sold in v1; they display only `Contact us`.
- Do not sell higher-priced plans before SSO, SCIM, audit logs, an SLA, and
  priority support actually exist.
- Do not introduce activity-based seat billing before usage measurement and
  billing reconciliation are reliable.

### 1.2 Add-ons

| Additional Package | Price | Additional Capacity |
|---|---:|---:|
| Storage | $15/month | +10GB |
| Transfer | $40/month | +25GB/month |

- Free has no add-ons or overage charges. Reaching its quota requires an upgrade.
- Solo and Team use only add-ons explicitly purchased by an owner.
- Quota usage never triggers automatic overage charges.

---

## 2. Seat Contract

### 2.1 contributor

Every accepted workspace member with one of the following roles consumes a
paid `contributor` seat, regardless of monthly activity:

- `puller`
- `member`
- `maintainer`
- `owner`

A `puller` consumes a seat because that role can download complete objects and
therefore creates transfer cost even without writing context.

### 2.2 Free Viewer Seats

- The `viewer` role is free up to the plan's viewer limit.
- Unaccepted invites are not counted as seats.
- Downgrading a member to `viewer` must immediately revoke every `puller`-or-higher action, including push, pull, and load. A cosmetic UI-only downgrade is forbidden.
- A Free owner can only own one Free workspace. Being a member of other paid or invited workspaces is separate.

---

## 3. Storage Calculation

Storage is calculated by summing the **actual billable object bytes** of all repos within the workspace.

### 3.1 Inclusion

- Unique doc/chunk bodies after zstd compression
- Memory bodies
- Settings object
- Encrypted secrets envelope
- Other user-generated large blobs actually retained by the repository

### 3.2 Exclusion

- snapshot/ref/reflog/pending etc. small metadata
- DB index and internal operational overhead
- reupload of stored objects confirmed to be deduped
- staged objects from failed uploads that have exceeded a specified GC grace period

Repository content isolation remains in force. An object deduplicated within one
repository is billed once, while isolated copies in separate repositories are
billed separately.

### 3.3 Reservation and Consistency

- Before writing a new unique object, the workspace remaining space must be atomically reserved.
- Deduplication status and actual storage increase must be determined within a storage transaction.
- The reservation amount of failed requests must be restored.
- Staging uploads that are interrupted are immediately included in the temporary hard limit, but are removed from billable usage if not reused within 24 hours.
- Periodic reconciliation must detect and recover the drift between the actual storage object sum and the usage projection.

---

## 4. Traffic Calculation

Workspace transfer is the sum of actual response bytes sent through context APIs.

### 4.1 Inclusion

- doc/chunk/memory/settings/secrets download
- Objects returned by `pull` and `load`
- Anonymous context body retrieval from a public workspace

### 4.2 Exclusion

- Static web assets
- Small authentication, membership, ref, and manifest metadata
- Uploaded bytes from user to server
- Small error body in failure responses

A public Free workspace permits anonymous web viewing only. Anonymous `puller`
access and bulk pull are paid-plan features. All public reads count against the
workspace's transfer quota.

---

## 5. Technical Safety Limits for All Plans

Enforce the following limits in addition to the quota.

| Item | Limit |
|---|---:|
| Canonical body of a single snapshot | Uncompressed 32MiB |
| Single event/tool result | Uncompressed 2MiB |
| Chunks per snapshot | 64 |
| Single chunk | Uncompressed 2MiB |
| Concurrent syncs per user | 2 |
| Concurrent syncs per workspace | 10 |
| Active pending sessions per user | 20 |

- If an event exceeds 2MiB, it must be rejected explicitly or sent to a separate attachment storage path instead of circumventing a whole-document transmission.
- The existing 4MiB JSON request/response limit for chunk transmission is maintained.
- Rate limits must be keyed by both the authenticated user and the workspace, not just the IP.

---

## 6. Actions on Limit Reach

### 6.1 Notifications

- At 80%: Issue a warning to the owner and maintainer and display it on the web usage screen.
- At 100%: Return a structured `quota_exceeded` error that includes the type of limit exceeded and the necessary actions.
- An internal buffer of 5% can be displayed externally to absorb concurrent request contention, but this is not promised as available capacity to the customer.

### 6.2 Storage Limit

- Stop new unique blob storage.
- Allow dedup push and small metadata updates that only reference existing objects.
- Allow existing context read, pull, and export.
- Do not automatically delete or replace existing snapshots/sessions/blobs with arbitrary summary snapshots.

### 6.3 Transfer Limit

- ref/manifest/usage/payment screen etc. small metadata queries are allowed.
- doc/chunk/memory bulk pull and load are stopped.
- push is allowed if within storage limit.
- resumes immediately after additional pack purchase or plan change.

---

## 7. Cost and Price Determination Basis

As of 2026-07-23, the conservative dogfood baseline is approximately 0.13GB of
new storage per active user each month. Under that baseline:

- Free's 250MB is a product-evaluation allowance, not permanent general-purpose storage.
- Solo's 2GB supports roughly one year or more of typical single-user growth.
- Team with three contributors starts at 11GB, providing roughly two years or more of headroom at the measured growth rate.

The primary expected costs are Neon storage at `$0.35/GB-month`, Neon paid
egress, Vercel Seoul-region transfer, and Cloud Run compute and egress. The
initial model therefore does not automatically sell storage or transfer
overages below `$1/GB`; add-ons carry additional margin for failures, retries,
backups, and support.

The intended market position is above GitHub Team, near collaborative tools
such as Linear, and below Cursor Teams, which also includes model-inference cost.

- [GitHub Pricing](https://github.com/pricing)
- [Linear Pricing](https://linear.app/pricing)
- [Cursor Pricing](https://cursor.com/pricing)
- [Neon Pricing](https://neon.com/pricing)
- [Vercel Seoul Pricing](https://vercel.com/docs/pricing/regional-pricing/icn1)
- [Cloud Run Pricing](https://cloud.google.com/run/pricing)

---

## 8. Preconditions for Selling Paid Plans

Do not expose paid checkout or accept payments until all of the following exist:

1. Persistence of workspace plan/seat/add-on status
2. Atomic reservation of storage byte and reconciliation with actual storage usage
3. Measurement of API egress byte
4. Server enforcement of role change and seat count
5. Usage amount query API and owner web interface
6. 80% warning and 100% fail-closed operation
7. Idempotent handling of payment webhook and authorization verification
8. Hard spend limit that can be set by an administrator
9. Integration tests for free, paid, downgrade, and payment failure scenarios

Before billing is implemented, do not sell plans through UI copy alone and do
not increase quotas through manual database edits.
