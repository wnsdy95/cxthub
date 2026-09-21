// PublicBrowse — Public read-only access for GitHub public repos.
// On entering /<username>/<slug>, if it's a public repository, show the context in read-only mode.
// No write UI (role=null → ContextView hides the rail assets section and ⚙).
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

export function PublicBrowse({
  route,
  onLogin,
}: {
  route: Extract<NonNullable<Route>, { kind: 'repository' }>;
  onLogin?: () => void;
}) {
  const t = useT();
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
      <header className="topbar">
        <div className="topbar-left">
          <button className="linkish-logo" onClick={() => navigate('/')} aria-label={t('common.home')}>
            <div className="brand sm">
              <Logo />
            </div>
          </button>
          {repositoryMetadata && (
            <Breadcrumb
              owner={repositoryMetadata.owner_username}
              name={repositoryMetadata.name}
            />
          )}
        </div>
        <div className="who">
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

      <div className="cols public-cols">
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
      </div>
    </div>
  );
}
