import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { build } from 'esbuild';
import ts from 'typescript';

const configSource = ts.createSourceFile(
  'vercel.mjs',
  readFileSync(new URL('../vercel.mjs', import.meta.url), 'utf8'),
  ts.ScriptTarget.Latest,
  true,
  ts.ScriptKind.JS,
);
const configDeclaration = configSource.statements
  .filter((statement) => ts.isVariableStatement(statement)
    && statement.modifiers?.some((modifier) => modifier.kind === ts.SyntaxKind.ExportKeyword))
  .flatMap((statement) => statement.declarationList.declarations)
  .find((declaration) => declaration.name.getText(configSource) === 'config');
assert.ok(configDeclaration?.initializer && ts.isCallExpression(configDeclaration.initializer),
  'environment-dependent config must be evaluated as a unit, not partially extracted as a literal',
);

process.env.CXT_API_ORIGIN = 'https://cxthub-api.onrender.com';
process.env.CXT_MCP_ORIGIN = 'https://cxthub-mcp.onrender.com';
process.env.VITE_FIREBASE_API_KEY = 'AIzaSyExampleFirebaseWebApiKey123456';
process.env.VITE_FIREBASE_AUTH_DOMAIN = 'example-firebase-project.firebaseapp.com';
process.env.VITE_FIREBASE_PROJECT_ID = 'example-firebase-project';
const { config, normalizeApiOrigin, normalizeFirebaseWebConfig } = await import('../vercel.mjs');

const serializedConfig = JSON.parse(JSON.stringify(config));
assert.deepEqual(serializedConfig, config);
const bundled = await build({
  entryPoints: [fileURLToPath(new URL('../vercel.mjs', import.meta.url))],
  bundle: true,
  platform: 'node',
  format: 'esm',
  packages: 'external',
  target: 'node20',
  write: false,
});
const bundledURL = `data:text/javascript;base64,${Buffer.from(bundled.outputFiles[0].text).toString('base64')}`;
const compiled = spawnSync(process.execPath, ['--input-type=module', '-e',
  `const { config } = await import(${JSON.stringify(bundledURL)}); process.stdout.write(JSON.stringify(config));`,
], { env: { ...process.env }, encoding: 'utf8' });
assert.equal(compiled.status, 0, compiled.stderr);
assert.deepEqual(JSON.parse(compiled.stdout), serializedConfig,
  'Vercel-style bundling and isolated evaluation must preserve the complete configuration');
for (const [index, rewrite] of serializedConfig.rewrites.entries()) {
  assert.equal(typeof rewrite.source, 'string', `rewrites[${index}].source`);
  assert.equal(typeof rewrite.destination, 'string', `rewrites[${index}].destination`);
  assert.ok(rewrite.destination.length > 0, `rewrites[${index}].destination must not be empty`);
}

assert.deepEqual(config.rewrites.slice(0, 5), [
  { source: '/api/v1/oauth/requests/:path*', destination: 'https://cxthub-mcp.onrender.com/api/v1/oauth/requests/:path*' },
  { source: '/api/:path*', destination: 'https://cxthub-api.onrender.com/api/:path*' },
  { source: '/mcp', destination: 'https://cxthub-mcp.onrender.com/mcp' },
  { source: '/oauth/:path*', destination: 'https://cxthub-mcp.onrender.com/oauth/:path*' },
  { source: '/.well-known/:path*', destination: 'https://cxthub-mcp.onrender.com/.well-known/:path*' },
]);
assert.equal(config.rewrites[5].destination, '/index.html');
assert.equal(normalizeApiOrigin('https://api.example.com'), 'https://api.example.com');
assert.equal(normalizeApiOrigin('https://cxthub-mcp.onrender.com/'), 'https://cxthub-mcp.onrender.com');

assert.equal(
  normalizeApiOrigin(' https://cxtd-123456789.asia-northeast3.run.app/ '),
  'https://cxtd-123456789.asia-northeast3.run.app',
);

for (const invalid of [
  undefined,
  '',
  'http://cxtd-123456789.asia-northeast3.run.app',
  'https://localhost',
  'https://service.example/#fragment',
  'https://service.example/?',
  'https://user:pass@cxtd-123456789.asia-northeast3.run.app',
  'https://cxtd-123456789.asia-northeast3.run.app/api',
  'https://cxtd-123456789.asia-northeast3.run.app/?query=1',
]) {
  assert.throws(() => normalizeApiOrigin(invalid));
}

assert.deepEqual(
  normalizeFirebaseWebConfig({
    VITE_FIREBASE_API_KEY: ' AIzaSyExampleFirebaseWebApiKey123456 ',
    VITE_FIREBASE_AUTH_DOMAIN: 'EXAMPLE-FIREBASE-PROJECT.FIREBASEAPP.COM',
    VITE_FIREBASE_PROJECT_ID: 'example-firebase-project',
  }),
  {
    apiKey: 'AIzaSyExampleFirebaseWebApiKey123456',
    authDomain: 'example-firebase-project.firebaseapp.com',
    projectId: 'example-firebase-project',
  },
);

for (const invalid of [
  {},
  {
    VITE_FIREBASE_API_KEY: 'short',
    VITE_FIREBASE_AUTH_DOMAIN: 'example-firebase-project.firebaseapp.com',
    VITE_FIREBASE_PROJECT_ID: 'example-firebase-project',
  },
  {
    VITE_FIREBASE_API_KEY: 'replace-with-firebase-web-api-key',
    VITE_FIREBASE_AUTH_DOMAIN: 'example-firebase-project.firebaseapp.com',
    VITE_FIREBASE_PROJECT_ID: 'example-firebase-project',
  },
  {
    VITE_FIREBASE_API_KEY: 'AIzaSyExampleFirebaseWebApiKey123456',
    VITE_FIREBASE_AUTH_DOMAIN: 'https://example-firebase-project.firebaseapp.com/path',
    VITE_FIREBASE_PROJECT_ID: 'example-firebase-project',
  },
  {
    VITE_FIREBASE_API_KEY: 'AIzaSyExampleFirebaseWebApiKey123456',
    VITE_FIREBASE_AUTH_DOMAIN: 'example-firebase-project.firebaseapp.com',
    VITE_FIREBASE_PROJECT_ID: '../wrong',
  },
]) {
  assert.throws(() => normalizeFirebaseWebConfig(invalid));
}

for (const name of [
  'CXT_API_ORIGIN',
  'CXT_MCP_ORIGIN',
  'VITE_FIREBASE_API_KEY',
  'VITE_FIREBASE_AUTH_DOMAIN',
  'VITE_FIREBASE_PROJECT_ID',
]) {
  const result = spawnSync(process.execPath, ['--input-type=module', '-e', "import('./vercel.mjs')"], {
    cwd: new URL('..', import.meta.url), env: { ...process.env, [name]: '' }, encoding: 'utf8',
  });
  assert.notEqual(result.status, 0, `missing ${name} must fail deployment`);
  assert.ok(result.stderr.includes(name), `missing ${name} must report its name`);
}

const colocated = spawnSync(process.execPath, ['--input-type=module', '-e', "import('./vercel.mjs')"], {
  cwd: new URL('..', import.meta.url),
  env: { ...process.env, CXT_MCP_ORIGIN: process.env.CXT_API_ORIGIN },
  encoding: 'utf8',
});
assert.notEqual(colocated.status, 0, 'co-located MCP origin must fail deployment');
assert.match(colocated.stderr, /API and MCP must use independent service origins/);

console.log('✓ Vercel config: dynamic export boundary · bundled evaluation · complete rewrites · independent origins · Firebase auth · fail-closed');
