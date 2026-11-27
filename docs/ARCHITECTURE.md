# Architecture and enforced boundaries

CXTHub keeps coding-agent sessions with the Git commits they produced. The
`cxt` CLI reads what the agent tools and Git recorded on a member's computer,
cuts each session at its commits, and uploads the result. The server stores
it, decides who may read which part, and serves the web application and the
remote MCP tools.

The server is the store that Web and MCP read. A repository's `.cxt` folder
holds the CLI's connection settings, the mark that cxt may act there, and
copies of what was uploaded (`.cxt/sessions`). Those copies are a record of
what was sent, not a source: every upload is computed again from the tools'
records and Git.

## What is stored

| Thing | What it is |
|---|---|
| Session report | One per (repository, tool, session identifier). It holds the session's segments, the commits each segment ends at with the rule that linked them, and the uncommitted part with the worktrees and branches it passed through. Uploading again replaces the report as a whole. It belongs to the account that first reported the session. |
| Conversation document | One document per session, holding the session's own records after masking. It is stored through the chunked document path and read through the read index stored with it. |
| Summary | What a CLI wrote of a session at one commit a segment of that session ends at. |
| Open ranges | Which parts of a session are readable by others. What was opened stays open. |
| Branch lifetimes | One record per lifetime of a branch name on the remote, with the branch IDs joined to it, so that two uses of one name are told apart. |
| Remote Git cache | What the server read from the Git hosting provider: commits, branch heads and pull requests. Nothing in it comes from a client. |
| Notes and worktree lists | Manual notes about a commit, a branch or a worktree, and each member's list of worktrees by name. |
| Archive marks and removals | An archive mark hides a session from lists, the search and the summaries. A removal deletes a session and keeps its report from being stored again until it is allowed again. |

A session is named by its tool and its identifier, a part of it by its segment
number, and a commit by its number. The hash of a session's conversation
document is shown to nobody but the uploader, and the MCP tools neither take
nor answer one.

## Who reads what

A report is read in full by the account that uploaded it. The server opens it
part by part to whoever may view the repository, from its own read of the
hosting provider and never from what a CLI says:

- a segment, when one of its commits, or a copy with the same author time,
  author and subject, is on the remote;
- the uncommitted part, up to the turns that had begun when a branch it passed
  through was seen pushed;
- a spawned session, when the point of its parent that spawned it is open.

The read routes of sessions ask for the viewer role, which a public repository
gives to a visitor who is not signed in. Storing a report, writing a note,
reporting worktrees and asking for a provider read need a member. Archiving
and removing a session are for its uploader and for whoever manages the
repository. REST and MCP answer through the same application rule, so a part
that is not open is not found by either.

## Server deployment boundary

Production uses Vercel for the frontend, independent Render `cxtd` API and
`cxt-mcp` services, and shared PostgreSQL. `internal/serverruntime` initializes
common store, authentication and lifecycle dependencies. `internal/mcpserver`
composes only the remote MCP and OAuth routes and the read side of sessions; it
holds no command service and starts no workers. Tests in
`internal/architecture` keep these executable dependencies apart. Both
services reuse the application rules and the PostgreSQL transaction contracts;
separation does not introduce a second authorization model or copied data.
See [deployment](../deploy/RENDER.md) for routing and rollout order.

With the default `embedded` execution profile the API process starts these
workers:

| Worker | Work |
|---|---|
| Remote Git | Reads commits, branch heads and pull requests from the hosting provider for every repository that has an origin, and then runs the rule that opens sessions |
| GitHub | GitHub App work, such as applying deliveries; started only where a GitHub App is configured |
| Document finalization | Verifies an uploaded document and publishes it |
| Notifications | Delivers queued webhook notifications |
| Invitation e-mail | Sends queued invitation e-mail |
| Maintenance (two workers) | Runtime and storage housekeeping |

