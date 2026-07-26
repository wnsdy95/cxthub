# /prompts:cxt-memorize

> This file is a Codex custom prompt. Install it at `~/.codex/prompts/cxt-memorize.md` to call it with the `/prompts:cxt-memorize` slash command.

Distill the active session to generate a MemoryDigest and attach it to the current branch.

Invoke the MCP tool `memorize` (`memory_save`'s in-session friendly alias). Absorb provider native memory (Codex rollout_summary, etc.) if available, otherwise perform distillation in CIR.

## Usage

```
/prompts:cxt-memorize [--provider claude|codex]
```

- `--provider` (optional): Target provider. If omitted, it is inferred from the current environment.

## MCP Tool Invocation

**Tool Name**: `memorize` (equivalent to `memory_save`)

**Input Example**:
```json
{
  "cwd": "$CWD",
  "provider": "codex"
}
```

**Output Example**:
```json
{
  "snapshot_id": "sha256:<hex>",
  "memory_hash": "sha256:<hex>",
  "attached": true
}
```

## Operation

1. Detects the native memory file from the current provider's MemorySource port.
2. Absorbs the native memory if present, otherwise performs distillation from the CIR (MemoryDistiller port).
3. Stores the MemoryDigest in `.cxt/objects/`.
4. Updates the `MemoryHash` of the current branch HEAD snapshot.
5. The snapshot contains both raw(DocHash) and memory(MemoryHash).

## Relationship with cxt-memory-load

- `/prompts:cxt-memorize`: Current session → MemoryDigest creation and attachment (save)
- `/prompts:cxt-memory-load`: Stored MemoryDigest → Inject into provider memory file (restore)

CLI equivalent command: `cxt memorize`
