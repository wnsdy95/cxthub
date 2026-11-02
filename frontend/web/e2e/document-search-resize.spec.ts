import { expect, test, type Page } from '@playwright/test';
import { build } from 'esbuild';

let script: string;
let styles: string;

test.beforeAll(async () => {
  const bundle = await build({
    stdin: { resolveDir: process.cwd(), loader: 'tsx', contents: `
      import { StrictMode, useLayoutEffect, useState } from 'react';
      import { createRoot } from 'react-dom/client';
      import { useContextSelection } from './src/components/ContextSelection';
      import './src/styles.css';

      function Harness() {
        const [snapshot, setSnapshot] = useState('snapshot');
        const [delivered, setDelivered] = useState(false);
        const { viewerRef, openSnapshot, selectedEventIndex } = useContextSelection('repo', snapshot, setSnapshot);
        useLayoutEffect(() => {
          window.deliverSearchTarget = () => setDelivered(true);
          return () => { delete window.deliverSearchTarget; };
        }, []);
        return <div className="app">
          <header className="topbar">
            <button onClick={() => openSnapshot('snapshot', undefined, 142)}>Select matched event</button>
          </header>
          <main style={{ padding: 16, minWidth: 0 }}>
            <div className="ctx-main" data-testid="document-scroll">
              <div style={{ height: 1800 }} />
              <div className="viewer" ref={viewerRef} data-testid="viewer" data-selected-event={selectedEventIndex}>
                <div className="viewer-head"><code>Pending search restoration</code></div>
                <div data-document-page="units:142" style={{ height: 6000 }}>
                  {delivered ? <div className="msg" data-document-target="142" data-testid="search-target">
                    <span className="msg-role">assistant</span>
                    <div className="msg-body">Exact restored search target</div>
                  </div> : <div data-document-page-loading role="status">Restoring target page</div>}
                </div>
              </div>
              <div style={{ height: 1200 }} />
            </div>
          </main>
        </div>;
      }

      createRoot(document.getElementById('root')).render(<StrictMode><Harness /></StrictMode>);
    ` },
    bundle: true, write: false, outdir: 'document-search-resize-fixture', platform: 'browser', format: 'esm', jsx: 'automatic',
    define: { 'import.meta.env': '{}', 'process.env.NODE_ENV': '"development"' },
  });
  script = bundle.outputFiles.find(output => output.path.endsWith('.js'))!.text;
  styles = bundle.outputFiles.find(output => output.path.endsWith('.css'))!.text;
});

async function harness(page: Page, width: number) {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.setViewportSize({ width, height: 800 });
  await page.route('**/api/**', route => {
    errors.push(`Unexpected API request: ${route.request().url()}`);
    return route.abort();
  });
  await page.route('**/__document-search-resize*', route => {
    const pathname = new URL(route.request().url()).pathname;
    if (pathname.endsWith('.js')) return route.fulfill({ contentType: 'application/javascript', body: script });
    if (pathname.endsWith('.css')) return route.fulfill({ contentType: 'text/css', body: styles });
    return route.fulfill({ contentType: 'text/html', body: `<!doctype html><html><head>
      <meta name="viewport" content="width=device-width, initial-scale=1" />
      <link rel="stylesheet" href="/__document-search-resize.css" /></head><body><div id="root"></div>
      <script type="module" src="/__document-search-resize.js"></script></body></html>` });
  });
  await page.goto('/__document-search-resize');
  await expectCurrentRoot(page, width);
  await expect(page.locator('.viewer-head')).toHaveCSS('position', 'sticky');
  await page.getByRole('button', { name: 'Select matched event' }).click();
  await expect(page.getByTestId('viewer')).toHaveAttribute('data-selected-event', '142');
  await expect(page.getByTestId('search-target')).toHaveCount(0);
  await expect(page.locator('[data-document-page="units:142"]')).toBeInViewport();
  return errors;
}

async function expectCurrentRoot(page: Page, width: number) {
  await expect(page.getByTestId('document-scroll')).toHaveCSS('overflow-y', width > 1000 ? 'auto' : 'visible');
}

