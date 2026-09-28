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

## OIDC browser verification

The API can configure an Enterprise OIDC provider and explicitly link an existing
CXTHub account to its signed issuer/subject. This is browser identity verification,
not mandatory SSO enforcement or a new unauthenticated sign-in method. Existing
repository grants remain authoritative. SAML and SCIM are still separate work.

Operator setup: set `CXT_IDENTITY_ENCRYPTION_KEY` in the API service environment
(or its private `.env`) to a cryptographically random, base64-encoded 32-byte key.
Preserve the same key across API replicas and restarts; keep it out of PostgreSQL,
frontend builds and the MCP environment. No key means the UI reports unavailable;
an invalid configured key fails API startup. Back up the key separately from the
database. Rotation/recovery tooling is not yet provided: do not replace a key
while its encrypted connections or pending attempts are needed.

An Enterprise owner first verifies a company domain, then opens **Identity** and
registers the displayed callback URL with the IdP. Enter the exact HTTPS issuer,
client ID, client secret and supported client authentication method. Configuration
checks discovery and does not assert that client credentials have successfully
logged in. A real authorization flow tests those credentials. Only public HTTPS
provider endpoints on port 443 are supported; private-network IdPs are rejected.

Members of the Enterprise or its organizations can verify their current browser.
The initial explicit account link requires a CXTHub login within 10 minutes.
Subsequent verification must return the same external subject; matching email
never merges accounts or replaces a binding. Reconfiguration changes the revision
and invalidates earlier session evidence. Disabling retains bindings/audit while
removing the active connection; it grants no access. Moving a binding to a new
external subject is not automatically supported.

Authorization uses S256 PKCE, nonce and `max_age=0`. Signed RS256/ES256 identity
tokens must match the exact issuer, audience/authorized party, nonce, expiry and
recent authentication time. Only `openid` is requested. Access, refresh and ID
tokens are discarded after verification. The server retains minimal issuer/subject
and authentication evidence, with encrypted client credentials and PKCE material.

The initiating browser session, connection revision and membership are rechecked
before state consumption and again after exchange. Concurrent callbacks exchange
only once. State consumption remains durable if the exchange fails; start again
instead of replaying a code. Binding, session evidence and audit commit together.
A different browser, CLI or MCP token gains no approval from this flow. MFA claims
are retained as evidence but are not advertised as an enforced MFA policy.

Provider discovery, token and key requests have bounded time/body sizes. Every
network connection pins a validated public IP; environment proxies, redirects,
private-address exceptions and insecure issuer/signature bypasses are disabled.
Callback responses never echo provider errors/codes and prohibit caching/referrers.
Reverse-proxy operators must also avoid logging callback query strings.

## Remaining implementation slices

These are planned boundaries, not enabled policies or delivered SSO features:

1. SP-initiated SAML, with entity/audience, request correlation, signature and
   replay validation. Reject unsolicited login; retain encrypted connection keys.
   Extend the implemented OIDC verification into policy-enforced sign-in.
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
