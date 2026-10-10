import type { ReactNode } from 'react';
import { useMe } from '../hooks';
import { navigate } from '../route';
import { useT } from '../i18n';
import { Logo } from './Logo';
import { LocaleSwitcher } from './LocaleSwitcher';
import { HeaderActions } from './HeaderActions';
import { AppLink } from './AppLink';

export function MarketingLink({ to, current, children }: { to: string; current?: boolean; children: ReactNode }) {
  return <AppLink href={to} aria-current={current ? 'page' : undefined}>{children}</AppLink>;
}

export function MarketingHeader({ onSignIn, children }: { onSignIn?: () => void; children: ReactNode }) {
  const t = useT();
  const me = useMe().data;
  return (
    <header className="landing-header">
      <button className="linkish-logo" onClick={() => navigate('/')} aria-label={t('common.home')}>
        <div className="brand sm"><Logo /></div>
      </button>
      <nav className="landing-nav" aria-label={t('landing.navLabel')}>{children}</nav>
      <div className="who">
        <LocaleSwitcher />
        {me ? (
          <HeaderActions user={me} />
        ) : (
          <>
            <button className="ghost" onClick={onSignIn}>{t('common.signIn')}</button>
            <button className="btn-primary" onClick={onSignIn}>{t('common.signUp')}</button>
          </>
        )}
      </div>
    </header>
  );
}

export function MarketingFooter() {
  const t = useT();
  return (
    <footer className="landing-footer">
      <span className="brand sm"><Logo /></span>
      <span>cxthub — coding agent context, on git.</span>
      <MarketingLink to="/pricing">{t('landing.navPricing')}</MarketingLink>
      <a href="https://github.com/wnsdy95/cxthub" target="_blank" rel="noreferrer">GitHub ↗</a>
    </footer>
  );
}
