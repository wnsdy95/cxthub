# Enterprise identity and security completion

Baseline: `0f9ffcf`, 2026-09-28. Tracking: #144 and #288.
Cloud provisioning/deployment and paid billing activation are excluded. The
existing repository, organization and Enterprise authority boundaries remain.

## Delivery and acceptance

**Enterprise identity and security.** Add interoperable SAML/OIDC connection
and verified-domain identity binding, SCIM with source-aware deprovisioning,
enforced MFA/IP/session policies and last-owner recovery. Add auditable
retention/deletion/restore controls and security audit delivery. Configuration
alone must never claim enforcement. Existing users, manual grants and unrelated
organizations must survive identity changes. Local protocol/IdP fixtures are
required; actual customer/provider credentials remain a separate verification.

Each logical change gets focused failure-path tests, affected full checks, a
signed PR and green CI. Record measured limitations rather than treating local
tests as cloud certification. Preserve user data and existing Git/provider hooks.

## Design review constraints

- Enterprise membership is not private repository access. Identity federation
  cannot merge accounts merely because emails match, or silently remove manual
  grants. A tenant must not lock out its last recoverable owner.
- Retention defaults preserve data. Policy controls and deletion mechanisms are
  tested with synthetic data; enabling deletion for live data requires a concrete
  policy selected by the administrator. Billing counters never authorize deletion.

## Status

- Enterprise IdP: use local protocol fixtures unless a customer IdP is supplied.
- Enterprise domain verification now has a PostgreSQL transaction boundary,
  owner-managed DNS challenges, audited release and an administration UI.
  Race tests cover competing tenants, owner revocation, rotation and release
  during DNS lookup. This grants no identity or repository authority. Remaining
  federation, provisioning and enforcement slices are in ENTERPRISE_IDENTITY.md;
  the overall Enterprise identity/security plan is not complete.

- OIDC browser verification adds encrypted provider credentials, PKCE/nonce,
  durable single-use attempts and explicit issuer/subject binding to the initiating
  browser session. Existing membership and repository access do not change.
  Mandatory SSO, SAML, CLI/MCP assurance renewal and security policies are not yet
  enabled; detailed boundaries are recorded in ENTERPRISE_IDENTITY.md.

- OIDC PR #304 merged after nine green CI checks.

- SAML browser verification (#307) is implemented with pinned metadata, signed
  requests/assertions, durable replay records and a browser-bound two-step
  callback. Local PostgreSQL and signed protocol fixtures are under validation.
  It is not mandatory SSO, SCIM, policy enforcement or customer IdP acceptance.

- SAML PR #308 merged after nine green CI checks. Browser verification does not
  enable mandatory SSO, CLI/MCP assurance, MFA/IP/session enforcement or
  customer IdP activation.

- Enterprise encryption-key lifecycle #312 adds staged read/write keys and a
  transactional, auditable operator rewrap workflow. It merged in #313 after
  nine green checks. No live IdP or key configuration was activated. Mandatory
  policy, credential assurance, SCIM, retention and security delivery remain
  outstanding.

- SAML signing lifecycle #315 adds owner-confirmed preparation/activation,
  current-revision browser verification before retirement, cancellation and valid
  previous-key rollback. IdP trust repair preserves both keys and resets verification.
  Expired SP certificates do not prevent publishing recovery metadata. Alternate
  keys participate in operator encryption rewrap. No customer IdP or mandatory
  policy is enabled.
- SAML signing lifecycle merged in #316 after nine green CI checks; migration
  0068 belongs to it. No live identity configuration was changed.
- Enterprise prerequisite #320 fixes MCP refresh relationships before credential
  assurance is added: one grant per consent, stable identity through rotation,
  committed replay revocation and audit, separate legacy-token handling, and no
  cross-device SSO inference. This does not enable mandatory SSO/MFA, SCIM,
  retention or external security-audit delivery. Those remain implementation work.
- Prerequisite #320 shipped in #321; independent API/MCP processes use the shared
  PostgreSQL grant state.
- Credential-assurance work #322 adds explicit per-Enterprise CLI/MCP approvals,
  exact browser/credential binding, a revisioned 1–24 hour lifetime (default 8),
  and audit in the identity transaction. Protocol-envelope validity and recorded
  session life are distinct; SAML IdP session limits still apply. Refresh preserves
  original authentication and cannot renew approval. API/UI expose server-computed
  state and non-bearer selectors. Mandatory policy enforcement and recoverable-owner
  activation remain the next security boundary; this feature alone does not
  restrict repository reads/writes or declare SSO/MFA enforced.

- Identity prerequisite #328 adds a shared credential assessment and owner recovery
  preparation/confirmation/redemption. Browser, CLI and MCP evidence keep their
  separate identities and lifetimes. Recovery is one-use, browser-bound, audited
  and invalidated on ownership loss, with no repository or SSO grant. This is not
  mandatory SSO/MFA enforcement or working policy repair: the next slice must
  apply assessment to protected reads/writes and wire narrowly scoped identity
  repair commands with safe last-owner activation. SCIM, retention, external
  security-audit delivery and live customer-provider acceptance remain open.
