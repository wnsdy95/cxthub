import { useEffect, useId, useRef, useState } from 'react';
import { accountCreationPath, repositoryCreationPath } from '../route';
import { useT } from '../i18n';
import { AppLink } from './AppLink';

export function CreateMenu() {
  const t = useT();
  const [open, setOpen] = useState(false);
  const container = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const items = useRef<Array<HTMLAnchorElement | null>>([]);
  const initialIndex = useRef(0);
  const menuId = useId();
  const actions = [
    { label: t('createMenu.repository'), href: repositoryCreationPath(), icon: 'M4 3h11v14H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2Zm0 10h11M5 6h6M5 9h4' },
    { label: t('createMenu.organization'), href: accountCreationPath('organizations'), icon: 'M3 18V2h10v16M13 8h4v10M6 5h1m2 0h1M6 8h1m2 0h1M6 11h1m2 0h1M6 18v-4h4v4M1 18h18' },
    { label: t('createMenu.enterprise'), href: accountCreationPath('enterprises'), icon: 'M2 7h16v10H2zM6 7V3h8v4M2 11h16M8 11v3h4v-3' },
  ];

  useEffect(() => {
    if (!open) return;
    items.current[initialIndex.current]?.focus();
    const close = () => setOpen(false);
    const outside = (event: PointerEvent) => {
      if (!container.current?.contains(event.target as Node)) close();
    };
    document.addEventListener('pointerdown', outside);
    window.addEventListener('popstate', close);
    return () => {
      document.removeEventListener('pointerdown', outside);
      window.removeEventListener('popstate', close);
    };
  }, [open]);

  return <div className="create-menu" ref={container} onBlur={(event) => {
    if (!event.currentTarget.contains(event.relatedTarget)) setOpen(false);
  }} onKeyDown={(event) => {
    if (event.key === 'Escape' && open) {
      event.preventDefault();
      event.stopPropagation();
      setOpen(false);
      trigger.current?.focus();
    }
  }}>
    <button type="button" className="ghost create-menu-trigger" ref={trigger} aria-label={t('createMenu.title')} title={t('createMenu.title')} aria-haspopup="menu" aria-expanded={open} aria-controls={open ? menuId : undefined} onClick={() => {
      initialIndex.current = 0;
      setOpen(!open);
    }} onKeyDown={(event) => {
      if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
        event.preventDefault();
        initialIndex.current = event.key === 'ArrowUp' ? actions.length - 1 : 0;
        if (open) items.current[initialIndex.current]?.focus();
        else setOpen(true);
      }
    }}>
      <svg width="18" height="18" viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true"><path d="M10 3v14M3 10h14" /></svg>
      <span aria-hidden="true">▾</span>
    </button>
    {open && <div className="create-menu-panel" role="menu" id={menuId} aria-label={t('createMenu.title')} onKeyDown={(event) => {
      const index = items.current.findIndex((item) => item === document.activeElement);
      const next = event.key === 'ArrowDown' ? (index + 1) % actions.length
        : event.key === 'ArrowUp' ? (index + actions.length - 1) % actions.length
        : event.key === 'Home' ? 0 : event.key === 'End' ? actions.length - 1 : null;
      if (next !== null) {
        event.preventDefault();
        items.current[next]?.focus();
      } else if (event.key === ' ') {
        event.preventDefault();
        items.current[index]?.click();
      }
    }}>
      {actions.map((action, index) => <AppLink key={action.href} href={action.href} role="menuitem" tabIndex={-1} elementRef={(element) => { items.current[index] = element; }}>
        <svg width="20" height="20" viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinejoin="round" aria-hidden="true"><path d={action.icon} /></svg>
        {action.label}
      </AppLink>)}
    </div>}
  </div>;
}
