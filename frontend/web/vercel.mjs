// Vercel keeps one public origin while forwarding API and MCP/OAuth requests
// to independent services. Missing or identical origins fail the deployment.
export function normalizeApiOrigin(raw, name = 'CXT_API_ORIGIN') {
  const value = raw?.trim();
  if (!value) {
    throw new Error(`${name} is required for a Vercel deployment`);
  }

  const parsed = new URL(value);
  if (
    parsed.protocol !== 'https:' ||
    !(/^https:\/\/[a-z0-9.-]+(?::[0-9]+)?\/?$/i.test(value)) ||
    !parsed.hostname.includes('.') ||
    parsed.username ||
    parsed.password ||
    parsed.pathname !== '/' ||
    (parsed.search || value.includes('?')) ||
    parsed.hash
  ) {
    throw new Error(`${name} must be an HTTPS service origin without credentials, path, query or fragment`);
  }
  return parsed.origin;
}

export function normalizeFirebaseWebConfig(env) {
  const apiKey = env.VITE_FIREBASE_API_KEY?.trim();
  const authDomain = env.VITE_FIREBASE_AUTH_DOMAIN?.trim().toLowerCase();
  const projectId = env.VITE_FIREBASE_PROJECT_ID?.trim();

  if (
    !apiKey ||
    apiKey.length < 20 ||
    /\s/.test(apiKey) ||
    apiKey.toLowerCase().startsWith('replace-with-')
  ) {
    throw new Error('VITE_FIREBASE_API_KEY must be a real Firebase Web API key');
  }
  if (!authDomain || !/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$/.test(authDomain)) {
    throw new Error('VITE_FIREBASE_AUTH_DOMAIN must be a DNS hostname without a scheme or path');
  }
  if (!projectId || !/^[a-z][a-z0-9-]{4,28}[a-z0-9]$/.test(projectId)) {
    throw new Error('VITE_FIREBASE_PROJECT_ID must be a Firebase/GCP project ID');
  }

  return { apiKey, authDomain, projectId };
}

const apiOrigin = normalizeApiOrigin(process.env.CXT_API_ORIGIN);
const mcpOrigin = normalizeApiOrigin(process.env.CXT_MCP_ORIGIN, 'CXT_MCP_ORIGIN');
if (apiOrigin === mcpOrigin) throw new Error('API and MCP must use independent service origins');
normalizeFirebaseWebConfig(process.env);

export const config = {
  rewrites: [
    // Consent uses the API cookie but is handled by the MCP OAuth service.
    { source: '/api/v1/oauth/requests/:path*', destination: `${mcpOrigin}/api/v1/oauth/requests/:path*` },
    { source: '/api/:path*', destination: `${apiOrigin}/api/:path*` },
    { source: '/mcp', destination: `${mcpOrigin}/mcp` },
    { source: '/oauth/:path*', destination: `${mcpOrigin}/oauth/:path*` },
    { source: '/.well-known/:path*', destination: `${mcpOrigin}/.well-known/:path*` },
    { source: '/((?!assets/).*)', destination: '/index.html' },
  ],
  headers: [
    {
      source: '/(.*)',
      headers: [
        {
          key: 'Content-Security-Policy',
          value:
            "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; connect-src 'self' https://*.googleapis.com https://securetoken.googleapis.com https://identitytoolkit.googleapis.com; frame-src https://*.firebaseapp.com https://accounts.google.com; upgrade-insecure-requests",
        },
        { key: 'X-Content-Type-Options', value: 'nosniff' },
        { key: 'X-Frame-Options', value: 'DENY' },
        { key: 'Referrer-Policy', value: 'strict-origin-when-cross-origin' },
        { key: 'Permissions-Policy', value: 'camera=(), microphone=(), geolocation=()' },
      ],
    },
  ],
};
