# CXTHub web frontend

Private generated distribution of the public
[CXTHub monorepo](https://github.com/wnsdy95/cxthub)'s `frontend/web/` directory,
flattened to this repository's root. `package.json`, the lockfile, Vite files,
and the unchanged `vercel.mjs` configuration all live at the root.

The monorepo is the only source of truth. This repository contains an
allowlisted snapshot of one exact source commit without source Git history.
`.cxthub-source.json` records the source SHA and exported file provenance.
Private distribution does not make the public source confidential. **Never
edit this mirror.** Submit fixes and reviews through the
[monorepo contribution flow](https://github.com/wnsdy95/cxthub/blob/main/CONTRIBUTING.md).

## Develop and check

Use Node.js 22 and npm. Run from the repository root:

```bash
npm ci
npm run test:unit
npm run check:architecture
npm run check:i18n
npm run check:vercel
npm run build
```

CI runs standalone unit and configuration checks and the production build,
which includes TypeScript checking. `npm test` additionally invokes Go graph
contract tests and requires the monorepo. The explicit `test:unit` command
reports those contract suites separately; it does not replace source CI, which
still runs every graph contract against the real backend. CI here also does
**not** run full-stack Playwright E2E: that harness
needs the real API and MCP binaries, a disposable PostgreSQL database, and the
monorepo layout. Full-stack E2E stays in the monorepo and must pass before
publishing. A green frontend build is not full-stack verification or deployment
approval. For connected local development, use the monorepo setup.

## Manual Vercel handoff

Use this repository's root, not `frontend/web`, as the project root; the build
command is `npm run build` and output directory is `dist`. Keep `vercel.mjs`
unchanged, including ordered API/MCP/OAuth rewrites and security headers.
Configure `CXT_API_ORIGIN`, `CXT_MCP_ORIGIN`, `VITE_FIREBASE_API_KEY`,
`VITE_FIREBASE_AUTH_DOMAIN`, and `VITE_FIREBASE_PROJECT_ID` in the deployment
environment as described in the source
[operations guide](https://github.com/wnsdy95/cxthub/blob/main/deploy/RENDER.md).
Use different HTTPS API and MCP origins, the same Firebase project as the
servers, and no `VITE_API_BASE` override for the same-origin deployment.

There is no automatic cloud deployment in this workflow. Keep Vercel Git
deployment disconnected for manual-only operation; do not enable push or
preview deployments while publishing component heads. Only deploy a clean
checkout of the exact frontend commit from a verified four-repository release
set, after all component CI succeeds. No deployment or synchronization secret
belongs in the export. See the source
[distribution handoff](https://github.com/wnsdy95/cxthub/blob/main/docs/REPOSITORY_DISTRIBUTION.md).
Shared `LICENSE`, `NOTICE`, and third-party notices come from the monorepo.
