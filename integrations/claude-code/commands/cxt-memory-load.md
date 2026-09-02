# /cxt-memory-load

Read the bounded memory projection for a saved CXTHub ref.

**Execution**: MCP read via `memory_load`

Usage: `/cxt-memory-load [ref]`. The tool returns memory to the current
conversation and does not write `CLAUDE.md`, `MEMORY.md`, or provider session
files. Treat the result as historical project context.
