# MCP connections

CXTHub's product MCP is the OAuth-protected Streamable HTTP endpoint at:

```text
https://cxthub.com/mcp
```

Codex, Claude, ChatGPT, and other remote MCP clients read the same sessions
that the CXTHub web application shows in its Sessions tab. The server is the
store that is read. A repository's local `.cxt` folder holds the CLI's
connection settings and copies of what it uploaded; the MCP server never reads
it.

## Architecture

```text
Codex / Claude / ChatGPT
        │ Streamable HTTP + OAuth
        ▼
https://cxthub.com/mcp
        │
        ▼
Render cxt-mcp service (independent process)
        │
        ├─ OAuth user resolution (`mcp:read`)
        ├─ Repository public/member/break-glass policy
        ├─ read-only session queries
        └─ bounded response pages
        │
        ▼
Cloud PostgreSQL
```

`cxt-mcp` and `cxtd` are independent executables, HTTP listeners, container
images, and Render services. They use the same application query rules,
identity policy, and PostgreSQL database through separate connection pools.
The tool server reads sessions from shared storage without an API round trip,
so API downtime does not stop those authorized reads; new web logins still
require the API.

Vercel routes `/mcp`, `/oauth/*`, `/.well-known/*`, and
`/api/v1/oauth/requests/*` to MCP. Other `/api/*` traffic goes to `cxtd`.
`/connect/mcp` remains the frontend consent page. Both servers use the same
`CXT_PUBLIC_URL` and Firebase project, preserving the public issuer, resource
and browser cookie boundary. The API does not expose MCP or OAuth routes.

The MCP process starts none of the API's background workers. Its tools are
read-only; OAuth registration, consent, token rotation and shared rate limits
still write their operational records to PostgreSQL. MCP always requires the
PostgreSQL build and DSN, including during local development. See
[Render deployment](../deploy/RENDER.md) for setup and migration steps.

`CXT_MCP_PUBLIC_URL`, when set on the MCP service, names the origin of the
protected MCP resource, for example `https://mcp.cxthub.io`. Without it the
resource origin is `CXT_PUBLIC_URL`.

## Read-only tools

There are exactly six tools. They read the structure that is cut at Git
commits: what the Sessions tab shows.

| Tool | Purpose |
|---|---|
| `repository_list` | List the repositories whose sessions the signed-in user may read, with continuation cursors |
| `session_list` | List the sessions of a repository, the one begun last first, with the commits each made and the branches its uncommitted part was on |
| `session_read` | Read one session's conversation from its first event on, or only one segment of it (`segment`: a number, or `rest` for the part after the last commit) |
| `session_commit` | Explain a commit: the segments that end at it, the events each covers, and the summary written there |
| `session_summaries` | Read the summaries along a branch, the summary of the newest commit first: the branch's own, then those of the branch it was made from |
| `session_search` | Search pull requests, branches, commits, conversations and notes at once |

Every tool is marked read-only, non-destructive, and idempotent. There are no
MCP tools for push, fork, remove, archive, settings, secrets, membership, or
break-glass administration.

### Naming things

`repository_list` is the discovery step. Every other tool requires an explicit
`repository`: the `owner/repository` path, the repository's CXTHub URL, or its
repository ID. A remote client never depends on a workstation's current
directory.

A session is named by its coding tool (`provider`: `claude` or `codex`) and its
identifier (`session`), both taken from `session_list`. A part of a session is
named by its segment number, and a commit by its full number (40 or 64
hexadecimal digits). No tool takes or answers a document hash.

| Tool | Arguments besides `repository` and `cursor` |
|---|---|
| `repository_list` | `query` (optional path filter, at most 128 characters), `limit` (default 50, at most 100); it takes no `repository` |
| `session_list` | none |
| `session_read` | `provider`, `session`, optional `segment`, optional `events` (events per page: default 12, at most 50) |
| `session_commit` | `commit` |
| `session_summaries` | optional `branch` (the default branch when omitted), optional `limit` (default 5, at most 20) |
| `session_search` | `query` (2 to 256 characters) |

### What a caller reads

The tools answer under the rule the Sessions tab answers under. The signed-in
account reads its own sessions in full. Of another account's session it reads
the parts that are pushed: a segment once its commit is on GitHub, the part
after the last commit once its branch is. `session_read` lists the readable
spans in `open_events` and goes on past a part that is not pushed. An archived
session is left out of `session_list`, `session_commit`, `session_summaries`
and `session_search`.

