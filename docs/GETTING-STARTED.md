# cxthub — Development Setup

> Development and operations guide. See [`ARCHITECTURE.md`](./ARCHITECTURE.md) for the system overview and the root [`README.md`](../README.md) for the user quick start.

---

## 0. Prerequisites

| Tool | Minimum Version | Purpose |
|---|---|---|
| Go | 1.26 | `cxt`(CLI) · `cxtd`(Server) build |
| Git | 2.28+ | Repository identity and optional automatic hooks, including `reference-transaction` |
| Node.js | 22 | Web UI(`frontend/web`) build |
| PostgreSQL | 16+ | Optional for development; required for the production `cxtd` adapter |
| Firebase project | — | Optional; local development can use explicit dev authentication |

---

## 1. Installation

**Team Member (CLI only)** — Release binary source:

```bash
curl -fsSL https://raw.githubusercontent.com/wnsdy95/cxthub/main/distrib/install | sh
# macOS/Linux × arm64/amd64. Specific version: CXT_VERSION=v0.1.0
# Release procedure (maintainer): git tag vX.Y.Z && git push --tags → Publish to current repo Release
```

**Development (Source Build — Server Administrator/Contributor)**:

```bash
git clone https://github.com/wnsdy95/cxthub && cd cxthub

make build          # → bin/cxt, bin/cxtd
make install        # Install into $(go env GOPATH)/bin
make test           # Run two Go module unit tests
make typecheck      # Web UI + Core TS strict type check
```

`cxt` to PATH (if GOPATH/bin is not in PATH):

```bash
ln -sf "$(go env GOPATH)/bin/cxt"  /opt/homebrew/bin/cxt
ln -sf "$(go env GOPATH)/bin/cxtd" /opt/homebrew/bin/cxtd
```

---

## 2. Server Startup (`cxtd`)

```bash
# dev authentication (local development default) — "dev:<email>:<name>" token is trusted as-is
cxtd serve --addr 127.0.0.1:8907 --data ./cxt-data
# or: make dev-server

# Firebase authentication (production) — ID token (RS256) validation using Google public key
CXT_AUTH=firebase CXT_FIREBASE_PROJECT=<firebase-project-id> \
  cxtd serve --addr 127.0.0.1:8907 --data ./cxt-data

# PostgreSQL store (default is file store) — auto apply schemas/db/migrations on startup
# (schema_migrations history-based idempotency. Location custom: CXT_MIGRATIONS_DIR)
cd backend && go build -tags postgres ./cmd/cxtd
CXT_POSTGRES_DSN='postgres://…' cxtd serve …
```

Main Environment Variables:

| Variable | Meaning |
|---|---|
| `CXT_AUTH=firebase` + `CXT_FIREBASE_PROJECT` | Firebase ID token validation mode (dev validator if not set) |
| `CXT_POSTGRES_DSN` | Use PG store in postgres tagged builds |
| `CXT_COOKIE_SECURE` / `CXT_COOKIE_SAMESITE` / `CXT_COOKIE_DOMAIN` | Session cookie attributes (per deployment topology) |
| `CXT_CORS_ORIGINS` | Allowed Origin whitelist (comma-separated, only loopback Origin if empty) |

DB Migration (PG): Automatically applies `schemas/db/migrations/` in order at startup (idempotent).

### Continuous Integration (macOS launchd)

Instead of running in the shell background, using launchd allows it to start automatically on reboot and login, and automatically restarts on crashes:

```bash
scripts/dogfood-daemon.sh install    # create plist + start (dev auth, 127.0.0.1:8907)
scripts/dogfood-daemon.sh status     # status + health check
scripts/dogfood-daemon.sh restart    # go install to rebuild binary and apply
scripts/dogfood-daemon.sh logs       # log tail (.cxt-daemon/)
scripts/dogfood-daemon.sh uninstall  # stop + remove
```

For Firebase authentication, install with:

```bash
CXT_AUTH=firebase CXT_FIREBASE_PROJECT=<project-id> scripts/dogfood-daemon.sh install
```

Binary update flow: `cd backend && go install ./cmd/cxtd` → `scripts/dogfood-daemon.sh restart`.
Logs and data are stored in the repo root (`.cxt-daemon/`, `cxt-data/`) and are excluded from git.

---

## 3. Start Web UI (`frontend/web`)

```bash
cd frontend/web
cp .env.example .env        # Firebase requires VITE_FIREBASE_* (empty for dev login mode)
npm install
npm run dev                 # http://localhost:5173 — /api proxied to 8907
```

- dev uses vite proxy for **same-origin** communication → HttpOnly session cookie (`cxt_session`) works as first-party
- For deployment, SPA rewrite rules are needed (all paths → index.html, exclude `/api/*`)
- URL structure: `/<username>/<workspace-slug>[/members]`, invite `/invite/<token>`

---

## 4. Connect to Code Repo — Git Native Workflow

cxthub's core: **configuration once, then use git only.**

Prerequisites (per team): Create a workspace on the web first (e.g., `http://<server>/alice/myteam`).
1 workspace = 1 repo — The workspace URL is the repo identity, and it is automatically registered with the server during the first connection (setup/push) — no separate repo name is appended to the URL. If the URL `<username>/<workspace-slug>` does not match the actual workspace, it will not be visible on the web — `cxt setup` will issue a warning in this case.

