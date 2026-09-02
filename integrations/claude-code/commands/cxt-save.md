# /cxt-save

Create an explicit CXTHub checkpoint for the active provider session.

**Execution**: explicit `cxt` CLI

Usage: `/cxt-save [-m <message>] [--provider claude|codex]`. Run `cxt save`
through the shell and report the snapshot ID. Routine capture belongs to hooks;
do not call save opportunistically during unrelated work.
