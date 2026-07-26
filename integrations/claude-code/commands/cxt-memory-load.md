# /cxt-memory-load

Injects the saved MemoryDigest into the provider's native memory file.

Calls the MCP tool `memory_load`. Claude records the distillation summary in `CLAUDE.md` (or `MEMORY.md`), and Codex records it in `AGENTS.md`. Useful for recovering the script when the context window is insufficient.

## Usage

```
/cxt-memory-load [ref-or-memory-hash] [--provider claude|codex]
```

- `ref-or-memory-hash` (optional): branch name, snapshot ID, or MemoryDigest hash (`sha256:<hex>`). Uses the memory of the current HEAD if omitted.
- `--provider` (optional): target provider for injection. Infers from the current environment if omitted.

## MCP Tool Invocation

**Tool Name**: `memory_load`

**Input Example**:
```json
{
  "repo_id": "<current repo ID>",
  "ref": "main",
  "provider": "claude",
  "cwd": "$CWD"
}
```

**Output Example**:
```json
{
  "written_path": "/Users/<user>/.claude/projects/<encoded>/CLAUDE.md",
  "fidelity": "memory"
}
```

## Operation

1. Loads the specified ref or the MemoryDigest of the current HEAD.
2. Injects the native memory file through the MemorySink port of the target provider.
   - Claude: `CLAUDE.md` or `MEMORY.md`
   - Codex: `AGENTS.md`
3. Overwrites only the cxt management section if an existing file exists.

## Relationship with cxt-memorize

- `/cxt-memorize`: Current session → MemoryDigest creation and attachment (save)
- `/cxt-memory-load`: Stored MemoryDigest → Inject into provider memory file (restore)

CLI equivalent command: `cxt load <ref> --mode memory`
