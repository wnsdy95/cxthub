import { defineConfig, devices } from '@playwright/test';

const port = 4174;
const backendPort = 18_907;
const fullStack = Boolean(process.env.CXT_E2E_FULLSTACK);

export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: process.env.CI
    ? [['line'], ['html', { open: 'never' }]]
    : [['list']],
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    screenshot: 'only-on-failure',
    trace: 'on-first-retry',
    video: 'retain-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  webServer: [
    ...(fullStack
      ? [
          {
            name: 'cxtd',
            command: 'bash e2e/run-cxtd.sh',
            url: `http://127.0.0.1:${backendPort}/api/v1/health`,
            reuseExistingServer: false,
            timeout: 120_000,
            stdout: 'ignore' as const,
            stderr: 'pipe' as const,
          },
        ]
      : []),
    {
      name: 'web',
      command: `${fullStack ? `VITE_DEV_PROXY=http://127.0.0.1:${backendPort} ` : ''}npm run dev -- --host 127.0.0.1 --port ${port} --strictPort`,
      url: `http://127.0.0.1:${port}`,
      // Full-stack mode must own the Vite process so its proxy cannot silently
      // reuse a developer server pointed at a different backend.
      reuseExistingServer: !process.env.CI && !fullStack,
      timeout: 60_000,
      stdout: 'ignore',
      stderr: 'pipe',
    },
  ],
});
