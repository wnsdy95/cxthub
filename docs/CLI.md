# `cxt` CLI reference

`cxt` keeps your coding-agent sessions with the Git commits they produced. It
reads what Claude Code and Codex recorded on your computer, and Git, cuts each
session at the commits it made, and uploads the result to your team's CXTHub
server, where a session becomes readable once the commits it explains are
pushed. This reference describes the public command surface on the `main`
branch.

Check the installed version and built-in command catalog first:

```bash
cxt --version
cxt --help
```

For installation and first-run setup, see
[Installing CXTHub](INSTALLATION.md).

## Conventions

- Run repository commands inside an existing Git working tree.
- New connections use `https://<host>/<owner>/<repository>`. The server resolves
  this canonical display address to the original connection identity before it
  is saved. Existing two- and three-segment remotes remain valid exact aliases.
  An unavailable server prevents saving a new unverified connection.
- A session is named by its tool and its identifier: `claude <session>` or
  `codex <session>`. `cxt segments` and `cxt log` print both.
- cxt never judges anything by whether a hook or cxt itself ran at some moment.
  Hooks only wake the reading of the tools' own records and of Git; a hook that
  did not run is made up for by the next one that does, and `cxt push` reads the
  same records at any time.
- cxt cannot stop a Git command or an agent. A failure is reported, kept in
  the collector's state, and shown by `cxt doctor`.
- Every public command supports `-h` and `--help`. Help and usage errors are
  resolved before cxt creates adapters, contacts a remote, or changes anything.
  Unknown flags, missing values, duplicate options, extra positionals and
  unsupported option combinations are rejected. Value flags also accept
  `--flag=value` when the value itself starts with a hyphen.
- Commands that take `--json` print one JSON document on standard output, and
  on failure one JSON error on standard error.

## Recommended onboarding

### `cxt setup`

```text
cxt setup [remote-url] [--no-login] [--git-remote <name>]
```

Runs the complete, idempotent onboarding sequence:

