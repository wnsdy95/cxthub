# /cxt-fork

Fork a new branch from the specified snapshot (or the current HEAD).

Calls the MCP tool `session_fork`. The original snapshot remains immutable, and only a new branch ref is created.

## Usage

```
/cxt-fork <new-branch> [snapshot-id]
```

- `new-branch` (required): Name of the new branch to fork.
- `snapshot-id` (optional): Snapshot ID (`sha256:<hex>`) to fork from. If omitted, the current HEAD snapshot is used.

## MCP Tool Invocation

**Tool Name**: `session_fork`

**Input Example**:
```json
{
  "repo_id": "<current repo ID>",
  "from_snapshot": "sha256:<hex>",
  "new_branch": "feature-experiment"
}
```

**Output Example**:
```json
{
  "branch": "feature-experiment",
  "head": "sha256:<hex>"
}
```

## Operation

1. Identifies the current repo (git remote URL or cwd fallback).
2. Creates a new `Branch` ref named `from_snapshot`.
3. The original branch and snapshot remain unchanged.

CLI equivalent command: `cxt fork`
