# /cxt-fork

Start new work from a commit: a Git branch, a continued conversation, or both.

**Execution**: explicit `cxt` CLI

Usage: `/cxt-fork --at <commit> --branch <name>` or
`/cxt-fork claude|codex <session> [--at <commit>] [--to claude|codex] [--branch <name>]`.
Run the matching `cxt fork` command through the shell.

- Without a session, `cxt fork --at <commit> --branch <name>` makes the Git
  branch at the commit and nothing else. No folder is switched to it.
- With a session, the conversation is read from the server, up to the segment
  that ends at the commit when `--at` is given, and written as a new session in
  this folder. `--to` names the tool of the new session.

Report what the command printed: the branch that was made, how many events the
new session holds, what was left out or not readable, and the command that
starts the new session. Do not start that session yourself.
