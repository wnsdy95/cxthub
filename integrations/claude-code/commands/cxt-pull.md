# /cxt-pull

Pulls a snapshot and ref from the central server to the local machine.

Calls the MCP tool `sync_pull`. Synchronizes snapshots pushed by team members to the local `.cxt/` store.

## Usage

```
/cxt-pull
```

## MCP Tool Invocation

**Tool Name**: `sync_pull`

**Input Example**:
```json
{
  "repo_id": "<Current repo ID>"
}
```

**Output Example**:
```json
{
  "pushed": 0,
  "pulled": 5,
  "new_refs": [
    { "kind": "branch", "name": "main--fork-7f3a", "target": "sha256:<hex>" }
  ]
}
```

## Operation

1. Downloads snapshot blobs and refs from the central server that are not present locally.
2. Content-addressed negotiation: Refuses to retransmit snapshots already present locally.
3. Returns the list of newly received refs.
4. After pulling, verify the list with `/cxt-list` and restore the desired branch with `/cxt-checkout`.

CLI Equivalent Command: `cxt pull`
