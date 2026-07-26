# /prompts:cxt-init

> This file is a Codex custom prompt. Install it in `~/.codex/prompts/cxt-init.md` to call it using the `/prompts:cxt-init` slash command.

Registers the current working directory as a cxt repo and initializes the local `.cxt/` store.

Calls the MCP tool `repo_init`. Automatically detects `git remote origin`, and falls back to the current working directory (cwd) if origin is not present.

## Usage

```
/prompts:cxt-init [remote-url]
```

- `remote-url` (optional): Explicit GitHub/remote URL. If omitted, `git remote origin` is automatically detected.

## MCP Tool Invocation

**Tool Name**: `repo_init`

**Input Example**:
```json
{
  "cwd": "$CWD",
  "remote_url": "$ARGUMENTS"
}
```

**Output Example**:
```json
{
  "repo_id": "sha256:<hex>",
  "local_store_path": "/path/to/project/.cxt"
}
```

## Operation

1. Queries `git remote origin` in the current working directory.
2. Creates the `.cxt/` directory structure (`objects/`, `refs/heads/`, `refs/tags/`, `HEAD`, `manifest.json`, `config`).
3. Records the remote URL and TeamIdentity in the `config`.
4. The native `AGENTS.md` is not modified (only written to as a sink during load).

## Caution

`.cxt/` should be added to `.gitignore` (runtime-generated store).

CLI equivalent commands: `cxt init` / `cxt repo create <github-url>`
