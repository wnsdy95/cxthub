# CXTHub — Codex integration

CXTHub integrates with Codex through three independent layers. Keeping these
layers separate is part of the safety contract.

1. **Lifecycle hooks** capture Codex CLI, Codex app, and IDE sessions. `cxt
   setup` installs these hooks; MCP is not required for automatic capture.
2. **Read-only MCP** lets an active Codex session inspect CXTHub history.
3. **Explicit CLI commands** perform state-changing operations only after the
   user invokes the corresponding custom prompt or asks for the command.

Codex stores MCP configuration in `~/.codex/config.toml`. Codex desktop, CLI,
and IDE clients on the same host share that configuration. See the
[official OpenAI MCP documentation](https://developers.openai.com/codex/mcp).

## Files

```text
codex/
├── config.snippet.toml
├── hooks.json
└── prompts/
    ├── AGENTS-snippet.md
    ├── cxt-list.md       # MCP: context_list
    ├── cxt-fetch.md      # MCP: context_fetch
    ├── cxt-memory-load.md # MCP: memory_load
    ├── cxt-search.md     # MCP: context_search
    └── cxt-*.md          # explicit cxt CLI operations
```

## Recommended setup

From the repository root:

```sh
cxt setup
```

This initializes `.cxt`, installs Git hooks, registers the workspace remote
when supplied, logs in unless disabled, and merges Codex lifecycle hooks.
Hooks preserve existing configuration and are fail-open.

Automatic capture is now active. The app does not need to be launched through
`cxt codex`; hook payloads identify the exact Codex session and worktree.

## Default cloud read-only MCP

Merge this block into `~/.codex/config.toml`, or add the same Streamable HTTP
URL from the Codex app's MCP settings:

```toml
[mcp_servers.cxt]
url = "https://cxthub.com/mcp"
auth = "oauth"
```

Restart the client, then authenticate the server. Codex desktop, CLI, and IDE
share this URL-based configuration. The server reads the user's authorized
repositories from CXTHub's cloud backend rather than the current machine's
`.cxt` directory.

The remote server exposes repository discovery plus four context tools:

| MCP tool | Purpose |
|---|---|
| `repository_list` | List cloud repositories visible to the signed-in user |
| `context_list` | List context commits in an authorized repository |
| `context_fetch` | Read metadata, bounded memory, and recent chat for a ref |
| `memory_load` | Read the bounded memory projection for a ref |
| `context_search` | Search synchronized team context |

All tools are marked read-only and non-destructive. There is no MCP tool
for save, fork, checkout, provider-file restoration, memorize, push, or pull.

For explicit offline development only, configure an STDIO server with
`command = "cxt"` and `args = ["mcp", "--local"]`. That helper reads the local
working replica and is not the product default.

## Optional custom prompts

Copy `prompts/cxt-*.md` into `~/.codex/prompts/` on clients that support custom
prompt files. The prompt assets use MCP only for the four reads above. A prompt
that changes state instructs Codex to run the matching local CLI command, such
as `cxt save`, `cxt checkout`, or `cxt push`, through its shell tool.

The separation is deliberate: automatic hooks own routine capture, while a
manual mutation happens only in response to an explicit user action.

## Desktop behavior

- Codex app sessions are captured by lifecycle and Git hooks after `cxt setup`.
- The remote MCP server gives the app an OAuth-scoped read-only view of cloud history.
- On a Git branch switch, the vendor-owned app session remains open and
  receives one bounded project-memory handoff; CXTHub does not replace the live
  rollout file.
- Full history stays in the immutable CXTHub DAG and is retrieved with
  `context_fetch` or the web UI.

## Troubleshooting

- `cxt: command not found`: install `cxt` and ensure the app can resolve it from
  its PATH. An absolute command path is also valid in the MCP config.
- Remote MCP unavailable: inspect `https://cxthub.com/.well-known/oauth-authorization-server`,
  then use `codex mcp login cxt`. Use `cxt mcp --local` only when deliberately
  testing the offline helper.
- Capture unavailable: rerun `cxt setup`, then inspect Codex lifecycle hooks.
- General ChatGPT web chats do not read local `config.toml`; local MCP applies
  to Codex-hosted desktop, CLI, and IDE clients.
