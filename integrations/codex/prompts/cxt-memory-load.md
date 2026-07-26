# /prompts:cxt-memory-load

> This file is a Codex custom prompt. Install it at `~/.codex/prompts/cxt-memory-load.md` to call it using the `/prompts:cxt-memory-load` slash command.

Injects the saved MemoryDigest into the provider's native memory file.

Calls the MCP tool `memory_load`. Codex records the distillation summary in `AGENTS.md`. Useful for recovering transcripts when the context window is insufficient.

## Usage

```
/prompts:cxt-memory-load [ref-or-memory-hash] [--provider claude|codex]
```

- `ref-or-memory-hash` (optional): branch name, snapshot ID, or MemoryDigest hash (`sha256:<hex>`). If omitted, the memory of the current HEAD is used.
- `--provider` (optional): target provider for injection. If omitted, it is inferred from the current environment.

## MCP Tool Invocation

**Tool Name**: `memory_load`

**Input Example**:
```json
{
  "repo_id": "<current repo ID>",
  "ref": "main",
  "provider": "codex",
  "cwd": "$CWD"
}
```

**Output Example**:
```json
{
  "written_path": "/path/to/project/AGENTS.md",
  "fidelity": "memory"
}
```

## Operation

1. Loads the specified ref or the current HEAD's MemoryDigest.
2. Injects the native memory file through the target provider's MemorySink port.
   - Codex: `AGENTS.md`
   - Claude: `CLAUDE.md` or `MEMORY.md`
3. Overwrites only the cxt management section if an existing file exists.

## Relationship with cxt-memorize

- `/prompts:cxt-memorize`: Current session → MemoryDigest creation and attachment (storage)
- `/prompts:cxt-memory-load`: Stored MemoryDigest → Injection into provider memory file (restoration)

CLI Equivalent Command: `cxt load <ref> --mode memory`
