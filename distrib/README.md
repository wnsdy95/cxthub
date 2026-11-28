# cxt — Git for coding-agent sessions

**Commits keep the code, but the AI conversation that produced it disappears.** cxt keeps
each coding-agent session (Claude Code · Codex) with the Git commits it produced. It reads
what the tools recorded on your computer, and Git, and uploads the result to your team's
server — you keep using `commit` and `push` as before.

- `git commit` → the session that made the commit is cut at it; that part is linked to the commit, with a summary of the session
- `git push` → once the commit is on GitHub, the part of the session that explains it opens to whoever may view the repository
- `git checkout <branch>` → a session on the branch is given the most recent summary another session wrote along it
- `cxt fork` → make a branch at a commit, or continue a session's conversation up to that commit as a new session
- Web UI to browse and search the conversation behind every commit

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/wnsdy95/cxthub/main/distrib/install | sh
```

Supports macOS (arm64/amd64) and Linux (arm64/amd64).
Pin a version by passing the variable to the installer shell:

```bash
curl -fsSL https://raw.githubusercontent.com/wnsdy95/cxthub/main/distrib/install |
  CXT_VERSION=v0.1.0 sh
```

The release installer installs the `cxt` client only. See the full
[installation guide](../docs/INSTALLATION.md) for manual verification, source
builds, upgrades, removal, and `cxtd`.

## Getting started

**Once per team:** create a repository on the web (e.g. `https://cxthub.com/alice/myteam`).
The repository URL is itself the repo — it registers automatically on first connect (no separate repo to name).

```bash
cd <your code repo>
cxt setup https://cxthub.com/<owner>/<repository>
#         └── your repository URL (this URL is the repo identity)
# ✓ .cxt → login (browser approval) → repository connection → git hooks → agent hooks → team settings
# From here on, just use git. Check with: cxt doctor
```

Codex CLI users: run `codex` once and approve the cxt entries under `/hooks` (one-time trust prompt).

## Releases

Source and verified cxt CLI release assets are published from the same cxthub repository.

## License

[Apache-2.0](LICENSE)