```bash
cd <code repo>
cxt setup http://<server>/<username>/<workspace-slug>
#   ✓ .cxt store initialization (.git side, git status auto-exclude)
#   ✓ install 6 git hooks (commit·checkout·merge·push·ref·rewrite — fail-open, chain existing hooks)
#   ✓ register remote origin (the URL determines both server address and shared repository identity)
#   ✓ login — device flow (browser approval, token copy unnecessary. --no-login to omit)
#   ✓ agent hooks: claude(.claude/settings.json — team propagation on git commit) · codex(~/.codex merge)
#   ⚠ codex user runs once: codex run → /hooks → cxt item approval
#   ✓ Team basic settings pull (.claude/.agents/.codex)
# Idempotence: Re-running it will only fill in missing steps, leaving individual commands intact.
#   cxt init / cxt hooks install / cxt remote add origin <url> / cxt login / cxt settings pull
# Login details: 5 roles (viewer/puller/member/maintainer/owner), manual fallback cxt login <token>,
# CI is CXT_TOKEN. ~/.cxt/auth.json(0600, per host) stored — revoke: cxt logout
```

Subsequent automatic synchronization of git actions:

| git | cxt auto-response |
|---|---|
| `commit` | Active agent session snapshot (commit message + `[git <sha>]` link) |
| `checkout`/`switch` | Restore context of the given branch (fork if new branch) |
| `branch X` (no switch) | Ref fork at commit X points to and chained context |
| `push` / `pull` | Context push/pull (non-FF rejected like git, `--force` exception) |
| `reset --hard` etc. ref move | Return to HEAD commit and chained context |
| `rebase` / `commit --amend` | Maintain link to old→new mapping record (`.cxt/rewrites.json`) |
| `stash` / `stash pop` | Save active session → revert to / restore head context |

Hooks are all fail-open (cxt failing does not block git) and existing user hooks are preserved through chaining.
Exceptions: `cxt init --no-hooks`, revoke: `cxt hooks uninstall`.

**Secret Masking**: Place secret values in the repo root `.cxtsecrets` one per line, and the local CLI replaces them with `{this is deleted by security policy}` before saving (deterministic, performed before saving → no plaintext on server).
`cxt init` extracts values from `.env` and automatically generates them (`no .env` means no generation),
automatically registering `.cxt/` and `.cxtsecrets` in `.gitignore`. Team sharing is via end-to-end encrypted envelopes:
Web About ⚙ "Encrypted Storage" → `cxt secrets pull -p <team passphrase> --remember`
(Passphrase is not stored on the server — the server cannot see plaintext).

### Agent (MCP/Hook) Integration — integrations/

- **Claude Code**: `integrations/claude-code/` — Plugin Manifest + `.mcp.json`(`cxt mcp` stdio) + Slash Command(`/cxt-*`) + Session Hook(`hooks/hooks.json`)
- **Codex CLI**: Merge `integrations/codex/config.snippet.toml` into `~/.codex/config.toml` + `hooks.json`

---

## 5. Directory Structure (Current)

```
cxthub/
├── Makefile · README.md · .gitignore
├── cli/                      # module github.com/wnsdy95/cxthub/cli  → binary cxt
│   ├── cmd/cxt/              # composition root (mcp/hook/CLI branching)
│   └── internal/
│       ├── domain/ ports/{inbound,outbound}/ app/
│       └── adapters/         # storage(.cxt) · codec(CIR) · capture · session · memory
│                             # · gitctx (.git source of truth) · remotecfg (origin/config)
│                             # · githooks(hook installation) · backendclient(REST) · delivery/{cli,mcp,hook}
├── backend/                  # module github.com/wnsdy95/cxthub/backend → binary cxtd
│   ├── cmd/cxtd/
│   └── internal/
│       ├── domain/ ports/ app/          # synchronization Service + IdentityService
│       └── adapters/                    # auth(firebase/dev) · store(FS/PG) · delivery/http · gitengine
├── frontend/
│   ├── src/                  # clean layered core (contract stubs, framework independent)
│   └── web/                  # React+Vite web UI (cxthub website)
├── integrations/{claude-code,codex}/
├── schemas/                  # cir.schema.json · manifest.schema.json · openapi.yaml
│   └── db/migrations/        # 0001_init … 0004_slugs
└── docs/
```

Local `.cxt/` layout (per code repo, next to `.git`):

```
.cxt/
├── objects/  refs/{heads,tags}/  HEAD  manifest.json   # content-addressed store (like .git)
├── config          # remotes(origin URL) · checkout.mode · add staging
├── stash.json      # stash stack (local only)
└── rewrites.json   # rebase/amend old→new commit mapping ([git <sha>] link interpretation)
```

---

## 6. Summary of Common Commands

```bash
cxt                       # Full usage
cxt list                  # Snapshot log (= cxt log)
cxt checkout <ref> [-b]   # Context recovery/branching → claude --resume <id> instructions
cxt stash / pop / list    # Active session storage/recovery
cxt tag <name> [ref]      # Immutable tag
cxt push --force          # Non-FF force (overwrites remote history — same caution as git)
cxt config checkout.mode prepare   # Automatic recovery instead of instructions on branch switch
```

---

## 7. Further Reading

- [`ARCHITECTURE.md`](./ARCHITECTURE.md) — System Overview
- [`CLI-ARCHITECTURE.md`](./CLI-ARCHITECTURE.md) — Detailed Internal (Hexagonal Architecture) of cxt
- [`BACKEND-ARCHITECTURE.md`](./BACKEND-ARCHITECTURE.md) — Detailed cxtd (Authentication, Workspace, Synchronization)
- [`FRONTEND-ARCHITECTURE.md`](./FRONTEND-ARCHITECTURE.md) — Web UI + Core Details
- [`SYNC-PROTOCOL.md`](./SYNC-PROTOCOL.md) — Push/Pull · Ref Policy (fast-forward only)
- [`OPEN-SOURCE-RELEASE.md`](./OPEN-SOURCE-RELEASE.md) — Procedures for Migrating and Verifying New Public Repository
