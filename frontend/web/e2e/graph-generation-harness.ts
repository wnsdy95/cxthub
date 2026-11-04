import { createElement } from 'react';
import { createRoot } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ContextView } from '../src/components/ContextView';
import { OnHoldView } from '../src/components/OnHoldView';
import { I18nProvider } from '../src/i18n';
import '../src/styles.css';

const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
Object.assign(window, { graphQueryClient: client });
const repo = { id: 'repo', remote_url: '', default_branch: 'main' };
const view = new URLSearchParams(location.search).has('onhold')
  ? createElement(OnHoldView, { repo, role: null })
  : createElement(ContextView, { repo, role: null, repositoryMetadata: null });
createRoot(document.getElementById('root')!).render(
  createElement(QueryClientProvider, { client }, createElement(I18nProvider, null, view)));