A GitHub push delivery records which branch name is alive or gone in the
branch ledger and asks for a read of the remote. A pull-request delivery asks
for that read, and so do a member's cxt after a push or a fetch and a look at
the Sessions tab whose last read started five minutes ago or more. Applying a
delivery reads nothing from GitHub; the remote Git reader does the reading. A
read that is asked for runs at once: the store tells the reader of every
request that is committed. On PostgreSQL the database does the telling
(`NOTIFY` in the transaction of the request, `LISTEN` on a connection the
reader keeps for it), so the reader hears of a request whichever process
stored it, and no process runs background work that is not its own. A claim
in the store keeps two readers from reading one repository at a time. Two
reads of a repository start at least ten seconds apart. Every thirty seconds
the reader also looks for every repository that is due, which finds what it
did not hear (its listening connection was down), a read to try again, and
the read nobody asked for, every six hours. The listening connection is one
more than the pool holds, for each process that runs the reader. A connection
pooler that hands a connection out for one transaction at a time passes no
notification on: behind one, the reader hears nothing and reads at its look.

An open Sessions tab stays current by following one mark of the repository
(`GET /api/v1/repos/{repoID}/revision`). The mark is another one after
anything the tab shows was stored: a session's report, summary, archive or
removal, a note, a worktree list, a branch registration, a push the Git host
delivered, and the end of a read of the Git host. The tab asks for it every
three seconds while it is in view and reads its lists again only when it is
another one; as a fallback it reads them every five minutes whatever the mark
says. The mark is the repository's revision counters, which each of those
writes moves in its own transaction. What a read of the Git host stores on
its way does not move them, so a page never reads half a pass.

## Ownership

- **Domain:** the rules that cut a session at its commits and link it to them,
  the validation of a report's shape, the rule that opens parts of a session,
  branch lifetimes, the diagnosis `cxt doctor` prints, and secrets policy. The
  report builder and the diagnosis are pure functions over the records that
  were read.
- **Application:** command and query orchestration through ports,
  authorization, transaction scopes and publication order.
- **Adapters:** PostgreSQL and file storage, Git and provider file reading,
  HTTP, MCP and hook protocol mapping. Composition roots choose concrete
  adapters. Public adapters consume application contracts rather than
  reconstructing domain decisions.
- **Web:** server query results plus local selection, folding, scroll and
  graph layout. Hiding a row is not deleting anything on the server.

These boundaries are incremental safeguards, not a claim that directory names
alone prove domain correctness.

## CLI: reading, rules, upload

