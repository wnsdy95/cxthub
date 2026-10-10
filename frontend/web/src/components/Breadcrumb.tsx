// GitHub style location breadcrumb: {owner} / [lock]{repository} [▾].
// Lock icon is on the left (private). repositories/onSelect opens a repository switch dropdown — search input, current display (✓), private (lock)/public (repo) icon, keyboard navigation (↑↓/Enter/Esc).
import { useEffect, useId, useMemo, useRef, useState } from 'react';
import type { Repository } from '../types';
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
  // octicon repo — public repository display (lock counterpart).
  return (
    <svg viewBox="0 0 16 16" width="13" height="13" fill="currentColor" aria-hidden="true" className={className}>
      <path d="M2 2.5A2.5 2.5 0 0 1 4.5 0h8.75a.75.75 0 0 1 .75.75v12.5a.75.75 0 0 1-.75.75h-2.5a.75.75 0 0 1 0-1.5h1.75v-2h-8a1 1 0 0 0-.714 1.7.75.75 0 1 1-1.072 1.05A2.495 2.495 0 0 1 2 11.5Zm10.5-1h-8a1 1 0 0 0-1 1v6.708A2.486 2.486 0 0 1 4.5 9h8ZM5 12.25v3.25a.25.25 0 0 0 .4.2l1.45-1.087a.25.25 0 0 1 .3 0L8.6 15.7a.25.25 0 0 0 .4-.2v-3.25a.25.25 0 0 0-.25-.25h-3.5a.25.25 0 0 0-.25.25Z" />
    </svg>
  );
}

function priv(w: Repository): boolean {
  return w.visibility !== 'public';
}

export function Breadcrumb({
  owner,
  name,
  repository,
  isPrivate,
  repositories,
  currentId,
  onSelect,
}: {
  owner: string;
  name: string;
  repository?: string;
  isPrivate?: boolean;
  repositories?: Repository[]; // If provided, opens a repository switch dropdown (omits login public read).
  currentId?: string;
  onSelect?: (repositoryMetadata: Repository) => void;
}) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [active, setActive] = useState(0);
  const hasSwitch = Boolean(repositories && onSelect);
  const menuId = useId();
  const container = useRef<HTMLElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const list = useRef<HTMLUListElement>(null);

  const filtered = useMemo(() => {
    const search = query.trim().toLowerCase();
    const entries = repositories ?? [];
    if (!search) return entries;
    return entries.filter((entry) => `${entry.owner_username}/${entry.name} ${entry.slug}`.toLowerCase().includes(search));
  }, [repositories, query]);
  const activeIndex = Math.min(active, Math.max(0, filtered.length - 1));

  useEffect(() => {
    if (!open) return;
    const onOutside = (event: PointerEvent) => {
      if (!container.current?.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener('pointerdown', onOutside);
    return () => document.removeEventListener('pointerdown', onOutside);
  }, [open]);

  useEffect(() => {
    if (open) list.current?.children[activeIndex]?.scrollIntoView({ block: 'nearest' });
  }, [open, activeIndex, query]);

  function close() {
    setOpen(false);
    trigger.current?.focus();
  }

  function choose(entry: Repository | undefined) {
    if (!entry) return;
    close();
    onSelect?.(entry);
  }

  function onSearchKey(e: React.KeyboardEvent) {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setActive(Math.min(activeIndex + 1, Math.max(0, filtered.length - 1)));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setActive(Math.max(activeIndex - 1, 0));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      choose(filtered[activeIndex]);
    }
  }

  return (
    <nav ref={container} className="topbar-crumb" aria-label={t('common.currentLocation')} onKeyDown={(event) => {
      if (open && event.key === 'Escape') {
        event.preventDefault();
        event.stopPropagation();
        close();
      }
    }} onBlur={(event) => {
      if (event.relatedTarget && !event.currentTarget.contains(event.relatedTarget as Node)) setOpen(false);
    }}>
      <button type="button" className="crumb-owner" onClick={() => navigate(`/${owner}`)} title={t('common.profileOf', { name: owner })}>
        {owner}
      </button>
      <span className="crumb-sep">/</span>
      {!hasSwitch && <span className="crumb-repositoryMetadata" title={`${owner}/${name}`}>
        {isPrivate && <LockIcon className="crumb-lock" />}
        <span className="crumb-name">{name}</span>
      </span>}
      {hasSwitch && (
        <div className="crumb-switch">
          <button
            type="button"
            ref={trigger}
            className="crumb-trigger"
            title={`${owner}/${name}`}
            onClick={() => {
              if (!open) {
                setQuery('');
                setActive(Math.max(0, repositories!.findIndex((entry) => entry.id === currentId)));
              }
              setOpen(!open);
            }}
            aria-label={t('common.switchRepository')}
            aria-expanded={open}
            aria-haspopup="dialog"
            aria-controls={open ? menuId : undefined}
          >
            {isPrivate && <LockIcon className="crumb-lock" />}
            <span className="crumb-name">{name}</span>
            <span className="crumb-chevron" aria-hidden="true">▾</span>
          </button>
          {open && (
              <div id={menuId} className="crumb-menu" role="dialog" aria-label={t('common.switchRepository')}>
                <div className="crumb-menu-head">{t('common.switchRepository')}</div>
                <input
                  className="crumb-search"
                  placeholder={t('common.searchRepository')}
                  value={query}
                  onChange={(event) => { setQuery(event.target.value); setActive(0); }}
                  onKeyDown={onSearchKey}
                  autoFocus
                  spellCheck={false}
                  aria-label={t('common.searchRepository')}
                  role="combobox"
                  aria-expanded="true"
                  aria-controls={`${menuId}-list`}
                  aria-autocomplete="list"
                  aria-activedescendant={filtered.length ? `${menuId}-option-${activeIndex}` : undefined}
                />
                <ul ref={list} id={`${menuId}-list`} className="crumb-menu-list" role="listbox" aria-label={t('common.repositories')}>
                  {filtered.map((entry, index) => (
                    <li key={entry.id} role="none">
                      <button
                        type="button"
                        id={`${menuId}-option-${index}`}
                        role="option"
                        aria-selected={entry.id === currentId}
                        tabIndex={-1}
                        title={`${entry.owner_username}/${entry.name} · ${t(priv(entry) ? 'common.private' : 'common.public')}`}
                        className={`crumb-menu-item${index === activeIndex ? ' active' : ''}${entry.id === currentId ? ' on' : ''}`}
                        onMouseEnter={() => setActive(index)}
                        onClick={() => choose(entry)}
                      >
                        <span className="crumb-check" aria-hidden="true">{entry.id === currentId ? '✓' : ''}</span>
                        {priv(entry) ? <LockIcon className="crumb-type" /> : <RepoIcon className="crumb-type" />}
                        <span className="crumb-menu-name">{entry.name}</span>
                        <span className="crumb-menu-owner">{entry.owner_username}</span>
                      </button>
                    </li>
                  ))}
                </ul>
                {filtered.length === 0 && <div className="crumb-menu-empty" role="status">{t('common.noRepositoryMatch')}</div>}
              </div>
          )}
        </div>
      )}
      {repository && (
        <>
          <span className="crumb-sep">/</span>
          <span className="crumb-repo" title={repository}>{repository}</span>
        </>
      )}
    </nav>
  );
}
