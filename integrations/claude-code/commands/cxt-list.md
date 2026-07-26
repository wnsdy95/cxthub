# /cxt-list

Retrieve the snapshot and ref list of the current repo/branch.

The MCP tool `session_list` is called.

## Usage

```
/cxt-list [branch]
```

- `branch` (optional): Branch to list. Defaults to the current branch.

## MCP Tool Invocation

**Tool Name**: `session_list`

**Input Example**:
```json
{
  "repo_id": "<current repo ID>",
  "branch": "main"
}
```

**Output Example**:
```json
{
  "snapshots": [
    {
      "id": "sha256:<hex>",
      "branch": "main",
      "message": "fix sync edge case",
      "author": { "name": "Alice", "email": "alice@example.com", "team": "acme" },
      "created_at": "2026-06-29T09:10:00Z",
      "provider": "claude",
      "fidelity": "full"
    }
  ],
  "refs": [
    { "kind": "branch", "name": "main", "target": "sha256:<hex>" }
  ]
}
```

CLI Equivalent Command: `cxt list`
