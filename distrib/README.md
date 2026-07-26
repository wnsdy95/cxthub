# cxt — Git for coding-agent sessions

**Commits keep the code, but the AI conversation that produced it disappears.** cxt puts
coding-agent sessions (Claude Code · Codex CLI) onto your git workflow, turning them into
shared team context — just use `commit` · `push` · `checkout` · `pull` and the conversation
is captured, shared, and restored alongside your commits.

- `git commit` → snapshots the agent session up to that point, linked to the commit
- `git checkout <branch>` → restores that branch's context into your agent session
- `git checkout -b` → starts the new branch with a seed session (distilled memory + recent context)
- `git pull` → briefs your live agent with a summary of teammates' incoming context
- Web UI to browse, search, and fork the conversation behind every commit

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/wnsdy95/cxthub/main/distrib/install | sh
```

Supports macOS (arm64/amd64) and Linux (arm64/amd64).
Pin a version with the `CXT_VERSION=v0.1.0` environment variable.

## Getting started

**Once per team:** create a workspace on the web (e.g. `https://cxthub.com/alice/myteam`).
The workspace URL is itself the repo — it registers automatically on first connect (no separate repo to name).

```bash
cd <your code repo>
cxt setup https://cxthub.com/<username>/<workspace>
#         └── your workspace URL (this URL is the repo identity)
# ✓ store init → git hooks → remote → login (browser approval) → agent hooks → team settings
# From here on, just use git.
```

Codex CLI users: run `codex` once and approve the cxt entries under `/hooks` (one-time trust prompt).

## Releases

Source and verified cxt CLI release assets are published from the same cxthub repository.

## License

[Apache-2.0](../LICENSE)
