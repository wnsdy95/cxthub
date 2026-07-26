// GitHub style location breadcrumb: {owner} / [lock]{workspace} [▾].
// Lock icon is on the left (private). workspaces/onSelect opens a workspace switch dropdown — search input, current display (✓), private (lock)/public (repo) icon, keyboard navigation (↑↓/Enter/Esc).
import { useEffect, useMemo, useState } from 'react';
import type { Workspace } from '../types';
import { navigate } from '../route';
import { useT } from '../i18n';

export function LockIcon({ className }: { className?: string }) {
  // octicon lock — currentColor inherits theme color.
  return (
    <svg viewBox="0 0 16 16" width="13" height="13" fill="currentColor" aria-hidden="true" className={className}>
      <path d="M4 4a4 4 0 0 1 8 0v2h.25c.966 0 1.75.784 1.75 1.75v5.5A1.75 1.75 0 0 1 12.25 15h-8.5A1.75 1.75 0 0 1 2 13.25v-5.5C2 6.784 2.784 6 3.75 6H4Zm8.25 3.5h-8.5a.25.25 0 0 0-.25.25v5.5c0 .138.112.25.25.25h8.5a.25.25 0 0 0 .25-.25v-5.5a.25.25 0 0 0-.25-.25ZM10.5 6V4a2.5 2.5 0 1 0-5 0v2Z" />
    </svg>
  );
}

function RepoIcon({ className }: { className?: string }) {
  // octicon repo — public workspace display (lock counterpart).
  return (
    <svg viewBox="0 0 16 16" width="13" height="13" fill="currentColor" aria-hidden="true" className={className}>
      <path d="M2 2.5A2.5 2.5 0 0 1 4.5 0h8.75a.75.75 0 0 1 .75.75v12.5a.75.75 0 0 1-.75.75h-2.5a.75.75 0 0 1 0-1.5h1.75v-2h-8a1 1 0 0 0-.714 1.7.75.75 0 1 1-1.072 1.05A2.495 2.495 0 0 1 2 11.5Zm10.5-1h-8a1 1 0 0 0-1 1v6.708A2.486 2.486 0 0 1 4.5 9h8ZM5 12.25v3.25a.25.25 0 0 0 .4.2l1.45-1.087a.25.25 0 0 1 .3 0L8.6 15.7a.25.25 0 0 0 .4-.2v-3.25a.25.25 0 0 0-.25-.25h-3.5a.25.25 0 0 0-.25.25Z" />
    </svg>
  );
}

function priv(w: Workspace): boolean {
  return w.visibility !== 'public';
}

export function Breadcrumb({
  owner,
  name,
  isPrivate,
  workspaces,
  currentId,
  onSelect,
}: {
  owner: string;
  name: string;
  isPrivate?: boolean;
  workspaces?: Workspace[]; // If provided, opens a workspace switch dropdown (omits login public read).
  currentId?: string;
  onSelect?: (ws: Workspace) => void;
}) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [active, setActive] = useState(0);
  const hasSwitch = Boolean(workspaces && workspaces.length > 0 && onSelect);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    const list = workspaces ?? [];
    if (!q) return list;
    return list.filter((w) => w.name.toLowerCase().includes(q) || w.owner_username.toLowerCase().includes(q));
  }, [workspaces, query]);

  // Initializes search and highlight on open (current workspace as active item).
  useEffect(() => {
    if (!open) return;
    setQuery('');
    const idx = (workspaces ?? []).findIndex((w) => w.id === currentId);
    setActive(idx >= 0 ? idx : 0);
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && setOpen(false);
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open, workspaces, currentId]);

  // Resets highlight to first result when search term changes.
  useEffect(() => {
    setActive(0);
  }, [query]);

  function choose(w: Workspace | undefined) {
    if (!w) return;
    onSelect!(w);
    setOpen(false);
  }

  function onSearchKey(e: React.KeyboardEvent) {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setActive((a) => Math.min(a + 1, filtered.length - 1));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setActive((a) => Math.max(a - 1, 0));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      choose(filtered[active]);
    }
  }

  return (
    <nav className="topbar-crumb" aria-label={t('common.currentLocation')}>
      <button type="button" className="crumb-owner" onClick={() => navigate(`/${owner}`)} title={t('common.profileOf', { name: owner })}>
        {owner}
      </button>
      <span className="crumb-sep">/</span>
      <span className="crumb-ws" title={`${owner}/${name}`}>
        {isPrivate && <LockIcon className="crumb-lock" />}
        <span className="crumb-name">{name}</span>
      </span>
      {hasSwitch && (
        <div className="crumb-switch">
          <button
            type="button"
            className="crumb-caret"
            onClick={() => setOpen((v) => !v)}
            aria-label={t('common.switchWorkspace')}
            aria-expanded={open}
          >
            ▾
          </button>
          {open && (
            <>
              <div className="dropdown-backdrop" onClick={() => setOpen(false)} />
              <div className="crumb-menu" role="dialog" aria-label={t('common.switchWorkspace')}>
                <div className="crumb-menu-head">{t('common.switchWorkspace')}</div>
                <input
                  className="crumb-search"
                  placeholder={t('common.searchWorkspace')}
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  onKeyDown={onSearchKey}
                  autoFocus
                  spellCheck={false}
                  aria-label={t('common.searchWorkspace')}
                />
                <ul className="crumb-menu-list" role="menu">
                  {filtered.map((w, i) => (
                    <li key={w.id} role="none">
                      <button
                        type="button"
                        role="menuitem"
                        className={`crumb-menu-item${i === active ? ' active' : ''}${w.id === currentId ? ' on' : ''}`}
                        onMouseEnter={() => setActive(i)}
                        onClick={() => choose(w)}
                      >
                        <span className="crumb-check">{w.id === currentId ? '✓' : ''}</span>
                        {priv(w) ? <LockIcon className="crumb-type" /> : <RepoIcon className="crumb-type" />}
                        <span className="crumb-menu-name">{w.name}</span>
                        <span className="crumb-menu-owner">{w.owner_username}</span>
                      </button>
                    </li>
                  ))}
                  {filtered.length === 0 && <li className="crumb-menu-empty">{t('common.noWorkspaceMatch')}</li>}
                </ul>
              </div>
            </>
          )}
        </div>
      )}
    </nav>
  );
}
