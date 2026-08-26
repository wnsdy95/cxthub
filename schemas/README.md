# schemas/ — Shared CLI, Backend, and Frontend Contracts

cxt keeps `cli/` and `backend/` in separate Go modules with independent domain types. The files in `schemas/` are the shared wire and persistence contracts used across the CLI, backend, and frontend.

## What This Directory Defines

| File | Role |
|---|---|
| `cir.schema.json` | CIR v1, the provider-independent canonical intermediate representation decoded from raw Claude and Codex JSONL. Changes require compatibility review. |
| `manifest.schema.json` | Manifest v1: repository refs, `snapshot_index`, and optional causal `memory_attachments` pointers used for push/pull have-want negotiation. Changes require compatibility review. |
| `openapi.yaml` | OpenAPI 3.1 REST contract shared by CLI, backend, and frontend. Route and field drift tests keep it aligned with the server. |
| `db/migrations/*.sql` | Ordered PostgreSQL migrations through 0036, covering repository objects, identity/workspaces, pending state, reflog, compaction, graft overlays, transcript/memory chunk storage, and scoped session refs. |

## DB Schema (ERDiagram)

```mermaid
erDiagram
    repos {
        TEXT id PK
        TEXT remote_url
        TEXT default_branch
        TEXT workspace_id FK
        TEXT git_remote_url
        TEXT description
        TEXT website
        TEXT topics
        BOOLEAN protect_default
        TEXT team "legacy(team-token)"
        TIMESTAMPTZ created_at
    }
    blobs {
        TEXT hash PK
        BYTEA bytes
    }
    branches {
        TEXT repo_id FK
        TEXT name
        TIMESTAMPTZ created_at
    }
    snapshots {
        TEXT id PK
        TEXT repo_id FK
        TEXT branch
        TEXT[] parents
        TEXT doc_hash FK
        TEXT memory_hash FK
        TEXT claude_settings
        TEXT agents_settings
        TEXT codex_settings
        BOOLEAN grafted
        TEXT session_id
        TEXT models
        TEXT provider
        TEXT fidelity
        TEXT message
        TEXT author_name
        TEXT author_email
        TEXT author_team
        TIMESTAMPTZ created_at
    }
    refs {
        TEXT repo_id FK
        TEXT kind
        TEXT name
        TEXT target FK
        TEXT symbolic
        BIGINT version
        TIMESTAMPTZ updated_at
    }
    memories {
        TEXT snapshot_id PK
        TEXT summary
        TEXT[] key_facts
        TEXT[] open_tasks
        TEXT provider
        TIMESTAMPTZ created_at
    }
    team_identities {
        UUID id PK "legacy — team-token discontinued"
        TEXT team
        TEXT name
        TEXT email
        TIMESTAMPTZ created_at
    }
    users {
        TEXT id PK
        TEXT email
        TEXT username
        TEXT load_mode
        TIMESTAMPTZ created_at
    }
    workspaces {
        TEXT id PK
        TEXT name
        TEXT slug
        TEXT owner_id FK
        TEXT visibility
        TIMESTAMPTZ created_at
    }
    pending_contexts {
        TEXT repo_id PK
        TEXT session_id PK
        TEXT branch
        TEXT target
        TIMESTAMPTZ updated_at
    }
    unsync_contexts {
        TEXT repo_id PK
        TEXT username PK
        TEXT branch PK
        TEXT target
        TIMESTAMPTZ updated_at
    }

    repos ||--o{ branches : "has"
    repos ||--o{ snapshots : "has"
    repos ||--o{ refs : "has"
    snapshots ||--|| blobs : "doc_hash"
    snapshots |o--o| blobs : "memory_hash"
    snapshots |o--o| memories : "has"
    refs }o--|| snapshots : "target"
    branches }o--|| repos : "belongs to"
    workspaces ||--o{ repos : "contains"
    users ||--o{ workspaces : "owns"
    repos ||--o{ pending_contexts : "has"
    repos ||--o{ unsync_contexts : "has"
```

## Usage Principles

1. **Object formats** — The CLI and frontend serialize JSON against `cir.schema.json` and `manifest.schema.json`. Go structs and TypeScript interfaces may be defined independently, but their fields must conform to these schemas.
2. **REST contract** — Both backend routes and the CLI HTTP client treat `openapi.yaml` as the source of truth. Update it first when adding or changing a path, then keep the CLI, backend, and frontend aligned.
3. **Database schema** — SQL files under `db/migrations/` are applied to backend PostgreSQL instances in order. The CLI and frontend never access the database directly; they use REST only.
4. **Compatibility** — Do not make backward-incompatible changes to the fields, types, or required sets in `cir.schema.json` or `manifest.schema.json` without an issue, migration plan, and maintainer approval. Adding fields also affects `"additionalProperties": false`.
