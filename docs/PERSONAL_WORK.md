# Explicit personal work handoff

Select a structured handoff file for a fresh session or an inspectable package:

```sh
cxt --work-state ./handoff.json codex --yolo
cxt --work-state ./handoff.json claude
cxt load --work-state ./handoff.json --provider codex
cxt load --work-state ./handoff.json --provider codex --output ./package.json
```

The launch flag belongs before `codex` or `claude`. Relative file paths resolve
from the invocation directory, including when a provider `--cd` selects another
worktree. The artifact must match that actual destination worktree. Native
resume/fork/continue, noninteractive launch, and legacy load `--mode` cannot be
combined with personal handoff. Local/account legacy load defaults do not
override an explicit handoff. Duplicate flags are errors.

An optional prefix `--pull --context-budget 200k` still selects history under the
existing host capability checks. `cxt load --context-budget 200k --output ...`
can prepare an inspectable history artifact. `--work-state` does not change those
checks or imply provider acceptance.

Without this flag, no personal state is selected. There is no latest-session
lookup, environment variable identity, shared team task inheritance, implicit
state write, or global session-to-worktree mapping. An invalid or unavailable
handoff fails preparation; it never starts the provider without the handoff.

## Artifact

The file is a regular JSON file of at most 1 MiB. Version 1 has three required
top-level fields: `version`, `repository_id`, and `state`. See
[the schema](../schemas/personal-work.schema.json). This illustrative file uses
placeholder hashes and must be filled with actual source identifiers:

```json
{
  "version": 1,
  "repository_id": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "state": {
    "scope": {
      "actor_id": "dev:alice@example.test",
      "session_id": "11111111-1111-4111-8111-111111111111",
      "worktree_id": "0123456789abcdef0123456789abcdef"
    },
    "goal": "Finish the explicit personal handoff",
    "acceptance": ["Reject mismatched provenance"],
    "completed": ["Added the parser"],
    "remaining": ["Review the tests"],
    "last_verification": "Targeted tests passed",
    "next_step": "Review the uncommitted diff",
    "exact_user_constraints": [
      {
        "text": "Do not deploy.\n  Keep IDs unchanged. ",
        "source": {
          "snapshot_id": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
          "doc_hash": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
          "start_event": 0,
          "end_event": 1,
          "tool": "context_fetch"
        }
      }
    ],
    "sources": [
      {
        "snapshot_id": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
        "doc_hash": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
        "start_event": 0,
        "end_event": 2,
        "tool": "context_fetch"
      }
    ]
  }
}
```

Use the repository ID and `selection.worktree_id` from `cxt status --json`.
`actor_id` is the account ID returned by a fresh authenticated `GET /me` on the
configured backend. It is not Git author email, a display name, or `CXT_EMAIL`.
Development account IDs can be `dev:<email>`; the backend account's separate
email is compared with each source snapshot's recorded author email. A backend
without an authenticated `/me` response containing both ID and email cannot
perform personal handoff. No local identity fallback is attempted. Supplying
`--work-state` is the authenticated caller's explicit adoption of the selected
goal, notes, and exact constraints. It does not prove prior authorship.

Select the exact session and snapshot identifiers from the source you intend
to continue. Every source, including each constraint's independent pointer,
must resolve through fresh repository-authorized reads to that account's
recorded author email and the exact `session_id` in both snapshot metadata and
the hash-verified document envelope. This checks recorded attribution; snapshot
author metadata is not a cryptographic proof of who typed each event.

Event ranges are zero-based, end-exclusive indices into the original archival
document's `events` array, never compacted/replayed event indices or `seq`
values. `doc_hash` must equal `snapshot_id`; `tool` must be `context_fetch`.
Memory pointers cannot establish personal provenance. There must be 1–128
sources, with at most 128 exact constraints. Validation reads at most 8 distinct
documents and 8 MiB of document JSON cumulatively; each HTTP read is bounded by
the remaining allowance before decoding. Larger inputs fail without launch. Ranges must be nonempty and inside
the document. Synthetic context packages, legacy branch/resume seeds, environment-context
blocks, agent messages, and compaction events/summaries are rejected in addressed ranges.

Each constraint must equal the complete text of a user message in its addressed
range. Multiple text blocks are joined with a newline. Whitespace is preserved;
assistant/tool prose, paraphrases, and substrings dropping a negation or
condition are rejected. The other state fields are explicitly authored handoff
notes; the importer does not derive them or infer approvals from team history.
The complete validated state enters the package unchanged. Budget failure does
not truncate mandatory personal constraints.

## Wiring dependencies

The runtime importer uses `PersonalWorkPrincipal`, `GetSnapshotRemote`, and
`FetchPersonalWorkDocument` on the backend client, then binds a read-only
`PersonalWorkReader` to the validated repository/scope for `AgentContextService`.
`WorkStatePath` is carried by launch intent, `LoadInput`, and
`PrepareAgentContextInput`. No changes to the personal-work storage writer or
the application package consumer are required.

The additive `cloudAgentDocuments.ReadAgentHistoryPage` delegation depends on
the separately supplied `domain.AgentHistoryPageRequest`,
`domain.AgentHistoryPage`, and `BackendClient.ReadAgentHistoryPage`; personal
artifact validation does not change those history-page contracts.
