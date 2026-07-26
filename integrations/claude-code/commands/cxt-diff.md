# /cxt-diff

Check the CIR event delta between two snapshots.

Call the MCP tool `session_diff`. Compare change contents on a per-turn/message/tool basis.

## Usage

```
/cxt-diff <left-snapshot-id> <right-snapshot-id>
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

**Output Example**:
```json
{
  "changes": [
    { "op": "add",    "seq": 12, "summary": "user message: 'Refactor start'" },
    { "op": "add",    "seq": 13, "summary": "tool_call: shell(git checkout -b refactor)" },
    { "op": "remove", "seq": 10, "summary": "assistant message: '...'"}
  ]
}
```

CLI Equivalent Command: None (Web/MCP `session_diff` exclusive — CLI not implemented)
