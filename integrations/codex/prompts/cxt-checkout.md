# /prompts:cxt-checkout

Restore a CXTHub ref, optionally creating a new context branch.

**Execution**: explicit `cxt` CLI

Usage: `/prompts:cxt-checkout [ref] [-b <branch>] [--provider claude|codex]
[--mode full|reconstructed|memory]`. Run `cxt checkout` through the shell with
the explicit arguments. This may materialize provider state, so never replace
it with a fabricated MCP call.
