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
  const active = new URLSearchParams(window.location.search).get('active') === '1';
  const reader = reading ? createElement(DocEvents, { repoId: 'repo', hash: 'document', base: 'base', start: -1, mode: 'chat' }) : null;
  return createElement('div', { className: 'app' },
    createElement('header', { className: 'topbar' },
      createElement('button', { onClick: () => setReading(previous => !previous) },
        reading ? 'Pause document readers' : 'Resume document readers')),
    createElement('main', { style: { padding: 16, minWidth: 0 } },
      createElement('div', { className: 'ctx-main', 'data-testid': 'document-scroll' },
        active ? createElement('div', { className: 'viewer' },
          createElement('div', { className: 'viewer-head' }, 'Synthetic context'), reader) : reader)));
}

createRoot(document.getElementById('root')!).render(
  createElement(QueryClientProvider, { client }, createElement(I18nProvider, null, createElement(Harness))));
