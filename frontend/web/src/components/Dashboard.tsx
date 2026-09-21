import { useEffect, useState, type FormEvent } from 'react';
import type { Invite } from '../types';
import type { Repository } from '../types';
import { useUiStore } from '../store';
import {
  repositoryPath,
  repoPath,
  invitePath,
  parseRoute,
  findByRoute,
  findRepositoryByRoute,
  resolvedRepositoryTab,
  navigate,
  replacePath,
} from '../route';
import {
  useMe,
  useRepositories,
  useMembers,
  useRepos,
  useCreateRepository,
  useCreateInvite,
  useInvites,
  useRevokeInvite,
  useLogout,
  useUpdateMemberRole,
  useRemoveMember,
} from '../hooks';
import { Logo } from './Logo';
import { Avatar } from './Avatar';
import { useT } from '../i18n';
import { Breadcrumb } from './Breadcrumb';
import { ContextView } from './ContextView';
import { OnHoldView } from './OnHoldView';
import { AccountSettings, RepositorySettings } from './Settings';
import { RoleCapabilities } from './RoleCapabilities';
import { LockIcon } from './Breadcrumb';
import { myRole, atLeast, ROLES } from '../roles';
import { gitWebUrl, sanitizeRemoteUrl } from '../urls';
import { AccessDenied } from './AccessDenied';

function short(hash: string): string {
  return hash.replace(/^sha256:/, '').slice(0, 12);
}