async function scrollPosition(page: Page) {
  return page.getByTestId('document-scroll').evaluate(element =>
    getComputedStyle(element).overflowY === 'auto' ? element.scrollTop : window.scrollY);
}

async function deliverTarget(page: Page) {
  await page.evaluate(() => {
    (window as unknown as { deliverSearchTarget: () => void }).deliverSearchTarget();
  });
  await expect(page.getByTestId('search-target')).toBeAttached();
}

async function settleFrames(page: Page) {
  await page.evaluate(() => new Promise<void>(resolve => {
    requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
  }));
}

async function targetGeometry(page: Page) {
  return page.getByTestId('search-target').evaluate(element => {
    const viewer = element.closest<HTMLElement>('.viewer')!;
    const header = viewer.querySelector<HTMLElement>('.viewer-head')!;
    const main = viewer.closest<HTMLElement>('.ctx-main')!;
    const pane = getComputedStyle(main).overflowY === 'auto' ? main : null;
    const bounds = element.getBoundingClientRect();
    return {
      clearance: bounds.top - header.getBoundingClientRect().bottom,
      aboveRoot: bounds.top < Math.max(0, pane?.getBoundingClientRect().top ?? 0),
      belowRoot: bounds.bottom > Math.min(innerHeight, pane?.getBoundingClientRect().bottom ?? innerHeight),
    };
  });
}

for (const [initialWidth, restoredWidth] of [[1280, 390], [390, 1280]] as const) {
  test(`pending search target follows the current scroll root across ${initialWidth} to ${restoredWidth}`, async ({ page }) => {
    const errors = await harness(page, initialWidth);
    const placeholder = page.locator('[data-document-page="units:142"]');
    const height = await placeholder.evaluate(element => element.getBoundingClientRect().height);
    await page.setViewportSize({ width: restoredWidth, height: 800 });
    await expectCurrentRoot(page, restoredWidth);
    await expect(page.getByTestId('search-target')).toHaveCount(0);
    await deliverTarget(page);
    const target = page.getByTestId('search-target');
    await expect(target.locator('.msg-body')).toHaveText('Exact restored search target');
    await expect(target).toBeVisible();
    await expect(target).toBeInViewport({ ratio: 1 });
    await expect.poll(async () => (await targetGeometry(page)).clearance).toBeGreaterThanOrEqual(0);
    await expect.poll(async () => {
      const geometry = await targetGeometry(page);
      return geometry.aboveRoot || geometry.belowRoot;
    }).toBe(false);
    await expect.poll(() => scrollPosition(page)).toBeGreaterThan(0);
    expect(await placeholder.evaluate(element => element.getBoundingClientRect().height)).toBe(height);
    expect(errors).toEqual([]);
  });

  test(`wheel cancels pending search restoration after resizing ${initialWidth} to ${restoredWidth}`, async ({ page }) => {
    const errors = await harness(page, initialWidth);
    await page.setViewportSize({ width: restoredWidth, height: 800 });
    await expectCurrentRoot(page, restoredWidth);
    await expect(page.locator('[data-document-page="units:142"]')).toBeInViewport();
    await settleFrames(page);
    const pointer = await page.getByTestId('document-scroll').evaluate(element => {
      const bounds = element.getBoundingClientRect();
      return {
        x: bounds.left + bounds.width / 2,
        y: (Math.max(100, bounds.top) + Math.min(innerHeight - 24, bounds.bottom)) / 2,
      };
    });
    const beforeWheel = await scrollPosition(page);
    await page.mouse.move(pointer.x, pointer.y);
    await page.mouse.wheel(0, 600);
    await expect.poll(() => scrollPosition(page)).toBeGreaterThan(beforeWheel + 300);
    await settleFrames(page);
    const cancelledPosition = await scrollPosition(page);
    await deliverTarget(page);
    await settleFrames(page);
    expect(Math.abs(await scrollPosition(page) - cancelledPosition)).toBeLessThanOrEqual(1);
    expect((await targetGeometry(page)).clearance).toBeLessThan(0);
    expect(errors).toEqual([]);
  });
}
