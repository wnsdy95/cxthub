# Enterprise identity and security delivery

Tracking: #144. No cloud deployment, paid billing or customer IdP activation.

## DNS domain verification

Enterprise owners manage domain claims from Enterprise settings. Administrators
can inspect them; members cannot. Create a challenge, publish the provided TXT
record at `_cxthub-verification.<domain>`, then verify it. Names are exact ASCII
DNS names (punycode is accepted), not URLs or wildcards. A verified parent does
not automatically verify a child, or vice versa.

Challenges last 24 hours. A successful DNS lookup creates a 30-day observation.
Renew by generating a new challenge and checking DNS again; existing verification
remains valid until its original expiry while renewal is pending. The server
returns the current state and deadlines. An expired observation is not ownership
proof. Release removes the active claim and records an audit event; a fresh
challenge can be created later. Each Enterprise may hold at most 20 claims.

This verifies DNS control only. It does not claim that an email belongs to a
person, merge accounts, add members, grant repository access or activate SSO.
No public-domain discovery/login routing is enabled by these records.

DNS calls have a bounded timeout outside the identity transaction. Publication
then rechecks owner authority, the exact challenge/revision, expiry and another
Enterprise's unexpired claim. DNS failure or a stale editor cannot replace an
observation. Claim changes and their audit entries commit together. Simultaneous
owners on separate servers cannot acquire the same exact domain. A record in
DNS cannot replay an old request after rotation or release.

This feature requires PostgreSQL identity transactions. The development FS
adapter reports `domain_verification_unavailable` and does not simulate a global
claim using a process-local lock. Existing FS repository features are unchanged.
The API uses the configured system DNS resolver; DNS provider credentials and
automated DNS record creation are unnecessary. Test DNS responses are injected
through the outbound resolver port, never through a production bypass flag.

## Remaining implementation slices

These are planned boundaries, not enabled policies or delivered SSO features:

1. OIDC authorization code with PKCE and SP-initiated SAML. Validate issuer/entity,
   audience, nonce/request correlation, signatures and replay. Keep attempts and
   encrypted connection credentials server-side. Reject unsolicited SAML login.
2. Bind the external issuer/subject to an explicitly authenticated existing account;
   never merge accounts solely on matching email. Carry credential-specific
   authentication time and MFA evidence into API, CLI and MCP authorization.
3. Enforce SSO/MFA/IP/session limits at reads and transaction-time writes. One
   browser's verification must not silently authorize unrelated long-lived tokens.
   Preserve a recoverable owner before enforcing or changing policy.
4. Implement SCIM Users/Groups with conditional atomic PATCH and source-specific
   membership grants. Deprovisioning must retain manual/GitHub grants and cannot
   remove the last recovery owner. Advertise only supported SCIM operations.
5. Add explicit retention/restore windows and durable security-audit delivery.
   Retention defaults preserve data. No live deletion policy is activated during
   development; billing counters never authorize deletion.

Protocol references: [OIDC Core](https://openid.net/specs/openid-connect-core-1_0.html),
[SAML profiles](https://docs.oasis-open.org/security/saml/v2.0/saml-profiles-2.0-os.pdf),
[SCIM protocol](https://www.rfc-editor.org/rfc/rfc7644.html). Local protocol fixtures
are required; customer IdP interoperability remains a separate acceptance step.
