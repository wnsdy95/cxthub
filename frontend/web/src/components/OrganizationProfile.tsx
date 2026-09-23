import { RenameSpace } from './NamespaceAdministration';
import { InvitationManager } from './CollaborationInvitations';
import { OrganizationTeams } from './OrganizationTeams';
import { StorageUsage } from './StorageUsage';
import { useEffect, useMemo, useState, type ChangeEvent, type FormEvent, type ReactNode } from 'react';
import { useInfiniteQuery, useQuery } from '@tanstack/react-query';
import { api } from '../api';
import {
  useCreateOrganization,
  useCreateOrganizationRepository,
  useOrganizationMembers,
  useOrganizationPolicy,
  useOrganizations,
  useOrganizationRepositories,
  useMe,
  useRemoveOrganizationMember,
  useUpdateOrganization,
  useUpdateOrganizationMember,
  useUpdateOrganizationPolicy,
  useRepositories,
} from '../hooks';
import { useT } from '../i18n';
import { navigate, repositoryPath } from '../route';
import { safeAvatarUrl } from '../urls';
import type {
  Organization,
  OrganizationMembership,
  OrganizationPolicy,
  OrganizationRole,
  PublicOrganization,
  PublicRepository,
  Repository,
} from '../types';
import { avatarColor } from './Avatar';
import { LockIcon } from './Breadcrumb';
import { resizeToDataURL } from './Settings';

type OrganizationTab = 'repositories' | 'teams' | 'people' | 'policies' | 'audit' | 'settings' | 'storage';

function roleRank(role: OrganizationRole | undefined): number {
  return role === 'owner' ? 3 : role === 'admin' ? 2 : role === 'member' ? 1 : 0;
}

