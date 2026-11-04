import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { test } from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { Markdown } from '../src/components/Markdown';
import { markdownDisplayText, parseMarkdownPresentation, type MarkdownInline } from '../src/markdownPresentation';
import { loadDisplayFixture, loadMarkdownDisplayFixture } from './markdownDisplayFixture';

function referenceInline(text: string): MarkdownInline[] {
  const parts: MarkdownInline[] = [];
  const inlinePattern = /(\*\*[^*]+\*\*|`[^`]+`|\[[^\]]+\]\(https?:\/\/[^)\s]+\))/g;
  let lastIndex = 0;
  for (const match of text.matchAll(inlinePattern)) {
    const matchIndex = match.index ?? 0;
    if (matchIndex > lastIndex) parts.push({ type: 'text', text: text.slice(lastIndex, matchIndex) });
    const token = match[0];
    if (token.startsWith('**')) {
      parts.push({ type: 'strong', text: token.slice(2, -2) });
    } else if (token.startsWith('`')) {
      parts.push({ type: 'code', text: token.slice(1, -1) });
    } else {
      const link = token.match(/^\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)$/);
      assert.ok(link);
      parts.push({ type: 'link', text: link[1], href: link[2] });
    }
    lastIndex = matchIndex + token.length;
  }
  if (lastIndex < text.length) parts.push({ type: 'text', text: text.slice(lastIndex) });
  return parts;
}

test('mini-markdown produces typed blocks and nonrecursive inline nodes', () => {
  assert.deepEqual(parseMarkdownPresentation([
    '# First **bold**',
    '## Second `code`',
    '### Third [label](https://example.test/path?q=value#fragment)',
    '#### literal',
    'next line',
    '',
    '- **first**',
    '  * second',
    '1. ordered',
    '23) `last`',
    '```language',
    '# literal **bold**\r',
    '```suffix',
  ].join('\n')), [
    { type: 'heading', level: 1, children: [{ type: 'text', text: 'First ' }, { type: 'strong', text: 'bold' }] },
    { type: 'heading', level: 2, children: [{ type: 'text', text: 'Second ' }, { type: 'code', text: 'code' }] },
    { type: 'heading', level: 3, children: [
      { type: 'text', text: 'Third ' },
      { type: 'link', text: 'label', href: 'https://example.test/path?q=value#fragment' },
    ] },
    { type: 'paragraph', children: [{ type: 'text', text: '#### literal\nnext line' }] },
    { type: 'list', ordered: false, items: [[{ type: 'strong', text: 'first' }], [{ type: 'text', text: 'second' }]] },
    { type: 'list', ordered: true, items: [[{ type: 'text', text: 'ordered' }], [{ type: 'code', text: 'last' }]] },
    { type: 'pre', text: '# literal **bold**\r' },
  ]);
  assert.deepEqual(parseMarkdownPresentation('**bold `literal`** `**code**` [**label**](http://example.test)'), [
    { type: 'paragraph', children: [
      { type: 'strong', text: 'bold `literal`' },
      { type: 'text', text: ' ' },
      { type: 'code', text: '**code**' },
      { type: 'text', text: ' ' },
      { type: 'link', text: '**label**', href: 'http://example.test' },
    ] },
  ]);
  assert.deepEqual(parseMarkdownPresentation('**first\nsecond** `third\nfourth` [fifth\nsixth](https://example.test)'), [
    { type: 'paragraph', children: [
      { type: 'strong', text: 'first\nsecond' },
      { type: 'text', text: ' ' },
      { type: 'code', text: 'third\nfourth' },
      { type: 'text', text: ' ' },
      { type: 'link', text: 'fifth\nsixth', href: 'https://example.test' },
    ] },
  ]);
});

