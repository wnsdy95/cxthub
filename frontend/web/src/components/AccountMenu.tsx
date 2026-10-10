import { useEffect, useId, useRef, useState } from 'react';
import { useLogout } from '../hooks';
import { useT } from '../i18n';
import { accountPath } from '../route';
import type { User } from '../types';
import { Avatar } from './Avatar';
import { AppLink } from './AppLink';

export function AccountMenu({ user }: { user: User }) {
  const t = useT();
  const logout = useLogout();
  const [open, setOpen] = useState(false);
  const container = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const panelId = useId();

  useEffect(() => {
    if (!open) return;
    const close = () => setOpen(false);
    const outside = (event: PointerEvent) => {
      if (!container.current?.contains(event.target as Node)) close();
    };
    const escape = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        close();
        trigger.current?.focus();
      }
    };
    document.addEventListener('pointerdown', outside);
    document.addEventListener('keydown', escape);
    window.addEventListener('popstate', close);
    return () => {
      document.removeEventListener('pointerdown', outside);
      document.removeEventListener('keydown', escape);
      window.removeEventListener('popstate', close);
    };
  }, [open]);

  return <div className="account-menu" ref={container} onBlur={(event) => {
    if (!event.currentTarget.contains(event.relatedTarget)) setOpen(false);
  }}>
    <button type="button" className="account-menu-trigger ghost" ref={trigger} aria-label={t('accountNav.menu')} aria-expanded={open} aria-controls={panelId} onClick={() => setOpen(!open)}>
      <Avatar user={user} /><span className="account-menu-name">{user.nickname || user.name || user.username}</span><span aria-hidden="true">▾</span>
    </button>
    {open && <nav className="account-menu-panel" id={panelId} aria-label={t('accountNav.menu')}>
      <span className="account-menu-identity">@{user.username}</span>
      <AppLink href={`/${encodeURIComponent(user.username)}`}>{t('accountNav.profile')}</AppLink>
      <AppLink href={accountPath('organizations')}>{t('accountNav.organizations')}</AppLink>
      <AppLink href={accountPath('enterprises')}>{t('accountNav.enterprises')}</AppLink>
      <AppLink href={accountPath('account')}>{t('settings.account')}</AppLink>
      <button type="button" className="ghost account-menu-logout" disabled={logout.isPending} onClick={() => logout.mutate()}>{t('common.logout')}</button>
    </nav>}
  </div>;
}
