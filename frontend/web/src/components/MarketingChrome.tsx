import type { MouseEvent, ReactNode } from 'react';
import { useMe, useLogout } from '../hooks';
import { navigate } from '../route';
import { useT } from '../i18n';
import { Logo } from './Logo';
import { Avatar } from './Avatar';
import { LocaleSwitcher } from './LocaleSwitcher';
import { AccountSettings } from './Settings';

export function MarketingLink({ to, current, children }: { to: string; current?: boolean; children: ReactNode }) {
  function follow(e: MouseEvent<HTMLAnchorElement>) {
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    navigate(to);
  }
  return <a href={to} aria-current={current ? 'page' : undefined} onClick={follow}>{children}</a>;
}

export function MarketingHeader({ onSignIn, children }: { onSignIn?: () => void; children: ReactNode }) {
  const t = useT();
  const me = useMe().data;
  const logout = useLogout();
  return (
    <header className="landing-header">
      <button className="linkish-logo" onClick={() => navigate('/')} aria-label={t('common.home')}>
        <div className="brand sm"><Logo /></div>
      </button>
      <nav className="landing-nav" aria-label={t('landing.navLabel')}>{children}</nav>
      <div className="who">
        <LocaleSwitcher />
        {me ? (
          <>
            <Avatar user={me} link />
            <span>{me.nickname || me.name || me.email}</span>
            <AccountSettings user={me} />
            <button className="ghost" onClick={() => logout.mutate()} disabled={logout.isPending}>
              {t('common.logout')}
            </button>
          </>
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
