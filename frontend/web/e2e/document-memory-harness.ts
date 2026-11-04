import { createElement, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { DocEvents } from '../src/components/DocEvents';
import { I18nProvider } from '../src/i18n';
import '../src/styles.css';

const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
Object.assign(window, { documentQueryClient: client });

function Harness() {
  const [reading, setReading] = useState(false);
  return createElement('main', null,
    createElement('button', { onClick: () => setReading(previous => !previous) },
      reading ? 'Pause document readers' : 'Resume document readers'),
    createElement('div', { 'data-testid': 'document-scroll',
      style: { height: 500, width: '100%', maxWidth: 900, overflowY: 'auto' } },
      reading ? createElement(DocEvents, { repoId: 'repo', hash: 'document', base: 'base', start: -1, mode: 'chat' }) : null));
}

createRoot(document.getElementById('root')!).render(
  createElement(QueryClientProvider, { client }, createElement(I18nProvider, null, createElement(Harness))));