1. decide which Git remote of the clone cxt follows (`origin`, the one named with `--git-remote`, or the repository's only remote when it has no `origin`; what is read of it is said where the automatic upload is described) and make the repository's `.cxt` directory and its mark (`.cxt/HEAD`) when they are absent;
2. log in and verify the repository remote when one is supplied (`--no-login` uses existing credentials);
3. register the connection with the server;
4. install the managed Git hooks;
5. merge the Claude Code and Codex hooks into their settings;
6. ask the server whether the repository is connected to GitHub; and
7. pull team settings when logged in.

Example:

```bash
cxt setup https://cxthub.example/alice/platform
```

Rerunning the command reports and repairs missing setup steps without replacing
an existing remote that points somewhere else. An explicitly requested
different repository is rejected while the existing connection is preserved.

The one exception is a repository the server moved. A clone that reaches its
server through a [transport endpoint](../cli/TRANSPORT_MIGRATION.md) keeps the
workspace the endpoint was verified for. When the server keeps that workspace's
repository under the requested address, because an operator moved it there
with `cxt-admin repo-address`, setup replaces `origin` with the requested
address. An active `capture.identity` root preference is confirmed again for the
new origin; when the server does not confirm it, setup says so and conversation
documents are sent in the legacy identity. Before the move, setup with the new
address says that the server keeps the repository under the clone's address,
and changes nothing.

Where the server has a GitHub App and says the repository is not connected to
it, setup prints the page where a maintainer of the repository connects it and ends
as not complete, with exit status 1, after everything on the computer was set
up: the server sees what is pushed through that connection. Run setup again once
it is connected. A server without a GitHub App is not asked for more; see
[GitHub connections](GITHUB_CONNECTIONS.md#what-readers-of-a-repository-are-told).

Nothing is uploaded by setup itself. From then on the hooks wake the collector
described under [Automatic upload](#automatic-upload); `cxt doctor` says whether
it runs and what, if anything, does not fit.

The agent hooks also cover Codex app/IDE sessions and Claude Desktop's **Code**
tab. Codex's hook is registered for every folder, but cxt acts only where
`cxt init` or `cxt setup` wrote `.cxt/HEAD`. A `.cxt` directory without that
mark is left as it is and is added to both `.gitignore` and
`.git/info/exclude`.

## Repository and authentication

### `cxt init`

```text
cxt init [--no-hooks] [--remote <repository-url>]
```

Makes `.cxt/` and its mark, updates local ignore protections, creates
`.cxtsecrets` from eligible `.env` values when appropriate, and installs the
managed Git hooks.

- `--no-hooks` skips Git hook installation.
- `--remote` also registers `origin`.

### `cxt repo create`

```text
cxt repo create <repository-url>
```

Convenience alias for `cxt init` plus `origin` registration.

### `cxt remote`

```text
cxt remote [-v]
cxt remote add <name> <repository-url>
cxt remote remove <name>
cxt remote set-endpoint <name> <https-api-endpoint>
```

The server resolves the display URL to its stable connection identity before
registration. The saved `origin` determines the API endpoint and the
repository's identity; renaming a repository on the server does not change it.

```bash
cxt login --server https://cxthub.example
cxt remote add origin https://cxthub.example/alice/platform
cxt remote -v
cxt remote remove origin
```

Changing an existing remote requires removing it and adding the replacement.
`set-endpoint` saves another API endpoint for the same repository after the
destination confirmed the login and the repository's identity.

### `cxt login`

```text
cxt login [token] [--server <server-url>]
cxt login -t <token> [--server <server-url>]
```

Without a token, starts the browser device flow. Use `--server https://<host>`
to authenticate before a new connection; otherwise it uses the configured
`origin`. `cxt setup <repository-url>` performs login and connection together.
The manual token form is intended as a fallback and may leave the token in
shell history. `CXT_TOKEN` is the non-interactive override.

### `cxt logout`

```text
cxt logout
```

Removes the locally stored credential for the configured origin host. It does
not revoke the token on the server.

## Session records

Every command of this section is also a top-level command: `cxt push` is
`cxt segments push`, and likewise `cxt log`, `cxt remove`, `cxt allow`,
`cxt doctor`, `cxt refresh`, `cxt archive`, `cxt unarchive`, `cxt resend` and
`cxt fork`. Both spellings take the same arguments and do the same thing; the
headings below use the long one.

### `cxt segments`

```text
cxt segments [--json]
```

Reads the session records Claude Code and Codex keep on this computer, and
what Git recorded about the current repository, and shows each session cut at
the commits it made. It is read-only: it writes nothing to the repository, Git
configuration, `.cxt`, the server, or a tool's own records, and it needs no
`cxt init`, remote or login. It does not depend on cxt or a hook having run
when the work happened, so the same records give the same report every time.

What is read:

- **Git:** every worktree of the repository, the HEAD log of each one, and
  every commit object Git still has.
- **Claude Code:** the session files under `~/.claude/projects`, including the
  sub-session files in each session's `subagents` folder.
- **Codex:** the newest `state_<n>.sqlite` and `thread_history_<n>.sqlite` in
  `~/.codex`, through the `sqlite3` program, opened read-only. Items still in a
  write-ahead log are read. A database with nothing in its log is opened as
  immutable, so that SQLite creates no lock file beside it. Older Codex rollout
  files are not read.

  These databases are Codex's own and their layout is not a published
  interface. cxt names only the tables and columns it needs (three tables), so
  Codex may add to them freely. When a Codex release lays them out another way,
  the threads are listed as unread with each difference named, for example
  `thread_history_2.sqlite: table thread_items has no column item_json`, and a
  newer cxt is needed. Nothing is lost meanwhile: the threads stay where Codex
  keeps them, and `cxt segments push` uploads them once cxt reads them again.

A session belongs to the repository when a working folder its records name is
inside one of the repository's worktrees. The one fallback is a Codex thread
whose recorded remote address is the repository's `origin`, for a thread whose
folder is no longer a worktree.

How the report is computed:

- **Session to commit.** A command's output printed the commit number, and the
  commit was created while that command ran, from two seconds before it started
  to three seconds after it ended (`printed-number`). The one fallback applies
  only to a commit no session printed: a worktree's HEAD log records that the
  commit was made there, and a command of the session was running in that
  worktree at that time (`same-folder-and-time`). Each link names its rule.
- **Segments.** A session is cut at every command that made a commit. The last
  commit of a turn keeps the rest of that turn; earlier commits of the same
  turn close right after their command. A command that made several commits
  ends one segment linked to all of them. Two sessions linked to one commit
  each keep their own segment. A range is given by the tool's own first and
  last record identifiers. A turn is the unit the tool recorded: Claude Code's
  prompt identifier, or Codex's turn identifier. Without a recorded turn, a
  segment closes at its commit.
- **Commits made in a worktree.** The HEAD log lines counted are a commit of
  any kind, a cherry-pick, a revert, and a merge that made a merge commit. A
  fast-forward creates nothing. A rebase is not counted: the commits it writes
  are copies of commits already made.
- **Uncommitted part.** Records after a session's last commit, or all of a
  session that made none, are not cut further. They are placed on every
  worktree and branch they passed through: the branch the tool recorded;
  otherwise the branch in that worktree's HEAD log at that time; otherwise, for
  a worktree with no recorded move, the branch it is on now. Work done without
  a branch is placed on its worktree. A folder outside every worktree is never
  turned into a guess, and a part with no readable place is reported as such.
- **Copied conversation.** A forked or resumed session repeats earlier records
  under the same identifiers. Each record is counted once, for the session that
  started first, and a commit in the copied part is linked only there. Because
  a copy keeps its original time, a session starts at its earliest record that
  no other session holds. A sub-session file copied into another session's
  folder is read once, and its parent is the session its records name, not the
  folder the file sits in.
- **Session relations.** Which session spawned which, and at which record of
  the parent: the record that made the recorded call, or, when the tool
  recorded none, the parent's last record when the spawned session started.
  Agent-to-agent calls are listed as Codex recorded them. No relation is
  inferred.
- **Unread records.** A record that exists but cannot be read, such as an
  unknown format, a database whose layout changed or a missing `sqlite3`, is
  listed under unread records and never skipped silently. A last line a tool is
  still writing is left for the next run.

A commit a HEAD log records but no session is linked to is listed with no
session, so the gap is visible. Each commit also carries a `local` state
(`on-a-branch`, `through-its-copy`, `not-on-a-branch`, `object-not-in-git`).
This is only what this clone shows: a copy is a commit on a local or
remote-tracking branch with the same author time, author and subject, and
pull-request commit lists are not read here.

`--json` prints one JSON object with `version`, `repository`, `summary`,
`unread`, `sessions`, `commits`, `spawns` and `agent_calls`. The layout
describes this report and is not a storage or wire format; it may change.

Every run reads all records again, which can take a while on a computer with
many long sessions.

### `cxt segments push`

```text
cxt segments push [--json]
cxt segments push claude|codex <session id> [--json]
```

Computes the same report and uploads it to the repository's CXTHub server, one
report per session, together with the conversation of each session's own
records. It needs a connected remote and a login, and repository member access.
On this computer it writes two things,
both described below: a branch ID in Git's own configuration of each branch,
and, where cxt is set up, a copy of what it sent under `.cxt/sessions`.

What is uploaded for a session:

- its tool, its identifier, whether another session spawned it, and for a
  spawned session its parent and the record of the parent it was spawned at;
- its segments: the first and last record identifier, where the range sits
  among the session's own records, the number of commands, the times, and the
  commits the segment ends at, each with the rule that linked it and the
  commit's creation time, author time, author email and subject. The server
  uses the last three to recognize a cherry-picked or rebased copy;
- its uncommitted part and the places it passed through. A worktree is uploaded
  as its folder name only, never as a path, together with this computer's host
  name so that your own computers can be told apart. The part is not cut any
  further, but where each of its turns begins is uploaded (the position of the
  turn's first record, its place in the conversation and its time), so that
  the server can tell which turns had begun when a branch was pushed;
- how many of its records are copies that belong to an earlier session, and the
  agent-to-agent calls the session recorded.

Each run also sends the list of the repository's worktrees on this computer:
for each one its folder name and the branch checked out in it, with this
computer's host name. No path is sent. The list replaces what you stored for
this computer; a worktree that is no longer listed keeps its name on the
server, marked removed, so that a note you wrote about it stays readable. Two
folders with the same name are listed once. Only you can read the list. A list
the server refuses does not stop the reports; the next run sends it again.

The conversation is uploaded in documents, one or more per session:

- They hold the session's own records only, in order. A record copied from an
  earlier session is stored once, with that session, so a forked session's
  documents hold only what the fork added.
- A part of the conversation is sealed where the bytes of its records, as the
  tool keeps them, reach 4 MiB. A sealed part is one document and never
  changes. What follows the last sealed part is one more document, which is
  built again while the session goes on. A session shorter than that is one
  document.
- The records are read back from where the tool keeps them, converted to one
  format for both tools, and masked by two steps: the values listed
  in `.cxtsecrets`, then the pattern scrub of the repository's `secrets.scrub`
  tier.
- Each document goes through the existing document path. The server says
  which chunks it lacks, only those are sent, and the server verifies the
  whole document. A session that did not change sends nothing.
- The report names the documents in order and, for every segment and for the
  uncommitted part, where it starts in the conversation and how many events
  it holds.
- A document's own header names the session, and no folder or branch. What
  the conversation itself says, including paths in commands and their output,
  is uploaded as recorded, after masking.

A Codex thread's conversation is read from the thread history database its
records were listed from, one record per item of the thread:

- An item is converted by the shape Codex publishes for it in its app-server
  protocol: a message, a plan, reasoning (the text Codex kept of it), a command with
  its output, a file change, a tool call with its result, a web search.
- An item of a kind cxt has no rule for is not dropped and does not stop the
  upload. It is carried whole as one event named after its kind, so a Codex
  release that adds a kind of item loses nothing.
- The folder a command ran in, a process number and a path where Codex saved
  a file stay on this computer.

A session is uploaded as its report alone, and listed as without conversation,
when its records changed while they were read, when a document of it was not
taken by the server, or when a Codex thread's items could not be read (no `sqlite3`
program, or a database Codex laid out another way); each one says why. That is
not a failure of the command: the next run uploads it.

On the server a report belongs to the account that uploaded it. A push is what
opens it, part by part, to whoever may view the repository: its members and
viewers, and in a public repository also a visitor who is not signed in:

- a segment, when one of its commits, or a cherry-picked or rebased copy of
  it, is on the remote;
- the uncommitted part, up to the turns that had begun when a branch it passed
  through was pushed. The turn in progress at the push is included whole;
- a spawned session as a whole, when the point of its parent that spawned it
  is open.

The server decides this from its own read of the Git hosting provider, never
from what a CLI says. The time of a push is the time the server saw the branch
changed: seconds after the push where the server was told of it, by the host
or by the collector, and later where nothing told it. The first read
of a repository says nothing about when its branches were pushed. What was
opened stays open: a forced push that removes the commit, or cutting the
session another way, takes nothing back.

Anyone but the uploader reads only the open parts, and is given the uploader's
public handle with them. Worktree names, the computer's host
name, the agent calls, and the author email of a commit stay with the
uploader. Only a member writes a note. The conversation is read through the session; the hash of the
document that holds it is shown to nobody else. That document is still an
ordinary repository document, so the server does not keep a member who learned
the hash some other way from reading it.

After a session's report is stored, cxt writes the session down at each commit
it made and stores that summary on the server: what was done, what is worth
keeping, what is left to do. It is made by fixed rules, without asking a
model, from the conversation up to the commit and, when cxt can read it, the
tool's own memory as it is when cxt runs. A session that made no commit has no
summary. A summary is read like the segment that ends at its commit: by the
uploader always, by anyone else once the commit is pushed. `summaries` in the
result counts what this run stored, and a session says in `summary_reason` why
one was not; the next run tries it again. The collector writes a commit's
summary once; `cxt segments push` writes every one again.

The server lists the summaries along a branch, the summary of the newest commit
first (`GET /repos/{id}/anchored-memory?branch=`). It walks back from the
branch's newest commit through the commits it read from GitHub, so the
summaries of the branch itself come first and those of the branch it was made
from after them. A commit a pull request was merged as, also by a squash,
stands for that pull request's commits, and a rebased or cherry-picked commit
is found where its copy stands. A branch GitHub does not hold is read as the
default branch. A commit that is on no branch of GitHub is not on the walk. The
caller names the newest commits of the branch as its own folder has it
(`local=`), and they are looked at first, so that what a member committed and
did not push, or pushed a moment ago, is found.

The Sessions tab searches all of this at once: pull request titles, branch
names, commit subjects and the beginning of commit numbers, the conversations
and the notes. Each result says which of these it is. A reader is answered
only from what that reader may read, so a part that is not pushed is found by
its uploader alone.

Two commits can be compared in the Sessions tab. One commit is set as the
base; every commit opened from the list afterwards shows what lies between the
two, as `git log base..commit` lists it: the commits the opened one descends
from and the base does not, newest first, each with the segments that explain
it. Only commits the server read from GitHub can be compared.

A session can be archived, by the member it belongs to or by a maintainer of
the repository. An archived session is left out of a commit's segments, of the
search and of the summaries, also those a session is given along a branch, for
every reader including its uploader. It is listed under "Archived sessions"
and is opened from there under the usual rule; nothing of it is removed, and a
report cxt uploads again leaves it archived. Restoring it brings it back
everywhere.

Uploading again replaces the session's report as a whole; a report with the
same content changes nothing. Reports do not advance the repository revision.

Branch IDs. Before the sessions, every branch of the repository that has no ID
gets a random one, stored by Git with the branch itself:

```text
branch.<name>.cxtBranchId            the ID (32 hexadecimal characters)
branch.<name>.cxtBranchIdIssuedFor   the name the ID was issued for
```

- Git keeps these settings with the branch: a rename moves them, deleting the
  branch drops them, a branch made again under the same name starts without an
  ID, and every worktree of the repository sees the same value. The ID only
  tells two uses of one name apart. It proves nothing about when or how the
  branch was made.
- `git branch -c` copies the settings. When two branches hold one ID, every
  holder whose name is not the name the ID was issued for gets a new ID. A
  branch that was renamed keeps the old issued-for name, so if it is copied
  later, both the branch and its copy get a new ID.
- The IDs of branches that Git's own records show on `origin` are then
  registered with the server: a branch whose configured upstream is on
  `origin`, or else one with a remote-tracking branch of `origin` under the
  same name. A branch that was never pushed is not reported. On the server a
  name alive on the remote has one lifetime, and a registration joins it, or
  opens one when none is alive.
- Neither step stops the upload. If Git refuses the write or the server does
  not take the report, the output says so and the next run tries again.

With a tool and a session identifier only that session is uploaded, by the
same rules: everything is read again and the report, the conversation and the
summaries of that one session are sent. A session the tools' records on this
computer do not hold makes the command fail and is named under `not_found`.

Every session is listed with what happened to it: `stored`, `unchanged`,
`refused` or `removed`. A session that was removed from the server (see
`cxt segments remove`) is `removed`: nothing of it is read back or sent, the
reason says who removed it and what brings it back, and it is no failure. A
run that is about to send something first asks the server, once, which of your
sessions are removed.

A report is refused when another account already reported the same
session identifier, when the tool's identifier cannot be used as a key, or when
the server finds the report too large or not well formed. The other sessions
are still uploaded, and the command then exits with a failure so that the gap
is noticed. A caller the server does not accept, such as a missing login or no
member access, stops the run at the first request. Records that could not be
read are listed again here, because no report covers them, and so is the number
of agent calls recorded by a session outside this repository, which are
uploaded with no session.

`--json` prints one JSON object with `version`, `stored`, `unchanged`,
`refused`, `content_not_uploaded`, `sessions`, `unread`,
`agent_calls_without_sender` and `branches` (`with_id`, `assigned`,
`on_origin`, `registered`, and `not_assigned` or `not_registered` with the
reason when a step did not happen) and `local_records`. Each session carries
`content` (`stored`, `none` or `not-uploaded`) and, for the last,
`content_reason`.

Every run reads all records again and rebuilds every session's document to
know its identity, which can take a while on a computer with many long
sessions, even when the server then needs nothing sent.

Copies on this computer. Where cxt is set up in the repository, every upload
keeps what it sent for a session in the repository's `.cxt` folder, which Git
ignores:

```text
.cxt/sessions/<tool>/<session id>/report.json                      the report as it was last sent and taken
.cxt/sessions/<tool>/<session id>/conversation/<document>.json.gz  each document of the conversation that report names
.cxt/sessions/<tool>/<session id>/log.jsonl                        one line per run that sent something or failed
```

- They are a record, kept so that the server and this computer can be compared
  when they do not agree, and so that what was sent can be sent again after
  the tool removed its own files. No upload is computed from them: every run
  still reads the tools' records and Git.
- A conversation is uploaded in parts, one document each, and the copy is
  those documents, each named by the hash the report names it by. They are the
  masked documents that were uploaded, not the tool's file. A session that
  goes on gets a file for what was added and another one for its last part;
  the files of the parts before stay as they are, and a file of a document the
  kept report no longer names is removed. Together the copies take about as
  much disk as the conversations uploaded from this computer, compressed.
- A log line says when the run was, whether a person asked for it or the
  collector ran, what the server did with the report (`stored` with its
  revision, `unchanged`, or `refused` with the reason), how many documents the
  report names for the conversation (`parts`) and which is the last of them
  (`doc`), how many summaries were stored, and why something was not. A run in which nothing was sent and nothing failed leaves
  no line, and a failure that repeats is logged once.
- The files are readable by you alone. A copy that cannot be written, or
  that lacks a document its report names, does not stop the upload: the
  result says so under `local_records` (`kept`, `not_kept`). The next run
  that reads that session's conversation builds all of it and writes the copy
  whole; `cxt segments push` does that at once. In a repository where cxt is not
  set up, nothing is kept.

### `cxt segments log`

```text
cxt segments log [--json]
cxt segments log claude|codex <session id> [--json]
```

Shows what `.cxt/sessions` keeps, from this computer alone: every session with
what its copies hold and its last log line, or one session with its whole log.
It reads only and needs no server. `--json` prints one object with `version`
and `sessions`; each session carries `report_kept`, `conversation_kept`
(true when every document the report names is kept), `segments`, `records`,
`parts`, `doc` (the last of the documents), `events`, `report_hash`, `lines`
and `log`.
`report_hash` is the fingerprint of the report copy; the last `stored` or
`unchanged` line of the log carries the same value when the copy is the report
the server last took.

### `cxt segments remove` and `cxt segments allow`

```text
cxt segments remove claude|codex <session id> --yes [--json]
cxt segments allow claude|codex <session id> [--json]
```

`remove` takes a session off the server for every reader: its report with its
commit links, its summaries and the ranges that were open, and its conversation
document when no other report uses it. The sessions it spawned, as far as they
are yours, are removed with it, and each one is listed in the result. The
member the session belongs to may remove it, and so may a maintainer of the
repository.

- Without `--yes` nothing is removed and the command fails, saying what it
  would do. That is deliberate: removing is not something to try.
- Nothing on this computer is removed. The tool's records and the copies under
  `.cxt/sessions` stay, and the session's log gets a line.
- The server remembers the removal. No upload sends the session again, neither
  `cxt segments push` nor the collector, and the server refuses its report
  from anyone, until it is allowed again. Archiving, by contrast, only hides a
  session from lists; an archived session is still opened by its address.
- Manual notes are not removed.
- On the server the conversation can no longer be read through any route. The
  pieces it was stored in stay in the server's storage until the operator
  removes pieces no document uses; with the file store that is `cxtd repack`,
  and the PostgreSQL store has no such step yet.

`allow` takes back the removal of one session and uploads it again at once,
from the tool's records. A session that was removed together with the one that
spawned it has a removal of its own and is allowed by its own name. When the
tool no longer holds the session's records, the session is allowed but not
uploaded, and the command fails saying so.

### `cxt segments doctor`

```text
cxt segments doctor [--json]
```

Finds what does not fit between the sessions the tools recorded on this
computer, what cxt keeps here and what the server holds, and gives for each
thing the command that puts it right. It is made for the coding agent to run
when something looks wrong: every finding has a code that does not change, a
sentence that says what is so, why it matters, and, when something can be done,
the exact command.

It reads everything again at that moment: the tools' records and Git, the
collector's memory, the copies under `.cxt/sessions`, and, when a server is
configured, the server. Nothing is taken from a hook having run, and nothing
is changed anywhere. A finding does not make the command fail; a server that
cannot be read is itself a finding, and what this computer alone shows is still
said. `checked` lists what was looked at, so that no finding means something.

| Code | What it says | The command it gives |
| --- | --- | --- |
| `not-set-up`, `not-connected` | cxt is not set up here, or no server is configured | `cxt setup` |
| `collector-off` | automatic upload is turned off | `cxt config segments.auto on` |
| `collector-never-ran` | the collector has not run here yet | `cxt segments push` |
| `collector-failing` | its last run failed, with the error | `cxt login` for a missing login, else `cxt segments push` |
| `collector-behind` | sessions have records newer than its last run, more than 10 minutes old | `cxt segments push` |
| `unread-records` | records exist that were not read, with the reason | install `sqlite3`, or update cxt, when that is the reason |
| `weak-link` | a commit is linked to a session by the fallback rule, not by a printed commit number | none: it is something to check |
| `not-on-server` | a session of this computer is not on the server | `cxt segments push <tool> <session>` |
| `server-behind` | the server holds fewer records of a session than this computer | `cxt segments push <tool> <session>` |
| `server-differs` | the report on the server is not the one this computer last sent | `cxt segments push <tool> <session>` |
| `push-not-seen` | a commit is on origin here, but the server has not seen it on GitHub | `cxt segments refresh` |
| `github-not-connected` | the server has a GitHub App and this repository is not connected to it | none: a maintainer of the repository opens the page the finding names |
| `summary-missing` | a session has no summary at one of its commits | `cxt segments push <tool> <session>` |
| `removed` | a session was removed from the server | `cxt segments allow <tool> <session>` |
| `archived` | a session is archived on the server | `cxt segments unarchive <tool> <session>` |
| `no-copy` | sessions on the server have no copy under `.cxt/sessions` | `cxt segments push` |
| `records-gone` | the tool no longer holds the records of sessions uploaded from here | none: the copies are what is left |
| `only-in-copy` | a session is neither on the server nor among the tool's records | `cxt segments resend <tool> <session>` |
| `server-not-read` | the server could not be read, with the error | `cxt login` for a missing login |

More than ten sessions that are not on the server are counted, not listed.
Summaries are asked about for the newest twenty commits of this computer's
sessions, one request each. Whether a commit is on origin is read from this
clone's remote-tracking branches, without asking the remote.

What it cannot find: a conversation linked to the wrong commit is not
detectable from records; `weak-link` only names the links made by the weaker
rule. Nothing re-links a commit by hand.

### `cxt segments refresh`, `archive`, `unarchive` and `resend`

```text
cxt segments refresh
cxt segments archive claude|codex <session id>
cxt segments unarchive claude|codex <session id>
cxt segments resend claude|codex <session id> [--json]
```

- `refresh` asks the server to read the repository from GitHub again soon. A
  part of a session opens to teammates when the server sees its commit there.
  The collector asks the same by itself after a push, so this command is for a
  push made where cxt does not run.
- `archive` and `unarchive` do what the Sessions tab does: an archived session
  is left out of commits, the search and the summaries for every reader, and
  nothing of it is removed.
- `resend` sends again what was last sent for a session, from the copy under
  `.cxt/sessions`: the conversation document and the report as they were, and
  the summaries written anew from that conversation. It is for a session whose
  records the tool no longer holds and that the server lost; nothing is read
  from the tool. A copy of the conversation that is not the document its
  report names is not sent, and neither is a session that is removed.

### `cxt segments fork`

```text
cxt segments fork --at <commit> --branch <name> [--json]
cxt segments fork claude|codex <session id> [--at <commit>] [--to claude|codex] [--branch <name>] [--json]
```

Starts new work from a commit, in one of two ways or in both at once.

**A Git branch only.** Without a session, the branch `<name>` is made at the
commit and nothing else happens: no folder is switched to it, and no upstream
is set. A session started on that branch is given the most recent summary
along it, like a session on any branch, so the work that led to the commit is
known to it without the conversation being copied. The commit is named by its
number, an abbreviation of it or any revision this clone holds.

**A continued conversation.** With a session, its conversation is read from the
server and written as a new session of a tool in this folder; the command
prints what starts it (`claude --resume …` or `codex resume …`).

- With `--at`, the new session holds the conversation up to the end of the
  segment that ends at that commit. Without it, all of the conversation.
- It is read as far as this account may read it. Your own session is read
  whole; a teammate's as far as it is pushed, and the result says how many
  events up to there were not readable. A commit no readable part ends at is
  refused.
- `--to` names the tool of the new session; without it, the tool that recorded
  the conversation. The same tool keeps the conversation as it was. Another
  tool gets it rebuilt in its own format, which loses what only the first tool
  can read, such as locked reasoning.
- A conversation larger than the tool takes at the start of a session is cut to
  its newest whole turns. The summary the session
  has at the commit is put first in place of what was left out; without a
  summary there, a line says how many events were left out. The result names
  which of the two it was.
- With `--branch` the Git branch is made too, after the session was written.
  When the branch cannot be made, the session is still there and the command
  says both.

Nothing is uploaded by a fork. The new session is a session of its tool like
any other: once it is used, the collector reads it and uploads it as yours.
Reading a long conversation takes one request for every 64 KiB of it.

In the Sessions tab every commit has "Fork from this commit", which shows these
commands filled in for that commit and for each session a segment of which
ends there. A browser cannot make a branch or write a session file on your
computer, so the tab shows the commands and runs nothing.

### Automatic upload

In a repository cxt is set up in and connected to a server, the same upload
also runs without being asked. Hooks only wake it: the Git hooks `post-commit`,
`post-checkout`, `post-merge`, `post-rewrite` and `pre-push`, and the agent
hooks at the end of a turn and of a session. A hook is never what is
read. The collector reads the records the tools and Git left, by the same
rules, whichever hook ran, so a hook that did not run is made up for by the
next one that does, and work in a worktree where no hook runs is read when a
hook runs anywhere in the repository.

It differs from `cxt segments push` in two ways only:

- **What it uploads.** Sessions recorded since automatic reading began in this
  repository: a session whose last own record, or recorded start when it has
  none, is at or after the moment the collector first ran here. Earlier
  sessions are uploaded only by `cxt segments push`.
- **How much it sends.** It remembers what the server last stored and sends
  only what changed: a report is uploaded again when it differs, a conversation
  is read back and built again when the session's records or their cut changed,
  or when the masking policy or the document identity did, and branch IDs are
  registered again when the branches on origin or their IDs changed.

**After a push.** The server opens a part of a session to others once it has
seen the part's commit on the Git host. It learns of a push from the host's
own notification where a GitHub App or webhook is connected. Without one it
reads the host when somebody opens the Sessions tab and its last read started
five minutes ago or more, and without being asked only every six hours. So a
run also asks
the server to read the host when origin's branches, as this clone holds them,
are not what they were when it last asked: after this computer pushed or
fetched. Git runs `pre-push` before the push and has no hook after it. So that
hook does not wake the collector at once: its script names the Git process
that runs it, the one that pushes, and the collector is woken when that
process has ended, however long the push took. That run finds the branches
changed, and it does not wait out the collector's pause between two runs: a
push of this clone ends the pause, because what was pushed is what the team
is shown. A push still going after thirty minutes is no longer waited for, and
is read at the next wake. Two kinds of push start no wait, because Git runs no
`pre-push` hook for them: one made by a graphical Git program that does not
call the `git` command but pushes with an implementation of its own (a
graphical program that calls `git` runs the hook like a terminal does), and
`git push --no-verify`. Such a push is read at the collector's next wake: the
next commit, checkout or merge in that clone, or the end of an agent's turn.
Nothing is asked when the branches are the same, or
when the clone holds no branch of origin. A server that knows no Git host for
the repository says so, and is not asked again about the same branches.

A run also tells the server about the collector itself, so that the Sessions
tab can say of a commit without a session where it was pushed from. Git
records in a clone each push that clone made, whether or not cxt ran at that
moment. The collector sends the newest 200 of those pushes (branch, commit and
the clone's own time), this computer's label, the version of cxt, and what
went wrong in its run, on one line, when the run failed. It sends this when
there is something the server was not told yet: the clone pushed or fetched,
or the run ended otherwise than the last one it reported. From that the server
says of each push one of four things. A push a collector named was made on a
computer where cxt runs; once a run of that collector has ended well after
it, a commit of that push without a session simply had no agent session, and
until then the cxt on that computer is failing. A push no collector named was
made on a computer where cxt does not run, once every collector heard from in
the last thirty days was heard from after the push. Until then it is not known
yet: a collector that has said nothing since may still name it. What this
cannot tell apart: a computer whose cxt cannot reach the server at all looks
like one that is switched off, until it is heard from again. A member is shown
what went wrong on that member's own computers; other members are shown only
that it failed. Two clones of one repository on the same computer report as
one collector.

All of this reads one Git remote of the clone: its address names the
repository, and its branches say what was pushed. That remote is the one named
`origin`. A repository whose remote has another name is told once:
`cxt setup --git-remote <name>` checks that the repository has a remote of
that name and keeps the name in the repository's own Git configuration
(`cxt.gitremote`), which every worktree of the clone shares. Without the flag
`cxt setup` follows the repository's only remote when it has exactly one and
none named `origin`; with several remotes and no `origin` it follows none and
says how to choose. Because the address of the followed remote names the
repository, choosing another remote in a clone that is already connected
makes it another repository to the server.

Like every upload it sends nothing of a session that was removed from the
server, and it then forgets what it had sent of that session, so that the
session is sent whole again once it is allowed again. A run with nothing new
still makes no request at all: the server is asked which sessions are removed
only by a run that is about to send something.

One collector runs per repository at a time, detached from the hook that woke
it. A wake that arrives while one runs is answered by one more run afterwards.
Two runs start at least 10 seconds apart. A run that took long is followed by
a longer pause, six times what it took and at most 10 minutes. A run converts
and sends only what follows the last sealed part of a session that went on,
which takes about a second also for a session file of more than 100 MB. It
reads a session from its beginning, which takes about ten seconds for such a
file, when it sees the session for the first time, when a summary is written
(a summary is written from the whole conversation), when the masking rules
changed, and when the copy on this computer was not whole. After a run that
failed for a reason that holds for every session (no login, no access, a
server without these routes, no connection) the next one waits 10 minutes. A failed run never
fails the hook, Git or the agent.

What it keeps on this computer:

- beside Git's own files of the repository (`<git-dir>/cxt/`): what was last
  sent (`collector-v1.json`, with the moment automatic reading began, how
  the last run ended and, for each session, where its sealed parts end and
  which documents hold them), a lock and a request marker. This is a memory of what
  was sent, not a source of facts: deleting it only makes the next run begin
  again from now and send more.
- in your user cache directory (`cxthub/session-records-v1`): what the readers
  made of session files and thread databases that have not changed since, so
  that a run reads again only what changed. It holds identifiers, times,
  folders, branch names and commit-number candidates, and no conversation text.
- in the repository's `.cxt` folder (`.cxt/sessions`): the same copies
  `cxt segments push` keeps, each log line marked as automatic. A run that
  did not read a session's conversation back, because nothing of it changed,
  leaves the conversation copies as they are.
  It can be deleted at any time.

`cxt segments` and `cxt segments push` use neither: they read everything again
and write no cache.

Turn automatic upload off with `cxt config segments.auto off`.

## Hooks

### `cxt hook`

```text
cxt hook --provider <claude|codex> --event <event>
```

What the agent tools run. `cxt setup` writes these invocations into the tools'
hook settings; users normally do not call it directly. A failure is reported
without blocking the tool.

The events are `SessionStart`, `UserPromptSubmit`, `Stop` and `SessionEnd`.
`Stop` and `SessionEnd`, the end of a turn and of a session, wake the collector
described under [Automatic upload](#automatic-upload): a commit and a push
usually happen in the middle of a turn and Git has no hook after a push, so
the Git hooks wake the collector while the turn is still being written, and
its end lets the collector read the rest. A start or prompt hook wakes nothing
and gives the session the most recent summary along the branch its folder is on, once for
each branch the session comes to: at its first hook, and again after the folder
moved to another branch. cxt reads the branch from Git when the hook runs and
remembers beside Git's files which branch each session was last given a summary
for, so nothing depends on a hook having run at the checkout. The summary is
the newest one another session wrote: first at the newest 50 commits of the
branch as this folder has it, then along the branch on GitHub, and where the
branch has none, along the branch it was made from. It is at most 16 KiB, and a
session is not given what it wrote itself. Of a summary's open tasks, a session
is given only the tool's own list, the one the tool wrote in the summary of its
conversation (`tasks_authoritative`); tasks that cxt picked out of a
conversation are shown to people and never handed to a session as what is left
to do. A branch without a summary gives nothing and is not asked for again; a
server that did not answer within four seconds, or a text that did not reach
the tool, is tried again at the next hook. A folder on no branch, and a
repository without a server, are given nothing.

A hook carries no conversation to the server. What a session said and did is
read from the tool's own records by the collector.

Claude Desktop's general Chat tab is outside this hook model.

### `cxt hooks`

```text
cxt hooks install
cxt hooks uninstall
```

Installs or removes the five managed Git hooks:

```text
post-commit
post-checkout
post-merge
post-rewrite
pre-push
```

Each one runs a hook you already had first, with the hook's own input, and
ends with that hook's result. Then it wakes the collector and tells it which
hook it is, by its own file name. It reads nothing Git passes to it, decides
nothing, and cannot stop the Git command: a missing
or failing cxt changes nothing about the result. Existing user hooks are
restored on uninstall.

Installation requires `.cxt/HEAD` (`cxt init` or `cxt setup`) and repairs both
`.gitignore` and `.git/info/exclude` before writing hook scripts.

Hook scripts that an earlier version of cxt installed are replaced the first
time any hook runs with this version, and by `cxt setup` and
`cxt hooks install`. The earlier `reference-transaction` hook is removed, and
a hook of your own that it had chained is put back in its place.

## Team settings and secret masks

### `cxt settings`

```text
cxt settings pull
cxt settings list
cxt settings restore [index]
```

- `pull` applies available team `.claude/`, `.agents/`, and `.codex/` settings.
- `list` shows current setting-object hashes and local replacement backups.
- `restore` restores a backup; the default index is `0`.

`pull` skips only typed absence (`ErrNotFound`, including the server's 204
"not configured" response). Authorization, network, verification, and local
write errors stop the command with a nonzero exit and preserve their typed
cause. Bundles applied before a later failure remain applied; the error reports
that partial progress. Other HTTP failures, including an unclassified 404,
are not treated as an empty settings bundle.

### `cxt secrets`

```text
cxt secrets push [-p <passphrase>] [--remember] [--rotate]
cxt secrets pull [-p <passphrase>] [--remember] [--force]
```

Encrypts or decrypts the repository's `.cxtsecrets` list locally. The
passphrase is not sent to the server. The values listed there are masked in
every conversation before it is uploaded.

Passphrase lookup order:

1. `-p`;
2. `CXT_SECRETS_PASSPHRASE`; and
3. the per-repository credential saved by `--remember`.

The server returns an editing revision with each encrypted envelope. `pull`
records that revision, server/repository identity, passphrase fingerprint and a
hash of the downloaded plaintext in this worktree's `.cxt/secrets-baseline.json`.
`push` uses that original revision; it never fetches a newer one to bless an
already-edited file. A missing baseline permits first creation only when the
server confirms that no envelope exists.

If another editor saves first, push fails and leaves your draft unchanged.
Copy the draft aside, pull the latest, compare and reapply your changes. A pull
refuses to overwrite unsaved local changes; `--force` explicitly permits that
replacement. A failed baseline write after a successful transfer requires
reloading before further edits. Do not delete the baseline to bypass a conflict.

`--rotate` performs a compare-and-swap passphrase rotation and rejects a stale
update. Share a rotated passphrase with authorized team members through a
separate secure channel.

The scrubber is defense in depth, not a credential-management system. Revoke
any secret that may have entered a session.

## Configuration

### `cxt config`

```text
cxt config <key>
cxt config <key> <value>
```

Reading a key prints its effective local value. Supported keys are:

| Key | Values | Default | Effect |
|---|---|---|---|
| `segments.auto` | `on`, `off`, `default` | `on` | Whether hooks wake the collector. `off` leaves uploads to `cxt push`. |
| `secrets.scrub` | `off`, `standard`, `strict`, `default` | `standard` | Pattern-based scrub tier for uploaded conversations |
| `secrets.redact` | replacement text, `default` | built-in redaction token | Exact-secret replacement text |
| `secrets.minlen` | `0` through `64` | `4` | Minimum exact-secret length; `0` restores the default |
| `capture.identity` | `cxt-manifest-sha256-v1`, `legacy`, `default` | `legacy` | The identity conversation documents are sent in; the root identity requires a successful capability confirmation for the configured origin. |

Examples:

```bash
cxt config segments.auto off
cxt config secrets.scrub strict
```

Configuration is repository-local.

`cxt config capture.identity cxt-manifest-sha256-v1` checks the selected
origin's current repository and upload capabilities before storing a preference
bound to that exact destination. It does not enable server policy. A concurrent
configuration change rejects the update. Changing the identity makes the
collector build every conversation again. See
[conversation storage](CONVERSATION_MEMORY_STORAGE.md#explicit-conversation-roots)
for rollout and verification limits.

## Global information

```text
cxt version
cxt --version
cxt help
cxt help <command>
cxt -h
cxt --help
```

`version` prints the release version. All help aliases print the public command
catalog and return success.

## Environment variables

| Variable | Purpose |
|---|---|
| `CXT_REMOTE` | API base fallback when no repository remote is configured |
| `CXT_TOKEN` | Non-interactive authentication token |
| `CXT_NAME`, `CXT_EMAIL`, `CXT_TEAM` | Identity overrides sent with requests |
| `CXT_NO_BROWSER=1` | Prevent automatic browser launch during login |
| `CXT_SECRETS_PASSPHRASE` | Passphrase fallback for encrypted secret-mask sharing |
| `CXT_CODEX_BIN` | Path of a Codex program for the opt-in test that checks what cxt reads against what Codex writes |

Do not expose tokens, passphrases, or private session content in command output,
shell history, issue reports, or CI logs.

## Coming from a version that captured

Earlier versions recorded a copy of each conversation ("capture") when a hook
ran and kept a history of those copies beside Git. This version does not: it
reads the tools' own records. What that changes on your computer:

- These commands no longer exist: `cxt save`, `add`, `commit`, `restore`,
  `status`, `diff`, `list`, `checkout`, `switch`, `load`, `memorize`, `memory`,
  `stash`, `tag`, `branch`, `pull`, `fetch`, `repair`, `fsck`, `reflog`,
  `repack`, `capture`, `sync`, `mcp --local`, and the wrappers `cxt claude` and
  `cxt codex`. Start Claude Code and Codex the way you normally do.
- `cxt push`, `cxt log`, `cxt fork` and `cxt doctor` now mean what this
  reference describes.
- The Git hooks are replaced by ones that only wake the collector, as described
  under `cxt hooks`. Nothing of cxt can stop a branch from being created any
  more.
- The files an earlier version kept under `.cxt` (objects, refs, journals) are
  not read and not changed. `.cxt/sessions`, the remote configuration and the
  mark `.cxt/HEAD` are what this version uses. The rest can be deleted.
- Before the server stops keeping captures, run `cxt push` once on every
  computer you worked on, so that the sessions recorded there are on the
  server in the new form.

## Machine failures

A command given `--json` that fails writes one JSON object on standard error:
`{"error": {"version": 1, "code": ..., "phase": ..., "message": ..., "retryable": ..., "exit_code": ..., "mutation_state": ...}}`.
The process exits `2` for arguments that were rejected before anything ran,
`130` when it was cancelled, and otherwise with a code that tells apart a
change underneath the command, damaged input, and a failure that needs
attention.
