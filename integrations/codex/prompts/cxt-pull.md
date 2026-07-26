# /prompts:cxt-pull

> This file is a Codex custom prompt. Install it at `~/.codex/prompts/cxt-pull.md` to call it using the `/prompts:cxt-pull` slash command.

Pulls a snapshot and ref from the central server to the local machine.

Calls the MCP tool `sync_pull`. Synchronizes snapshots pushed by team members to the local `.cxt/` store.

## Usage

```
/prompts:cxt-pull
```

## MCP Tool Invocation

**Tool Name**: `sync_pull`

**Input Example**:
```json
{
  "repo_id": "<current repo ID>"
}
```

**Output Example**:
```json
{
  "pushed": 0,
  "pulled": 5,
  "new_refs": [
    { "kind": "branch", "name": "main--fork-7f3a", "target": "sha256:<hex>" }
  ]
}
```

## Operation

1. Downloads snapshot blob and ref from the central server that are not present locally.
2. Content-addressed negotiation: Does not re-download snapshots already present locally.
3. Returns the list of newly received refs.
4. After pulling, verify the list at `/prompts:cxt-list` and restore the desired branch using `/prompts:cxt-checkout`.

CLI Equivalent Command: `cxt pull`
