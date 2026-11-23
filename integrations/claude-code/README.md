# CXTHub — Claude Code integration

CXTHub uses three separate integration layers with Claude Code and Claude
Desktop's **Code** tab:

1. lifecycle hooks, which wake the reading of your sessions and give a session
   the most recent summary along its branch;
2. an optional read-only MCP server for reading sessions on the server; and
3. explicit `cxt` CLI commands for everything that changes state.

A hook carries no conversation to the server. `cxt` reads what Claude Code
recorded in its own session files, cuts each session at the commits it made,
and uploads the result. A hook that did not run is made up for by the next one
that does.

Claude Desktop's general **Chat** tab is not a Claude Code session. It does not
run these lifecycle hooks and is outside this integration.

## Files

```text
claude-code/
├── .claude-plugin/plugin.json
├── .mcp.json
├── hooks/hooks.json
└── commands/
    ├── cxt-search.md      # MCP: session_search
    ├── cxt-init.md        # CLI: cxt init
    ├── cxt-push.md        # CLI: cxt push
    ├── cxt-log.md         # CLI: cxt log
    ├── cxt-fork.md        # CLI: cxt fork
    └── cxt-doctor.md      # CLI: cxt doctor
```

## Recommended setup

Run this once from the repository root:

```sh
cxt setup https://<host>/<owner>/<repository>
```

It sets up `.cxt`, logs in, connects the repository, installs the Git hooks,
and merges the Claude lifecycle hooks into the project's
`.claude/settings.json` without replacing existing entries. The hooks run
`cxt hook --provider claude` at session start, at each prompt, at stop and at
session end. MCP is not required for any of this.

Nothing is uploaded by setup itself. From then on the hooks wake a collector
that uploads the sessions recorded in this repository since it first ran;
earlier sessions are uploaded by `cxt push`. Run `cxt doctor` to see whether
the collector runs and what, if anything, does not fit.

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
it does not read the workstation's `.cxt` directory. The server exposes six
read-only tools:

| MCP tool | Purpose |
|---|---|
| `repository_list` | List the repositories whose sessions the signed-in user may read |
| `session_list` | List the sessions of a repository with the commits each made |
| `session_read` | Read one session's conversation, or one segment of it |
| `session_commit` | Explain a commit: the segments that end at it and the summary written there |
| `session_summaries` | Read the summaries along a branch, the newest commit first |
| `session_search` | Search pull requests, branches, commits, conversations and notes |

Every tool except `repository_list` needs an explicit `repository`. A session
is named by its tool and identifier, a part of it by its segment number, and a
commit by its full number. You read your own sessions in full and, of another
account's session, the parts that are pushed.

There are no MCP tools for push, fork, remove, archive, settings or secrets.

## Optional slash commands

Install `commands/*.md` in a Claude Code slash-command location if desired.
`/cxt-search` calls the MCP tool `session_search`. The other commands run the
matching local `cxt` command through the shell, and only after the user
invokes the command explicitly.

Uploading is the collector's work. An agent should not call `cxt push`
opportunistically during an unrelated turn.

## Desktop behavior

- At session start and at a prompt, a session is given the most recent summary
  another session wrote along the branch its folder is on, once for each
  branch the session comes to. The text is at most 16 KiB.
- The conversation of a session is not copied into another session by a hook.
  It is read on the server with `session_read` or in the web Sessions tab, and
  `cxt fork` continues one as a new session when the user asks for that.
- Claude Desktop's general Chat tab is not covered.

## Troubleshooting

- `cxt: command not found`: install `cxt` and make sure the app can resolve it
  from its PATH. The hook commands in `.claude/settings.json` call `cxt` by
  name.
- Remote MCP unavailable: inspect the connector URL and use `/mcp` to retry
  OAuth.
- Sessions do not appear on the server: run `cxt doctor`. It names what does
  not fit and the command that puts it right. Rerun `cxt setup` and inspect
  `.claude/settings.json` when the hooks are missing.
