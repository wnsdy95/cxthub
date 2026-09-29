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
repository grants remain authoritative. SCIM and enforced access policies remain separate work.

Operator setup: set `CXT_IDENTITY_ENCRYPTION_KEY` in the API service environment
(or its private `.env`) to a cryptographically random, base64-encoded 32-byte key.
Preserve the same key across API replicas and restarts; keep it out of PostgreSQL,
frontend builds and the MCP environment. No key means the UI reports unavailable;
an invalid configured key fails API startup. Back up the key separately from the
database. Use the staged rotation procedure below; replacing the legacy key
in place makes existing encrypted connections and attempts unreadable.

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

## CLI and MCP identity approvals

Enterprise **Identity → CLI and MCP identity approvals** lists only the current
browser user's live CLI credentials and individual MCP grants. Select a connection
you recognize after verifying this exact browser with OIDC or SAML within ten
minutes. One approval applies to one credential and one Enterprise. Matching email,
user, client name, token creation time or device label never transfers approval.
CLI selectors are random non-bearer IDs, not display hints or token hashes. Legacy
MCP credentials appear only after a successful refresh upgrade or fresh consent.

An owner can set **Maximum approval duration** from 1 to 24 hours (default 8).
Time starts at the provider's original signed authentication time. The browser's
original expiry, the target credential/grant expiry and any SAML
`SessionNotOnOrAfter` bound it further. Token refresh and repeated approval do not
renew authentication or extend that deadline. Changing the duration invalidates
previous evidence; it does not stretch existing approvals. A shorter credential
expiry captured at approval remains a bound even if the MCP grant later refreshes.

