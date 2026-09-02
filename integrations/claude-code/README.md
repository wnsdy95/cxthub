# CXTHub — Claude Code integration

CXTHub uses three separate integration layers with Claude Code and Claude
Desktop's **Code** tab:

1. lifecycle hooks for automatic capture;
2. an optional read-only MCP server for history queries; and
3. explicit `cxt` CLI commands for state-changing manual operations.

Claude Desktop's general **Chat** tab is not a Claude Code session. It does not
emit the lifecycle hooks required for passive capture and is outside this
integration.

## Files

```text
claude-code/
├── .claude-plugin/plugin.json
├── .mcp.json
├── hooks/hooks.json
└── commands/
    ├── cxt-list.md        # MCP: context_list
    ├── cxt-fetch.md       # MCP: context_fetch
    ├── cxt-memory-load.md # MCP: memory_load
    ├── cxt-search.md      # MCP: context_search
    └── cxt-*.md           # explicit cxt CLI operations
```

## Recommended setup

Run this once from the repository root:

```sh
cxt setup
```

It initializes the local store, installs Git hooks, and merges Claude lifecycle
hooks into project settings without replacing existing entries. Those hooks
capture Claude Code CLI sessions and the Claude Desktop Code tab. MCP is not
required for capture.

## Default cloud read-only MCP

Add `https://cxthub.com/mcp` as a custom connector in Claude, or merge the
following project-scoped HTTP server into Claude Code's `.mcp.json`:

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

Use `/mcp` to complete OAuth. Claude's cloud connector reaches this public URL;
it does not read the workstation's `.cxt` directory. The server exposes
repository discovery plus four non-destructive context tools:

| MCP tool | Purpose |
|---|---|
| `repository_list` | List cloud repositories visible to the signed-in user |
| `context_list` | List context commits in an authorized repository |
| `context_fetch` | Read metadata, bounded memory, and recent chat for a ref |
| `memory_load` | Read the bounded memory projection for a ref |
| `context_search` | Search synchronized team context |

There are no MCP tools for save, fork, checkout, provider-file restoration,
memorize, push, or pull.

For explicit offline development only, a local STDIO entry may use `command`:
`"cxt"` with `args`: `["mcp", "--local"]`. It is not the product connector.

## Optional slash commands

Install `commands/*.md` in a Claude Code slash-command location if desired.
Read commands call the four MCP tools above. State-changing commands run the
matching local `cxt` CLI command through the shell only after the user invokes
the command explicitly.

Automatic capture remains hook-owned; agents should not call `cxt save` or
`cxt push` opportunistically during an unrelated turn.

## Desktop behavior

- Claude Desktop's Code tab keeps its vendor-owned live session on branch
  switches and receives one bounded project-memory handoff.
- Full conversation history remains in the immutable CXTHub DAG and is read
  with `context_fetch` or the web UI.
- Claude Desktop's general Chat tab is not passively captured.

## Troubleshooting

- `cxt: command not found`: install `cxt`, or use its absolute path in
  `.mcp.json` when the desktop app has a restricted PATH.
- Remote MCP unavailable: inspect the connector URL and use `/mcp` to retry
  OAuth. Run `cxt mcp --local` only when deliberately testing the offline helper.
- Capture unavailable: rerun `cxt setup` and inspect `.claude/settings.json`.
