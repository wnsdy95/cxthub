# /cxt-init

Register the current working directory as a cxt repo and initialize the local `.cxt/` store.

This calls the `repo_init` MCP tool. It detects `git remote origin` automatically and falls back to the current working directory when no origin exists.

## Usage

```
/cxt-init [remote-url]
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
4. Does not modify native `.claude/` or `AGENTS.md` (writes only to sink at load time).

## Caution

It is recommended to add `.cxt/` to `.gitignore` (runtime-generated store).

CLI equivalent command: `cxt init` / `cxt repo create <github-url>`