export function OrganizationProfile({ data }: { data: PublicOrganization }) {
  const t = useT();
  const me = useMe().data;
  const organizations = useOrganizations();
  const joined = (organizations.data ?? []).find((organization) => organization.id === data.id) ?? null;
  const privateOrganization = useQuery({
    queryKey: ['organization', joined?.id],
    queryFn: () => api.getOrganization(joined!.id),
    enabled: Boolean(joined),
  });
  const organization: Organization | PublicOrganization = privateOrganization.data ?? joined ?? data;
  const members = useOrganizationMembers(joined?.id ?? null);
  const role = members.data?.find((member) => member.user_id === me?.id)?.role;
  const canAdmin = roleRank(role) >= roleRank('admin');
  const [tab, setTab] = useState<OrganizationTab>('repositories');
  const logo = safeAvatarUrl(organization.logo);
  const initial = (organization.name || organization.slug || '?').trim().charAt(0).toUpperCase();

  useEffect(() => {
    if (!joined && tab !== 'repositories') setTab('repositories');
    if (tab === 'audit' && !canAdmin) setTab('repositories');
    if ((tab === 'settings' || tab === 'storage') && !canAdmin) setTab('repositories');
  }, [joined, canAdmin, tab]);

  return (
    <div className="profile organization-profile">
      <div className="profile-grid">
        <aside className="profile-side">
          {logo ? (
            <img className="avatar-lg avatar-img organization-logo" src={logo} alt={organization.name} />
          ) : (
            <div
              className="avatar-lg organization-logo"
              style={{ background: avatarColor(organization.slug) }}
              aria-hidden="true"
            >
              {initial}
            </div>
          )}
          <div className="profile-id">
            <h1 className="profile-name">{organization.name}</h1>
            <div className="profile-handle">{organization.slug}</div>
          </div>
          <span className="organization-badge">{t('organization.label')}</span>
          {joined ? (
            <p className="hint">{t('organization.roleLabel', { role: role ?? t('organization.roleLoading') })}</p>
          ) : (
            <p className="hint">{t('organization.publicProfile')}</p>
          )}
        </aside>

        <main className="profile-main">
          <p className="organization-access-note">{t('organization.accessSeparation')}</p>
          <div className="tabs organization-tabs" role="tablist" aria-label={t('organization.tabsAria')}>
            <OrganizationTabButton active={tab === 'repositories'} onClick={() => setTab('repositories')}>
              {t('common.repositories')}
            </OrganizationTabButton>
            {joined && <OrganizationTabButton active={tab === 'teams'} onClick={() => setTab('teams')}>{t('teams.title')}</OrganizationTabButton>}
            {joined && (
              <OrganizationTabButton active={tab === 'people'} onClick={() => setTab('people')}>
                {t('organization.people')}
              </OrganizationTabButton>
            )}
            {joined && (
              <OrganizationTabButton active={tab === 'policies'} onClick={() => setTab('policies')}>
                {t('organization.policies')}
              </OrganizationTabButton>
            )}
            {canAdmin && (
              <OrganizationTabButton active={tab === 'audit'} onClick={() => setTab('audit')}>
                {t('organization.audit')}
              </OrganizationTabButton>
            )}
            {canAdmin && <OrganizationTabButton active={tab === 'storage'} onClick={() => setTab('storage')}>{t('storage.title')}</OrganizationTabButton>}
            {canAdmin && (
              <OrganizationTabButton active={tab === 'settings'} onClick={() => setTab('settings')}>
                {t('organization.settings')}
              </OrganizationTabButton>
            )}
          </div>

          {tab === 'repositories' && (
            <OrganizationRepositories
              publicRepositories={data.repositories ?? []}
              organizationId={joined?.id ?? null}
              role={role}
            />
          )}
          {tab === 'teams' && joined && <OrganizationTeams organizationId={joined.id} canCreate={canAdmin} />}
          {tab === 'people' && joined && (
            <OrganizationPeople organization={organization as Organization} members={members.data ?? []} role={role} />
          )}
          {tab === 'policies' && joined && <OrganizationPolicies organizationId={joined.id} canAdmin={canAdmin} canOwner={role === 'owner'} />}
          {tab === 'audit' && joined && canAdmin && <OrganizationAudit organizationId={joined.id} />}
          {tab === 'storage' && joined && canAdmin && <StorageUsage namespace={joined.namespace_id} canReconcile={role === 'owner'} />}
          {tab === 'settings' && joined && canAdmin && (
            <><OrganizationSettings organization={organization as Organization} />{role === 'owner' && <RenameSpace key={organization.slug} kind="organization" id={organization.id} slug={organization.slug} />}</>
          )}
        </main>
      </div>
    </div>
  );
}

function OrganizationTabButton({
  active,
  onClick,
  children,
}: {
  active: boolean;
  onClick: () => void;
  children: ReactNode;
}) {
  return (
    <button type="button" role="tab" aria-selected={active} className={`tab${active ? ' on' : ''}`} onClick={onClick}>
      {children}
    </button>
  );
}

export function MyOrganizations() {
  const t = useT();
  const organizations = useOrganizations();
  const create = useCreateOrganization();
  const [name, setName] = useState('');
  const [slug, setSlug] = useState('');

  return (
    <section className="profile-organizations">
      <div className="profile-section-head">
        <h2 className="profile-section">{t('organization.plural')}</h2>
        <span className="count-badge">{(organizations.data ?? []).length}</span>
      </div>
      <div className="organization-list">
        {(organizations.data ?? []).map((organization) => (
          <button key={organization.id} className="organization-list-card" onClick={() => navigate(`/${organization.slug}`)}>
            {safeAvatarUrl(organization.logo) ? (
              <img src={safeAvatarUrl(organization.logo)} alt="" />
            ) : (
              <span style={{ background: avatarColor(organization.slug) }}>{organization.name.charAt(0).toUpperCase()}</span>
            )}
            <span>
              <strong>{organization.name}</strong>
              <code>/{organization.slug}</code>
            </span>
          </button>
        ))}
      </div>
      <form
        className="organization-create-form"
        onSubmit={(event) => {
          event.preventDefault();
          if (!name.trim()) return;
          create.mutate(
            { name: name.trim(), slug: slug.trim() },
            {
              onSuccess: (organization) => {
                setName('');
                setSlug('');
                navigate(`/${organization.slug}`);
              },
            },
          );
        }}
      >
        <h3>{t('organization.createTitle')}</h3>
        <div className="organization-inline-form">
          <input value={name} onChange={(event) => setName(event.target.value)} placeholder={t('organization.displayName')} />
          <input value={slug} onChange={(event) => setSlug(event.target.value.toLowerCase())} placeholder={t('organization.namespace')} spellCheck={false} />
          <button className="primary" disabled={!name.trim() || create.isPending}>
            {create.isPending ? t('common.creating') : t('common.create')}
          </button>
        </div>
        <p className="hint">{t('organization.createHint')}</p>
        {create.isError && <p className="err">{create.error.message}</p>}
      </form>
    </section>
  );
}

