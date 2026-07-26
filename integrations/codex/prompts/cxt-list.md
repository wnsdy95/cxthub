# /prompts:cxt-list

> This file is a Codex custom prompt. Install it in `~/.codex/prompts/cxt-list.md` to call it using the `/prompts:cxt-list` slash command.

Retrieves the snapshot and ref list of the current repo/branch.

Calls the MCP tool `session_list`.

## Usage

```
/prompts:cxt-list [branch]
```

- `branch` (optional): The name of the branch to query. If omitted, the current branch will be used.

## MCP Tool Invocation

**Tool Name**: `session_list`

**Input Example**:
```json
{
  "repo_id": "<current repo ID>",
  "branch": "main"
}
```

**Example Output**:
```json
{
  "snapshots": [
    {
      "id": "sha256:<hex>",
      "branch": "main",
      "message": "fix sync edge case",
      "author": { "name": "Alice", "email": "alice@example.com", "team": "acme" },
      "created_at": "2026-06-29T09:10:00Z",
      "provider": "codex",
      "fidelity": "full"
    }
  ],
  "refs": [
    { "kind": "branch", "name": "main", "target": "sha256:<hex>" }
  ]
}
```

CLI Equivalent Command: `cxt list`
