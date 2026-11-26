# GitHub connections

Implementation tracking: [#266](https://github.com/wnsdy95/cxthub/issues/266).

## Contract

GitHub sign-in identifies a person. GitHub App installation authorizes server
access to selected repositories. Neither grants CXTHub membership by itself.
Existing Firebase UIDs, repository IDs, stored sessions and manual access
grants remain unchanged.

The first release targets GitHub.com personal accounts and organizations.
Teams are mapped within an organization installation. Enterprise displays its
organizations' connections without gaining implicit repository access.
Enterprise-level API operations, SCIM/SSO, GHES, and GitHub role mirroring are
separate extensions. Connecting a GitHub repository does not make its CXTHub
repository public.

## Implementation sequence

Each phase must pass its focused checks before proceeding to the dependent phase.
Shared regression checks run after integration. Production registration and real
provider verification are reported separately from code completion.

### 1. Sign-in and account linking — implemented

- Add Firebase GitHub sign-in with identity-only scopes.
- Keep existing Google and email/password sign-in.
- Link a pending GitHub credential only after authentication to the matching
  existing account; never merge accounts from email text alone.
- Keep pending credentials in memory with an expiry, never in application logs
  or durable browser storage.
- Handle missing/unverified email, verification resend and return, cancelled
  popups, and credential collisions without weakening the backend verifier.
- Preserve the current UID and profile when providers are linked.
- Validate the workflow with synthetic provider fixtures and browser tests.

### 2. Installation and repository binding — implemented

- Add external identity, installation, connection request, and repository binding
  domain records and PostgreSQL/FS storage adapters.
- Bind state to the CXTHub actor, selected owner, expiry and one-time callback.
- Verify GitHub identity, installation association, and external/local management
  authority server-side; an installation_id query parameter is not proof.
- Store immutable GitHub account/repository IDs; GitHub.com is the explicit host boundary.
- Expose backend-derived connection status and selected repositories in personal
  and organization settings. New repositories are candidates, not auto-created.
- Validate spoofed/replayed callbacks, cross-owner binding, renamed repositories,
  duplicate connections, and revoked administration during a request.

### 3. Server authentication and events — implemented

- Generate and refresh installation tokens only on the server.
- Scope token caches, request limits and backoff to installations.
- Persist signed events before acknowledging them; reconcile missed changes.
- Handle installation suspension/removal and repository selection changes.
- On a branch push, record in the branch ledger that the branch name is alive
  or gone, and ask for a read of the remote. On a pull-request event, ask for
  that read.
- Read the hosting provider with the installation's token for a repository
  that is bound to a connection.
- Recheck the current binding before committing results from external requests.
- Validate expiry, concurrent refresh, duplicate/out-of-order delivery, restart
  and revocation during work.

### 4. Team connections — implemented

- Explicitly map an authorized GitHub team to a CXTHub team.
- Membership synchronization is opt-in; repository roles remain CXTHub policy.
- Track synchronized membership independently from manually assigned membership.
- Only match users through verified external identity; unresolved members do not
  silently become active CXTHub accounts.
- Removing a GitHub-derived grant does not remove an independent manual grant.
- Validate offboarding, nested teams, missing external identities, manual grants,
  permission changes and synchronization failures.

### 5. Enterprise connection management — implemented

- Show connection health for organizations belonging to the Enterprise.
- Enforce Enterprise visibility and organization connection-management permissions
  separately. Enterprise membership is not repository read access.
- Allow navigation to each organization's connection setup and repair flow.
- Validate unrelated organizations and member/admin/owner boundaries.

## Architecture and persistence

The application/domain owns connection authorization and synchronization policy.
GitHub HTTP, signatures and token exchange live in adapters behind outbound
ports. PostgreSQL persists connections and jobs; production transaction checks
include concurrent authority changes. Web keeps selection/dialog state and renders
server decisions. FS remains a single-process development adapter; no unsupported
multi-file crash rollback guarantee is implied.

Stored sessions are not migrated or changed by this feature. Installation
removal stops external synchronization and invalidates its derived authority;
it does not erase stored sessions.

## Verification and rollout

- Focused backend domain/application/adapter tests, including a fake GitHub server.
- Real PostgreSQL migration, isolation, rollback and revocation tests.
- Frontend unit, i18n, architecture, typecheck, build and browser E2E checks.
- Existing session, secrets and account login regressions.
- Signed commit and PR checks, then install the exact merged binaries for dogfood.
- Real GitHub/Firebase verification requires configured registrations, callback
  URLs, App credentials and a reachable signed HTTPS webhook receiver. Do not
  label fixture-only verification as a completed live integration.

## Provider configuration

There are two independent registrations. The Firebase OAuth App signs a person
in; the server GitHub App reads selected repository data. Do not reuse their
client secrets or imply that signing in installs the server App.

### Firebase login

1. Register an OAuth App in GitHub for Firebase authentication. Copy the exact
   authorization callback URL shown by Firebase's GitHub provider configuration
   (normally `https://<auth-domain>/__/auth/handler`).
2. Enable GitHub in Firebase Authentication and enter that OAuth client ID and
   secret. Keep one account per email enabled, and configure authorized domains
   and verification-email action URLs for the actual frontend origin.
3. Keep the existing Firebase frontend/server settings. Set
   `VITE_GITHUB_LOGIN=true` and rebuild the frontend only after the provider is
   configured. The default is `false`; there is no dead sign-in button by default.
4. Validate new sign-in, existing Google/email account linking, popup cancellation,
   email verification and a subsequent sign-in to the same Firebase UID.
   The backend still rejects an unverified email. Credentials waiting to link
   expire in page memory after ten minutes; refreshing starts the flow again.

### Server GitHub App

Register an App installable by the intended personal and organization accounts:

- Callback URL: `<public-origin>/api/v1/github/callback`.
- Setup URL: `<public-origin>/api/v1/github/setup`.
- Webhook URL: `<public-origin>/api/v1/github/webhook`, with a strong webhook secret.
- Leave **Request user authorization (OAuth) during installation** disabled. The
  setup endpoint starts a separate PKCE authorization flow after installation.
- Repository permissions: Metadata read, Contents read, Pull requests read.
- Organization Members read is required to verify an organization administrator
  during organization connection and to read its teams. Personal installations
  do not require this organization permission. If it is later withdrawn, team
  grants stop; already connected repository operations remain independent.
- Subscribe to Pull request, Push, Membership, Team and Member changes; process
  the automatically delivered Installation and Installation repositories events.
- Generate an RSA private key; the server accepts PEM with actual newlines or
  literal `\n` line separators. Never place it in a `VITE_` variable.

Set all six server variables together; partial configuration stops startup:

| Variable | Value |
| --- | --- |
| `CXT_GITHUB_APP_ID` | Numeric GitHub App ID |
| `CXT_GITHUB_APP_CLIENT_ID` | GitHub App OAuth client ID |
| `CXT_GITHUB_APP_CLIENT_SECRET` | GitHub App OAuth client secret |
| `CXT_GITHUB_APP_PRIVATE_KEY` | GitHub App RSA PEM private key |
| `CXT_GITHUB_APP_SLUG` | App slug used in its installation URL |
| `CXT_GITHUB_APP_WEBHOOK_SECRET` | The exact webhook signing secret |

`CXT_PUBLIC_URL` is the externally reachable backend origin. `CXT_WEB_URL`, when
set, is the frontend origin used after authorization; otherwise the public origin
is used. Only HTTPS origins (or loopback HTTP for development) are accepted.
Use the same hostname for development login and callback so the authenticated
session cookie is available; ports can differ. An HTTPS tunnel is needed for
GitHub to deliver webhooks to a local backend. Do not point production callbacks
at the local fixture server.

Cloud Run's optional `github_app` Terraform input takes the public App fields and
three existing Secret Manager secret IDs. The runtime service account receives
access only to those secrets. Secrets are referenced by ID, not copied into
Terraform variables or source. This change validates configuration but does not
create an App, issue credentials, or run `terraform apply`.

### Connecting data

1. Open **GitHub connections** from account or organization settings.
2. Verify the external identity, or connect an installation (which also verifies
   that identity). Firebase provider linking and this external-ID proof are
   separate; no GitHub account is inferred from an email or display name.
3. The actor must administer the CXTHub owner and prove GitHub account ownership
   or GitHub organization administrator membership. Pending organization approval
   grants nothing; after approval, restart Connect from this page.
4. Select an installed GitHub repository and an existing CXTHub repository
   with the same Git origin. Binding requires that you maintain the CXTHub
   repository, as a maintainer or an owner. The repository can be in another
   owner space than the installation: the code of a repository often lies in
   the GitHub account of a collaborator, who then connects it through their
   own installation. Organization owners inherit the owner role; organization
   administrators do not. Connection management never reveals another private
   repository's metadata.
5. Optionally map teams and explicitly enable **Sync members**. Both verified
   GitHub identity and current CXTHub organization membership are required. The
   workflow does not create accounts, invite outsiders, mirror GitHub roles or
   change repository visibility.
6. Enterprise's Organizations view reports connection status and links to setup
   only where the actor has local organization management authority. Enterprise
   membership does not grant repository access.

## What readers of a repository are told

`GET /api/v1/repos/{repoID}/github-connection` answers, to whoever may view the
repository, one of three states.

| State | Meaning |
| --- | --- |
| `connected` | an installation in use was told to read this repository: the server reads it on GitHub with that installation |
| `not_connected` | no installation in use was told to read this repository |
| `not_configured` | this server has no GitHub App |

An installation is in use while it is enabled, not suspended, and GitHub
answered for it within the last ten minutes. The answer is made by the rule
that gives the reader of GitHub its credential, so a repository is told as
connected exactly while the server reads it with the installation.

Except on a server without a GitHub App the answer carries `connect_url`, the
GitHub connections page, where a maintainer of the repository connects it
through an installation they manage. A repository that is not connected is
said in three places:

- the Sessions tab shows a member a notice with a link to that page; one who
  only views the repository is not told, and is not asked for;
- `cxt setup` prints the line with the address and ends as not complete, with
  exit status 1, after it set up everything on the computer;
- `cxt doctor` reports the problem `github-not-connected` with the address.

A server without a GitHub App says nothing in those places: it reads GitHub as
the next section describes. A server from before this route answers 404, which
cxt and the web read as a server that does not say.

To know before an owner installs the App: from then on the server reads that
owner's repositories only through an installation. A repository of the owner
that no connection was told to read is no longer read with `CXT_GITHUB_TOKEN`,
and is told as `not_connected`. A repository that was bound through the
installation of another owner space is read through that installation.

## Two ways GitHub tells the server

There are two, and they are not the same thing.

| | Repository webhook | GitHub App (the connections of this document) |
| --- | --- | --- |
| Set up | one webhook on one GitHub repository (`scripts/github-webhook.sh`) | one installation on an account or organization |
| Route | `/api/v1/hooks/github`, secret `CXT_GITHUB_WEBHOOK_SECRET` | `/api/v1/github/webhook`, secret `CXT_GITHUB_APP_WEBHOOK_SECRET` |
| Tells of | a push to a branch, a change of a pull request | the same, for every repository of the installation |
| Lets the server read | nothing | the installed repositories, private ones too |

Either one only tells: what the server stores of commits, branches and pull
requests it reads from GitHub itself. With a repository webhook alone the
server reads with `CXT_GITHUB_TOKEN` when that is set and without a credential
when it is not. Without a credential GitHub answers for public repositories
only and allows 60 requests an hour for each address the server sends from;
the first read of a repository with a few hundred pull requests needs several
hundred. The Terraform files of this repository set the webhook secret and no
`CXT_GITHUB_TOKEN`.

## Operational behavior and limits

- One installation is linked to one CXTHub owner namespace. Multiple repository
  bindings share that installation; an external repository is bound once within
  it, and a CXTHub repository in at most one connection. A binding ends when a
  maintainer of the repository who manages the connection removes it, when the
  connection is disconnected, or when the GitHub repository leaves the
  installation. An unrelated GitHub account cannot replace an installation
  through a reconnect.
- Source repository and account IDs survive GitHub renames. Verified API requests
  use the new name while the stored origin remains unchanged. Transfers
  to another GitHub owner require new authorized installation and binding.
- Webhooks persist before acknowledgement. Duplicate delivery IDs must have the
  same event kind and content hash. Completed payloads are discarded while the
  idempotency proof remains. PostgreSQL transactions and leases allow multiple
  workers and restarts; applying a delivery rechecks the connection and its
  binding inside the repository transaction.
- Installation/member changes immediately suspend imported grants until an
  authoritative REST refresh. Reconciliation runs every five minutes; imported
  grants expire after ten minutes without refresh. Remote failures narrow access
  and retry. Disconnect and organization offboarding remove derived grants;
  independent manual memberships remain.
- Team members returned by GitHub include child-team membership according to the
  GitHub API. This does not create or mirror a nested CXTHub team hierarchy.
- A push or pull-request delivery only asks for a read of the remote; the
  remote Git reader reads branch heads, commits and pull requests from GitHub,
  at once: it hears from the store that a read was asked for. A second read of the same
  repository starts no sooner than ten seconds after the first, so a pull
  request opened right after its push is read up to ten seconds later. A read
  with nothing new costs three requests; one after a push of up to a hundred
  commits costs four. The first read of a repository goes through every pull
  request, forty requests at a time with a minute between: a repository with
  a few hundred pull requests takes about ten minutes.
  The reader also reads without being asked, every six hours, which makes up
  for a delivery that never arrived; a member's cxt asks for a read after it
  pushed or fetched, and so does a look at the Sessions tab once the last read
  started five minutes ago or more. That look is what reads a push made where
  no delivery arrives and no cxt runs. No session is moved or merged by a
  delivery, and nothing is inferred from a pull request's title or a
  repository URL.
- Server operator-token fallback (`CXT_GITHUB_TOKEN`) remains for repositories
  whose owner has no connection.
- GitHub.com only. GHES, direct Enterprise installations/API operations, SCIM/SSO,
  automatic role mirroring, billing and provider-user-token storage are excluded.
- Durable request/idempotency metadata has no retention sweep in this release;
  completed webhook bodies are cleared and PostgreSQL workers query only due jobs.
  Retention can be added without changing the external access contract.

## Validation status

Code and synthetic/local verification are complete before live rollout:

- Backend full test/vet, focused GitHub OAuth/token/webhook tests with race detector.
- PostgreSQL 16 migration and three repeated race runs of transaction, permissions,
  rollback and source-aware team/offboarding contracts.
- Frontend unit, i18n, dependency-boundary, typecheck and production build checks.
- Full browser E2E at the time, including MCP and authorization regressions;
  GitHub connection selection/generation and mobile layout covered.
- Terraform initialization without backend state and validation; no cloud apply.

Still required in the operator environment: create/configure both registrations,
set real secrets and callback domains, verify real personal and organization
installations, sign-in/account-linking, signed webhook delivery, team removal and
reconnect. Fixture tests do not certify those live operations. Keep
`VITE_GITHUB_LOGIN=false` and the server App variables unset until configured.


Firebase GitHub login uses an OAuth registration configured in Firebase. Server
repository access uses a GitHub App; its private key and webhook secret never
reach the browser. Development/staging and production use separate callbacks and
credentials. No user needs to supply a PAT for the normal installation flow.

## Research basis (checked 2026-09-25)

- [Firebase GitHub authentication](https://firebase.google.com/docs/auth/web/github-auth)
- [Email trust and linking](https://docs.cloud.google.com/identity-platform/docs/concepts-manage-users)
- [Installation callback verification](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/about-the-setup-url)
- [Installation authentication](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation)
- [GitHub team members](https://docs.github.com/en/rest/teams/members#list-team-members)
- [Enterprise installation limitations](https://docs.github.com/en/enterprise-cloud@latest/apps/using-github-apps/installing-a-github-app-on-your-enterprise)
- [Webhook failure recovery](https://docs.github.com/en/webhooks/using-webhooks/handling-failed-webhook-deliveries)
