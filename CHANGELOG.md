# Changelog

All notable changes to CXTHub are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and releases follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.1] - 2026-07-29

### Changed

- Expanded the installation guide and CLI reference with release, update,
  removal, CI, restore-mode, synchronization, and configuration workflows.

### Fixed

- Return a normal empty result when optional team settings or shared secrets
  have not been configured.
- Capture current Codex rollouts that contain tool-search control records or
  object-form function arguments.
- Rebind restored Claude Code and Codex sessions to the destination working
  tree so relocated clones remain discoverable by live capture.
- Keep the symbolic context `HEAD` aligned with branch checkouts while leaving
  tag and direct-snapshot restores detached.
- Preserve the latest user request and valid tool-call structure when a large
  restored session must be reduced to the seed context budget.

## [0.1.0] - 2026-07-26

### Added

- Initial public alpha of the `cxt` CLI, `cxtd` server, and web application.
- Git-integrated capture, branch, restore, stash, push, and pull workflows.
- Claude Code and Codex capture, materialization, and cross-provider support.
- Content-addressed snapshots, chunked document transport, reflog, and
  integrity checks.
- Filesystem and PostgreSQL server stores.
- Workspace access control, pending-session review, shared settings, and
  end-to-end encrypted secret-mask distribution.

[Unreleased]: https://github.com/wnsdy95/cxthub/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/wnsdy95/cxthub/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/wnsdy95/cxthub/releases/tag/v0.1.0
