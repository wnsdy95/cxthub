import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './App';
import { I18nProvider } from './i18n';
import { upgradeLegacyHash } from './route';
import './styles.css';

// Convert old hash links (#/…) to actual paths (bookmark and share link compatibility).
upgradeLegacyHash();

const el = document.getElementById('root');
if (!el) throw new Error('#root not found');
createRoot(el).render(
  <StrictMode>
    <I18nProvider>
      <App />
    </I18nProvider>
  </StrictMode>,
);
