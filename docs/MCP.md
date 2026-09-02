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
        ├─ Workspace public/member/break-glass policy
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
| `repository_list` | Discover a bounded list of repositories visible to the signed-in user |
| `context_list` | List committed context snapshots in one authorized repository |
| `context_fetch` | Fetch bounded metadata, memory, and recent conversation text for a ref |
| `memory_load` | Load the bounded memory projection at a ref or nearest reachable ancestor |
| `context_search` | Search committed messages and readable conversation text in one repository |

Every tool is marked read-only, non-destructive, and idempotent. There are no
MCP tools for save, commit, checkout, fork, restore, push, pull, settings,
secrets, membership, or break-glass administration.

`repository_list` is the cloud-only discovery step. The four context tools
then require an explicit `namespace/workspace/repository` selector so a remote
client never depends on a workstation's current directory.

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

- public Workspace repositories are readable;
- private Workspace repositories require at least Viewer membership;
- Enterprise administration alone grants no repository context access;
- an Enterprise Owner may use an active, reason-bound, expiring, audited
  read-only break-glass grant;
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
