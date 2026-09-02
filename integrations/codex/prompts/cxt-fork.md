# /prompts:cxt-fork

Fork a saved CXTHub ref into a new context branch and restore it.

**Execution**: explicit `cxt` CLI

Usage: `/prompts:cxt-fork <ref> --as <branch> [--provider claude|codex]
[--mode full|reconstructed|memory]`. Run the matching `cxt fork` command
through the shell and report the created branch, fidelity, and resume guidance.