Reading and rules are separate in the CLI. The readers (`sessionrecords` for
the tools' session records, `gitctx` for Git) make tool-neutral records, and
the report is built from those records by one function without side effects.
The read-only command `cxt segments` constructs no store and writes nothing.

- **Claude Code** sessions are read from the session files the tool keeps,
  including the sub-session files of each session.
- **Codex** threads are read from Codex's own databases through the `sqlite3`
  program, opened read-only.
- **Git** is read for every worktree, the HEAD log of each one, and the commit
  objects.

The upload (`cxt push`, and the collector that hooks wake) sends one report
per session, the conversation document, the summaries, the worktree list and
the branch IDs. A hook is never what is read: Git hooks and agent hooks only
wake a detached collector, one per repository, which reads the same records by
the same rules and sends only what changed. A hook cannot stop a Git command
or an agent.

Conversations are masked before they leave the computer: first the values
listed in `.cxtsecrets`, then the pattern scrub of the configured tier.

The CLI and backend are separate Go modules with no shared package. A parity
test (`go test -tags monorepo ./internal/domain` in `cli/`) keeps the files
both modules carry for the conversation manifest byte for byte identical.

## Conversation documents

The conversation of a session is kept in one or more documents, one after
another. The CLI takes the session's own records in order and seals a part
where the bytes of its records, as the tool keeps them, reach 4 MiB. A sealed
part never changes, so a session that goes on gets a further document for what
was added, and only that is converted and sent.

Each document is uploaded the same way. The upload negotiates which chunks the
server lacks (`POST /push/negotiate`), sends only those (`POST /push/chunks`),
and submits the document for finalization (`POST /push/doc-jobs`, with
`GET /push/doc-jobs/{jobID}` for its state). The finalization worker verifies
the whole document before it is published. The process that stored the job
wakes its own worker when the job is committed, and a worker takes the jobs
that wait one after another; a worker in another process finds a job by
looking once a second. `POST /push/objects` takes conversation documents; a
batch that carries a capture is refused.

A report names its documents in order, with the number of events in each
(`content.parts`), and, for every segment and for the uncommitted part, where
the range starts in the conversation and how many events it holds. A report
that names a document the repository does not hold is refused. When a report
no longer names a document the report before it named, that document is
deleted when no other report uses it.

Two document identities exist: the hash of the canonical document, and the
manifest-root identity described in
[conversation identity](CONVERSATION_IDENTITY.md) and
[conversation storage](CONVERSATION_MEMORY_STORAGE.md).

## Production store requirements

`cxtd` validates the required `ProductionStore` capability set before workers
or HTTP start when PostgreSQL is configured or required. One concrete store
must provide repository and identity transactions, repository access locks,
revisions, durable document jobs, notifications, shared runtime and
authentication state, storage accounting, verified document publication,
indexed document reads, and every store of the session structure: reports,
the branch ledger, notes, reported worktrees, the remote Git cache, open
ranges, summaries and removals. A missing capability is a startup error.
PostgreSQL also has a build-time interface assertion.

The file store remains an explicit development and test adapter and is not
accepted as the cloud transaction boundary. Interface conformance is necessary
but does not prove ACID: real PostgreSQL tests cover rollback, racing commands,
authorization changes, leases and duplicate delivery against shared state. The
[collaboration transaction contract](COLLABORATION_TRANSACTIONS.md) says what
commits together.

PostgreSQL processes verify the schema at startup and never apply migrations;
see [database migration ownership](DATABASE_MIGRATIONS.md).

## Shared admission of heavy work

Heavy reads and uploads share one process-level admission gate with two
classes, interactive and bulk (`app.AcquireResources`). The gate admits a
bounded number of operations and queues a bounded number of callers. A caller
that is not admitted in time gets `resource_overloaded` (HTTP 503 with a
`Retry-After` header) instead of waiting without bound. The session search, the read of summaries along a
branch and the read of a session's content enter as bulk reads.

## Web

The repository page opens on the Sessions tab, which is also what an address
without a tab shows. A list named "On Hold" inside that tab shows what of the
reader's own sessions nobody else can read yet. The other tabs are
Connections, Members and Settings. An address that still names the earlier
`onhold` tab leads to the On Hold list, and one that names the earlier
`context` tab leads to the Sessions tab.

The tab draws the commit graph as the server read it from the hosting
provider, with the segments that end at each commit, pull requests, branches,
worktrees, notes, a search, a comparison of two commits and the list of
archived sessions. The selection is part of the address. A conversation is
read through the session routes, page by page.

## Continuous checks

- Backend `internal/architecture` parses non-test Go imports in both modules:
  domain cannot import ports/application/adapters, ports cannot import application
  or adapters, application cannot import adapters, and internal packages cannot
  cross the backend/CLI module boundary. Integration tests may assemble adapters.
- Web `npm run check:architecture` uses the TypeScript parser and module resolver,
  including aliases and barrel exports, to reject runtime dependency cycles.
  Explicit type-only edges are exempt; mixed and side-effect imports are checked.
- `scripts/e2e.sh` and `scripts/e2e-sync.sh` run the real `cxt` and `cxtd`
  binaries; the second runs the chain a Git hook starts, on the file store or,
  with `CXT_E2E_DSN`, on PostgreSQL.
- API contracts and real browser E2E remain required. Dependency checks
  complement those regressions rather than replacing behavior verification.
