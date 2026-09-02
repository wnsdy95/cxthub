// PublicBrowse — Public read-only access for GitHub public repos.
// On entering /<username>/<slug>, if it's a public workspace, show the context in read-only mode.
// No write UI (role=null → ContextView hides the rail assets section and ⚙).
import { useEffect, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import type { PublicWorkspace } from '../types';
import { api } from '../api';
import { navigate, type WsTab } from '../route';
import { Logo } from './Logo';
import { LocaleSwitcher } from './LocaleSwitcher';
import { Breadcrumb } from './Breadcrumb';
import { useT } from '../i18n';
import { ContextView } from './ContextView';
import { sanitizeRemoteUrl } from '../urls';
import { AccessDenied } from './AccessDenied';

export function PublicBrowse({
  username,
  slug,
  tab,
  onLogin,
}: {
  username: string;
  slug: string;
  tab?: WsTab;
  onLogin: () => void;
}) {
  const t = useT();
  const wsQ = useQuery<PublicWorkspace>({
    queryKey: ['public-ws', username, slug],
    queryFn: () => api.publicWorkspace(username, slug),
    retry: false,
  });
  const ws = wsQ.data ?? null;
  const reposQ = useQuery({
    queryKey: ['repos', ws?.id],
    queryFn: () => api.listRepos(ws!.id),
    enabled: Boolean(ws),
  });
  const repos = reposQ.data ?? [];
  const [activeRepoId, setActiveRepoId] = useState<string | null>(null);
  const activeRepo = repos.find((r) => r.id === activeRepoId) ?? repos[0] ?? null;

  // private or non-existent — Do not leak existence, redirect to login (no setState during render → effect).
  const notFound = !wsQ.isLoading && !ws;
  useEffect(() => {
    if (notFound) onLogin();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [notFound]);

  if (wsQ.isLoading || notFound) return <div className="loading">…</div>;
  if (!ws) return null;

  return (
    <div className="app">
      <header className="topbar">
        <div className="topbar-left">
          <button className="linkish-logo" onClick={() => navigate('/')} aria-label={t('common.home')}>
            <div className="brand sm">
              <Logo />
            </div>
          </button>
          {ws && <Breadcrumb owner={ws.owner_username} name={ws.name} />}
        </div>
        <div className="who">
          <LocaleSwitcher />
          <span className="vis-chip">{t('common.publicView')}</span>
          <button
            className="ghost"
            onClick={() => {
              navigate('/');
              onLogin();
            }}
          >
            {t('common.signIn')}
          </button>
        </div>
      </header>

      <div className="cols public-cols">
        <main className="main">
          <div className="ws-head">
            <h2>
              {ws.name}
              <span className="vis-chip">{t('common.public')}</span>
            </h2>
            <p className="ws-meta">
              <code>
                {ws.owner_username}/{ws.slug}
              </code>
            </p>
          </div>

          {tab !== 'settings' && repos.length > 1 && (
            <div className="pub-repos">
              {repos.map((r) => (
                <button
                  key={r.id}
                  className={`ghost mini${activeRepo?.id === r.id ? ' on' : ''}`}
                  onClick={() => setActiveRepoId(r.id)}
                >
                  {sanitizeRemoteUrl(r.remote_url) || r.id.slice(7, 19)}
                </button>
              ))}
            </div>
          )}

          <section className="panel">
            {tab === 'settings' ? (
              <AccessDenied message={t('dashboard.workspaceAccessDenied')} />
            ) : activeRepo ? (
              <ContextView repo={activeRepo} ws={ws} role={null} />
            ) : (
              <div className="empty-box">{t('common.noPushedContext')}</div>
            )}
          </section>
        </main>
      </div>
    </div>
  );
}
