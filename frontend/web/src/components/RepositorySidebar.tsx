import type { Repo, Repository } from '../types';
import { atLeast, canWriteAsset, type Role } from '../roles';
import { useUiStore } from '../store';
import { useT } from '../i18n';
import { About, SecretsPanel, TeamSettings } from './About';

export function RepositorySidebarToggle() {
  const t = useT();
  const open = useUiStore((state) => state.repositoryDetailsOpen);
  const setOpen = useUiStore((state) => state.setRepositoryDetailsOpen);
  const label = t(open ? 'repositorySidebar.hide' : 'repositorySidebar.show');
  return <button type="button" id="repository-details-toggle" className="ghost repository-side-toggle" aria-label={label} title={label} aria-expanded={open} aria-controls="repository-details" onClick={() => setOpen(!open)}>
    <svg width="18" height="18" viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
      <rect x="2" y="3" width="16" height="14" rx="2" />
      <path d="M12 3v14" />
      {open && <path d="M14 6h2m-2 3h2m-2 3h2" />}
    </svg>
    <span>{t('repositorySidebar.title')}</span>
  </button>;
}

export function RepositorySidebar({ repo, repositoryMetadata, role }: {
  repo: Repo;
  repositoryMetadata: Pick<Repository, 'visibility'> & Partial<Pick<Repository, 'settings_policy' | 'secrets_policy'>>;
  role: Role | null;
}) {
  const t = useT();
  const open = useUiStore((state) => state.repositoryDetailsOpen);
  const setOpen = useUiStore((state) => state.setRepositoryDetailsOpen);
  function close() {
    setOpen(false);
    document.getElementById('repository-details-toggle')?.focus();
  }
  if (!open) return null;
  return <aside id="repository-details" className="app-side app-side-right" aria-label={t('repositorySidebar.title')} onKeyDown={(event) => {
    if (event.key === 'Escape' && event.currentTarget.contains(event.target as Node)) {
      event.stopPropagation();
      close();
    }
  }}>
    <div className="repository-details-heading">
      <h3>{t('repositorySidebar.title')}</h3>
      <button type="button" className="ghost" aria-label={t('common.close')} title={t('repositorySidebar.hide')} onClick={close}>×</button>
    </div>
    <About repo={repo} canEdit={canWriteAsset(role, undefined)} />
    {atLeast(role, 'puller') && <>
      <TeamSettings repoId={repo.id} canWrite={canWriteAsset(role, repositoryMetadata.settings_policy)} showLockedControl={repositoryMetadata.visibility === 'public'} />
      <SecretsPanel key={repo.id} repoId={repo.id} canWrite={canWriteAsset(role, repositoryMetadata.secrets_policy)} showLockedControl={repositoryMetadata.visibility === 'public'} />
    </>}
  </aside>;
}
