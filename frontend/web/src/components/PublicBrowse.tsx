// PublicBrowse — Public read-only access for GitHub public repos.
// On entering /<username>/<slug>, if it's a public repository, show the context in read-only mode.
import { useEffect } from 'react';
import { useQuery } from '@tanstack/react-query';
import type { PublicRepository } from '../types';
import { api } from '../api';
import {
  navigate,
  repositoryPath,
  replacePath,
  findRepositoryByRoute,
  resolvedRepositoryTab,
  type Route,
} from '../route';
import { Logo } from './Logo';
import { LocaleSwitcher } from './LocaleSwitcher';
import { Breadcrumb } from './Breadcrumb';
import { useT } from '../i18n';
import { ContextView } from './ContextView';
import { AccessDenied } from './AccessDenied';
import { useMe, useRepositories } from '../hooks';
import { HeaderActions } from './HeaderActions';
import { RepositorySidebar, RepositorySidebarToggle } from './RepositorySidebar';
import { useUiStore } from '../store';

export function PublicBrowse({
  route,
  onLogin,
}: {
  route: Extract<NonNullable<Route>, { kind: 'repository' }>;
  onLogin?: () => void;
}) {
  const t = useT();
  const me = useMe().data;
  const repositories = useRepositories().data ?? [];
  const detailsOpen = useUiStore((state) => state.repositoryDetailsOpen);
  const { username, slug } = route;
  const repositoryQuery = useQuery<PublicRepository>({
    queryKey: ['public-repository', username, slug],
    queryFn: () => api.publicRepository(username, slug),
    retry: false,
  });
  const repositoryMetadata = repositoryQuery.data ?? null;
  const reposQ = useQuery({
    queryKey: ['repos', repositoryMetadata?.id],
    queryFn: () => api.listRepos(repositoryMetadata!.id),
    enabled: Boolean(repositoryMetadata),
  });
  const repos = reposQ.data ?? [];
  useEffect(() => {
    if (repositoryMetadata && (repositoryMetadata.owner_username !== username || repositoryMetadata.slug !== slug)) {
      replacePath(repositoryPath(repositoryMetadata) + location.search);
    }
  }, [repositoryMetadata, username, slug]);
  const routedRepo = findRepositoryByRoute(route, repos);
  const activeRepo = routedRepo;
  const tab = resolvedRepositoryTab(route, repos);

  // private or non-existent — Do not leak existence, redirect to login (no setState during render → effect).
  const notFound = !repositoryQuery.isLoading && !repositoryMetadata;
  useEffect(() => {
    if (notFound) onLogin?.();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [notFound]);

  if (repositoryQuery.isLoading || (notFound && onLogin)) return <div className="loading">…</div>;
  if (notFound) return <div className="loading">{t('common.repositoryUnavailable')}</div>;
  if (!repositoryMetadata) return null;

  return (
    <div className="app">
      <header className="topbar public-topbar">
        <div className="topbar-left">
          <button className="linkish-logo" onClick={() => navigate('/')} aria-label={t('common.home')}>
            <div className="brand sm">
              <Logo />
            </div>
          </button>
          {repositoryMetadata && (
            <Breadcrumb
              key={repositoryMetadata.id}
              owner={repositoryMetadata.owner_username}
              name={repositoryMetadata.name}
              isPrivate={repositoryMetadata.visibility !== 'public'}
              repositories={me ? repositories : undefined}
              currentId={repositoryMetadata.id}
              onSelect={(entry) => navigate(repositoryPath(entry))}
            />
          )}
        </div>
        <div className="who">
          {activeRepo && <RepositorySidebarToggle />}
          {me && <HeaderActions user={me} />}
          <LocaleSwitcher />
          <span className={`vis-chip${repositoryMetadata.visibility === 'public' ? '' : ' emergency'}`}>
            {repositoryMetadata.visibility === 'public' ? t('common.publicView') : t('organization.emergencyReadOnly')}
          </span>
          {onLogin && (
            <button
              className="ghost"
              onClick={() => {
                navigate('/');
                onLogin();
              }}
            >
              {t('common.signIn')}
            </button>
          )}
        </div>
      </header>

      <div className={`cols public-cols repository-cols${activeRepo && detailsOpen ? ' has-details' : ''}`}>
        <main className="main">
          <div className="repository-head">
            <h2>
              {repositoryMetadata.name}
              <span className={`vis-chip${repositoryMetadata.visibility === 'public' ? '' : ' emergency'}`}>
                {repositoryMetadata.visibility === 'public' ? t('common.public') : t('common.private')}
              </span>
            </h2>
            <p className="repository-meta">
              <code>
                {repositoryMetadata.owner_username}/{repositoryMetadata.slug}
              </code>
            </p>
            {repositoryMetadata.visibility !== 'public' && <p className="warn-red">{t('organization.emergencySessionNotice')}</p>}
          </div>

          <section className="panel">
            {tab === 'settings' ? (
              <AccessDenied message={t('dashboard.repositoryAccessDenied')} />
            ) : activeRepo ? (
              <ContextView repo={activeRepo} repositoryMetadata={repositoryMetadata} role={null} />
            ) : (
              <div className="empty-box">{t('common.noPushedContext')}</div>
            )}
          </section>
        </main>
        {activeRepo && <RepositorySidebar key={activeRepo.id} repo={activeRepo} repositoryMetadata={repositoryMetadata} role={null} />}
      </div>
    </div>
  );
}
