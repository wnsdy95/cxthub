# /prompts:cxt-init

Initialize CXTHub in the current Git repository.

**Execution**: explicit `cxt` CLI

Usage: `/prompts:cxt-init [--remote <workspace-url>] [--no-hooks]`.
Run `cxt init` through the shell with the requested flags, then report the
result. Do not attempt an MCP call; initialization changes local repository
state.
