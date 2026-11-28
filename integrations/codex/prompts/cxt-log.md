# /prompts:cxt-log

Show what this computer keeps about the session uploads of this repository.

**Execution**: explicit `cxt` CLI

Usage: `/prompts:cxt-log [claude|codex <session>]`. Run `cxt log` through the
shell, or `cxt log <tool> <session>` for the whole upload log of one session.
The command reads the copies under `.cxt/sessions` on this computer only: it
does not ask the server and changes nothing. Present each session with its
last upload, and say so when no upload has been recorded yet.
