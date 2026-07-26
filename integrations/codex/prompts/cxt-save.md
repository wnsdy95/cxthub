# /prompts:cxt-save

> This file is a Codex custom prompt. Install it in `~/.codex/prompts/cxt-save.md` to call it using the `/prompts:cxt-save` slash command.

Saves the active session of the current working directory as a snapshot.

Calls the MCP tool `session_save`. The branch is automatically detected from the `gitBranch` field or a git lookup.

## Usage

```
/prompts:cxt-save [message]
```

- `message` (optional): Snapshot description (commit message). If empty, it will be automatically generated.

## MCP Tool Invocation

**Tool Name**: `session_save`

**Input Example**:
```json
{
  "provider": "codex",
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

1. Detects the active session file (`~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl`) relative to the current working directory (cwd).
2. Decodes the JSONL to CIR v1.
3. Commits the content hash snapshot.
4. Advances the HEAD ref to the new snapshot.

CLI Equivalent Command: `cxt save`
