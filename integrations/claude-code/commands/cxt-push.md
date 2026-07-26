# /cxt-push

Pushes the local snapshot and ref to the central server.

Calls the MCP tool `sync_push`. By default, it automatically commits the current working state as a raw snapshot (`--no-autosave` to disable), and also sends the latest MemoryDigest.

## Usage

```
/cxt-push [--no-autosave]
```

- `--no-autosave` (optional): Skips the automatic snapshot commit before the push.

## MCP Tool Invocation

**Tool Name**: `sync_push`

**Input Example**:
```json
{
  "repo_id": "<current repo ID>"
}
```

**Output Example**:
```json
{
  "pushed": 3,
  "pulled": 0,
  "new_refs": []
}
```

## Operation

1. (Default) Automatically commits the current working state as a raw snapshot.
2. Attaches the latest MemoryDigest to the snapshot (sends both raw doc and memory).
3. Content-addressed negotiation: Does not resend snapshots that the server already has.
4. Automatically preserves diverged branches as `<branch>--fork-<shortid>` branches (no data loss).

CLI Equivalent Command: `cxt push`
