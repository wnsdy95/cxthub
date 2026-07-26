# /prompts:cxt-load

> This file is a Codex custom prompt. Install it in `~/.codex/prompts/cxt-load.md` to call it using the `/prompts:cxt-load` slash command.

Restores the snapshot to the target provider session file.

Calls the MCP tool `session_load`. It supports two modes: `full` (restores the entire transcript) or `memory` (injects a distilled summary).

## Usage

```
/prompts:cxt-load <ref-or-snapshot-id> [--mode full|memory] [--provider claude|codex]
```

- `ref-or-snapshot-id` (required): branch name, tag name, or snapshot ID (`sha256:<hex>`).
- `--mode` (optional, default `full`): recovery mode. `full`=transcript full, `memory`=distillation summary only injection.
- `--provider` (optional): recovery target provider. Inferred from the current environment if omitted.

## MCP Tool Invocation

**Tool Name**: `session_load`

**Input Example**:
```json
{
  "repo_id": "<current repo ID>",
  "ref": "main",
  "target_provider": "codex",
  "mode": "full",
  "cwd": "$CWD"
}
```

**Output Example**:
```json
{
  "written_path": "/Users/<user>/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl",
  "fidelity": "full"
}
```

## Fidelity Tier Information

- **`full`**: Recovery from the same provider. Includes reasoning without loss.
- **`reconstructed`**: Cross-provider recovery. Preserves text and tool calls, deactivates reasoning.
- **`memory`**: Injects only distillation summaries (AGENTS.md). Does not restore transcripts.

## Relationship Between Codex Native Resume and Codex

Native `codex resume <id>` continues a Codex-owned session file.
On top of that, cxt's `/prompts:cxt-load` supports cross-provider restoration
(Claude → Codex and Codex → Claude) and branch-based ref selection.

CLI Equivalent Command: `cxt load`