ID-token `exp` and SAML assertion/confirmation `NotOnOrAfter` govern initial
protocol acceptance, not the separately recorded authenticated session lifetime.
All protocol expiry checks still run. In SAML the pending, single-use browser
completion is also capped by the assertion deadline and two minutes. Only a fully
completed, verified flow creates usable evidence. Existing pre-0070 proofs have
no policy/domain provenance and require verification again; they are never
silently extended. The optional IdP session bound is retained independently.
See [OIDC Core §2](https://openid.net/specs/openid-connect-core-1_0.html#IDToken)
and [SAML Core §2.7.2](https://docs.oasis-open.org/security/saml/v2.0/saml-core-2.0-os.pdf).

Approval and audit commit atomically with fresh checks of browser ownership,
membership, identity binding, connection/policy revision, DNS observation and
target lifecycle. Reads recompute validity; they do not trust a stored `approved`
flag. Domain challenge renewal changes provenance and requires new verification.
Revoking an approval preserves the CLI/MCP credential, data, membership and audit.
Revoking the credential itself makes its approval unusable. Browser logout prevents
new approvals; it does not undo approvals already issued to separate credentials.

Apply migration 0070 before the compatible API/MCP binaries. This is PostgreSQL-only
and uses the shared identity transaction; the FS adapter reports unavailable.
No machine token/hash, external subject or bearer proof is returned to the UI.
The UI refreshes on focus, manual refresh and displayed deadlines, without a
periodic polling loop.

This is explicit verification evidence, **not mandatory SSO/MFA enforcement**.
No repository permission changes. ACR/AMR are retained without inferring MFA.
Common authenticated-credential checks for reads and transactional writes,
recoverable-owner policy activation, SCIM and customer IdP acceptance remain
separate delivery steps. No customer IdP or enforced policy is activated locally.

## SAML browser verification

SAML uses the same API-only `CXT_IDENTITY_ENCRYPTION_KEY` and PostgreSQL identity
transaction boundary as OIDC. It links an existing, recently authenticated
CXTHub account. Neither protocol currently enables mandatory SSO or grants
organization/repository membership. No customer IdP is activated by this change.

An owner verifies the company domain, opens **Identity → Configure SAML**, and
registers the displayed entity ID and assertion consumer URL with the IdP. Paste
that provider's exact EntityDescriptor XML with its current signing certificates.
The API pins this metadata; it never downloads certificates or metadata from
assertion-supplied URLs. The owner must update metadata for IdP key rotation.
After saving, import CXTHub's public SP metadata into the IdP to trust our signed
AuthnRequests. Metadata updates preserve the SP key and change the connection
revision, invalidating previous proofs and outstanding requests.

The supported profile is SP-initiated HTTP Redirect request / HTTP POST response,
ForceAuthn, persistent NameID, and exactly one signed plaintext assertion. Issuer,
audience, recipient, destination and request ID must match exactly. Signed
responses must also validate; response signing never substitutes for assertion
signing. Only RSA/ECDSA SHA-256/384/512 signatures and SHA-256/384/512 digests are
accepted. Assertions have at most ten minutes remaining validity, require fresh
authentication and must be unexpired at receipt and completion. An authentication
context is retained without inferring that it proves MFA. IdP-initiated login,
transient/email subject identifiers, encrypted assertions, artifact binding,
metadata aggregates and single logout are not supported. Unsupported input fails
closed. XML size/depth, directives, duplicate IDs and ambiguous assertion shapes
are checked before the protocol library.

Cross-site POST cannot rely on the main SameSite=Lax cookie. The ACS endpoint
ignores cookie authority: it validates the signed response against a durable
request, atomically reserves the assertion ID, and stages a proof with a random,
one-use completion ticket for at most two minutes. A 303 redirect then requires
the exact initiating browser credential. Binding, session proof, consumption and
audit commit together after rechecking domain, connection revision, membership
and session. A separate browser or CLI/MCP credential gains no verification.
This exact ACS route is exempt from cookie-CSRF validation because it grants no
cookie authority. Other writes retain the existing Origin/header boundary.

Assertion receipt and completion survive API process restart. Replayed assertion
IDs and repeated callbacks cannot publish a second binding. Raw XML responses
and attributes are discarded. The database retains only identity/authentication
evidence, hashes of state/tickets and replay IDs. Callback pages are no-store and
no-referrer; operators must exclude callback query strings and form bodies from
proxy/access logs. SP signing keys are generated in the API, encrypted at rest,
and never returned. Certificates expire after three years; use the coordinated
signing lifecycle below before expiry. Encryption-key rewrap preserves signing
keys and certificates. Mandatory policy remains disabled until credential
assurance, enforcement and owner recovery are implemented.

Local signed IdP fixtures exercise API/PostgreSQL, duplicate callbacks, wrong
browser, restart, replay and revocation. These fixtures do not replace acceptance
against a customer's real IdP configuration.

## Encryption key rotation and recovery

`CXT_IDENTITY_ENCRYPTION_KEYRING` is API/operator-only JSON with `active` and
`keys` fields. `keys` maps immutable IDs (1–48 ASCII letters, digits, `_`, `-`)
to independent random base64-encoded 32-byte keys; at most 16 IDs are supported.
`legacy` is reserved for `CXT_IDENTITY_ENCRYPTION_KEY`. Unknown fields, duplicate
JSON keys, missing active keys and invalid material fail startup without printing
the supplied configuration. Never put this value in MCP/frontend configuration,
command-line arguments, audit reasons or Git. A key ID is not a secret; its
material must never be replaced under the same ID.

The legacy-only setting remains compatible with existing `v1` ciphertext. A
keyring writer uses `v2`, authenticating its key ID together with the existing
tenant/revision or attempt purpose. An unknown ID, incorrect key, changed header
or ciphertext fails authentication. The rotation workflow never changes external
bindings, memberships, connection revisions, browser proofs or SP certificates.

Use this order for every API replica:

1. Back up PostgreSQL and the exact current keys separately. Rehearse restoring
   both in an isolated environment. Losing all copies of a required key cannot
   be repaired from ciphertext or account email.
2. Roll out keyring-capable code while retaining the legacy setting. Add the new
   key to every replica's read set, with `active` still `legacy`. For example,
   the configuration shape is `{"active":"legacy","keys":{"rotation-1":"<base64 key>"}}`;
   the placeholder is deliberately invalid. Verify every replica before step 3.
3. Change `active` to `rotation-1` on all replicas, retaining the old read key.
   Do not run rewrap while old binaries or old-only readers remain. Existing
   requests may have loaded old ciphertext and still need the old key.
4. Build `go build -tags postgres -o cxt-admin ./cmd/cxt-admin` from `backend`.
   Give this operator process the database DSN and exact API key configuration
   through its private environment. It does not load `.env` automatically or
   print configuration. Run `cxt-admin identity-keys` to inspect up to 100 values.
   Follow each returned `next` cursor with `--after` until `complete` is true.
   Inspection authenticates values but writes neither credentials nor receipts.
5. Apply with `cxt-admin identity-keys --apply --operation rotation-1 --actor <operator-id> --reason <reason>`.
   Repeat using the returned cursor until complete. Each bounded page commits
   replacements and its audit receipt together. A failed page rolls back; earlier
   completed pages remain valid. If the outcome is unknown, retry the exact same
   operation/cursor/limit/actor/reason to retrieve its receipt. A changed request
   conflicts. No values, plaintext or ciphertext appear in the receipt.
6. Run a fresh inspection from the beginning after all writers have switched.
   New rows may have appeared behind an earlier cursor. `--enterprise <ep_id>`
   can limit a rehearsal or rollout, but that is not a global retirement check.
   Inspect all pages and all Enterprises before considering retirement.
7. Retain old keys while any replica, in-flight request, database backup or
   rollback version needs them. A zero old-key count proves only the inspected
   database view; it cannot certify replica state or backup retention. Remove a
   read key only after those independent conditions are verified. Restoring an
   older backup also requires restoring its matching keys before opening traffic.

The operator command requires the migrated schema. Its audit receipts are stored
in `identity_key_batches`; retries recover committed progress after process
restart. Inspection/apply includes OIDC client secrets, all retained OIDC PKCE
verifiers (including consumed attempts), and SAML SP private keys. Unreadable
material stops that page instead of silently omitting it. No deletion or automatic
key generation/retirement occurs. An unavailable backup is a recovery limitation,
not permission to discard a connection or bind a different identity.

## Remaining implementation slices

The credential foundation now persists a separate MCP authorization per consent,
retains its identity through rotation, and revokes that authorization on refresh
reuse. CLI credentials already have individual stored token hashes. These are
identifiers, not IdP verification or MFA proof. The explicit approval flow above
can attach recent browser evidence to one of these credentials. Issuance and refresh
never copy evidence automatically. See [MCP lifecycle](MCP.md#authentication-and-authorization)
and [OAuth refresh security](https://www.rfc-editor.org/rfc/rfc9700.html#section-4.14.2).

These are planned boundaries, not enabled policies or delivered SSO features:

1. Extend OIDC/SAML browser verification into policy-enforced sign-in. The
   coordinated SP signing lifecycle is implemented; additional SAML profiles
   require their own interoperability coverage. Mandatory policy remains disabled.
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

## SAML request signing lifecycle

Enterprise owners can rotate the SP signing key in **Identity → SAML → Request
signing certificate**. The server reports the current and alternate certificate's
SHA-256 fingerprint, validity dates and expiry state (including a 90-day warning).
Private keys are generated by the API and never returned to the browser.

1. **Prepare new certificate** creates one encrypted alternate key. The current
   key continues signing; public SP metadata advertises both certificates, each
   in its own signing KeyDescriptor. Preparation can be canceled without changing
   the active key. No automatic timer switches or removes keys.
2. Import the updated metadata into the customer's IdP and configure it to trust
   both certificates. The owner must explicitly confirm this before **Activate
   new certificate**. Publishing metadata does not prove the IdP imported it.
3. Activation signs subsequent requests with the new key and retains the previous
   key for rollback. Complete **Verify with SAML** in the same browser. Only an
   authenticated, current-revision callback completion records the roundtrip;
   merely receiving an assertion, another credential or a replay cannot do so.
4. Before retirement, **Restore previous certificate** returns to the old signer
   if its certificate is still valid and cancels this rotation. It also removes
   the unsuccessful new key from current configuration. To retry, prepare a new
   rotation and update the IdP again. If the previous certificate has expired,
   rollback is rejected; update IdP trust and complete the new flow instead.
5. After a successful roundtrip, **Retire previous key** explicitly removes the
   old private key and certificate from current configuration/metadata. Keep
   independently required backups. Retirement removes the rollback option.

A successful response demonstrates interoperability for that browser flow. It
does **not** prove that the IdP requires request signatures; that provider policy
must be checked independently. No customer IdP is configured during development.

Every lifecycle action checks the owner and editor revision within the identity
transaction and commits its audit together with the state. Failed audit/storage
rolls back the entire transition; stale concurrent requests cannot replace a key.
All transitions invalidate previous attempts/proofs by changing the connection
revision. A current-revision verification mark is saved atomically with browser
binding, attempt consumption and audit. Replays cannot mark a new rotation tested.

IdP metadata can be repaired during rollover: it preserves both SP keys and their
rotation identity while changing the connection revision and clearing the old
verification mark. This avoids lockout if the IdP changes trust mid-rotation.
The metadata endpoint and configuration repair allow an expired SP certificate
so a replacement can be published. New signed login requests still reject expired
or not-yet-valid active certificates. Key-pair validation is never skipped.

Encryption inspection/rewrap includes the prepared or previous alternate key in
`sealed_alternate_key`. Apply schema migration 0068 and deploy the updated operator
with the API before using rollover. An older operator cannot inventory this new
column and must not be used to decide encryption-key retirement. Coordinated
replica rollout, database/keys backup and restore rules remain unchanged.

Reference: [OASIS Metadata Interoperability Profile](https://docs.oasis-open.org/security/saml/Post2.0/sstc-metadata-iop.html)
permits publishing future signing keys for rollover. Customer-specific metadata
import/caching and signature-enforcement settings still require acceptance.
