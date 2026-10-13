import { expect, test as base, type Page } from '@playwright/test';
import { spawnSync } from 'node:child_process';

const fixtureOrigin = 'https://firebase-csp.example.test';
const fixtureUrl = `${fixtureOrigin}/`;
const bootstrapUrl = `${fixtureOrigin}/bootstrap.js`;
const googleApiUrl = 'https://apis.google.com/js/api.js?onload=__firebaseCspLoaded';
const googleModuleUrl = 'https://apis.google.com/_/scs/abc-static/_/js/k=gapi.fixture/m=auth/rt=j/sv=1/cb=gapi.loaded_0';

const test = base.extend<{ productionCsp: string }>({
  productionCsp: async ({}, use) => {
    const configUrl = new URL('../vercel.mjs', import.meta.url).href;
    const result = spawnSync(process.execPath, ['--input-type=module', '--eval',
      `const { config } = await import(${JSON.stringify(configUrl)}); process.stdout.write(JSON.stringify(config));`,
    ], {
      encoding: 'utf8',
      env: {
        ...process.env,
        CXT_API_ORIGIN: 'https://api.example.test',
        CXT_MCP_ORIGIN: 'https://mcp.example.test',
        VITE_FIREBASE_API_KEY: 'AIzaSyExampleFirebaseWebApiKey123456',
        VITE_FIREBASE_AUTH_DOMAIN: 'example-firebase-project.firebaseapp.com',
        VITE_FIREBASE_PROJECT_ID: 'example-firebase-project',
      },
    });
    expect(result.status, result.stderr || result.error?.message).toBe(0);
    const config = JSON.parse(result.stdout) as {
      headers: { source: string; headers: { key: string; value: string }[] }[];
    };
    const csp = config.headers.find(entry => entry.source === '/(.*)')?.headers
      .find(header => header.key.toLowerCase() === 'content-security-policy')?.value;
    if (!csp) throw new Error('Production Vercel configuration must supply an enforcing CSP');
    await use(csp);
  },
});

test.use({ bypassCSP: false, serviceWorkers: 'block' });

type MockResource = { contentType: string; body: string };

async function openCspFixture(page: Page, csp: string, bootstrap: string, resources: Record<string, MockResource>) {
  const requested: string[] = [];
  const unexpected: string[] = [];
  const pageErrors: string[] = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  await page.route('**/*', async route => {
    const url = route.request().url();
    requested.push(url);
    if (url === fixtureUrl) {
      await route.fulfill({
        contentType: 'text/html',
        headers: { 'Content-Security-Policy': csp },
        body: '<!doctype html><html><head><link rel="icon" href="data:,"></head><body><ol id="violations"></ol><script src="/bootstrap.js"></script></body></html>',
      });
      return;
    }
    if (url === bootstrapUrl) {
      await route.fulfill({
        contentType: 'text/javascript',
        body: `
          document.body.dataset.selfLoaded = 'true';
          document.addEventListener('securitypolicyviolation', event => {
            const violation = document.createElement('li');
            violation.dataset.directive = event.effectiveDirective;
            violation.dataset.blocked = event.blockedURI;
            violation.dataset.disposition = event.disposition;
            violation.textContent = event.effectiveDirective + ': ' + event.blockedURI;
            document.getElementById('violations').appendChild(violation);
          });
          ${bootstrap}
        `,
      });
      return;
    }
    const resource = resources[url];
    if (resource) {
      await route.fulfill(resource);
      return;
    }
    unexpected.push(url);
    await route.abort();
  });
  const response = await page.goto(fixtureUrl);
  expect(response?.headers()['content-security-policy']).toBe(csp);
  await expect(page.locator('body')).toHaveAttribute('data-self-loaded', 'true');
  return { requested, unexpected, pageErrors };
}

function violations(page: Page, directive: string, blocked: string) {
  return page.locator(`#violations > li[data-disposition="enforce"][data-directive="${directive}"][data-blocked^="${blocked}"]`);
}

