# /prompts:cxt-memorize

Distill a saved or active CXTHub context into a durable MemoryDigest.

**Execution**: explicit `cxt` CLI

Usage: `/prompts:cxt-memorize [ref] [--provider claude|codex]`. Run
`cxt memorize` through the shell and report the resulting memory hash. To read an
existing digest without changing state, use the MCP tool `memory_load`.
