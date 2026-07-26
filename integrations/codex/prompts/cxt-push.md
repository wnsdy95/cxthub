# /prompts:cxt-push

> This file is a Codex custom prompt. Install it in `~/.codex/prompts/cxt-push.md` to call it using the `/prompts:cxt-push` slash command.

Pushes the local snapshot and ref to the central server.

Calls the MCP tool `sync_push`. By default, it automatically commits the current working state as a raw snapshot (`--no-autosave` to disable), and also sends the recent MemoryDigest.

## Usage

```
/prompts:cxt-push [--no-autosave]
```

- `--no-autosave` (optional): Skips automatic snapshot commits before push.

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
4. Automatically preserves the `<branch>--fork-<shortid>` branch when a divergence is detected (no data loss).

CLI equivalent command: `cxt push`
