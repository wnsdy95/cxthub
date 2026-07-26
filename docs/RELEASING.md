# Releasing cxthub

cxthub uses Semantic Versioning tags:

```text
vMAJOR.MINOR.PATCH
vMAJOR.MINOR.PATCH-rc.N
```

The project is currently pre-1.0. Minor versions may introduce substantial
changes, but every breaking change still requires explicit migration notes.

## Release ownership

Only maintainers may create release tags. The repository owner is the final
release authority. Release automation uses the repository-scoped
`GITHUB_TOKEN`; no personal access token is required.

Service deployment and open-source releases are separate operations. Creating a
tag does not deploy a hosted cxthub service.

## Before tagging

1. Confirm the release commit is on `main` and the working tree is clean.
2. Confirm all required GitHub checks pass for that commit.
3. Review user-visible changes and update `CHANGELOG.md`.
4. Run the local release gate:

   ```bash
   make build
   make test
   make typecheck
   make lint
   make e2e
   make e2e-sync
   make public-check-full
   ```

5. For persistence changes, run PostgreSQL tests and migration checks.
6. Confirm the version has not already been tagged or released.
7. Confirm installer, module, repository, and release workflow URLs still point
   to `wnsdy95/cxthub`.

Use a release candidate for changes that need broader installation or
compatibility testing:

```bash
git tag -s v0.2.0-rc.1 -m "cxthub v0.2.0-rc.1"
git push origin v0.2.0-rc.1
```

## Stable release

Create a signed annotated tag on the verified `main` commit:

```bash
git tag -s v0.2.0 -m "cxthub v0.2.0"
git push origin v0.2.0
```

The release workflow runs GoReleaser, builds macOS and Linux archives for
amd64 and arm64, produces checksums, and publishes a GitHub Release.

After automation finishes:

1. Verify the workflow and GitHub Release succeeded.
2. Verify checksums and expected platform archives are attached.
3. Install one published archive in a clean temporary directory.
4. Run `cxt version` and `cxt --help`.
5. Mark release candidates as prereleases; keep stable releases non-prerelease.
6. Announce material compatibility or migration requirements.

## Immutability and rollback

Published tags and release assets are immutable. Do not force-update a tag,
delete it to hide a defect, or replace an asset under the same version.

If a release is defective:

1. Document the impact.
2. Fix the issue on a short-lived branch.
3. Merge the fix into `main`.
4. Publish the next patch version.
5. Mark the affected GitHub Release as deprecated when appropriate.

For a compromised release, follow the private vulnerability process in
[SECURITY.md](../SECURITY.md) before public disclosure.
