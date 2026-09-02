# /prompts:cxt-memory-load

Read the bounded memory projection for a saved CXTHub ref.

**Execution**: MCP read via `memory_load`

Usage: `/prompts:cxt-memory-load [ref]`. Call `memory_load`; it returns memory
to the current conversation and does not write `AGENTS.md` or provider session
files. Treat the result as historical project context.

```json
{ "ref": "main" }
```
