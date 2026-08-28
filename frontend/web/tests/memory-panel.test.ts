import assert from 'node:assert/strict';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { I18nProvider, translate } from '../src/i18n/index.tsx';
import { MemoryPanel } from '../src/components/MemoryPanel.tsx';
import type { MemoryDigest } from '../src/types.ts';

const memory: MemoryDigest = {
  snapshot_id: `sha256:${'1'.repeat(64)}`,
  summary: 'Archived summary body.',
  key_facts: ['Stored historical fact.'],
  open_tasks: ['Finished historical task.'],
  provider: 'codex',
};

const html = renderToStaticMarkup(
  createElement(I18nProvider, null, createElement(MemoryPanel, { memory })),
);

assert.match(html, /data-memory-representation="archival"/);
assert.match(html, /Stored memory at this snapshot/);
assert.match(html, /immutable stored MemoryDigest/);
assert.match(html, /Stored facts/);
assert.match(html, /Tasks recorded at this snapshot/);
assert.match(html, /Finished historical task\./, 'archived task records remain inspectable');
assert.doesNotMatch(html, /☐/, 'archived records must not look like current unchecked tasks');
assert.match(
  translate('ko', 'context.compactMemory'),
  /\uC774 \uC2DC\uC810\uC5D0 \uC800\uC7A5\uB41C \uBA54\uBAA8\uB9AC/,
);
assert.match(
  translate('ko', 'context.memoryArchiveNote'),
  /\uC2E4\uC81C \uB2E4\uC74C \uC5D0\uC774\uC804\uD2B8 \uD504\uB86C\uD504\uD2B8/,
);
