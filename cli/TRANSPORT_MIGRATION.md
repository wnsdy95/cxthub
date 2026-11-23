# Moving a CLI transport without changing repository identity

When the server has restored the **same existing repository rows** at another
address, authenticate to the destination and explicitly bind its API endpoint:

```sh
cxt login --server https://api.cxthub.io
cxt remote set-endpoint origin https://api.cxthub.io/api/v1
cxt remote -v
```

Keep the original repository URL as the remote, for example
`http://localhost:5173/acme/project`. Do not remove it and add the public URL
instead: the hash of the normalized URL is the immutable RepoID under which the
repository's sessions are stored. `set-endpoint` rewrites nothing on this
computer besides the configuration, and it creates no repository on the
server.

## Verification

- Endpoints must use HTTPS, or HTTP with literal loopback/localhost, and exactly
  `/api/v1` (one trailing slash is accepted). Userinfo, queries, fragments,
  escaped paths and invalid ports are rejected. Redirects are not followed.
- The command requires a destination-host token saved by `cxt login --server`.
  It does not use `CXT_TOKEN` for this explicit operation, and it verifies the
  login with `/me`.
- The destination must resolve the **old URL**, then return an existing
  `/repos/<old-repo-id>` row with that exact URL, RepoID and the same nonempty
  server `RepositoryID`. A missing row, an alias to a new identity, or a
  mismatch fails without changing the configuration. The first binding pins
  the server RepositoryID from these two authorized reads; a later one must
  keep the pinned RepositoryID. This trusts the explicitly selected
  authenticated server, not proof from the unavailable old server.
- The endpoint binding is stored separately in the shared `.cxt/config`, keyed
  by the immutable URL.
- A concurrent change of the configuration rejects the update. Run the command
  again to get a fresh verification.
- An active `capture.identity` root preference requires a fresh confirmation of
  the root-publication capability on the destination before the endpoint and
  the preference are updated together. Failure preserves the old preference
  and endpoint.

## What uses the endpoint

Login and logout, setup, repository registration and the session uploads read
the saved endpoint for the repository's URL. `cxt remote -v` prints it with the
verified repository. After a move to another host, use the login saved for
that host.

Changing the endpoint, moving the server's data, installing the CLI and
deploying services are separate operations, not effects of this command.

## Device login across API and frontend hosts

Device pairing accepts the server's optional `verification_uri_complete`, such
as `https://cxthub.io/login/device?code=...`. It must contain only the emitted
pairing code at `/login/device`, without userinfo or fragments, over HTTPS (or
loopback HTTP only when the selected API is also loopback). A server that sends
none makes the CLI use the selected API host. Polling, token verification and
credential storage always stay on the explicitly selected API endpoint, never
the browser host.

## Validation

`TestEndpoint*` tests in `cli/cmd/cxt`, `cli/internal/adapters/remotecfg` and
`cli/internal/adapters/delivery/cli` cover this command.
