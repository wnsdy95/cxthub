# Installing CXTHub

This guide covers the `cxt` client, source builds, upgrades, removal, and the
separate `cxtd` server.

> [!WARNING]
> CXTHub is alpha software. Back up important data, and know who can read a
> repository before you connect it: a connected repository uploads the
> conversations of your coding-agent sessions.

## Choose what to install

| Goal | Recommended path |
|---|---|
| Use CXTHub in a Git repository | Install the published `cxt` CLI |
| Contribute to CXTHub | Build the repository from source |
| Run a local development server | Build `cxtd` from source |
| Operate an internet-facing server | Review the source deployment templates and the operator requirements in [SECURITY.md](../SECURITY.md) |

The published release installer installs **only the `cxt` client**. It does not
install `cxtd`, the web application, PostgreSQL, or cloud infrastructure.

## Requirements

Published `cxt` binaries support:

- macOS on arm64 or amd64;
- Linux on arm64 or amd64;
- Git 2.28 or newer;
- at least one supported coding agent, Claude Code or Codex, whose session
  records `cxt` reads; and
- the `sqlite3` program, when Codex is used: `cxt` reads Codex's thread
  databases through it. Without it, Codex threads are listed as unread.

The installer also requires `curl`, `tar`, and either `sha256sum` or `shasum`.
Native Windows archives are not currently published.

Source builds additionally require:

- Go 1.26.11 or newer for `cxt` and `cxtd`;
- Node.js 22 and npm for the web application; and
- PostgreSQL 16 for PostgreSQL adapter and migration testing.

PostgreSQL API, MCP, and worker processes verify the schema at startup; they do
not create or upgrade it. Prepare it with the separate operator command before
serving, and keep migration credentials out of runtime services. Follow
[database migration ownership](DATABASE_MIGRATIONS.md) for the coordinated
procedure. Filesystem-only local development does not need a SQL migration.

## Install the latest release

The installer detects the operating system and architecture, downloads the
matching GitHub Release archive, verifies it against `checksums.txt`, and
installs `cxt`:

```bash
curl -fsSL https://raw.githubusercontent.com/wnsdy95/cxthub/main/distrib/install | sh
```

The destination is selected in this order:

1. `CXT_INSTALL_DIR`, when set;
2. `/usr/local/bin`, when writable; or
3. `$HOME/.local/bin`.

When the selected directory is not already in `PATH`, the installer adds a
marked entry to the appropriate shell startup file.

Verify the installation:

```bash
cxt --version
cxt --help
```

