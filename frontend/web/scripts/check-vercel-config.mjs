import assert from 'node:assert/strict';

process.env.CXT_API_ORIGIN = 'https://cxthub-api.onrender.com';
process.env.CXT_MCP_ORIGIN = 'https://cxthub-mcp.onrender.com';
process.env.VITE_FIREBASE_API_KEY = 'AIzaSyExampleFirebaseWebApiKey123456';
process.env.VITE_FIREBASE_AUTH_DOMAIN = 'example-firebase-project.firebaseapp.com';
process.env.VITE_FIREBASE_PROJECT_ID = 'example-firebase-project';
const { config, normalizeApiOrigin, normalizeFirebaseWebConfig } = await import('../vercel.mjs');

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


const { spawnSync } = await import('node:child_process');
for (const mcp of ['', process.env.CXT_API_ORIGIN]) {
  const result = spawnSync(process.execPath, ['--input-type=module', '-e', "import('./vercel.mjs')"], {
    cwd: new URL('..', import.meta.url), env: { ...process.env, CXT_MCP_ORIGIN: mcp }, encoding: 'utf8',
  });
  assert.notEqual(result.status, 0, 'missing or co-located MCP origin must fail deployment');
}

console.log('✓ Vercel config: independent API/MCP origins · Firebase production auth · fail-closed');
