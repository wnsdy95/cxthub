# /prompts:cxt-push

Upload the sessions of this repository to its CXTHub server now.

**Execution**: explicit `cxt` CLI

Usage: `/prompts:cxt-push [claude|codex <session>]`. Run `cxt push` through the
shell, or `cxt push <tool> <session>` when the user named one session, only
because the user invoked this command. Report how many session reports were
stored, unchanged, refused or removed, and name every refused session with its
reason. A refused report makes the command exit with a failure. The command
takes no `--force` or `--append` flag. MCP is read-only and has no push tool.
