# /cxt-checkout

Restores the specified ref to the current provider session. The `-b` flag can be used to integrate fork and load operations.

Calls the MCP tool `session_checkout`. The `--mode full` (default) restores the entire transcript to a native session file, while `--mode memory` injects only the distillation summary.

## Usage

```
/cxt-checkout <ref>
/cxt-checkout -b <new-branch> [--from <ref>] [--mode full|memory] [--provider claude|codex]
```

- `ref` (required, for simple checkout): branch name, tag name, or snapshot ID (`sha256:<hex>`).
- `-b <new-branch>` (required, for fork+load): name of the new branch to create.
- `--from <ref>` (optional): base ref for branching. Uses the current HEAD if omitted.
- `--mode` (optional, default `full`): restoration mode. `full`=entire transcript, `memory`=only distillation summary.
- `--provider` (optional): target provider for restoration. Infers from the current environment if omitted.

## MCP Tool Invocation

**Tool Name**: `session_checkout`

**Input Example** (simple checkout):
```json
{
  "repo_id": "<current repo ID>",
  "from": "main",
  "new_branch": "",
  "target_provider": "claude",
  "mode": "full",
  "cwd": "$CWD"
}
```

**Input Example** (fork + load):
```json
{
  "repo_id": "<Current repo ID>",
  "from": "main",
  "new_branch": "feature-experiment",
  "target_provider": "claude",
  "mode": "full",
  "cwd": "$CWD"
}
```

**Output Example**:
```json
{
  "branch": "feature-experiment",
  "head": "sha256:<hex>",
  "written_path": "/Users/<user>/.claude/projects/<encoded>/<newSessionId>.jsonl",
  "resume_cmd": "claude --resume <sessionId>",
  "fidelity": "full"
}
```

## Fidelity Tier Information

- **`full`**: Recovery from the same provider. Includes reasoning for a lossless operation.
- **`reconstructed`**: Recovery from cross-provider sources. Preserves text and tool calls, deactivates reasoning.
- **`memory`**: Injects only distillation summary (CLAUDE.md). Does not restore transcripts.

`full` recovery fails automatically falls back to `memory` mode (Fidelity downgrade).

## Relationship between cxt-fork / cxt-load

- `/cxt-checkout -b <branch>`: Integrated fork + load (recommended in most cases)
- `/cxt-fork <branch>`: Fork only, without load
- `/cxt-load <ref>`: Load only, without fork

CLI equivalent command: `cxt checkout`