test('production CSP permits the mocked Firebase popup dynamic Google script chain', async ({ page, productionCsp }) => {
  const { requested, unexpected, pageErrors } = await openCspFixture(page, productionCsp, `
    window.__firebaseCspLoaded = () => { document.body.dataset.apiCallback = 'loaded'; };
    const apiScript = document.createElement('script');
    apiScript.src = ${JSON.stringify(googleApiUrl)};
    document.head.appendChild(apiScript);
  `, {
    [googleApiUrl]: {
      contentType: 'text/javascript',
      body: `
        document.body.dataset.apiLoaded = 'true';
        const callback = new URL(document.currentScript.src).searchParams.get('onload');
        const moduleScript = document.createElement('script');
        moduleScript.src = ${JSON.stringify(googleModuleUrl)};
        moduleScript.onload = () => window[callback]();
        document.head.appendChild(moduleScript);
      `,
    },
    [googleModuleUrl]: {
      contentType: 'text/javascript',
      body: "document.body.dataset.moduleLoaded = 'true';",
    },
  });

  await expect(page.locator('body')).toHaveAttribute('data-api-callback', 'loaded');
  await expect(page.locator('body')).toHaveAttribute('data-api-loaded', 'true');
  await expect(page.locator('body')).toHaveAttribute('data-module-loaded', 'true');
  await expect(page.locator('#violations > li')).toHaveCount(0);
  expect(requested).toEqual([fixtureUrl, bootstrapUrl, googleApiUrl, googleModuleUrl]);
  expect(unexpected).toEqual([]);
  expect(pageErrors).toEqual([]);
});

test('production CSP still blocks unrelated scripts, inline JavaScript and eval', async ({ page, productionCsp }) => {
  const blockedScripts = [
    'https://unrelated.example.test/untrusted.js',
    'https://untrusted.googleapis.com/untrusted.js',
    'https://apis.google.com/js/unrelated.js',
    'https://apis.google.com/js/api.js/extra',
    'https://apis.google.com/_/scs/abc-static-evil/module.js',
  ];
  const { requested, unexpected, pageErrors } = await openCspFixture(page, productionCsp, `
    for (const url of ${JSON.stringify(blockedScripts)}) {
      const script = document.createElement('script');
      script.src = url;
      document.head.appendChild(script);
    }
    const inlineScript = document.createElement('script');
    inlineScript.textContent = "document.body.dataset.inlineExecuted = 'true';";
    document.head.appendChild(inlineScript);
    try {
      eval("document.body.dataset.evalExecuted = 'true';");
    } catch (error) {
      document.body.dataset.evalError = error.name;
    }
  `, Object.fromEntries(blockedScripts.map(url => [url, {
    contentType: 'text/javascript',
    body: "document.body.dataset.untrustedExecuted = 'true';",
  }])));

  await expect(violations(page, 'script-src-elem', 'https://unrelated.example.test')).toHaveCount(1);
  await expect(violations(page, 'script-src-elem', 'https://untrusted.googleapis.com')).toHaveCount(1);
  await expect(violations(page, 'script-src-elem', 'https://apis.google.com')).toHaveCount(3);
  await expect(violations(page, 'script-src-elem', 'inline')).toHaveCount(1);
  await expect(violations(page, 'script-src', 'eval')).toHaveCount(1);
  await expect(page.locator('body')).toHaveAttribute('data-eval-error', 'EvalError');
  await expect(page.locator('body')).not.toHaveAttribute('data-untrusted-executed');
  await expect(page.locator('body')).not.toHaveAttribute('data-inline-executed');
  await expect(page.locator('body')).not.toHaveAttribute('data-eval-executed');
  expect(requested).toEqual([fixtureUrl, bootstrapUrl]);
  expect(unexpected).toEqual([]);
  expect(pageErrors).toEqual([]);
});

test('production CSP preserves mocked Firebase and Google auth frames without opening unrelated frames', async ({ page, productionCsp }) => {
  const firebaseFrameUrl = 'https://example-firebase-project.firebaseapp.com/__/auth/iframe';
  const googleFrameUrl = 'https://accounts.google.com/fixture-auth';
  const blockedFrameUrl = 'https://unrelated.example.test/auth';
  const frameUrls = [firebaseFrameUrl, googleFrameUrl, blockedFrameUrl];
  const { requested, unexpected, pageErrors } = await openCspFixture(page, productionCsp, `
    for (const [index, url] of ${JSON.stringify(frameUrls)}.entries()) {
      const frame = document.createElement('iframe');
      frame.id = 'auth-frame-' + index;
      frame.src = url;
      document.body.appendChild(frame);
    }
  `, Object.fromEntries(frameUrls.map(url => [url, {
    contentType: 'text/html',
    body: '<!doctype html><html><body><p>Mock auth frame loaded</p></body></html>',
  }])));

  await expect(page.frameLocator('#auth-frame-0').getByText('Mock auth frame loaded')).toBeVisible();
  await expect(page.frameLocator('#auth-frame-1').getByText('Mock auth frame loaded')).toBeVisible();
  await expect(violations(page, 'frame-src', 'https://unrelated.example.test')).toHaveCount(1);
  await expect(page.locator('#violations > li')).toHaveCount(1);
  expect([...requested].sort()).toEqual([fixtureUrl, bootstrapUrl, firebaseFrameUrl, googleFrameUrl].sort());
  expect(unexpected).toEqual([]);
  expect(pageErrors).toEqual([]);
});