export function Dashboard() {
  const t = useT();
  const user = useMe().data;
  const selectedId = useUiStore((s) => s.selectedRepositoryId);
  const selectRepository = useUiStore((s) => s.selectRepository);

  const repositoriesQ = useRepositories();
  const repositories = repositoriesQ.data ?? [];
  // Guard against null with [] (backend can return null for empty lists — destructuring defaults only cover undefined).
  const members = useMembers(selectedId).data ?? [];
  const repos = useRepos(selectedId).data ?? [];

  const createRepositoryAction = useCreateRepository();
  const logout = useLogout();
  const setRole = useUpdateMemberRole();
  const removeMember = useRemoveMember();

  const [newName, setNewName] = useState('');
  // Repository name rules (enforced in English): start with a letter, end with a letter or number, middle can be letters, numbers, -, or _.
  const NAME_RE = /^[A-Za-z]([A-Za-z0-9_-]*[A-Za-z0-9])?$/;
  const nameValid = NAME_RE.test(newName.trim());

  // A repository has one stable content identity; routes never select an arbitrary child.
  const [accessNotice, setAccessNotice] = useState<string | null>(null);

  // URL (/<username>/<slug>) → synchronize selection. The URL is the source of truth on entry, refresh, and back navigation.
  // slug → id interpretation requires a list, so the path is kept in state and reinterpreted with the list.
  // (navigate/replacePath synthesizes popstate events, so a single listener is sufficient).
  const [path, setPath] = useState(() => ({ pathname: location.pathname, search: location.search }));
  useEffect(() => {
    const onChange = () => setPath({ pathname: location.pathname, search: location.search });
    window.addEventListener('popstate', onChange);
    return () => window.removeEventListener('popstate', onChange);
  }, []);
  useEffect(() => {
    const found = findByRoute(parseRoute(path.pathname, path.search), repositories);
    if (found) selectRepository(found.id);
  }, [path, repositories, selectRepository]);

  // After list load: if URL doesn't point to my repository, correct to the first item (without history).
  // Invite paths (/invite/…) are handled by App on accept→redirect, so don't interfere here.
  useEffect(() => {
    if (!repositoriesQ.isSuccess || repositories.length === 0) return;
    const route = parseRoute(path.pathname, path.search);
    if (route?.kind === 'invite') return;
    if (!findByRoute(route, repositories)) {
      selectRepository(repositories[0].id);
      replacePath(repositoryPath(repositories[0]));
    }
  }, [repositoriesQ.isSuccess, repositories, path, selectRepository]);

  const route = parseRoute(path.pathname, path.search);
  const routedRepo = findRepositoryByRoute(route, repos);
  const activeRepo = routedRepo;

  // Repository click = navigate to that path (history is pushed for back action).
  function goRepository(w: Repository) {
    setAccessNotice(null);
    selectRepository(w.id);
    navigate(repositoryPath(w));
  }

  const selected = repositories.find((w) => w.id === selectedId) ?? null;
  // My role — UI gating (security boundary is server requireRepoRole).
  const role = myRole(selected, user?.id, members);

  // Tabs are query parameters, independent of repository names.
  const routedTab = resolvedRepositoryTab(route, repos);
  const tab: 'context' | 'connections' | 'members' | 'onhold' | 'settings' =
    routedTab === 'members' || routedTab === 'connections' || routedTab === 'onhold' || routedTab === 'settings'
      ? routedTab
      : 'context';
  function goTab(next: 'context' | 'connections' | 'members' | 'onhold' | 'settings') {
    if (!selected) return;
    if (next === 'settings' && !atLeast(role, 'owner')) {
      setAccessNotice(t('dashboard.repositoryAccessDenied'));
      return;
    }
    setAccessNotice(null);
    if (next === 'context') {
      navigate(activeRepo ? repoPath(selected, activeRepo) : repositoryPath(selected));
      return;
    }
    if (next === 'onhold') {
      if (activeRepo) navigate(repoPath(selected, activeRepo, 'onhold'));
      return;
    }
    navigate(repositoryPath(selected, next));
  }

  function createRepository(e: FormEvent) {
    e.preventDefault();
    const name = newName.trim();
    if (!name || !nameValid) return;
    createRepositoryAction.mutate(name, {
      onSuccess: (w) => {
        setNewName('');
        goRepository(w);
      },
    });
  }

  const err = repositoriesQ.error ?? createRepositoryAction.error;

  return (
    <div className="app">
      <header className="topbar">
        <div className="topbar-left">
          <button className="linkish-logo" onClick={() => navigate('/')} aria-label={t('common.home')}>
            <div className="brand sm">
              <Logo />
            </div>
          </button>
          {selected && (
            <Breadcrumb
              owner={selected.owner_username}
              name={selected.name}
              isPrivate={selected.visibility !== 'public'}
              repositories={repositories}
              currentId={selected.id}
              onSelect={goRepository}
            />
          )}
        </div>
        <div className="who">
          {user && <Avatar user={user} link />}
          <span>{user?.nickname || user?.name || user?.email}</span>
          {user && <AccountSettings user={user} />}
          <button className="ghost" onClick={() => logout.mutate()} disabled={logout.isPending}>
            {t('common.logout')}
          </button>
        </div>
      </header>

      <div className="cols">
        <aside className="app-side">
          <div className="side-head">
            <span className="label">{t('common.repositories')}</span>
            <span className="count-badge">{repositories.length}</span>
          </div>
          {repositoriesQ.isLoading ? (
            <div className="repository-list" aria-hidden="true">
              <div className="skel" />
              <div className="skel" />
              <div className="skel" />
            </div>
          ) : (
            <ul className="repository-list">
              {repositories.map((w) => (
                <li key={w.id}>
                  <button className={`repository-item${selectedId === w.id ? ' on' : ''}`} onClick={() => goRepository(w)}>
                    {w.name}
                  </button>
                </li>
              ))}
              {repositories.length === 0 && <li className="repository-empty">{t('dashboard.noRepositoriesShort')}</li>}
            </ul>
          )}
          <form onSubmit={createRepository} className="newws">
            <input
              placeholder={t('dashboard.newWsPlaceholder')}
              aria-label={t('dashboard.newWsAria')}
              value={newName}
              onChange={(e) => setNewName(e.target.value)}
              spellCheck={false}
            />
            {newName.trim() !== '' && !nameValid && <p className="err name-rule">{t('dashboard.nameRule')}</p>}
            <button type="submit" disabled={createRepositoryAction.isPending || !nameValid}>
              {createRepositoryAction.isPending ? t('common.creating') : t('dashboard.createRepositoryAction')}
            </button>
          </form>
        </aside>

        <main className="main">
          {err && <p className="err-banner">{err.message}</p>}
          {selected ? (
            <>
              <div className="repository-head">
                <h2>
                  {selected.name}
                  <span className="repository-card-vis">
                    {selected.visibility === 'public' ? (
                      t('common.public')
                    ) : (
                      <>
                        <LockIcon /> {t('common.private')}
                      </>
                    )}
                  </span>
                </h2>
                <p className="repository-meta">
                  <code>
                    {selected.owner_username}/{selected.slug}
                  </code>
                </p>
                <nav className="tabs" aria-label={t('dashboard.tabsAria')}>
                  <button className={`tab${tab === 'context' ? ' on' : ''}`} onClick={() => goTab('context')}>
                    {t('dashboard.context')}
                  </button>
                  <button className={`tab${tab === 'onhold' ? ' on' : ''}`} onClick={() => goTab('onhold')}>
                    On Hold
                  </button>
                  <button className={`tab${tab === 'connections' ? ' on' : ''}`} onClick={() => goTab('connections')}>
                    {t('dashboard.connected')} <span className="count-badge">{repos.length}</span>
                  </button>
                  <button className={`tab${tab === 'members' ? ' on' : ''}`} onClick={() => goTab('members')}>
                    {t('dashboard.members')} <span className="count-badge">{members.length}</span>
                  </button>
                  {(selected.visibility === 'public' || (user && atLeast(role, 'owner'))) && (
                    <button className={`tab${tab === 'settings' ? ' on' : ''}`} onClick={() => goTab('settings')}>
                      {t('dashboard.settings')}
                    </button>
                  )}
                </nav>
              </div>

              {accessNotice && (
                <div className="toast" role="alert" onClick={() => setAccessNotice(null)}>
                  {accessNotice}
                </div>
              )}

              {tab === 'members' ? (
                <>
                  {atLeast(role, 'maintainer') && <InvitePanel key={selected.id} repositoryId={selected.id} />}

                  <RoleCapabilities />

                  <section className="panel members-panel">
                    <div className="panel-head">
                      <h4>
                        {t('dashboard.members')} <span className="count-badge">{members.length}</span>
                      </h4>
                    </div>
                    <ul className="members">
                      {members.map((m) => {
                        const isCreator = m.user_id === selected.owner_id; // Creator — role fixed and removal not allowed
                        const iAmOwner = role === 'owner';
                        const isSelf = m.user_id === user?.id;
                        return (
                          <li key={m.user_id}>
                            <span>{m.user?.nickname || m.user?.name || m.user_id}</span>
                            <em>{m.user?.email}</em>
                            {iAmOwner && !isCreator ? (
                              <select
                                className="role-select"
                                value={m.role}
                                onChange={(e) =>
                                  setRole.mutate({ repositoryId: selected.id, userId: m.user_id, role: e.target.value as 'owner' | 'member' })
                                }
                                disabled={setRole.isPending}
                                aria-label={t('dashboard.roleChangeAria')}
                              >
                                <option value="viewer">viewer</option>
                                <option value="puller">puller</option>
                                <option value="member">member</option>
                                <option value="maintainer">maintainer</option>
                                <option value="owner">owner</option>
                              </select>
                            ) : (
                              <span className={`role ${m.role}`}>{isCreator ? t('dashboard.ownerCreator') : m.role}</span>
                            )}
                            {!isCreator && (iAmOwner || isSelf) && (
                              <button
                                className="ghost mini"
                                onClick={() => removeMember.mutate({ repositoryId: selected.id, userId: m.user_id })}
                                disabled={removeMember.isPending}
                              >
                                {isSelf ? t('dashboard.leave') : t('common.remove')}
                              </button>
                            )}
                          </li>
                        );
                      })}
                    </ul>
                    {(setRole.error || removeMember.error) && (
                      <p className="err">{(setRole.error ?? removeMember.error)!.message}</p>
                    )}
                  </section>

                </>
              ) : tab === 'context' ? (
                <section className="panel">
                  <div className="panel-head">
                    <h4>
                      {t('dashboard.context')} {activeRepo && <code className="repo-tag">{sanitizeRemoteUrl(activeRepo.remote_url) || short(activeRepo.id)}</code>}
                    </h4>
                  </div>
                  {activeRepo ? (
                    <ContextView repo={activeRepo} repositoryMetadata={selected} role={role} />
                  ) : (
                    <div className="empty-box">
                      {t('dashboard.contextEmptyLead')}
                      <br />
                      <code>
                        cxt setup {location.origin}/{selected.owner_username}/{selected.slug}
                      </code>
                      <br />
                      {t('dashboard.contextEmptyTail')}
                    </div>
                  )}
                </section>
              ) : tab === 'onhold' ? (
                <section className="panel">
                  <div className="panel-head">
                    <h4>
                      On Hold{' '}
                      {activeRepo && <code className="repo-tag">{sanitizeRemoteUrl(activeRepo.remote_url) || short(activeRepo.id)}</code>}
                    </h4>
                  </div>
                  {activeRepo ? (
                    <OnHoldView repo={activeRepo} repositoryMetadata={selected} role={role} />
                  ) : (
                    <div className="empty-box">{t('dashboard.noRepo')}</div>
                  )}
                </section>
              ) : null}

              {tab === 'settings' && user && atLeast(role, 'owner') && (
                <section className="panel">
                  <div className="panel-head">
                    <h4>{t('dashboard.repositorySettings')}</h4>
                  </div>
                  <RepositorySettings repositoryMetadata={selected} isCreator={user.id === selected.owner_id} />
                </section>
              )}
              {tab === 'settings' && (!user || !atLeast(role, 'owner')) && (
                <section className="panel">
                  <div className="panel-head">
                    <h4>{t('dashboard.repositorySettings')}</h4>
                  </div>
                  <AccessDenied message={t('dashboard.repositoryAccessDenied')} />
                </section>
              )}

              {tab === 'connections' && (
                <section className="panel">
                  <div className="panel-head">
                    <h4>
                      {t('dashboard.connectedRepos')} <span className="count-badge">{repos.length}</span>
                    </h4>
                  </div>
                  {repos.length > 0 ? (
                    <ul className="repos">
                      {repos.map((r) => {
                        const gh = gitWebUrl(r.git_remote_url);
                        return (
                          <li key={r.id}>
                            <button
                              className={`repo-row${activeRepo?.id === r.id ? ' on' : ''}`}
                              onClick={() => {
                                navigate(repoPath(selected, r));
                              }}
                              title={t('dashboard.viewRepoContext')}
                            >
                              <code>{short(r.id)}</code>
                              <span className="remote">{sanitizeRemoteUrl(r.remote_url) || '(local)'}</span>
                              <span className="branch-chip">{r.default_branch}</span>
                            </button>
                            {gh && (
                              <a
                                className="gh-link"
                                href={gh}
                                target="_blank"
                                rel="noreferrer"
                                title={t('dashboard.openCodeRepo')}
                                onClick={(e) => e.stopPropagation()}
                              >
                                {gh.replace(/^https?:\/\//, '')} ↗
                              </a>
                            )}
                          </li>
                        );
                      })}
                    </ul>
                  ) : (
                    <div className="empty-box">
                      {t('dashboard.connEmptyPre')}
                      <code>
                        cxt setup {location.origin}/{selected.owner_username}/{selected.slug}
                      </code>
                      {t('dashboard.connEmptyTail')}
                    </div>
                  )}
                </section>
              )}
            </>
          ) : repositoriesQ.isLoading ? (
            <div className="skel" style={{ height: 120, marginTop: '8vh' }} aria-label={t('common.loadingLabel')} />
          ) : (
            <div className="empty-box">{t('dashboard.pickRepository')}</div>
          )}
        </main>
      </div>
    </div>
  );
}

// InvitePanel — Invite management (maintainer and above): create (role and email restrictions) + pending list + revoke.
// Like GitHub, all invite records are kept (including revoked) — only pending is shown by default.
function InvitePanel({ repositoryId }: { repositoryId: string }) {
  const t = useT();
  const createInv = useCreateInvite();
  const revoke = useRevokeInvite();
  const invites = useInvites(repositoryId, true).data ?? [];
  const [role, setRole] = useState('member');
  const [email, setEmail] = useState('');
  const [expDays, setExpDays] = useState(0); // 0=unlimited
  const [made, setMade] = useState<Invite | null>(null);
  const [copiedTok, setCopiedTok] = useState<string | null>(null);
  const [showAll, setShowAll] = useState(false);

  const list = (invites ?? []).filter((i) => showAll || i.status === 'pending');
  const roleLabel = {
    viewer: t('roles.viewer'),
    puller: t('roles.puller'),
    member: t('roles.member'),
    maintainer: t('roles.maintainer'),
    owner: t('roles.owner'),
  } as const;

  function make() {
    createInv.mutate(
      { repositoryId: repositoryId, role, email: email.trim(), expiresInDays: expDays },
      {
        onSuccess: (inv) => {
          setMade(inv);
          setEmail('');
        },
      },
    );
  }

  function copy(token: string) {
    navigator.clipboard?.writeText(location.origin + invitePath(token));
    setCopiedTok(token);
    setTimeout(() => setCopiedTok(null), 1600);
  }

  return (
    <section className="panel invite-panel">
      <div className="panel-head">
        <h4>
          {t('dashboard.invites')} <span className="count-badge">{list.length}</span>
        </h4>
        <button className="ghost mini" onClick={() => setShowAll(!showAll)}>
          {showAll ? t('dashboard.onlyPending') : t('dashboard.allHistory')}
        </button>
      </div>

      <div className="invite-make">
        <select value={role} onChange={(e) => setRole(e.target.value)} aria-label={t('dashboard.inviteRoleAria')}>
          {ROLES.map((r) => (
            <option key={r} value={r}>
              {roleLabel[r]}
            </option>
          ))}
        </select>
        <select value={expDays} onChange={(e) => setExpDays(Number(e.target.value))} aria-label={t('dashboard.inviteExpiryAria')}>
          <option value={0}>{t('dashboard.expNever')}</option>
          <option value={1}>{t('dashboard.expInDays', { n: 1 })}</option>
          <option value={7}>{t('dashboard.expInDays', { n: 7 })}</option>
          <option value={30}>{t('dashboard.expInDays', { n: 30 })}</option>
        </select>
        <input
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          placeholder={t('dashboard.inviteEmailPlaceholder')}
          aria-label={t('dashboard.inviteEmailAria')}
        />
        <button onClick={make} disabled={createInv.isPending}>
          {createInv.isPending ? t('common.creating') : t('dashboard.createInvite')}
        </button>
      </div>
      {made && (
        <div className="invite-row fresh">
          <code>{location.origin + invitePath(made.token)}</code>
          <button className={`copy${copiedTok === made.token ? ' done' : ''}`} onClick={() => copy(made.token)}>
            {copiedTok === made.token ? t('common.copied') : t('common.copy')}
          </button>
        </div>
      )}

      {list.length > 0 ? (
        <ul className="members invites">
          {list.map((i) => (
            <li key={i.token}>
              <span className={`role ${i.role}`}>{i.role}</span>
              <span className="inv-target">{i.email || t('dashboard.anyoneWithLink')}</span>
              <em>
                {i.status}
                {i.created_at ? ` · ${i.created_at.slice(0, 10)}` : ''}
                {i.expires_at &&
                  (new Date(i.expires_at) < new Date() ? ` · ${t('dashboard.expired')}` : ` · ~${i.expires_at.slice(0, 10)}`)}
              </em>
              {i.status === 'pending' && (
                <>
                  <button className="ghost mini" onClick={() => copy(i.token)}>
                    {copiedTok === i.token ? t('common.copied') : t('dashboard.copyLink')}
                  </button>
                  <button
                    className="ghost mini"
                    onClick={() => revoke.mutate({ repositoryId: repositoryId, token: i.token })}
                    disabled={revoke.isPending}
                  >
                    {t('dashboard.revoke')}
                  </button>
                </>
              )}
            </li>
          ))}
        </ul>
      ) : (
        <div className="empty-box">
          {showAll ? t('dashboard.noInviteHistory') : t('dashboard.noPendingInvites')}
        </div>
      )}
      <p className="hint">{t('dashboard.inviteHint')}</p>
      {(createInv.error || revoke.error) && <p className="err">{(createInv.error ?? revoke.error)!.message}</p>}
    </section>
  );
}
