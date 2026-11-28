# Independent local PostgreSQL services

The development runtime follows the deployment boundary: separate `cxtd` API
and `cxt-mcp` processes share PostgreSQL. A repository's `.cxt` folder holds the
CLI's connection settings, the mark that cxt may act there, and copies of what
was uploaded; it is not a source of sessions. This procedure does not provision
Render, Vercel or any cloud database.

## Prepare persistent state

Use a private persistent directory outside the checkout and temporary directories.
Run PostgreSQL 16 on loopback, with password authentication and private credential
files. Keep its data directory and dumps through server or CLI upgrades. On
macOS, set `LC_ALL=C` and `LANG=C` in the PostgreSQL launch agent: an inherited
launchd locale can otherwise prevent Homebrew PostgreSQL from starting. Do not
print the DSN or put its password in command arguments.

Build both server binaries with the `postgres` tag. Keep the old FS server binary
and launch-agent configuration in a private rollback directory before replacing
anything. `scripts/dogfood-daemon.sh` manages the legacy FS daemon only; do not
use it to restart the new independent services.

## An existing FS dataset is not transferred

There is no tool that moves the data of a local FS store into PostgreSQL. The
offline transfer command carried captures, their refs and their history, and was
removed together with captures. Sessions are uploaded again by the CLI: after
the new services run, `cxt segments push` on each computer sends what that
computer holds. Keep the old data directory until that is done.

## Configure launchd

`scripts/local-services.py` manages only `com.cxthub.api` and `com.cxthub.mcp`.
It does not stop the legacy FS daemon, migrate data, create a database, alter
PostgreSQL, or delete any data. Run it from the verified checkout. Its private
JSON configuration must be owned by the operator with mode 0600:

```json
{
  "root": "/absolute/path/to/cxthub",
  "runtime": "/absolute/private/runtime",
  "api_binary": "/absolute/path/to/cxtd",
  "mcp_binary": "/absolute/path/to/cxt-mcp",
  "environment": {
    "CXT_POSTGRES_DSN": "postgres://USER:PASSWORD@127.0.0.1:PORT/DATABASE?sslmode=disable",
    "CXT_AUTH": "firebase",
    "CXT_FIREBASE_PROJECT": "YOUR_EXISTING_PROJECT",
    "CXT_PUBLIC_URL": "http://localhost:5173",
    "CXT_COOKIE_SECURE": "0",
    "CXT_CORS_ORIGINS": "http://localhost:5173,http://127.0.0.1:5173",
    "CXT_ENV_FILE": "/absolute/private/api.env"
  }
}
```

The HTTP/secure-cookie exception is loopback development only; cloud deployment
continues to require HTTPS and secure cookies. Preserve the existing Firebase
project, API credentials and permitted frontend origin. The API env file is
optional and defaults to `/dev/null`; existing values can be retained there.
MCP receives only database and identity settings and never loads the API env file
or its GitHub/Resend credentials. Migration paths are taken from the checkout.

```bash
python3 scripts/local-services.py check --config /private/services.json
python3 scripts/local-services.py configure --config /private/services.json
python3 scripts/local-services.py start --config /private/services.json
python3 scripts/local-services.py status --config /private/services.json
```

`check` performs no mutation. `configure` writes private launch agents without
starting them. `start` requires both loopback ports 8907/8908 to be free and
configuration to match; it starts separate services and requires both readiness
checks. A startup failure unloads only newly started agents. `status` reports
launch-agent presence, not database or query integrity. Check `/healthz`, then
exercise authenticated web/API and MCP/OAuth requests. Vite routes API and MCP
to their respective ports and maintains one browser origin.

`stop` unloads only these two services. It preserves PostgreSQL, configuration,
backups and live provider sessions. Stop before reconfiguring binaries/settings.

## Rollback boundary

Before the new services accept writes, rollback can unload the independent
services and restart the preserved old FS binary/config against the original
data directory. Never start the old FS service and the PostgreSQL API on the
same port.

After PostgreSQL accepts new writes, restarting the old FS copy would omit those
records. No automatic fallback to an older FS dataset is provided. A healthy HTTP
endpoint alone is not proof that every session was uploaded again.