test('only existing inline grammar is recognized and link destinations stay unsanitized in the AST', () => {
  for (const href of ['https://example.test', 'http://example.test/a?q=b#c', 'https://user:pass@example.test/private', 'https://?', 'https://example.test/\u0000']) {
    assert.deepEqual(parseMarkdownPresentation(`[label](${href})`), [
      { type: 'paragraph', children: [{ type: 'link', text: 'label', href }] },
    ]);
  }
  for (const source of [
    '[label](javascript:alert(1))', '[label](data:text/html,payload)', '[label](/relative)',
    '[label](HTTP://example.test)', '[label](https://example.test/has space)', '[label](https://)', '[](https://example.test)',
    '**unfinished', '****', '``', '*italic*', '_italic_', '~~strike~~', '<script>alert(1)</script> &amp;',
  ]) {
    assert.deepEqual(parseMarkdownPresentation(source), [{ type: 'paragraph', children: [{ type: 'text', text: source }] }], source);
  }
  assert.deepEqual(parseMarkdownPresentation('![label](https://example.test/image) [paren](https://example.test/a_(b))'), [
    { type: 'paragraph', children: [
      { type: 'text', text: '!' },
      { type: 'link', text: 'label', href: 'https://example.test/image' },
      { type: 'text', text: ' ' },
      { type: 'link', text: 'paren', href: 'https://example.test/a_(b' },
      { type: 'text', text: ')' },
    ] },
  ]);
});

test('linear inline scanning exactly matches the previous regex on small finite inputs', () => {
  const compare = (content: string) => {
    const source = `prefix ${content} suffix`;
    assert.deepEqual(parseMarkdownPresentation(source), [{ type: 'paragraph', children: referenceInline(source) }], JSON.stringify(content));
  };
  let samples = [''];
  for (let length = 0; length < 5; length++) {
    samples = samples.flatMap(sample => ['*', '`', '[', ']', 'a', ' '].map(character => sample + character));
    for (const sample of samples) compare(sample);
  }
  const fragments = [
    '**bold**', '**broken*', '`code`', '``', '[label](https://example.test)',
    '[outer[inner](http://example.test/a_(b))', '[label](https://?)', '[label](https://)',
    '[](http://example.test)', '[label](HTTP://example.test)', '[label](https://example.test/a b)',
    '[', ']', '](', ')', '**', '*', '`', 'plain',
  ];
  for (const first of fragments) for (const second of fragments) compare(first + second);
  for (const whitespace of [' ', '\t', '\n', '\r', '\f', '\v', '\u0085', '\u00a0', '\u2028', '\u2029', '\ufeff', '\u0000']) {
    compare(`[label](https://example.test/${whitespace})`);
    compare(`[label${whitespace}tail](https://example.test)`);
  }
});

test('JavaScript line, whitespace, heading and fence semantics remain unchanged', () => {
  assert.deepEqual(parseMarkdownPresentation('\u00a0\n\ufeff\n\t\r\n\u2028\n'), []);
  assert.deepEqual(parseMarkdownPresentation('before\r\nsecond\rafter'), [
    { type: 'paragraph', children: [{ type: 'text', text: 'before\r\nsecond\rafter' }] },
  ]);
  assert.deepEqual(parseMarkdownPresentation('#\u00a0Title\r\n# \n- \n1) '), [
    { type: 'paragraph', children: [{ type: 'text', text: '#\u00a0Title\r' }] },
    { type: 'heading', level: 1, children: [] },
    { type: 'list', ordered: false, items: [[]] },
    { type: 'list', ordered: true, items: [[]] },
  ]);
  assert.deepEqual(parseMarkdownPresentation('#\u00a0Title'), [{ type: 'heading', level: 1, children: [{ type: 'text', text: 'Title' }] }]);
  assert.deepEqual(parseMarkdownPresentation('\ufeff  ````ignored\n  **literal**\r\n\u00a0```suffix\n```unclosed\nlast\n'), [
    { type: 'pre', text: '  **literal**\r' },
    { type: 'pre', text: 'last\n' },
  ]);
  for (const source of [' # indented', '#missing', '#### fourth', '-missing', '+ unsupported', '1.missing', '1] unsupported', '\u0085']) {
    assert.deepEqual(parseMarkdownPresentation(source), [{ type: 'paragraph', children: [{ type: 'text', text: source }] }]);
  }
});

