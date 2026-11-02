import { expect, test, type Page } from '@playwright/test';
import { loadDisplayFixture } from '../tests/markdownDisplayFixture';

async function harness(page: Page, source: string) {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  const literal = JSON.stringify(source).replace(/</g, '\\u003c').replace(/\u2028/g, '\\u2028').replace(/\u2029/g, '\\u2029');
  await page.route('**/__event-display-harness', route => route.fulfill({ contentType: 'text/html', body: `<!doctype html><html><head><meta charset="utf-8"></head><body><div id="root"></div><script type="module">
import RefreshRuntime from '/@react-refresh';
RefreshRuntime.injectIntoGlobalHook(window);
window.$RefreshReg$ = () => {};
window.$RefreshSig$ = () => type => type;
window.__vite_plugin_react_preamble_installed__ = true;
const reactModule = await import('/@id/react');
const clientModule = await import('/@id/react-dom/client');
const React = reactModule.default ?? reactModule;
const ReactDOMClient = clientModule.default ?? clientModule;
await import('/src/styles.css');
const { I18nProvider } = await import('/src/i18n/index.tsx');
const { DocumentPresentation } = await import('/src/components/DocumentPresentation.tsx');
const { DocumentEventRow } = await import('/src/components/EventStream.tsx');
ReactDOMClient.createRoot(document.getElementById('root')).render(React.createElement(I18nProvider, null,
  React.createElement(DocumentPresentation, null, React.createElement(DocumentEventRow, {
    event: JSON.parse(${literal}), index: 0, inherited: 0, repoId: 'repo', hash: 'document'
  }))));
</script></body></html>` }));
  await page.goto('/__event-display-harness', { waitUntil: 'domcontentloaded' });
  await expect(page.locator('[data-event-index="0"]')).toBeAttached();
  for (const summary of await page.locator('details > summary').all()) await summary.click();
  return errors;
}

for (const name of ['event-display-v1', 'tool-display-v1'] as const) {
  for (const fixture of loadDisplayFixture(name).cases) test(`${name} actual content profile: ${fixture.name}`, async ({ page }, testInfo) => {
    const errors = await harness(page, fixture.source);
    const event = JSON.parse(fixture.source) as { kind: string };
    const actual = await page.locator('[data-event-index="0"]').evaluate((root, kind) => {
      if (kind === 'tool_call') {
        const header = root.querySelector<HTMLElement>('.tool-head')?.innerText ?? '';
        const body = root.querySelector<HTMLElement>('.tool-diff, .tool-cmd, details pre')?.innerText ?? '';
        return header + (body ? '\n' + body : '');
      }
      if (kind === 'tool_result') return root.querySelector<HTMLElement>('details pre')?.innerText ?? '';
      return root.querySelector<HTMLElement>('details .md')?.innerText ?? root.querySelector<HTMLElement>('.msg-body')?.innerText ?? '';
    }, event.kind);
    if (actual !== fixture.text) await testInfo.attach('event-display-mismatch', { contentType: 'application/json',
      body: JSON.stringify({ fixture, actual, html: await page.locator('#root').innerHTML() }, null, 2) });
    expect(actual, 'Expanded data content, not translated controls or duplicate previews').toBe(fixture.text);
    expect(errors).toEqual([]);
  });
}
