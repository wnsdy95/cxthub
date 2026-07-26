# /cxt-load

Recover the snapshot as the target provider session file.

Invoke the MCP tool `session_load`. It supports two modes: `full` (full script recovery) or `memory` (memory summary injection).

## Usage

```
/cxt-load <ref-or-snapshot-id> [--mode full|memory] [--provider claude|codex]
```

- `ref-or-snapshot-id` (required): branch name, tag name, or snapshot ID (`sha256:<hex>`).
- `--mode` (optional, default `full`): recovery mode. `full`=full script, `memory`=memory summary only injection.
- `--provider` (optional): target provider for recovery. If omitted, it infers from the current environment.

## MCP Tool Invocation

**Tool Name**: `session_load`

**Input Example**:
```json
{
  "repo_id": "<current repo ID>",
  "ref": "main",
  "target_provider": "claude",
  "mode": "full",
  "cwd": "$CWD"
}
```

**Output Example**:
```json
{
  "written_path": "/Users/<user>/.claude/projects/<encoded>/<newSessionId>.jsonl",
  "fidelity": "full"
}
```

## Fidelity Tier Information

- **`full`**: Recovery from the same provider. Includes reasoning with no loss.
- **`reconstructed`**: Cross-provider recovery. Preserves text and tool calls, deactivates reasoning.
- **`memory`**: Injects only distillation summary (CLAUDE.md). Does not restore transcripts.

CLI Equivalent Command: `cxt load`
