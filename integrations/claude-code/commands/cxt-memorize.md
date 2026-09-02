# /cxt-memorize

Distill a saved or active CXTHub context into a durable MemoryDigest.

**Execution**: explicit `cxt` CLI

Usage: `/cxt-memorize [ref] [--provider claude|codex]`. Run `cxt memorize`
through the shell and report the memory hash. Use MCP `memory_load` to read an
existing digest without changing state.
