import { expect, test, type Page } from '@playwright/test';
import { build } from 'esbuild';

const documentWindowEnabled = process.env.CXT_DOCUMENT_WINDOW === '1' || process.env.CXT_DOCUMENT_BASELINE === '1';

let script: string;
let styles: string;

test.describe('document window keyboard boundaries', () => {
  test.skip(!documentWindowEnabled, 'Explicit render-window candidate gate; browser find remains a release blocker');

  test.beforeAll(async () => {
    const bundle = await build({
      stdin: { resolveDir: process.cwd(), loader: 'tsx', contents: `
        import { StrictMode, useLayoutEffect, useState } from 'react';
        import { createRoot } from 'react-dom/client';
        import { DocumentPageWindow } from './src/components/DocumentPageWindow';
        import './src/styles.css';

        const states = new Map();
        const listeners = new Map();
        window.setKeyboardBody = (identity, phase, height = 6000) => {
          const state = { phase, height };
          states.set(identity, state);
          listeners.get(identity)?.(state);
        };

        function Body({ identity, children }) {
          const [state, setState] = useState(() => states.get(identity) ?? { phase: 'ready', height: 6000 });
          useLayoutEffect(() => {
            listeners.set(identity, setState);
            return () => listeners.delete(identity);
          }, [identity]);
          if (state.phase !== 'ready') return <div data-document-page-loading role={state.phase === 'error' ? 'alert' : 'status'}>
            {state.phase === 'error' ? 'Restore failed' : 'Restoring page'}
            {state.phase === 'error' && <button id="retry-body">Retry body</button>}
          </div>;
          return children(state.height);
        }

        function NormalPages() {
          return <>
            <DocumentPageWindow identity="units:0">{() => <Body identity="units:0">{height => <>
              <button id="first-first">First page first</button>
              <button id="first-last">First page last</button>
              <div style={{ height }} />
            </>}</Body>}</DocumentPageWindow>
            {new URLSearchParams(location.search).has('empty') && <DocumentPageWindow identity="units:empty">{() => <>
              <p>A page without tab stops</p><div style={{ height: 6000 }} />
              <button disabled>Disabled gap control</button>
              <button hidden>Hidden gap control</button>
            </>}</DocumentPageWindow>}
            <DocumentPageWindow identity="units:1">{() => <Body identity="units:1">{height => <>
              <div style={{ height }} />
              <button disabled>Disabled control</button>
              <button hidden>Hidden control</button>
              <button tabIndex={-1}>Programmatic control</button>
              <details><summary id="second-first">Second page first</summary><button>Closed detail control</button></details>
              <button id="second-last">Second page last</button>
            </>}</Body>}</DocumentPageWindow>
          </>;
        }

        function FoldPages() {
          return <>
            <DocumentPageWindow identity="units:0">{() => <>
              <button id="before-fold">Before fold</button>
              <div className="asst-work" data-document-fold="0:2">
                <DocumentPageWindow identity="fold:0">{() => <>
                  <button id="fold-first">Fold first</button><div style={{ height: 6000 }} />
                </>}</DocumentPageWindow>
                <DocumentPageWindow identity="fold:1">{() => <>
                  <div style={{ height: 6000 }} /><a id="fold-last" href="#fold-last">Fold last</a>
                </>}</DocumentPageWindow>
              </div>
              <button id="after-fold">After fold</button>
            </>}</DocumentPageWindow>
            <DocumentPageWindow identity="units:1">{() => <>
              <div style={{ height: 6000 }} /><button id="following-page">Following page</button>
            </>}</DocumentPageWindow>
          </>;
        }

        const root = createRoot(document.getElementById('root'));
        window.rerenderKeyboardHarness = () => root.render(<StrictMode><main>
          <button id="before">Before document</button>
          <div className="ctx-main" data-testid="document-scroll">
            {new URLSearchParams(location.search).has('fold') ? <FoldPages /> : <NormalPages />}
          </div>
          <button id="after">After document</button>
        </main></StrictMode>);
        window.rerenderKeyboardHarness();
      ` },
      bundle: true, write: false, outdir: 'document-keyboard-fixture', platform: 'browser', format: 'esm', jsx: 'automatic',
      define: { 'import.meta.env.VITE_DOCUMENT_RENDER_WINDOW': '"1"', 'process.env.NODE_ENV': '"development"' },
    });
    script = bundle.outputFiles.find(output => output.path.endsWith('.js'))!.text;
    styles = bundle.outputFiles.find(output => output.path.endsWith('.css'))!.text;
  });

  async function harness(page: Page, query = '') {
    const errors: string[] = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.route('**/__document-window-keyboard*', route => {
      const path = new URL(route.request().url()).pathname;
      if (path.endsWith('.js')) return route.fulfill({ contentType: 'application/javascript', body: script });
      if (path.endsWith('.css')) return route.fulfill({ contentType: 'text/css', body: styles });
      return route.fulfill({ contentType: 'text/html', body: `<!doctype html><html><head>
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <link rel="stylesheet" href="/__document-window-keyboard.css" /></head><body><div id="root"></div>
        <script type="module" src="/__document-window-keyboard.js"></script></body></html>` });
    });
    await page.goto(`/__document-window-keyboard${query}`);
    await expect(page.locator('[data-document-page="units:1"]')).toHaveAttribute('data-page-mounted', 'false');
    return errors;
  }

  async function setBody(page: Page, identity: string, phase: 'loading' | 'error' | 'ready', height = 6000) {
    await page.evaluate(({ identity, phase, height }) => {
      (window as unknown as { setKeyboardBody: (identity: string, phase: string, height: number) => void })
        .setKeyboardBody(identity, phase, height);
    }, { identity, phase, height });
  }

  for (const width of [390, 1280]) test(`Tab and Shift+Tab cross recycled tall pages at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 });
    const errors = await harness(page);
    const firstPage = page.locator('[data-document-page="units:0"]');
    const secondPage = page.locator('[data-document-page="units:1"]');
    await page.locator('#first-first').focus();
    await page.keyboard.press('Tab');
    await expect(page.locator('#first-last')).toBeFocused();
    await expect(secondPage).toHaveAttribute('data-page-mounted', 'false');
    await page.keyboard.press('Tab');
    await expect(page.locator('#second-first')).toBeFocused();
    await expect(firstPage).toHaveAttribute('data-page-mounted', 'false');
    await page.keyboard.press('Tab');
    await expect(page.locator('#second-last')).toBeFocused();
    await page.keyboard.press('Shift+Tab');
    await expect(page.locator('#second-first')).toBeFocused();
    await page.keyboard.press('Shift+Tab');
    await expect(page.locator('#first-last')).toBeFocused();
    await expect(secondPage).toHaveAttribute('data-page-mounted', 'false');
    await page.keyboard.press('Shift+Tab');
    await expect(page.locator('#first-first')).toBeFocused();
    expect(errors).toEqual([]);
  });

  test('tab traversal skips recycled pages without tab stops in both directions', async ({ page }) => {
    const errors = await harness(page, '?empty');
    await expect(page.locator('[data-document-page="units:empty"]')).toHaveAttribute('data-page-mounted', 'false');
    await page.locator('#first-last').focus();
    await page.keyboard.press('Tab');
    await expect(page.locator('#second-first')).toBeFocused();
    await expect(page.locator('[data-document-page="units:0"]')).toHaveAttribute('data-page-mounted', 'false');
    await page.keyboard.press('Shift+Tab');
    await expect(page.locator('#first-last')).toBeFocused();
    expect(errors).toEqual([]);
  });

  test('nested fold pages keep their place when crossing fold and parent boundaries', async ({ page }) => {
    const errors = await harness(page, '?fold');
    await expect(page.locator('[data-document-page="fold:1"]')).toHaveAttribute('data-page-mounted', 'false');
    await page.locator('#before-fold').focus();
    await page.keyboard.press('Tab');
    await expect(page.locator('#fold-first')).toBeFocused();
    await page.keyboard.press('Tab');
    await expect(page.locator('#fold-last')).toBeFocused();
    await expect(page.locator('[data-document-page="fold:0"]')).toHaveAttribute('data-page-mounted', 'false');
    await page.keyboard.press('Shift+Tab');
    await expect(page.locator('#fold-first')).toBeFocused();
    await page.keyboard.press('Tab');
    await expect(page.locator('#fold-last')).toBeFocused();
    await page.keyboard.press('Tab');
    await expect(page.locator('#after-fold')).toBeFocused();
    await page.keyboard.press('Tab');
    await expect(page.locator('#following-page')).toBeFocused();
    await expect(page.locator('[data-document-page="units:0"]')).toHaveAttribute('data-page-mounted', 'false');
    await page.keyboard.press('Shift+Tab');
    await expect(page.locator('#after-fold')).toBeFocused();
    await page.keyboard.press('Shift+Tab');
    await expect(page.locator('#fold-last')).toBeFocused();
    await expect(page.locator('[data-document-page="fold:0"]')).toHaveAttribute('data-page-mounted', 'false');
    await page.keyboard.press('Shift+Tab');
    await expect(page.locator('#fold-first')).toBeFocused();
    expect(errors).toEqual([]);
  });

  for (const backward of [false, true]) test(`asynchronous ${backward ? 'backward' : 'forward'} restoration keeps height and completes the pending Tab`, async ({ page }) => {
    const errors = await harness(page);
    await page.locator('#first-last').focus();
    const identity = backward ? 'units:0' : 'units:1';
    const restoredPage = page.locator(`[data-document-page="${identity}"]`);
    if (backward) {
      await page.keyboard.press('Tab');
      await expect(page.locator('#second-first')).toBeFocused();
      await expect(restoredPage).toHaveAttribute('data-page-mounted', 'false');
    }
    const height = await restoredPage.evaluate(element => element.getBoundingClientRect().height);
    await setBody(page, identity, 'loading');
    await page.keyboard.press(backward ? 'Shift+Tab' : 'Tab');
    await expect(restoredPage).toBeFocused();
    await expect(restoredPage).toHaveAccessibleDescription('Restoring page');
    await expect.poll(() => restoredPage.evaluate(element => element.getBoundingClientRect().height)).toBe(height);
    await setBody(page, identity, 'ready');
    await expect(page.locator(backward ? '#first-last' : '#second-first')).toBeFocused();
    await expect(restoredPage).not.toHaveAttribute('aria-describedby');
    expect(errors).toEqual([]);
  });

  test('loading and error markers retain the measured height until the real body can be measured', async ({ page }) => {
    const errors = await harness(page);
    const restoredPage = page.locator('[data-document-page="units:1"]');
    const height = await restoredPage.evaluate(element => element.getBoundingClientRect().height);
    await setBody(page, 'units:1', 'loading');
    await page.locator('#first-last').focus();
    await page.keyboard.press('Tab');
    await expect(restoredPage).toBeFocused();
    await setBody(page, 'units:1', 'error');
    await expect(page.locator('#retry-body')).toBeFocused();
    await expect.poll(() => restoredPage.evaluate(element => element.getBoundingClientRect().height)).toBe(height);
    await setBody(page, 'units:1', 'ready', 1800);
    await expect(page.locator('#second-first')).toBeAttached();
    await expect.poll(() => restoredPage.evaluate(element => element.getBoundingClientRect().height)).toBeLessThan(height - 4000);
    await page.evaluate(() => (window as unknown as { rerenderKeyboardHarness: () => void }).rerenderKeyboardHarness());
    await expect.poll(() => restoredPage.evaluate(element => (element as HTMLElement).style.minHeight)).toBe('');
    const actual = await restoredPage.evaluate(element => element.getBoundingClientRect().height);
    await page.locator('#before').focus();
    await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = 0; });
    await expect(restoredPage).toHaveAttribute('data-page-mounted', 'false');
    expect(await restoredPage.evaluate(element => element.getBoundingClientRect().height)).toBe(actual);
    expect(errors).toEqual([]);
  });

  test('leaving a restoring boundary does not steal focus when its body arrives', async ({ page }) => {
    const errors = await harness(page);
    await setBody(page, 'units:1', 'loading');
    await page.locator('#first-last').focus();
    await page.keyboard.press('Tab');
    await expect(page.locator('[data-document-page="units:1"]')).toBeFocused();
    await page.keyboard.press('Tab');
    await expect(page.locator('#after')).toBeFocused();
    await setBody(page, 'units:1', 'ready');
    await expect(page.locator('#second-first')).toBeAttached();
    await expect(page.locator('#after')).toBeFocused();
    expect(errors).toEqual([]);
  });
});
