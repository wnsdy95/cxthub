# /prompts:cxt-list

List saved CXTHub context commits for the current repository.

**Execution**: MCP read via `context_list`

Interpret `$ARGUMENTS` as an optional branch and optional result limit. Call
`context_list` with only those two fields. Summarize the returned commits; do
not save, push, pull, or change refs.

```json
{ "branch": "main", "limit": 20 }
```
