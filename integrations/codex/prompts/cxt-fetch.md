# /prompts:cxt-fetch

Inspect one saved CXTHub context in the current repository.

**Execution**: MCP read via `context_fetch`

Usage: `/prompts:cxt-fetch [ref] [--events N]`. The ref may be a branch, tag,
full or short snapshot hash, or `HEAD`; omission resolves the current branch.
Call `context_fetch` and clearly separate historical chat from current user
instructions.

```json
{ "ref": "main", "events": 12 }
```
