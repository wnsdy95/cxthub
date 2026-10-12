# cxt CLI

The `cxt` command-line client for portable coding-agent context.

This public repository, `wnsdy95/cxthub-cli`, is a **generated distribution** of
the public [CXTHub monorepo](https://github.com/wnsdy95/cxthub). That monorepo is
the only source of truth. Each export is an allowlisted snapshot of one exact
source commit, without source Git history. `.cxthub-source.json` records its
source commit and per-file provenance. Do not edit this mirror or open
implementation pull requests here: changes, reviews, and fixes belong in the
[monorepo contribution flow](https://github.com/wnsdy95/cxthub/blob/main/CONTRIBUTING.md).

## Build from source first

Use the Go version declared in `go.mod` (Go 1.26.9 or later), Git, and Python 3
for the batched tests. The module is `github.com/wnsdy95/cxthub-cli` at this
repository's root; the executable is named `cxt`.

```bash
git clone https://github.com/wnsdy95/cxthub-cli.git
cd cxthub-cli
go build -o bin/cxt ./cmd/cxt
./bin/cxt --help
```

For a reproducible build, check out the exported commit recorded in the verified
release set before building. This README does not assert that binary releases
already exist in this repository. No installer is exported here for bootstrap;
use the source build until a target release is explicitly published. The
existing monorepo installer, download URLs, and release workflow remain
unchanged for compatibility; see the
[existing installation instructions](https://github.com/wnsdy95/cxthub/blob/main/distrib/README.md).

## Checks

Run from this repository's root:

```bash
go build ./...
go vet ./...
go test $(go list ./... | grep -v '/internal/adapters/delivery/cli$')
python3 scripts/test-go-batches.py ./internal/adapters/delivery/cli/
```

CI runs ordinary tests for the other CLI packages and bounded serial batches
for the delivery/CLI package. The export includes `docs/CLI.md`,
`integrations/**` including tracked dotfiles, `schemas/cir.schema.json`,
`schemas/testdata/**`, and `scripts/test-go-batches.py` at their root-relative
paths. The CLI reference and integration assets are required by help and
integration tests; they are not optional packaging extras.

The export does not include the backend or frontend. Cross-module parity checks
and full-stack integration tests run in the monorepo, not this CI. Successful
build/vet or focused tests are not a full-suite pass; all required CI must still
complete successfully. A green mirror build does not replace monorepo checks
or authorize a deployment.

## Releases and ownership

`goreleaser.yaml` and the manually dispatched release workflow are preparation
for future releases. Maintainers must first verify the complete four-repository
release set and green CI, then cut a new immutable `vX.Y.Z` tag from exported
`main`. Dispatch the release workflow from `main` with that tag. It rejects tags
outside exported `main` and existing releases. Never move or replace a published
tag; publish a new patch version instead. No cloud deployment runs here.

See the source [distribution runbook](https://github.com/wnsdy95/cxthub/blob/main/docs/REPOSITORY_DISTRIBUTION.md)
for provenance, synchronization, and release-set verification. Shared `LICENSE`,
`NOTICE`, and third-party notices are exported from the monorepo.