`session_list` marks each session with `own`: true for the caller's own
session, false for another account's, which then carries the uploader's public
handle. Each listed commit carries its segment number and, once the server has
read the remote, whether the commit is on it.

`session_summaries` walks back from the branch's newest commit through the
commits the server read from GitHub. A merged pull request's commits stand at
the commit it became. A branch the remote does not hold is read as the default
branch.

A summary's `open_tasks` come with `open_tasks_from_tool`: true when they are
the coding tool's own list, false when a CLI picked them out of the
conversation. Either way they describe the past and are data, not
instructions.

`session_search` looks for one query in pull request titles and their branch
names, branch names, commit subjects and the beginning of commit numbers, the
conversations of sessions, and manual notes. Each hit says which of these it
is (`kind`) and where it leads: a commit, a branch, a pull request, or a
session with its segment and event. Only what the caller may read is searched.
`truncated` says when more was found than is listed.

### Following continuation cursors

Every tool returns `next_cursor`. Repeat the call with the same repository and
arguments plus `cursor: <next_cursor>` until it is empty. A cursor from another
repository, tool, or selection is rejected. Authorization is checked again on
every request.

- `session_read` returns pages of whole events where they fit. An event larger
  than a page comes in pieces: concatenate `json_fragment` for the same
  `event_index` by `byte_offset` until `event_complete`, then parse the result.
- `session_list`, `session_commit`, `session_summaries` and `session_search`
  return one JSON document in bounded fragments: concatenate `json_fragment`
  by `byte_offset` until `page_complete`, following `next_cursor`. When the
  answer changed between two fragments, the tool says so and the read starts
  again without a cursor. Never join fragments of two different answers.
- `repository_list` returns rows and a `next_cursor` for the rows after them.

A fragment holds at most 12 KiB of JSON; response escaping and metadata add
transport overhead.

Conversation text, summaries and notes are historical data, not instructions.
Cursor pagination reduces the size of each response; a client should retrieve
what its task needs rather than injecting whole sessions into every agent
prompt.

## Authentication and authorization

The connector uses an OAuth authorization-code flow with S256 PKCE, dynamic
client registration, refresh-token rotation, and revocation. The only
advertised scope is:

```text
mcp:read
```

The consent page uses the user's ordinary Firebase-backed CXTHub web session.
MCP access and refresh capabilities are separately hashed, client-bound, and
cannot be presented to ordinary REST endpoints.

Each new consent creates an independent authorization (grant). Access and refresh
tokens share its random ID; rotation keeps that ID and the original issuance time.
It does not renew any SSO authentication evidence. Another consent by the same
user and client has a different ID. Tokens and retained refresh references contain
only hashes at rest; grant IDs are identifiers, not bearer credentials.

