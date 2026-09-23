# MCP connections

CXTHub's product MCP is the OAuth-protected Streamable HTTP endpoint at:

```text
https://cxthub.com/mcp
```

Codex, Claude, ChatGPT, and other remote MCP clients query the same cloud
context that the CXTHub web application reads. The local `.cxt` directory is a
CLI working replica and offline cache; it is not the product MCP database.

## Architecture

```text
Codex / Claude / ChatGPT
        │ Streamable HTTP + OAuth
        ▼
https://cxthub.com/mcp
        │
        ▼
cxtd remote MCP delivery adapter
        │
        ├─ OAuth user resolution (`mcp:read`)
        ├─ Repository public/member/break-glass policy
        ├─ read-only context application service
        └─ bounded response projection
        │
        ▼
Cloud PostgreSQL
```

The MCP adapter and REST adapter are composed in the same `cxtd` process for
now. They share application services, repository objects, identity policy, and
the production PostgreSQL store. This is a composition boundary, so MCP can be
moved to a separate service later without changing its tool contract.

## Read-only tools

| Tool | Purpose |
|---|---|
| `repository_list` | Discover authorized repositories with continuation cursors |
| `context_list` | Browse all, current, previous, or archived context snapshots |
| `context_history` | Read recorded branch births, attachments, selections, and continuations |
| `context_fetch` | Retrieve the entire archived event stream in bounded fragments |
| `memory_load` | Project merged memory or retrieve an exact archived object in bounded fragments |
| `context_search` | Search messages and readable events, continuing through older records |

Every tool is marked read-only, non-destructive, and idempotent. There are no
MCP tools for save, commit, checkout, fork, restore, push, pull, settings,
secrets, membership, or break-glass administration.

`repository_list` is the cloud discovery step. The other tools require an
explicit repository selector (`owner/repository` or repository
ID). A remote client never depends on a workstation's current directory.

### History scope and working position

`context_list` and `context_search` accept `scope`: `all` (default), `current`,
`previous`, or `archived`. `current` and `previous` require `position`, an
explicit context snapshot or cloud ref. The first page resolves this selection;
continuation cursors pin it even if the branch later moves. `previous` contains
retained context outside that selected ancestry. Shared snapshot labels do not
hide content belonging to another branch: branch filtering follows verified
refs, lifecycle/history roots, and recorded memberships.

An omitted ref or remote `HEAD` resolves to the repository default branch. It
never identifies a caller's local worktree. Read `context_history` to find
recorded code/context selections and their pinned `memory_hash`; then use that
snapshot as `ref` and the exact `memory_hash` for historical memory retrieval.
These reads do not move code, server refs, or the provider conversation.

### Following continuation cursors

Every tool returns `next_cursor`. Repeat the call with the same repository,
selection, and query plus `cursor: <next_cursor>` until it is empty. A cursor
from another repository, tool, or filter is rejected. Authorization is checked
again on every request. List pages contain at most 100 rows. Search limits its
scan per call and can return an empty result page with a nonempty cursor: that
means older data remains to be searched.

`context_fetch` starts at the oldest archived event and includes all event
kinds. Each fragment has `event_index`, `byte_offset`, `event_complete`, and
`json_fragment`. Concatenate fragments for the same event by byte offset, then
parse the resulting JSON. This preserves oversized events and opaque provider
state as stored; encrypted state is not decrypted or synthesized. The default
is up to 12 fragments per call (maximum 50), with a shared 12 KiB raw JSON
fragment budget. JSON response escaping and metadata add transport overhead.

`memory_load` defaults to `mode: "project"`. It reconstructs knowledge across
all current natural and graft parents, including the previous main immediately
after PR promotion. The source snapshot's saved memory is never rewritten.
Fragments retain provenance; removed grafts are excluded unless explicitly
pinned imports, and opaque legacy cumulative digests are not repeatedly stacked.
As in CLI prompt loading, provider cumulative generations and byte-contained
summary duplicates are collapsed, transport noise is removed from facts, and
unattested task lists are excluded from active project knowledge. These are
read projections: stored mode preserves every original byte and task list.

Both modes return `byte_offset`, `json_fragment`, `complete`, and `next_cursor`;
join fragments before parsing. Project mode returns `projection_hash` (derived
JSON identity) and `lineage_hash` (dependency version), **not** a stored
`memory_hash`. Its cursor pins the selected snapshot and dependency version.
If an ancestor attachment or graft changes between pages, the tool returns
`memory projection changed; restart memory_load without cursor`. Discard those
partial fragments and restart; never concatenate different projections. Reads
are stateless across server replicas. Versioned projection cursors make older
replicas reject them during rolling upgrades instead of reading an offset from
an unrelated stored object. A moving branch ref does not move an
existing cursor's selected snapshot.

Use `mode: "stored"` for the nearest immutable saved digest, or pass an exact
`memory_hash` together with its owning snapshot `ref` from `context_history`.
An explicit hash implies stored mode; combining it with project mode is an
error. Stored cursors pin the original blob across later attachment changes;
pre-upgrade stored cursors remain readable. A past code position must use its
recorded memory hash, since a snapshot's current grafts may include later work.
Stored snapshot/hash REST endpoints keep their original exact-object meaning.