function OrganizationRepositories({
  publicRepositories,
  organizationId,
  role,
}: {
  publicRepositories: PublicRepository[];
  organizationId: string | null;
  role?: OrganizationRole;
}) {
  const t = useT();
  const policy = useOrganizationPolicy(organizationId);
  const privateRepositories = useOrganizationRepositories(organizationId);
  const personalAccess = useRepositories();
  const create = useCreateOrganizationRepository();
  const [name, setName] = useState('');
  const repositories: Array<Repository | PublicRepository> = organizationId
    ? privateRepositories.data ?? []
    : publicRepositories;
  const accessible = useMemo(
    () => new Set((personalAccess.data ?? []).map((repository) => repository.id)),
    [personalAccess.data],
  );
  const canCreate =
    Boolean(organizationId) &&
    (roleRank(role) >= roleRank('admin') ||
      (role === 'member' && policy.data?.repository_creation === 'members'));

  function submitRepository(event: FormEvent) {
    event.preventDefault();
    if (!organizationId || !name.trim()) return;
    create.mutate(
      { organizationId, name: name.trim() },
      { onSuccess: () => setName('') },
    );
  }

  function openRepository(repository: Repository | PublicRepository) {
    if (repository.visibility === 'public' || accessible.has(repository.id)) navigate(repositoryPath(repository));
  }

  return (
    <section className="organization-panel">
      <div className="profile-section-head">
        <h2 className="profile-section">{t('common.repositories')}</h2>
        <span className="count-badge">{repositories.length}</span>
      </div>
      {canCreate && (
        <form className="organization-inline-form" onSubmit={submitRepository}>
          <input
            value={name}
            onChange={(event) => setName(event.target.value)}
            placeholder={t('organization.repositoryName')}
            aria-label={t('organization.repositoryName')}
          />
          <button className="primary" disabled={!name.trim() || create.isPending}>
            {create.isPending ? t('common.creating') : t('organization.createRepository')}
          </button>
        </form>
      )}
      {create.isError && <p className="err">{create.error.message}</p>}
      {repositories.length === 0 ? (
        <div className="empty-box">{t('organization.noRepositories')}</div>
      ) : (
        <div className="repository-cards">
          {repositories.map((repository) => {
            const canOpen = repository.visibility === 'public' || accessible.has(repository.id);
            return (
              <div className="organization-repository-card" key={repository.id}>
                <button className="repository-card" disabled={!canOpen} onClick={() => openRepository(repository)}>
                  <span className="repository-card-top">
                    <span className="repository-card-name">{repository.name}</span>
                    <span className="repository-card-vis">
                      {repository.visibility === 'public' ? (
                        t('common.public')
                      ) : (
                        <><LockIcon /> {t('common.private')}</>
                      )}
                    </span>
                  </span>
                  <span className="repository-card-path">{repository.owner_username}/{repository.slug}</span>
                  {!canOpen && <span className="repository-card-access">{t('organization.explicitRepositoryRole')}</span>}
                </button>

              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}

function OrganizationPeople({
  organization,
  members,
  role,
}: {
  organization: Organization;
  members: OrganizationMembership[];
  role?: OrganizationRole;
}) {
  const t = useT();
  const update = useUpdateOrganizationMember();
  const remove = useRemoveOrganizationMember();
  const [removing, setRemoving] = useState('');
  const [removalAccess, setRemovalAccess] = useState<'' | 'revoke' | 'retain'>('');
  const canAdmin = roleRank(role) >= roleRank('admin');
  const canAssignOwner = role === 'owner';
  const ownerCount = members.filter((member) => member.role === 'owner').length;

  return (
    <section className="organization-panel">
      <div className="profile-section-head">
        <h2 className="profile-section">{t('organization.people')}</h2>
        <span className="count-badge">{members.length}</span>
      </div>
      {canAdmin && <InvitationManager kind="organization" spaceId={organization.id} owner={canAssignOwner} />}
      <p className="hint">{t('organization.memberAccessNote')}</p>
      {(update.isError || remove.isError) && <p className="err">{update.error?.message ?? remove.error?.message}</p>}
      {removing && <form className="management-form" onSubmit={event => {
       event.preventDefault(); if (!removalAccess) return;
       remove.mutate({organizationId:organization.id,userId:removing,access:removalAccess},{onSuccess:()=>setRemoving('')});
      }}>
       <label>{t('organization.removalAccess')}<select required value={removalAccess} onChange={event=>setRemovalAccess(event.target.value as 'revoke' | 'retain')}>
        <option value="">{t('organization.chooseRemovalAccess')}</option><option value="revoke">{t('organization.revokeDirectAccess')}</option><option value="retain">{t('organization.retainDirectAccess')}</option>
       </select></label>
       <p className="hint">{t('organization.removalHint')}</p>
       <button disabled={!removalAccess || remove.isPending}>{t('common.remove')}</button><button type="button" className="ghost" onClick={()=>setRemoving('')}>{t('common.cancel')}</button>
      </form>}
      <div className="organization-member-list">
        {members.map((member) => {
          const display = member.user?.nickname || member.user?.name || member.user?.username || member.user_id;
          const isLastOwner = member.role === 'owner' && ownerCount <= 1;
          const canRemove = canAssignOwner ? !isLastOwner : member.role === 'member';
          return (
            <div className="organization-member-row" key={member.user_id}>
              <div>
                <strong>{display}</strong>
                <code>{member.user?.username ? `@${member.user.username}` : member.user_id}</code>
              </div>
              {canAdmin ? (
                <div className="settings-row">
                  {canAssignOwner ? (
                    <select
                      aria-label={t('organization.changeRole', { name: display })}
                      value={member.role}
                      disabled={isLastOwner}
                      title={isLastOwner ? t('organization.lastOwnerRequired') : undefined}
                      onChange={(event) =>
                        update.mutate({
                          organizationId: organization.id,
                          userId: member.user_id,
                          role: event.target.value as OrganizationRole,
                        })
                      }
                    >
                      <option value="member">member</option>
                      <option value="admin">admin</option>
                      <option value="owner">owner</option>
                    </select>
                  ) : (
                    <span className="ref-badge">{member.role}</span>
                  )}
                  {canRemove && (
                    <button
                      type="button"
                      className="ghost mini"
                      onClick={() => { setRemoving(member.user_id); setRemovalAccess(''); }}
                    >
                      {t('common.remove')}
                    </button>
                  )}
                </div>
              ) : (
                <span className="ref-badge">{member.role}</span>
              )}
            </div>
          );
        })}
      </div>
    </section>
  );
}

function OrganizationPolicies({ organizationId, canAdmin, canOwner }: { organizationId: string; canAdmin: boolean; canOwner: boolean }) {
  const t = useT();
  const query = useOrganizationPolicy(organizationId);
  const effective = useQuery({ queryKey: ['organizationEffectivePolicy', organizationId, query.data], queryFn: () => api.effectiveOrganizationPolicy(organizationId) });
  const update = useUpdateOrganizationPolicy();
  const [draft, setDraft] = useState<OrganizationPolicy | null>(null);
  useEffect(() => {
    if (query.data) setDraft((previous) => previous ?? query.data);
  }, [query.data]);
  if (!draft) return <div className="loading">…</div>;

  return (
    <section className="organization-panel">
      <h2 className="profile-section">{t('organization.policies')}</h2>
      <p className="hint">{t('organization.policyEnforcedOnly')}</p>
      {effective.data && <details className="organization-access-note"><summary>{t('enterprise.inherited')}</summary><ul>
        <li>{t('enterprise.creation')}: {effective.data.repository_creation === 'admins' ? t('enterprise.adminsOnly') : t('organization.allMembers')}</li>
        <li>{t('organization.allowPublic')}: {effective.data.allow_public_repositories ? t('common.yes') : t('common.no')}</li>
      </ul></details>}
      {effective.error && <p className="err" role="alert">{effective.error.message}</p>}
      <div className="organization-policy-list">
        <label className="policy-row">
          <span>{t('organization.repositoryCreation')}</span>
          <select
            disabled={!canAdmin}
            value={draft.repository_creation}
            onChange={(event) => setDraft({ ...draft, repository_creation: event.target.value as 'admins' | 'members' })}
          >
            <option value="admins">{t('organization.adminsOnly')}</option>
            <option value="members">{t('organization.allMembers')}</option>
          </select>
        </label>
        <label className="policy-row">
          <span>{t('organization.defaultVisibility')}</span>
          <select
            disabled={!canAdmin}
            value={draft.default_repository_visibility}
            onChange={(event) =>
              setDraft({ ...draft, default_repository_visibility: event.target.value as 'private' | 'public' })
            }
          >
            <option value="private">{t('common.private')}</option>
            <option value="public" disabled={!draft.allow_public_repositories}>{t('common.public')}</option>
          </select>
        </label>
        <label className="policy-row">
          <span>{t('organization.allowPublic')}</span>
          <input
            type="checkbox"
            disabled={!canAdmin}
            checked={draft.allow_public_repositories}
            onChange={(event) =>
              setDraft({
                ...draft,
                allow_public_repositories: event.target.checked,
                default_repository_visibility: event.target.checked ? draft.default_repository_visibility : 'private',
              })
            }
          />
        </label>
        <label className="policy-row">
          <span>{t('organization.baseAccess')}</span>
          <select disabled={!canOwner} value={draft.default_repository_role ?? ''} onChange={(event) => setDraft({ ...draft, default_repository_role: event.target.value as OrganizationPolicy['default_repository_role'] })}>
            <option value="">{t('organization.noBaseAccess')}</option>
            {['viewer', 'puller', 'member', 'maintainer', 'owner'].map((role) => <option key={role} value={role}>{role}</option>)}
          </select>
        </label>
        <p className="hint">{t('organization.baseAccessHint')}</p>
      </div>
      {canAdmin && (
        <button
          className="primary organization-save"
          disabled={update.isPending}
          onClick={() =>
            update.mutate({
              organizationId,
              patch: {
                expected_updated_at: draft.updated_at,
                default_repository_role: draft.default_repository_role ?? '',
                repository_creation: draft.repository_creation,
                default_repository_visibility: draft.default_repository_visibility,
                allow_public_repositories: draft.allow_public_repositories,
              },
            }, { onSuccess: (policy) => setDraft(policy) })
          }
        >
          {update.isPending ? t('common.saving') : t('common.save')}
        </button>
      )}
      {update.isError && <p className="err">{update.error.message}</p>}
    </section>
  );
}

function OrganizationAudit({ organizationId }: { organizationId: string }) {
  const t = useT();
  const audit = useInfiniteQuery({ queryKey: ['organization-audit', organizationId, 'pages'], initialPageParam: '', queryFn: ({ pageParam }) => api.organizationAuditPage(organizationId, pageParam), getNextPageParam: page => page.next_cursor || undefined });
  const events = audit.data?.pages.flatMap(page => page.events) ?? [];
  const download = () => {
    const url = URL.createObjectURL(new Blob([events.map(event => JSON.stringify(event)).join('\n') + '\n'], { type: 'application/x-ndjson' }));
    const link = document.createElement('a'); link.href = url; link.download = 'organization-audit.jsonl'; link.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
  };
  return (
    <section className="organization-panel">
      <h2 className="profile-section">{t('organization.audit')}</h2>
      <button className="ghost mini" disabled={!events.length} onClick={download}>{t('organization.auditExport')}</button>
      {audit.error && <p className="err" role="alert">{audit.error.message}</p>}
      {audit.isLoading ? (
        <div className="loading">…</div>
      ) : events.length === 0 ? (
        <div className="empty-box">{t('organization.noAudit')}</div>
      ) : (
        <div className="organization-audit-list">
          {events.map((event) => (
            <div className="organization-audit-row" key={event.id}>
              <strong>{event.action}</strong>
              <code>{event.actor_id}</code>
              {(event.target_type || event.target_id) && (
                <span>{[event.target_type, event.target_id].filter(Boolean).join(' · ')}</span>
              )}
              {event.reason && <p>{event.reason}</p>}
              {event.correlation_id && <code>{event.correlation_id}</code>}
              <time dateTime={event.created_at}>{new Date(event.created_at).toLocaleString()}</time>
            </div>
          ))}
        </div>
      )}
      {audit.hasNextPage && <button className="ghost" disabled={audit.isFetchingNextPage} onClick={() => void audit.fetchNextPage()}>{t('organization.auditMore')}</button>}
    </section>
  );
}

function OrganizationSettings({ organization }: { organization: Organization }) {
  const t = useT();
  const update = useUpdateOrganization();
  const [name, setName] = useState(organization.name);
  const [logo, setLogo] = useState(safeAvatarUrl(organization.logo));
  const [imageError, setImageError] = useState('');

  useEffect(() => {
    setName(organization.name);
    setLogo(safeAvatarUrl(organization.logo));
  }, [organization.name, organization.logo]);

  async function pickLogo(event: ChangeEvent<HTMLInputElement>) {
    const file = event.target.files?.[0];
    event.target.value = '';
    if (!file) return;
    setImageError('');
    try {
      setLogo(await resizeToDataURL(file, t));
    } catch (error) {
      setImageError(error instanceof Error ? error.message : t('settings.imgProcessFail'));
    }
  }

  return (
    <section className="organization-panel">
      <h2 className="profile-section">{t('organization.settings')}</h2>
      <form
        className="form organization-settings-form"
        onSubmit={(event) => {
          event.preventDefault();
          const patch: { name?: string; logo?: string } = {};
          if (name.trim() !== organization.name) patch.name = name.trim();
          if (logo !== (organization.logo ?? '')) patch.logo = logo;
          if (Object.keys(patch).length > 0) update.mutate({ organizationId: organization.id, patch });
        }}
      >
        <div className="avatar-field">
          <div className="avatar-preview organization-logo">
            {logo ? <img src={logo} alt={organization.name} /> : <span className="avatar-preview-empty">{organization.name.charAt(0)}</span>}
          </div>
          <div className="avatar-actions">
            <label className="file-btn">
              {t('organization.uploadLogo')}
              <input type="file" accept="image/*" onChange={pickLogo} hidden />
            </label>
            {logo && <button type="button" className="ghost mini" onClick={() => setLogo('')}>{t('common.remove')}</button>}
            {imageError && <p className="err">{imageError}</p>}
          </div>
        </div>
        <label>
          {t('organization.displayName')}
          <input value={name} onChange={(event) => setName(event.target.value)} maxLength={128} />
        </label>
        <label>
          {t('organization.namespace')}
          <input value={organization.slug} disabled />
        </label>
        <p className="hint">{t('organization.namespaceImmutable')}</p>
        <button className="primary" disabled={!name.trim() || update.isPending}>
          {update.isPending ? t('common.saving') : t('common.save')}
        </button>
        {update.isError && <p className="err">{update.error.message}</p>}
      </form>
    </section>
  );
}
