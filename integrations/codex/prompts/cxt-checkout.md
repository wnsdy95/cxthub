# /prompts:cxt-checkout

> This file is a Codex custom prompt. Install it at `~/.codex/prompts/cxt-checkout.md` to call it using the `/prompts:cxt-checkout` slash command.

Restores the specified ref to the current provider session. Use the `-b` flag to integrate fork and load operations.

Calls the MCP tool `session_checkout`. The `--mode full` (default) restores the entire transcript to a native session file, while `--mode memory` injects only the distillation summary.

## Usage

```
/prompts:cxt-checkout <ref>
/prompts:cxt-checkout -b <new-branch> [--from <ref>] [--mode full|memory] [--provider claude|codex]
```

- `ref` (required, for simple checkout): branch name, tag name, or snapshot ID (`sha256:<hex>`).
- `-b <new-branch>` (required, for fork+load): name of the new branch to create.
- `--from <ref>` (optional): base ref for branching. Uses current HEAD if omitted.
- `--mode` (optional, default `full`): restoration mode. `full`=entire transcript, `memory`=distillation summary only.
- `--provider` (optional): target provider for restoration. Infers from the current environment if omitted.

## MCP Tool Invocation

**Tool Name**: `session_checkout`

**Example Input** (simple checkout):
```json
{
  "repo_id": "<current repo ID>",
  "from": "main",
  "new_branch": "",
  "target_provider": "codex",
  "mode": "full",
  "cwd": "$CWD"
}
```

**Input Example** (fork + load):
```json
{
  "repo_id": "<current repo ID>",
  "from": "main",
  "new_branch": "feature-experiment",
  "target_provider": "codex",
  "mode": "full",
  "cwd": "$CWD"
}
```

**Output Example**:
```json
{
  "branch": "feature-experiment",
  "head": "sha256:<hex>",
  "written_path": "/Users/<user>/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl",
  "resume_cmd": "codex resume <sessionId>",
  "fidelity": "full"
}
```

## Fidelity Tier Information

- **`full`**: Recovery from the same provider. Includes reasoning and is lossless.
- **`reconstructed`**: Cross-provider recovery. Preserves text and tool calls, deactivates reasoning.
- **`memory`**: Injects only distillation summaries (AGENTS.md). Does not restore transcripts.

Automatically falls back to `memory` mode if `full` recovery fails (Fidelity downgrade).

## Relationship between cxt-fork and cxt-load

- `/prompts:cxt-checkout -b <branch>`: fork + load integration (recommended in most cases)
- `/prompts:cxt-fork <branch>`: branch only, no load
- `/prompts:cxt-load <ref>`: restore only, no branch

CLI equivalent command: `cxt checkout`
