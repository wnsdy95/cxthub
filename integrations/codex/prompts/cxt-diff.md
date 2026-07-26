# /prompts:cxt-diff

> This file is a Codex custom prompt. Install it at `~/.codex/prompts/cxt-diff.md` to call it using the `/prompts:cxt-diff` slash command.

Verify the CIR event delta between two snapshots.

Invoke the MCP tool `session_diff` to compare changes at the turn/message/tool unit level.

## Usage

```
/prompts:cxt-diff <left-snapshot-id> <right-snapshot-id>
```

- `left-snapshot-id` (required): The base snapshot ID (`sha256:<hex>`) or branch name.
- `right-snapshot-id` (required): The comparison target snapshot ID (`sha256:<hex>`) or branch name.

## MCP Tool Invocation

**Tool Name**: `session_diff`

**Input Example**:
```json
{
  "repo_id": "<current repo ID>",
  "left": "sha256:<hex>",
  "right": "sha256:<hex>"
}
```

**Example Output**:
```json
{
  "changes": [
    { "op": "add",    "seq": 12, "summary": "user message: 'start the refactor'" },
    { "op": "add",    "seq": 13, "summary": "tool_call: shell(git checkout -b refactor)" },
    { "op": "remove", "seq": 10, "summary": "assistant message: '..." }
  ]
}
```

CLI Equivalent Command: None (Web/MCP `session_diff` exclusive — CLI not implemented)
