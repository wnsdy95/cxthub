// User profile — GitHub style /<username> page. Left avatar/name, right workspace list
// (repo card space). Anonymous view (only public workspaces), owner includes private + 'Edit Profile'.
// (Contribution graph/activity feed requires commit date aggregation — next step.)
import { useQuery } from '@tanstack/react-query';
import { api } from '../api';
import { useMe, useLogout } from '../hooks';
import { navigate, wsPath } from '../route';
import { Logo } from './Logo';
import { Avatar, avatarColor } from './Avatar';
import { useT } from '../i18n';
import { LockIcon } from './Breadcrumb';
import { AccountSettings } from './Settings';
import { ContributionGraph } from './ContributionGraph';
import { ActivityFeed } from './ActivityFeed';
import { safeAvatarUrl } from '../urls';
import { EnterpriseProfile, MyEnterprises } from './EnterpriseProfile';

export function UserProfile({ username, onLogin }: { username: string; onLogin?: () => void }) {
  const t = useT();
  const me = useMe().data;
  const logout = useLogout();
  const q = useQuery({
    queryKey: ['publicUser', username],
    queryFn: () => api.publicUser(username),
    retry: false,
  });
  const enterpriseQ = useQuery({
    queryKey: ['publicEnterprise', username],
    queryFn: () => api.publicEnterprise(username),
    retry: false,
  });

  if (q.isLoading || enterpriseQ.isLoading) return <div className="loading">…</div>;

  const data = q.data;
  const enterprise = enterpriseQ.data;
  const canonicalSlug = data?.user.username ?? enterprise?.slug ?? username;
  return (
    <div className="app">
      <header className="topbar">
        <div className="topbar-left">
          <button className="linkish-logo" onClick={() => navigate('/')} aria-label={t('common.home')}>
            <div className="brand sm">
              <Logo />
            </div>
          </button>
          <nav className="topbar-crumb" aria-label={t('common.currentLocation')}>
            <button
              type="button"
              className="crumb-user"
              onClick={() => navigate(`/${canonicalSlug}`)}
              title={canonicalSlug}
            >
              {canonicalSlug}
            </button>
          </nav>
        </div>
        <div className="who">
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
            <button className="ghost" onClick={() => onLogin?.()}>
              {t('common.signIn')}
            </button>
          )}
        </div>
      </header>

      {!data && !enterprise ? (
        <div className="profile">
          <div className="empty-box">
            {t('profile.namespaceNotFound')} <code>{username}</code>
          </div>
        </div>
      ) : enterprise ? (
        <EnterpriseProfile data={enterprise} />
      ) : (
        <ProfileBody data={data!} isSelf={Boolean(me && me.username === data!.user.username)} me={me ?? null} />
      )}
    </div>
  );
}

function ProfileBody({
  data,
  isSelf,
  me,
}: {
  data: { user: import('../types').PublicUser; workspaces: import('../types').PublicWorkspace[] };
  isSelf: boolean;
  me: import('../types').User | null;
}) {
  const t = useT();
  const u = data.user;
  const displayName = u.nickname || u.name || u.username;
  const initial = (displayName || u.username || '?').trim().charAt(0).toUpperCase();
  const workspaces = data.workspaces ?? [];
  const avatar = safeAvatarUrl(u.avatar);

  return (
    <div className="profile">
      <div className="profile-grid">
      <aside className="profile-side">
        {avatar ? (
          <img className="avatar-lg avatar-img" src={avatar} alt={displayName} />
        ) : (
          <div className="avatar-lg" style={{ background: avatarColor(u.username) }} aria-hidden="true">
            {initial}
          </div>
        )}
        <div className="profile-id">
          <h1 className="profile-name">{displayName}</h1>
          <div className="profile-handle">{u.username}</div>
        </div>
        {isSelf && me && <AccountSettings user={me} trigger="button" />}
      </aside>

      <main className="profile-main">
        <div className="profile-section-head">
          <h2 className="profile-section">{t('common.workspaces')}</h2>
          <span className="count-badge">{workspaces.length}</span>
        </div>
        {workspaces.length === 0 ? (
          <div className="empty-box">
            {isSelf ? t('profile.noWorkspacesSelf') : t('profile.noWorkspacesPublic')}
          </div>
        ) : (
          <div className="ws-cards">
            {workspaces.map((w) => (
              <button key={w.id} className="ws-card" onClick={() => navigate(wsPath(w))}>
                <span className="ws-card-top">
                  <span className="ws-card-name">{w.name}</span>
                  <span className="ws-card-vis">
                    {w.visibility === 'public' ? (
                      t('common.public')
                    ) : (
                      <>
                        <LockIcon /> {t('common.private')}
                      </>
                    )}
                  </span>
                </span>
                <span className="ws-card-path">
                  {w.owner_username}/{w.slug}
                </span>
              </button>
            ))}
          </div>
        )}

        {isSelf && <MyEnterprises />}

        <ContributionGraph username={u.username} />
        <ActivityFeed username={u.username} />
      </main>
      </div>
    </div>
  );
}
