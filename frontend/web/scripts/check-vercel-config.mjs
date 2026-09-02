import assert from 'node:assert/strict';

process.env.CXT_API_ORIGIN = 'https://cxtd-123456789.asia-northeast3.run.app';
process.env.VITE_FIREBASE_API_KEY = 'AIzaSyExampleFirebaseWebApiKey123456';
process.env.VITE_FIREBASE_AUTH_DOMAIN = 'example-firebase-project.firebaseapp.com';
process.env.VITE_FIREBASE_PROJECT_ID = 'example-firebase-project';
const { config, normalizeApiOrigin, normalizeFirebaseWebConfig } = await import('../vercel.mjs');

assert.equal(
  config.rewrites[0].destination,
  'https://cxtd-123456789.asia-northeast3.run.app/api/:path*',
);
assert.deepEqual(config.rewrites.slice(1, 4), [
  { source: '/mcp', destination: 'https://cxtd-123456789.asia-northeast3.run.app/mcp' },
  { source: '/oauth/:path*', destination: 'https://cxtd-123456789.asia-northeast3.run.app/oauth/:path*' },
  {
    source: '/.well-known/:path*',
    destination: 'https://cxtd-123456789.asia-northeast3.run.app/.well-known/:path*',
  },
]);
assert.equal(
  normalizeApiOrigin(' https://cxtd-123456789.asia-northeast3.run.app/ '),
  'https://cxtd-123456789.asia-northeast3.run.app',
);

for (const invalid of [
  undefined,
  '',
  'http://cxtd-123456789.asia-northeast3.run.app',
  'https://evil.example',
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

console.log('✓ Vercel config: Cloud Run origin · Firebase production auth · fail-closed');
