# /cxt-log

Lists the snapshot and ref list of the current repo/branch.

Alias for `/cxt-list`. Calls the MCP tool `session_list`.

## Usage

```
/cxt-log [branch]
```

- `branch` (optional): Name of the branch to query. Uses the current branch if omitted.

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

CLI Equivalent Command: `cxt log` (`cxt list` is synonymous)

> Note: This command performs the same action as `/cxt-list`. `cxt log` and `cxt list` are synonymous in the CLI (_RECONCILIATION.md §A.1).
