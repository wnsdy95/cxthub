# /cxt-save

The active session of the current working directory is saved as a snapshot.

The MCP tool `session_save` is called. The branch is automatically detected from the `gitBranch` field or a git lookup.

## Usage

```
/cxt-save [message]
```

- `message` (optional): Snapshot description (commit message). If empty, it will be automatically generated.

## MCP Tool Invocation

This command invokes the following MCP tool:

**Tool Name**: `session_save`

**Input Example**:
```json
{
  "provider": "claude",
  "message": "$ARGUMENTS",
  "cwd": "$CWD"
}
```

**Output Example**:
```json
{
  "snapshot_id": "sha256:<hex>",
  "branch": "main",
  "fidelity": "full"
}
```

## Operation

1. Detects the active session file (e.g., ~/.claude/projects/...) relative to the current working directory (cwd).
2. Decodes JSONL to CIR v1.
3. Commits the content hash snapshot.
4. Advances the HEAD ref to the new snapshot.

CLI Equivalent Command: `cxt save`
