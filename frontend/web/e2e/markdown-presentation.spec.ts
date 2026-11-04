import { expect, test, type Page, type TestInfo } from '@playwright/test';
import { loadMarkdownDisplayFixture } from '../tests/markdownDisplayFixture';

const fixtures = loadMarkdownDisplayFixture();

async function harness(page: Page, source: string) {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  const literal = JSON.stringify(source)
    .replace(/</g, '\\u003c')
    .replace(/\u2028/g, '\\u2028')
    .replace(/\u2029/g, '\\u2029');
  await page.route('**/__markdown-presentation-harness', route => route.fulfill({
    contentType: 'text/html',
    body: `<!doctype html><html><head><meta charset="utf-8"></head><body><div id="root"></div><script type="module">
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
const { Markdown } = await import('/src/components/Markdown.tsx');
ReactDOMClient.createRoot(document.getElementById('root')).render(React.createElement(Markdown, { text: ${literal} }));
</script></body></html>`,
  }));
  await page.goto('/__markdown-presentation-harness', { waitUntil: 'domcontentloaded' });
  await expect(page.locator('#root > .md')).toBeAttached();
  return errors;
}

async function expectPresentation(page: Page, expected: string, testInfo: TestInfo, source: string) {
  const rendered = await page.locator('#root > .md').evaluate(root => ({
    text: (root as HTMLElement).innerText,
    html: root.innerHTML,
    leaves: Array.from(root.querySelectorAll<HTMLElement>(
      ':scope > :is(p, h4, h5, h6, pre), :scope > :is(ul, ol) > li',
    ), element => ({
      tag: element.tagName,
      text: element.innerText,
      textContent: element.textContent,
      whiteSpace: getComputedStyle(element).whiteSpace,
    })),
  }));
  if (rendered.text !== expected) {
    await testInfo.attach('markdown-presentation-mismatch', {
      contentType: 'application/json',
      body: JSON.stringify({ source, expected, rendered }, null, 2),
    });
  }
  expect(rendered.text, 'Actual .md.innerText must match the static golden without normalization').toBe(expected);
}

for (const fixture of fixtures.cases) {
  test(`Markdown container innerText matches shared golden: ${fixture.name}`, async ({ page }, testInfo) => {
    const errors = await harness(page, fixture.source);
    await expectPresentation(page, fixture.text, testInfo, fixture.source);
    expect(errors).toEqual([]);
  });
}

test('Markdown renders actual block and inline elements with the production stylesheet', async ({ page }) => {
  const errors = await harness(page, [
    '# Level **one**',
    '## Level `two`',
    '### Level [three](https://example.test/three)',
    '',
    'Paragraph with **bold** and `inline` and [link](https://example.test/paragraph).',
    '',
    '- First **item**',
    '* Second `item`',
    '1. Ordered [item](https://example.test/item)',
    '2) Last item',
    '',
    '```typescript',
    '  const answer = 42;',
    '\treturn answer;',
    '```',
  ].join('\n'));
  const root = page.locator('#root > .md');
  expect(await root.evaluate(element => Array.from(element.children, child => child.tagName)))
    .toEqual(['H4', 'H5', 'H6', 'P', 'UL', 'OL', 'PRE']);
  await expect(root.locator(':scope > h4 > strong')).toHaveText('one');
  await expect(root.locator(':scope > h5 > code')).toHaveText('two');
  await expect(root.locator(':scope > h6 > a')).toHaveAttribute('href', 'https://example.test/three');
  await expect(root.locator(':scope > p > strong')).toHaveText('bold');
  await expect(root.locator(':scope > p > code')).toHaveText('inline');
  await expect(root.locator(':scope > ul > li')).toHaveCount(2);
  await expect(root.locator(':scope > ol > li')).toHaveCount(2);
  await expect(root.locator(':scope > p')).toHaveCSS('white-space', 'pre-wrap');
  for (const selector of [':scope > h4', ':scope > h5', ':scope > h6', ':scope > ul > li', ':scope > ol > li']) {
    const elements = root.locator(selector);
    for (let index = 0; index < await elements.count(); index++) {
      await expect(elements.nth(index)).toHaveCSS('white-space', 'normal');
    }
  }
  await expect(root.locator(':scope > pre')).toHaveCSS('white-space', 'pre');
  expect(await root.locator(':scope > pre').innerText()).toBe('  const answer = 42;\n\treturn answer;');
  expect(errors).toEqual([]);
});

const unicodeWhitespace = 'A\u00a0\u00a0B\u2003\u2003C\u2009\u2009D\u202f\u202fE\u3000\u3000F\u200b\u200bG\ufeff\ufeffH\f\fI\v\vJ';

