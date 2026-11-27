# /cxt-init

Set up CXTHub in the current Git repository.

**Execution**: explicit `cxt` CLI

Usage: `/cxt-init [--remote <repository-url>] [--no-hooks]`. Run `cxt init`
through the shell with the requested flags and report the result. The command
makes the `.cxt` folder with its mark, keeps the files of cxt out of Git, and
installs the Git hooks unless `--no-hooks` is given; `--remote` also connects
the server repository. To log in and register the agent hooks as well, the
user runs `cxt setup <repository-url>` instead. Setting up changes local state
and is not an MCP operation.
