# /prompts:cxt-push

Synchronize local CXTHub snapshots and refs to the configured origin.

**Execution**: explicit `cxt` CLI

Usage: `/prompts:cxt-push [--force|--append]`. Run `cxt push` through the shell
only because the user invoked this command, then report pushed, fetched, and
ref-convergence results. MCP is read-only and has no push tool.