The version should match a published
[CXTHub release](https://github.com/wnsdy95/cxthub/releases).

## Install a specific version

Pass `CXT_VERSION` to the shell that executes the downloaded installer:

```bash
curl -fsSL https://raw.githubusercontent.com/wnsdy95/cxthub/main/distrib/install |
  CXT_VERSION=v0.1.0 sh
```

To install into a specific directory:

```bash
curl -fsSL https://raw.githubusercontent.com/wnsdy95/cxthub/main/distrib/install |
  CXT_VERSION=v0.1.0 CXT_INSTALL_DIR="$HOME/.local/bin" sh
```

`CXT_VERSION` must include the leading `v`. `CXT_REPO=<owner>/<repository>` may
be used when testing a compatible fork with the same release asset layout.

## Manual archive installation

If running a remote script is not acceptable, download the archive and
checksum file from the GitHub Release page. Choose one of:

```text
cxt_darwin_arm64.tar.gz
cxt_darwin_amd64.tar.gz
cxt_linux_arm64.tar.gz
cxt_linux_amd64.tar.gz
```

For example:

```bash
CXT_VERSION=v0.1.0
CXT_ASSET=cxt_darwin_arm64.tar.gz
CXT_RELEASE_URL="https://github.com/wnsdy95/cxthub/releases/download/$CXT_VERSION"
CXT_TEMP_DIR="$(mktemp -d)"

curl -fsSL -o "$CXT_TEMP_DIR/$CXT_ASSET" "$CXT_RELEASE_URL/$CXT_ASSET"
curl -fsSL -o "$CXT_TEMP_DIR/checksums.txt" "$CXT_RELEASE_URL/checksums.txt"
```

Verify the archive on macOS:

```bash
cd "$CXT_TEMP_DIR"
grep " $CXT_ASSET\$" checksums.txt | shasum -a 256 -c -
```

Or on Linux:

```bash
cd "$CXT_TEMP_DIR"
grep " $CXT_ASSET\$" checksums.txt | sha256sum -c -
```

Only continue after verification succeeds:

```bash
tar -xzf "$CXT_ASSET"
mkdir -p "$HOME/.local/bin"
install -m 0755 cxt "$HOME/.local/bin/cxt"
"$HOME/.local/bin/cxt" --version
```

Add `$HOME/.local/bin` to `PATH` if needed. The checksum protects against
corruption or an unexpected archive; it is distributed from the same GitHub
Release as the archive and is not a separate publisher signature.

## Set up a repository

Run `cxt` inside an existing Git repository. A User or Organization directly owns each
Repository. Use the canonical address shown in the web UI:

```bash
cd /path/to/code-repository
cxt setup https://<host>/<owner>/<repository>
```

`cxt setup` is idempotent and performs:

1. making the repository's `.cxt` folder and its mark (`.cxt/HEAD`) when they
   are absent;
2. browser-based login and verification of the repository address;
3. registration of the connection with the server;
4. managed Git hook installation;
5. Claude Code and Codex hook registration when available; and
6. team settings pull when authenticated.

It preserves existing provider hooks while adding the CXTHub entries. Claude
Code settings are written under the repository's `.claude/` directory. Codex
hooks are merged into `~/.codex/hooks.json`; Codex may require a one-time `/hooks`
approval.

Setup uploads nothing itself, and it neither starts an agent nor adds anything
to a conversation. An existing `origin` that points to another repository is
kept, and setup stops instead of replacing it. See the
[setup reference](CLI.md#cxt-setup) for each step.

### What happens after setup

`cxt` reads the session records Claude Code and Codex keep on your computer,
and Git. It cuts each session at the commits it made and uploads one report
per session, with the session's conversation after masking. Hooks only wake
that reading:

- the five managed Git hooks (`post-commit`, `post-checkout`, `post-merge`,
  `post-rewrite`, `pre-push`); and
- the agent hooks at session start, prompt, stop and session end.

A hook is never what is read, so a hook that did not run is made up for by the
next one that does. The automatic upload covers sessions recorded since it
first ran in the repository. Run `cxt push` once to upload earlier sessions,
and `cxt doctor` at any time to see what does not fit. Turn the automatic
upload off with `cxt config segments.auto off`; see
[automatic upload](CLI.md#automatic-upload).

On the server a session is read by the account that uploaded it. A part of it
opens to whoever may view the repository once the commit it explains is
pushed. The repository page in the web opens on the **Sessions** tab.

The Codex hook registration is global, but `cxt` acts only where `cxt init` or
`cxt setup` wrote the `.cxt/HEAD` mark. In any other Git repository the hook
does nothing and does not create `.cxt`. If a `.cxt` directory exists without
that mark, the next hook leaves its contents as they are and adds `.cxt/` and
`.cxtsecrets` to `.gitignore` and `.git/info/exclude`.

The agent hooks apply to both provider CLIs and supported app surfaces:

- Codex app and Codex IDE sessions that run Codex lifecycle hooks;
- Claude Desktop's **Code** tab, which runs the Claude Code engine; and
- ordinary Claude Code and Codex terminal sessions.

Start Claude Code and Codex the way you normally do; nothing has to be launched
through `cxt`. Linked worktrees use the one `.cxt` folder of the primary
repository, while each worktree keeps its own Git branch.

At session start and at a prompt, a hook gives the session the most recent
summary another session wrote along the branch its folder is on, once for each
branch the session comes to. The text is at most 16 KiB. No conversation is
copied into the session.

Claude Desktop's general **Chat** tab does not run Claude Code lifecycle hooks
and is outside this integration.

To let the Codex app or the Claude app read sessions that are on the server,
use the default product connector at `https://cxthub.com/mcp` (or the
corresponding self-hosted origin). This is an OAuth-protected Streamable HTTP
service; it is separate from the hooks and from the `cxt` CLI. See
[MCP connections](MCP.md) for setup, permissions, and deployment boundaries.

To set up a repository without a server:

```bash
cxt init
```

Nothing is uploaded without a server. `cxt segments` still shows the sessions
of the folder cut at their commits, from this computer alone.

To skip browser login during setup (requires an existing credential or
`CXT_TOKEN` for private repositories):

```bash
cxt setup https://<host>/<owner>/<repository> --no-login
```

See the [CLI reference](CLI.md) for manual setup and all commands.

## Update or downgrade

Rerun the installer without `CXT_VERSION` to install the latest stable release:

```bash
curl -fsSL https://raw.githubusercontent.com/wnsdy95/cxthub/main/distrib/install | sh
```

Rerun it with an explicit `CXT_VERSION` to upgrade, reinstall, or downgrade to
a published version. The installer replaces only the `cxt` executable at the
selected destination. It does not delete repository data, authentication
files, or provider configuration.

After an update:

```bash
cxt --version
cxt --help
```

Review the release notes before moving between versions, especially while the
project is pre-1.0.

## Remove the client

First locate the executable:

```bash
command -v cxt
```

Remove that exact file after inspecting the path. Common installer destinations
are:

```text
/usr/local/bin/cxt
$HOME/.local/bin/cxt
```

If the installer added a shell startup entry, remove the line ending with:

```text
# added by cxt install
```

Client removal does not remove:

- `.cxt/` folders inside repositories, including the copies of what was
  uploaded under `.cxt/sessions`;
- what the automatic upload keeps beside Git's own files (`<git-dir>/cxt/`)
  and in your user cache directory (`cxthub/session-records-v1`);
- `.cxtsecrets`;
- `~/.cxt/` credentials;
- managed Git hooks;
- the `cxt hook` entries in `.claude/settings.json` and `~/.codex/hooks.json`; or
- sessions already uploaded to a server.

Before uninstalling, run `cxt hooks uninstall` inside each repository where the
managed Git hooks should be removed, and remove the `cxt hook` entries from the
agent settings by hand. To take a session off the server, run
`cxt remove <tool> <session> --yes` before uninstalling. Delete local data only
after reviewing it.

## Build from source

Clone the public repository:

```bash
git clone https://github.com/wnsdy95/cxthub.git
cd cxthub
make build
```

This creates:

```text
bin/cxt
bin/cxtd
```

Verify the binaries:

```bash
./bin/cxt --version
./bin/cxt --help
```

Development builds report `cxt dev` unless a version is injected at link time.

To install both Go binaries into `$(go env GOPATH)/bin`:

```bash
make install
```

To build the web application:

```bash
cd frontend/web
npm ci
npm run build
```

Contributor verification commands are documented in
[CONTRIBUTING.md](../CONTRIBUTING.md).

## Run `cxtd` locally

The release installer does not publish or install `cxtd`. Build it from source,
then bind development authentication only to loopback:

```bash
./bin/cxtd serve --addr 127.0.0.1:8907 --data ./cxt-data
```

Connect a test Git repository with a repository-shaped URL:

```bash
cxt setup http://127.0.0.1:8907/<owner>/<repository> --no-login
```

Development authentication and the filesystem store are for trusted local
testing. `cxtd` refuses an externally bound development-auth server unless the
operator explicitly opts in. Internet-facing installations require production
authentication, TLS, PostgreSQL, backups, rate limits, monitoring, and managed
secrets. See [SECURITY.md](../SECURITY.md).

## Non-interactive environments

The CLI recognizes these environment variables:

| Variable | Purpose |
|---|---|
| `CXT_REMOTE` | API base fallback when no repository `origin` is configured |
| `CXT_TOKEN` | Non-interactive authentication token; takes precedence over stored credentials |
| `CXT_NAME`, `CXT_EMAIL`, `CXT_TEAM` | Identity overrides sent with requests |
| `CXT_NO_BROWSER=1` | Do not attempt to open a browser during device login |
| `CXT_SECRETS_PASSPHRASE` | Passphrase fallback for `cxt secrets push|pull` |

Do not print tokens or passphrases in CI logs. Prefer a secret manager and the
smallest repository-scoped permissions available.

## Troubleshooting

### `cxt: command not found`

Open a new shell after installation, source the startup file named by the
installer, or add the installation directory to `PATH`.

### Repository URL rejected

Use an HTTP or HTTPS URL with owner and repository segments:

```text
https://<host>/<owner>/<repository>
```

The server resolves this display address before the CLI stores its connection.
Existing two- and three-segment connections keep their original repository IDs.
Three-segment addresses only resolve when registered as exact migration aliases;
arbitrary extra path segments are rejected. Credentials, query strings,
fragments, and one-segment paths are rejected. New connections require a
reachable server and sufficient access; existing remotes are not rewritten.

### Sessions do not appear on the server

Run `cxt doctor` inside the repository. It reads this computer and the server,
changes nothing, and gives for each finding the command that puts it right:
for example `cxt setup` when the repository is not connected, `cxt login` when
the login is missing, or `cxt push` when the automatic upload has not run yet.
A session belongs to the repository when a working folder its records name is
inside one of the repository's worktrees.

### Login cannot open a browser

Run with `CXT_NO_BROWSER=1` to print the approval URL, or use the manual token
fallback:

```bash
cxt login <token>
```

Prefer the browser device flow because a token supplied as a command argument
may remain in shell history.

### `cxtd` refuses to bind

Development authentication is intentionally restricted to loopback. Use
`127.0.0.1` for local testing or configure production authentication before
binding to an external interface.

## Upgrading the ownership model

Existing installations must follow the coordinated backup and migration steps in
[Repository ownership upgrade](REPOSITORY_ORGANIZATIONS.md#upgrade-and-recovery).
The migration preserves content repository IDs and existing CLI remotes while
flattening old containers into independently owned repositories.

## Invitation email with Resend

The backend reads `.env` from its working directory on `cxtd serve` startup.
For the repository's local daemon, this is the repository root:

```sh
cp .env.example .env
chmod 600 .env
```

Set `RESEND_API_KEY` in that file, then restart `cxtd` (for local dogfooding,
`bash scripts/dogfood-daemon.sh restart`). With no key, inbox and link invitations
continue to work. The key belongs only to the backend: never use `VITE_` or put
it in a frontend environment file.

The default sender is a `noreply` address at `cxthub.com`. Its domain must first be
[verified in Resend](https://resend.com/docs/dashboard/domains/introduction).
If your account uses another verified domain, set `RESEND_FROM`. `CXT_WEB_URL`
can override the invitation link origin: its default is `http://localhost:5173`
for a loopback backend and `CXT_PUBLIC_URL` for a hosted backend. A custom
frontend port or domain needs that override. Hosted links require HTTPS.

An explicit `CXT_ENV_FILE` selects another file; a missing explicit file is an
error. A missing default `.env` is allowed. Existing process variables take
precedence, including an empty key used to disable delivery. The parser supports
literal `KEY=value`, optional `export`, comments, and single/double quotes;
it does not execute shell commands or expand variables. Configuration changes
require a restart. Real `.env` files are ignored by Git.

Only newly created or explicitly renewed Organization/Enterprise invitations
queue mail. Enabling a key never mails the historical inbox. Repository share
links remain share links. Check status in the space's invitation list, using
Refresh when needed. `Resend accepted` means the provider acknowledged the
request, not that the recipient received or opened it. Wrong credentials or an
unverified sender require fixing configuration and renewing the invitation.