Projection reads verify complete topology and the hashes of consumed objects;
missing/corrupt dependencies fail instead of reporting partial memory as
complete. Reads retry up to three times on concurrent mutation and are bounded
at 4,096 reachable snapshots and 64 MiB of consumed memory JSON. Larger requests
must select a narrower ref; stored reads remain available. Metadata is fetched
in batches and immutable objects are cached only within each request.

Merged conversations remain separate immutable documents. To explore them,
use `context_list` with `scope: "current", position: "main"`, then
`context_fetch` on each desired snapshot. This does not concatenate teammates'
transcripts into the active app conversation. CLI loading and new branch seeds
use the same provenance rules with their existing bounded prompt budgets;
rewinding still reads the exact historical attachment. The explicit offline
`cxt mcp --local` helper uses this same CLI lineage projection and reports an
incomplete replica instead of silently returning one available memory. The
cloud endpoint remains the default product connector.

Archived material is data, not instructions. Cursor pagination reduces the
size of each response; clients should retrieve the scope needed for their task
rather than injecting the whole archive into every agent prompt.

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

Repository visibility follows the context-viewing boundary:

- public repositories are readable;
- private repositories require an effective repository role of at least Viewer;
- Organization Owners inherit Owner access to all repositories in their organization;
- current Team grants and direct repository memberships contribute to effective access;
- Enterprise account administration alone grants no repository context access;
- all other private repositories are omitted without leaking their contents.

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

## Explicit local helper

For offline development, the CLI can expose the current repository's local
working replica over stdio:

```bash
cxt mcp --local
```

This helper is intentionally not the default product connector. Bare
`cxt mcp` fails with the remote endpoint and explicit local usage in its error
message. The helper has no `repository_list`; its four context tools resolve
the current local repository, and `context_search` may still use the configured
origin.

## Production storage invariant

The production container is built with the PostgreSQL adapter. Any externally
bound `cxtd` process requires PostgreSQL and fails before serving if
`CXT_POSTGRES_DSN` is missing or unusable. Cloud Run additionally sets
`CXT_REQUIRE_POSTGRES=1`, loads the DSN from Secret Manager, and applies the
ordered migrations before accepting traffic.

Filesystem storage remains available only to a loopback-bound development
server and tests. It must not be treated as a production fallback.

## Indexed reads and existing-data rollout

Event offsets and searchable text are derived from hash-verified canonical
archives. Fetch reads only the required 512 KiB storage chunks; the 12 KiB MCP
fragment budget also applies when a single event is very large. Canonical event
fragments have versioned cursors. A cursor issued by the previous serializer
must be restarted without `cursor` after upgrading, so byte offsets from two
encodings cannot silently mix.

Migration `0041_doc_read_index.sql` adds PostgreSQL event locations and a
`pg_trgm` GIN index. Identical inherited events share searchable text. Repository
ownership is checked on every read and search; deleting the last owning document
also deletes its derived search text. Searches remain literal, case-insensitive
substring matches (including Korean, identifiers, `%`, `_`, and backslashes),
not vector similarity. This addresses archive scanning without adding a second
database or an embedding dependency. GraphQL is not required for range reads.

New documents create their read index on write. Before routing production
traffic to a large existing archive, run this restartable maintenance command
with the same PostgreSQL build, database secret, and migration directory as the
server:

```bash
cxtd index --migrations ./schemas/db/migrations
```

It reads `CXT_POSTGRES_DSN` from the environment and never prints it. For a local
loopback development store, use `cxtd index --data ./cxt-data`. Missing indexes
are built lazily for compatibility, so the first read of an unindexed legacy
document may still be slow. The archive and its content hashes remain unchanged.

The web viewer uses `GET /repos/{repoID}/docs/{hash}/events` (50 events by default,
100 maximum, 512 KiB except for one intact oversized event). `base` selects the
exact inherited prefix and `offset=-1` skips it. Inherited history and memory
load when expanded; complete raw downloads remain available explicitly.
React Query owns page caching and cancellation. Zustand continues to own client
selection state, without duplicating server documents.

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

## Capture is separate

MCP only reads synchronized history. Codex and Claude coding sessions are
captured through lifecycle/Git hooks, while the CLI maintains the local `.cxt`
working replica and synchronizes it with `cxtd`. Connecting MCP does not grant
the server access to a live desktop conversation and does not replace capture
hooks.

### Branch integration selection

For `context_list(scope="current", position="main")`, the application includes
completed PR contributions proven present at main's recorded Git code position.
`code_commit` optionally selects another exact code SHA. Results follow Git
integration order with causal children before parents; cursor pages pin this
ordered projection rather than the captures' timestamps. A changed selection or
dependency generation requires restarting pagination.

`memory_load(ref="main", mode="project")` uses the same integrated branch roots,
including retained PR source memory after a later context-placement change.
The response's `inclusion` summarizes included, excluded and unresolved PRs.
An exact snapshot/tag still reads that archived lineage; `mode="stored"` or an
explicit memory hash keeps the immutable original. Effective mode assesses typed
claims against explicit code; historical prose remains unverified. None of these
queries moves shared refs, worktree positions or provider conversations.

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