for (const scenario of [
  { name: 'paragraph', source: unicodeWhitespace },
  { name: 'heading', source: `# ${unicodeWhitespace}` },
  { name: 'unordered list', source: `- ${unicodeWhitespace}` },
  { name: 'ordered list', source: `1. ${unicodeWhitespace}` },
  { name: 'preformatted block', source: `\`\`\`\n${unicodeWhitespace}\n\`\`\`` },
]) {
  test(`Markdown preserves Unicode whitespace and FF/VT content in ${scenario.name}`, async ({ page }, testInfo) => {
    const errors = await harness(page, scenario.source);
    await expectPresentation(page, unicodeWhitespace, testInfo, scenario.source);
    expect(errors).toEqual([]);
  });
}

for (const scenario of [
  { name: 'paragraph', source: 'before\rafter\r\nlast', text: 'before\rafter\r\nlast' },
  { name: 'preformatted block', source: '```\n  first\r\n\tsecond\rthird\n```', text: '  first\r\n\tsecond\rthird' },
]) {
  test(`Markdown exports DOM-inserted CR verbatim without normalizing CRLF in ${scenario.name}`, async ({ page }, testInfo) => {
    const errors = await harness(page, scenario.source);
    await expectPresentation(page, scenario.text, testInfo, scenario.source);
    expect(errors).toEqual([]);
  });
}

test('Markdown allows only safe HTTP links and renders raw HTML as inert text', async ({ page }, testInfo) => {
  await page.addInitScript(() => { Reflect.set(window, '__markdownExecuted', false); });
  const dialogs: string[] = [];
  page.on('dialog', async dialog => {
    dialogs.push(dialog.message());
    await dialog.dismiss();
  });
  const source = [
    '[secure](https://example.test/path?q=ok#section)',
    '[plain](http://example.test/plain)',
    '[credentialed](https://user:secret@example.test/private)',
    '[javascript](javascript:globalThis.__markdownExecuted=true)',
    '[mixed case](JaVaScRiPt:globalThis.__markdownExecuted=true)',
    '[data](data:text/html,<script>globalThis.__markdownExecuted=true</script>)',
    '[file](file:///etc/passwd)',
    '[protocol relative](//example.test/path)',
    '[malformed](https://)',
    '<b>literal</b> &amp;',
    '<script>globalThis.__markdownExecuted=true</script>',
    '<img src="data:image/png;base64,AA==" onerror="globalThis.__markdownExecuted=true">',
    '<svg onload="globalThis.__markdownExecuted=true"></svg>',
    '<iframe srcdoc="<script>parent.__markdownExecuted=true</script>"></iframe>',
  ].join('\n');
  const expected = [
    'secure',
    'plain',
    'credentialed',
    '[javascript](javascript:globalThis.__markdownExecuted=true)',
    '[mixed case](JaVaScRiPt:globalThis.__markdownExecuted=true)',
    '[data](data:text/html,<script>globalThis.__markdownExecuted=true</script>)',
    '[file](file:///etc/passwd)',
    '[protocol relative](//example.test/path)',
    '[malformed](https://)',
    '<b>literal</b> &amp;',
    '<script>globalThis.__markdownExecuted=true</script>',
    '<img src="data:image/png;base64,AA==" onerror="globalThis.__markdownExecuted=true">',
    '<svg onload="globalThis.__markdownExecuted=true"></svg>',
    '<iframe srcdoc="<script>parent.__markdownExecuted=true</script>"></iframe>',
  ].join('\n');
  const errors = await harness(page, source);
  const root = page.locator('#root > .md');
  await expectPresentation(page, expected, testInfo, source);
  expect(await root.locator('a').evaluateAll(elements => elements.map(element => ({
    href: (element as HTMLAnchorElement).href,
    target: element.getAttribute('target'),
    rel: element.getAttribute('rel'),
  })))).toEqual([
    { href: 'https://example.test/path?q=ok#section', target: '_blank', rel: 'noopener noreferrer' },
    { href: 'http://example.test/plain', target: '_blank', rel: 'noopener noreferrer' },
  ]);
  await expect(root.locator('script, img, svg, iframe, object, embed, style, link, b, [onload], [onerror]')).toHaveCount(0);
  expect(await page.evaluate(() => Reflect.get(window, '__markdownExecuted'))).toBe(false);
  expect(dialogs).toEqual([]);
  expect(errors).toEqual([]);
});

for (const separator of ['\u2028', '\u2029']) {
  test(`Markdown terminates on a long malformed heading containing U+${separator.charCodeAt(0).toString(16).toUpperCase()}`, async ({ page }, testInfo) => {
    test.setTimeout(15_000);
    const source = `# before${separator}after${' tail'.repeat(16_384)}`;
    const errors = await harness(page, source);
    await expect(page.locator('#root > .md > p')).toHaveCount(1);
    await expect(page.locator('#root > .md > :is(h4, h5, h6)')).toHaveCount(0);
    await expectPresentation(page, source, testInfo, source);
    expect(errors).toEqual([]);
  });
}