test('malformed headings become literal paragraphs and always advance', () => {
  for (const separator of ['\u2028', '\u2029', '\r']) {
    const malformed = `# before${separator}after`;
    assert.deepEqual(parseMarkdownPresentation(malformed), [{ type: 'paragraph', children: [{ type: 'text', text: malformed }] }]);
    assert.deepEqual(parseMarkdownPresentation(`before\n${malformed}\nafter\n## valid`), [
      { type: 'paragraph', children: [{ type: 'text', text: `before\n${malformed}\nafter` }] },
      { type: 'heading', level: 2, children: [{ type: 'text', text: 'valid' }] },
    ]);
    assert.deepEqual(parseMarkdownPresentation(`# before${separator}`), [
      { type: 'paragraph', children: [{ type: 'text', text: `# before${separator}` }] },
    ]);
    assert.deepEqual(parseMarkdownPresentation(`# ${separator}after`), [
      { type: 'heading', level: 1, children: [{ type: 'text', text: 'after' }] },
    ]);
  }
});

test('renderer keeps existing element structure, literal content and URL security', () => {
  const render = (text: string) => renderToStaticMarkup(React.createElement(Markdown, { text }));
  assert.equal(render('# First\n## Second\n### Third\n- **bold**\n* `code`\n1) [link](https://example.test)\n```\n<literal>\n```\nlast'),
    '<div class="md"><h4>First</h4><h5>Second</h5><h6>Third</h6><ul><li><strong>bold</strong></li><li><code>code</code></li></ul><ol><li><a href="https://example.test/" target="_blank" rel="noopener noreferrer">link</a></li></ol><pre>&lt;literal&gt;</pre><p>last</p></div>');
  assert.equal(render('[credentials](https://user:pass@example.test/private) [invalid](https://?)'), '<div class="md"><p>credentials invalid</p></div>');
  assert.equal(render('<script>alert(1)</script> [label](javascript:alert(1))'), '<div class="md"><p>&lt;script&gt;alert(1)&lt;/script&gt; [label](javascript:alert(1))</p></div>');
  assert.equal(render('# before\u2028after'), '<div class="md"><p># before\u2028after</p></div>');
});

test('display projection follows container breaks and DOM whitespace rather than source line normalization', () => {
  assert.equal(markdownDisplayText('first\n\nsecond\n# heading\n- item\n```\ncode\n```\nlast'), 'first\n\nsecond\n\nheading\nitem\ncode\n\nlast');
  assert.equal(markdownDisplayText('before\n```\n```\n# \n- \n# after'), 'before\n\nafter');
  assert.equal(markdownDisplayText('```\n\nfirst\n\n```\n# after'), '\nfirst\n\nafter');
  assert.equal(markdownDisplayText('first\r\nsecond\rthird'), 'first\r\nsecond\rthird');
  assert.equal(markdownDisplayText('```\n  first\r\n\tsecond\rthird\n```'), '  first\r\n\tsecond\rthird');
  assert.equal(markdownDisplayText('# first\fsecond\u000bthird\u00a0\n- alpha\tbeta\rnext\fend'), 'first\fsecond\u000bthird\u00a0\nalpha beta next\fend');
  assert.equal(markdownDisplayText('[credentials](https://user:pass@example.test) [invalid](https://?)'), 'credentials invalid');
});

test('display projection matches the shared markdown display v1 fixture', () => {
  const fixture = loadMarkdownDisplayFixture();
  assert.equal(fixture.version, 1);
  assert.ok(fixture.cases.length > 0);
  for (const entry of fixture.cases) assert.equal(markdownDisplayText(entry.source), entry.text, entry.name);
});

