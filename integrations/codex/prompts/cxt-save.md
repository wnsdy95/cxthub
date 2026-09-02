# /prompts:cxt-save

Create an explicit CXTHub checkpoint for the active provider session.

**Execution**: explicit `cxt` CLI

Usage: `/prompts:cxt-save [-m <message>] [--provider claude|codex]`.
Run `cxt save` through the shell, preserving a supplied message as one
argument, and report the snapshot ID. Do not call save opportunistically;
normal capture belongs to lifecycle and Git hooks.