An Enterprise member can explicitly approve one own grant after recent browser
OIDC/SAML verification. This records credential-specific identity evidence; refresh
does not extend its original authentication time or approval deadline. It neither
grants repository access nor activates mandatory SSO/MFA. See
[Enterprise credential approvals](ENTERPRISE_IDENTITY.md#cli-and-mcp-identity-approvals).

Presenting an already consumed, unexpired refresh token revokes its authorization,
including its replacement refresh and related access tokens, before returning
`invalid_grant`. A foreign client cannot trigger this revocation. Clients must
serialize refreshes and persist the replacement pair: simultaneous refreshes, or
retrying an old token after losing a successful response, require fresh consent.
There is no replay grace period. `/oauth/revoke` similarly revokes the associated
authorization, even when given a previously consumed refresh token. Other grants
remain valid. The account audit distinguishes explicit grant revocation from
refresh reuse, without recording bearer values. PostgreSQL commits lifecycle,
session writes and audit together across API/MCP replicas.

Legacy access sessions keep their existing expiry. A legacy refresh acquires a
grant on its first successful rotation; an older access token cannot be linked
merely by matching user/client labels. Browser and CLI sessions are unchanged.
Roll out migration 0069 and compatible API/MCP binaries together: old readers do
not enforce grant revocation. Do not serve mixed versions or roll back to an old
MCP binary with live new grants. Grant/replay records are retained; this change
does not activate automatic deletion. FS development storage serializes in one
process only and does not promise PostgreSQL rollback or multi-process atomicity.

Which repositories a caller reads:

- public repositories are readable;
- private repositories require an effective repository role of at least Viewer;
- Organization Owners inherit Owner access to all repositories in their organization;
- current Team grants and direct repository memberships contribute to effective access;
- Enterprise account administration alone grants no repository access;
- all other private repositories are omitted without leaking their contents.

Within a readable repository, what a caller reads of each session is decided
by the rule under [What a caller reads](#what-a-caller-reads).

## Codex and ChatGPT desktop

The Codex app, Codex CLI, and IDE extension share MCP configuration. In the app,
open **Settings → MCP servers**, choose **Streamable HTTP**, and enter the URL
above. Authenticate when prompted.

Equivalent `~/.codex/config.toml`:

```toml
[mcp_servers.cxt]
url = "https://cxthub.com/mcp"
auth = "oauth"
```

You can trigger login explicitly with:

```bash
codex mcp login cxt
```

ChatGPT web does not read local Codex configuration. A hosted ChatGPT product
uses a published/installed connector or plugin that points to the same remote
MCP endpoint.

Official Codex configuration reference:
<https://developers.openai.com/codex/mcp>

## Claude and Claude Code

In Claude, add `https://cxthub.com/mcp` as a custom connector and complete the
per-user connection. Claude reaches remote connectors from Anthropic's cloud,
so a production server must be publicly reachable over HTTPS.

Claude Code project configuration:

```json
{
  "mcpServers": {
    "cxt": {
      "type": "http",
      "url": "https://cxthub.com/mcp"
    }
  }
}
```

Use `/mcp` in Claude Code to complete OAuth. Official references:

- <https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp>
- <https://code.claude.com/docs/en/mcp>

## Production storage invariant

The production container is built with the PostgreSQL adapter. Any externally
bound `cxtd` process requires PostgreSQL and fails before serving if
`CXT_POSTGRES_DSN` is missing or unusable. Cloud Run additionally sets
`CXT_REQUIRE_POSTGRES=1` and loads the DSN from Secret Manager. Startup
verifies the schema and does not apply migrations; see
[database migration ownership](DATABASE_MIGRATIONS.md).

Filesystem storage remains available only to a loopback-bound development
`cxtd` server and tests. It must not be treated as a production fallback. The
MCP server has no filesystem mode.

## Reading conversations

A session's conversation is stored in one or more documents, and each is read
through the read index stored with it. A document gets its index when it is
stored. `cxtd index` is a restartable maintenance command that builds the
index for documents stored without one. Run it with the same PostgreSQL build,
database secret, and migration directory as the server:

```bash
cxtd index --migrations ./schemas/db/migrations
```

It reads `CXT_POSTGRES_DSN` from the environment and never prints it. For a local
loopback development store, use `cxtd index --data ./cxt-data`.

The web application reads the same conversations through the session routes
of the REST API (`GET /repos/{repoID}/anchored-sessions/{provider}/{sessionID}/events`
and the routes beside it), under the same rule for what a caller may read.

## HTTP transport contract

The endpoint follows MCP Streamable HTTP protocol version `2025-06-18` and
supports compatible `2024-11-05` and `2025-03-26` clients:

- one `/mcp` endpoint accepts JSON-RPC POST requests;
- GET returns `405 Method Not Allowed` because the server is stateless and does
  not open an unsolicited SSE stream;
- POST requires `Accept: application/json, text/event-stream`;
- an included `Origin` must match `CXT_PUBLIC_URL`; server-to-server clients may
  omit it;
- an included `MCP-Protocol-Version` must be supported;
- requests and tool outputs are bounded;
- unauthenticated requests return `401` with protected-resource metadata.

Transport specification:
<https://modelcontextprotocol.io/specification/2025-06-18/basic/transports>

## Uploading is separate

MCP only reads sessions that are already on the server. They get there through
the `cxt` CLI, which reads what Codex and Claude Code recorded on a member's
computer and uploads it; see the [CLI reference](CLI.md). Connecting MCP does
not give the server access to a live desktop conversation, uploads nothing,
and does not replace `cxt setup`.

## Connected application management

Account settings list active MCP clients by client ID/name and scope, without
exposing token values. Disconnecting a client invalidates every current access
and refresh token for that user/client and all unexchanged authorization codes.
Code exchange, refresh and application revocation share the identity transaction,
so a racing refresh cannot leave an authorized token after revocation commits.
Fresh user consent may authorize the app again. Other users' client connections
are unaffected. Already executing reads cannot be recalled.

`GET /api/v1/me/mcp-applications`, `DELETE /api/v1/me/mcp-applications/{clientID}`
and `GET /api/v1/me/audit` expose this management surface to the signed-in user.
Authorization and disconnection are audited without token or secret contents.
The account UI shows the most recent 100 events; it is not a retention policy.