test('standalone frontend exports retain the shared presentation fixture without a parent monorepo', () => {
  const root = mkdtempSync(join(tmpdir(), 'cxt-markdown-fixture-'));
  const fixture = loadMarkdownDisplayFixture();
  try {
    mkdirSync(join(root, 'tests/fixtures'), { recursive: true });
    writeFileSync(join(root, 'tests/fixtures/markdown-display-v1.json'), JSON.stringify(fixture));
    assert.deepEqual(loadMarkdownDisplayFixture(root), fixture);
  } finally { rmSync(root, { recursive: true, force: true }); }
});

test('standalone event and tool display fixtures retain their complete source and expected content', () => {
  const root = mkdtempSync(join(tmpdir(), 'cxt-event-fixture-'));
  try {
    mkdirSync(join(root, 'tests/fixtures'), { recursive: true });
    for (const name of ['event-display-v1', 'tool-display-v1'] as const) {
      const fixture = loadDisplayFixture(name);
      writeFileSync(join(root, `tests/fixtures/${name}.json`), JSON.stringify(fixture));
      assert.deepEqual(loadDisplayFixture(name, root), fixture);
    }
  } finally {
    rmSync(root, { force: true, recursive: true });
  }
});

test('multiple presentation fixture copies cannot silently disagree', () => {
  const root = mkdtempSync(join(tmpdir(), 'cxt-markdown-fixture-drift-'));
  const readerRoot = join(root, 'frontend/web');
  const fixture = loadMarkdownDisplayFixture();
  try {
    mkdirSync(join(root, 'schemas/fixtures'), { recursive: true });
    mkdirSync(join(readerRoot, 'tests/fixtures'), { recursive: true });
    const source = JSON.stringify(fixture);
    writeFileSync(join(root, 'schemas/fixtures/markdown-display-v1.json'), source);
    writeFileSync(join(readerRoot, 'tests/fixtures/markdown-display-v1.json'), source);
    assert.deepEqual(loadMarkdownDisplayFixture(readerRoot), fixture);
    writeFileSync(join(readerRoot, 'tests/fixtures/markdown-display-v1.json'), source + '\n');
    assert.throws(() => loadMarkdownDisplayFixture(readerRoot), /copies disagree/);
  } finally { rmSync(root, { recursive: true, force: true }); }
});

test('long input makes one advancing pass through paragraphs, lists and fenced lines', { timeout: 10_000 }, () => {
  const count = 30_000;
  const paragraph = Array.from({ length: count }, (_, index) => `# before\u2028after ${index}`).join('\n');
  assert.deepEqual(parseMarkdownPresentation(paragraph), [{ type: 'paragraph', children: [{ type: 'text', text: paragraph }] }]);
  const items = Array.from({ length: count }, (_, index) => `- item ${index}`).join('\n');
  const list = parseMarkdownPresentation(items);
  assert.equal(list.length, 1);
  assert.equal(list[0].type, 'list');
  if (list[0].type !== 'list') assert.fail('long list must remain one list');
  assert.equal(list[0].items.length, count);
  assert.deepEqual(list[0].items[count - 1], [{ type: 'text', text: `item ${count - 1}` }]);
  assert.equal(markdownDisplayText(items), items.replace(/^- /gm, ''));
  assert.deepEqual(parseMarkdownPresentation(`\`\`\`\n${paragraph}\n\`\`\``), [{ type: 'pre', text: paragraph }]);
});

test('long unmatched inline delimiters and URL suffixes are bounded linear scans', { timeout: 10_000 }, () => {
  for (const content of [
    '['.repeat(128 * 1024), '`'.repeat(128 * 1024), '*'.repeat(128 * 1024),
    '[label](https://'.repeat(10_000), '**missing* '.repeat(16_384),
  ]) {
    const source = `prefix ${content} suffix`;
    assert.deepEqual(parseMarkdownPresentation(source), [{ type: 'paragraph', children: [{ type: 'text', text: source }] }]);
    assert.equal(markdownDisplayText(source), source);
  }
});
