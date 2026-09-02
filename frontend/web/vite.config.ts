import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// cxt web UI build.
// During development, /api requests are proxied to the backend cxtd → the browser must communicate from the same origin (localhost:5173).
// This ensures HttpOnly session cookies operate as first-party (SameSite=Lax) and CORS is not required.
// The proxy target can be changed to VITE_DEV_PROXY (default 127.0.0.1:8907).
const proxyTarget = process.env.VITE_DEV_PROXY ?? 'http://127.0.0.1:8907';

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': { target: proxyTarget, changeOrigin: true },
      '/mcp': { target: proxyTarget, changeOrigin: true },
      '/oauth': { target: proxyTarget, changeOrigin: true },
      '/.well-known': { target: proxyTarget, changeOrigin: true },
    },
  },
});
