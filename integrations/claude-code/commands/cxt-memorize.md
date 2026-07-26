# /cxt-memorize

Distill the active session to generate a MemoryDigest and attach it to the current branch.

Invoke the MCP tool `memorize` (`memory_save`'s in-session alias). Absorb the provider's native memory (Claude MEMORY.md / Codex rollout_summary, etc.) if available, otherwise perform distillation in CIR.

## Usage

```
/cxt-memorize [--provider claude|codex]
```

- `--provider` (optional): Target provider. Infer from the current environment if omitted.

## MCP Tool Invocation

**Tool Name**: `memorize` (equivalent to `memory_save`)

**Input Example**:
```json
{
  "cwd": "$CWD",
  "provider": "claude"
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

1. Detect the current provider's native memory through the MemorySource port.
2. Absorb native memory when available; otherwise distill it from CIR through MemoryDistiller.
3. Store the MemoryDigest under `.cxt/objects/`.
4. Update `MemoryHash` on the current branch's HEAD snapshot.
5. The snapshot now carries both raw context (`DocHash`) and memory (`MemoryHash`).

## Relationship to cxt-memory-load

- `/cxt-memorize`: Current session → MemoryDigest creation and attachment (save)
- `/cxt-memory-load`: Stored MemoryDigest → Inject into provider memory file for restoration

CLI equivalent command: `cxt memorize`
