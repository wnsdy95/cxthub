# /cxt-checkout

Restore a CXTHub ref, optionally creating a new context branch.

**Execution**: explicit `cxt` CLI

Usage: `/cxt-checkout [ref] [-b <branch>] [--provider claude|codex] [--mode
full|reconstructed|memory]`. Run `cxt checkout` through the shell. It may
materialize provider state and must not be replaced by a fabricated MCP call.
