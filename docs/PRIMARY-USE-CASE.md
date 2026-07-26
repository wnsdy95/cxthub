# Primary Use Case (canonical user journey) — User Confirmation Flow

> This document describes a primary user journey defined by the user. The command vocabulary and data model follow this flow.
> The command vocabulary is git-like (favoring familiarity): init/create · save(commit) · checkout(-b) · memorize · push/pull · fork · log · diff.

## Flow
| # | User Action | Command (Terminal / CLI Slash / Hook) | Internal Operation |
|---|---|---|---|
| 1 | Repo Creation | `cxt repo create <github-url>` or `cxt init` (origin auto-detection) | Repo Identifier = normalized GitHub origin URL. Register empty registry on central server + local `.cxt` connection |
| 2 | Save code analysis context to main | `cxt save -m "..."` · `/save` · Stop hook auto | Active session jsonl → CIR decoding → content hash → **snapshot S0** commit, `main` HEAD=S0 |
| 3 | Fork main to new branch | `cxt checkout -b analysis/refactor` | New branch ref points to `main@S0` (parent tracking). **Current provider format encodes S0 CIR into live session** (load). Cross-provider conversion if necessary |
| 4 | Memorize current context | `cxt memorize` · `/memorize` | Current live context → MemoryDistiller → **MemoryDigest M1** (provider-independent). Attach to current branch working |
| 5 | Push (memory + raw both) | `cxt push` | Current working → **raw CIR snapshot S1** commit + M1 attachment → server and blob negotiation (have/want) → **raw + memory blob both uploaded**. HEAD=S1 |

## Flow Sequence Diagram

```mermaid
sequenceDiagram
    actor User
    participant cli as "cxt CLI (Go)"
    participant local as ".cxt objects/refs"
    participant be as "cxtd Backend (Go)"

    User->>cli: 1. cxt repo create &lt;github-url&gt; / cxt init
    cli->>local: Local .cxt/ store creation
    cli->>be: Empty registry registration

    User->>cli: 2. cxt save -m "..." (or Stop hook auto)
    cli->>cli: Active session jsonl → CIR decoding → content hash
    cli->>local: snapshot S0 commit, main HEAD=S0

    User->>cli: 3. cxt checkout -b analysis/refactor
    cli->>local: New branch ref created (based on main@S0, parent tracking)
    cli->>cli: S0 CIR → current provider format encoding → live session load (load)

    User->>cli: 4. cxt memorize / /memorize
    cli->>cli: Live context → MemoryDistiller → MemoryDigest M1
    cli->>local: Attach M1 to current branch working

    User->>cli: 5. cxt push
    cli->>local: working → raw CIR snapshot S1 commit + M1 attachment, HEAD=S1
    cli->>be: raw blob + memory blob upload (have/want negotiation)
    be-->>cli: Completion response
```

## Invariants of this flow that cannot be enforced by the data model
1. **Snapshot = { raw_cir_blob, memory_digest_blob? }** — A snapshot can contain both raw context and memory. Push transmits both.
2. **checkout -b = fork + load integration** — Create a new branch from any reachable snapshot (parent tracking) and load the context into the live session. Cross-provider checkout soon becomes an entry point for cross-compatibility.
3. **memorize is a first-class verb for the live session** — Independent of save(raw commit). Result is attached to the next push.
4. **push automatically snapshots the current working** (save not explicitly called) + recent memorize result attachment → both uploaded. [Default, confirmation needed]
5. **repo identity = GitHub origin URL** (not local path). Same URL means same repo.
