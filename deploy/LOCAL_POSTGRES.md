# Independent local PostgreSQL services

The development runtime follows the deployment boundary: separate `cxtd` API
and `cxt-mcp` processes share PostgreSQL. The CLI's `.cxt` remains a working
replica. This procedure does not provision Render, Vercel or any cloud database.

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

## Transfer and acceptance

Follow [FS transfer](FS_TRANSFER.md) against a **fresh** frozen source. Stop the
legacy `com.cxthub.cxtd` launch agent and wait for its process to exit before
cloning its data directory. Do not stop provider sessions or remove local CLI
queues. Network sync may fail during maintenance; locally retained work is
retried after the API returns.

Preserve permissions/mtime and independently compare a size/SHA-256 inventory of
every original and copied file. Run the fail-closed offline importer against a
new empty local database. Keep its source hash/count report, dump the accepted
database and restore into another empty database. Compare authority, context,
refs, history, memory and job records before accepting the result. Never remove
unsupported source categories to make an import pass. Ordinary server startup
can run copied jobs; perform the offline validation before starting API workers.

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
frozen source. Never start the old FS service and PostgreSQL API on the same
port. Keep the original source and its independent backup through acceptance.

After PostgreSQL accepts new writes, simply restarting the old FS copy would
omit those records. Freeze and preserve PostgreSQL and the local sync queues,
then plan a verified reconciliation or reverse transfer. No automatic fallback
to an older FS dataset is provided. A healthy HTTP endpoint alone is not proof
of a complete transfer, and a local rehearsal is not cloud load certification.
