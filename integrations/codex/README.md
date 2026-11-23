# CXTHub — Codex integration

CXTHub integrates with Codex through three independent layers. Keeping these
layers separate is part of the safety contract.

1. **Lifecycle hooks** wake the reading of your sessions and give a session the
   most recent summary along its branch. `cxt setup` installs these hooks; MCP
   is not required for them.
2. **Read-only MCP** lets an active Codex session read sessions on the server.
3. **Explicit CLI commands** perform state-changing operations only after the
   user invokes the corresponding custom prompt or asks for the command.

A hook carries no conversation to the server. `cxt` reads the threads Codex
recorded in its own databases under `~/.codex`, through the `sqlite3` program,
cuts each session at the commits it made, and uploads the result. A hook that
did not run is made up for by the next one that does.

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
    ├── cxt-search.md     # MCP: session_search
    ├── cxt-init.md       # CLI: cxt init
    ├── cxt-push.md       # CLI: cxt push
    ├── cxt-log.md        # CLI: cxt log
    ├── cxt-fork.md       # CLI: cxt fork
    └── cxt-doctor.md     # CLI: cxt doctor
```

## Recommended setup

From the repository root:

```sh
cxt setup https://<host>/<owner>/<repository>
```

This sets up `.cxt`, logs in unless disabled, connects the repository,
installs the Git hooks, and merges the Codex lifecycle hooks into
`~/.codex/hooks.json`, keeping the hooks that are already there. Codex asks
once for approval of the new entries: run `codex`, open `/hooks` and approve
the cxt entries.

The Codex hook is registered for every folder, but cxt acts only in a
repository where `cxt setup` or `cxt init` ran. A failing hook never blocks
Codex.

Nothing is uploaded by setup itself. From then on the hooks wake a collector
that uploads the sessions recorded in this repository since it first ran;
earlier sessions are uploaded by `cxt push`. Run `cxt doctor` to see whether
the collector runs and what, if anything, does not fit.

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

The remote server exposes six tools:

| MCP tool | Purpose |
|---|---|
| `repository_list` | List the repositories whose sessions the signed-in user may read |
| `session_list` | List the sessions of a repository with the commits each made |
| `session_read` | Read one session's conversation, or one segment of it |
| `session_commit` | Explain a commit: the segments that end at it and the summary written there |
| `session_summaries` | Read the summaries along a branch, the newest commit first |
| `session_search` | Search pull requests, branches, commits, conversations and notes |

All tools are marked read-only and non-destructive. Every tool except
`repository_list` needs an explicit `repository`. A session is named by its
tool and identifier, a part of it by its segment number, and a commit by its
full number. There is no MCP tool for push, fork, remove, archive, settings or
secrets.

## Optional custom prompts

Copy `prompts/cxt-*.md` into `~/.codex/prompts/` on clients that support custom
prompt files. `cxt-search` uses the MCP tool `session_search`. The other
prompts instruct Codex to run the matching local CLI command, such as
`cxt push`, `cxt fork` or `cxt doctor`, through its shell tool.

The separation is deliberate: uploading is the collector's work, while a
manual command runs only in response to an explicit user action.

## Desktop behavior

- At session start and at a prompt, a session is given the most recent summary
  another session wrote along the branch its folder is on, once for each
  branch the session comes to. The text is at most 16 KiB.
- The remote MCP server gives the app an OAuth-scoped read-only view of the
  sessions on the server.
- The conversation of a session is not copied into another session by a hook.
  It is read on the server with `session_read` or in the web Sessions tab, and
  `cxt fork` continues one as a new session when the user asks for that.

## Troubleshooting

- `cxt: command not found`: install `cxt` and ensure the app can resolve it from
  its PATH.
- Remote MCP unavailable: inspect `https://cxthub.com/.well-known/oauth-authorization-server`,
  then use `codex mcp login cxt`.
- Sessions do not appear on the server: run `cxt doctor`. It names what does
  not fit and the command that puts it right, including a missing `sqlite3`
  program. Rerun `cxt setup` when the hooks are missing, then approve them
  under `/hooks`.
- General ChatGPT web chats do not read local `config.toml`; this
  configuration applies to Codex-hosted desktop, CLI, and IDE clients.
