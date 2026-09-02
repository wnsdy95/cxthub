import { useEffect, useMemo, useState, type ChangeEvent, type FormEvent, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../api';
import {
  useCreateBreakGlassGrant,
  useCreateEnterprise,
  useCreateEnterpriseWorkspace,
  useEnterpriseAudit,
  useEnterpriseMembers,
  useEnterprisePolicy,
  useEnterprises,
  useEnterpriseWorkspaces,
  useMe,
  useRemoveEnterpriseMember,
  useUpdateEnterprise,
  useUpdateEnterpriseMember,
  useUpdateEnterprisePolicy,
  useWorkspaces,
} from '../hooks';
import { useT } from '../i18n';
import { navigate, wsPath } from '../route';
import { safeAvatarUrl } from '../urls';
import type {
  Enterprise,
  EnterpriseMembership,
  EnterprisePolicy,
  EnterpriseRole,
  PublicEnterprise,
  PublicWorkspace,
  Workspace,
} from '../types';
import { avatarColor } from './Avatar';
import { LockIcon } from './Breadcrumb';
import { resizeToDataURL } from './Settings';

type EnterpriseTab = 'workspaces' | 'people' | 'policies' | 'audit' | 'settings';

function roleRank(role: EnterpriseRole | undefined): number {
  return role === 'owner' ? 3 : role === 'admin' ? 2 : role === 'member' ? 1 : 0;
}

export function EnterpriseProfile({ data }: { data: PublicEnterprise }) {
  const t = useT();
  const me = useMe().data;
  const enterprises = useEnterprises();
  const joined = (enterprises.data ?? []).find((enterprise) => enterprise.id === data.id) ?? null;
  const privateEnterprise = useQuery({
    queryKey: ['enterprise', joined?.id],
    queryFn: () => api.getEnterprise(joined!.id),
    enabled: Boolean(joined),
  });
  const enterprise: Enterprise | PublicEnterprise = privateEnterprise.data ?? joined ?? data;
  const members = useEnterpriseMembers(joined?.id ?? null);
  const role = members.data?.find((member) => member.user_id === me?.id)?.role;
  const canAdmin = roleRank(role) >= roleRank('admin');
  const [tab, setTab] = useState<EnterpriseTab>('workspaces');
  const logo = safeAvatarUrl(enterprise.logo);
  const initial = (enterprise.name || enterprise.slug || '?').trim().charAt(0).toUpperCase();

  useEffect(() => {
    if (!joined && tab !== 'workspaces') setTab('workspaces');
    if (tab === 'audit' && !canAdmin) setTab('workspaces');
    if (tab === 'settings' && !canAdmin) setTab('workspaces');
  }, [joined, canAdmin, tab]);

  return (
    <div className="profile enterprise-profile">
      <div className="profile-grid">
        <aside className="profile-side">
          {logo ? (
            <img className="avatar-lg avatar-img enterprise-logo" src={logo} alt={enterprise.name} />
          ) : (
            <div
              className="avatar-lg enterprise-logo"
              style={{ background: avatarColor(enterprise.slug) }}
              aria-hidden="true"
            >
              {initial}
            </div>
          )}
          <div className="profile-id">
            <h1 className="profile-name">{enterprise.name}</h1>
            <div className="profile-handle">{enterprise.slug}</div>
          </div>
          <span className="enterprise-badge">{t('enterprise.label')}</span>
          {joined ? (
            <p className="hint">{t('enterprise.roleLabel', { role: role ?? t('enterprise.roleLoading') })}</p>
          ) : (
            <p className="hint">{t('enterprise.publicProfile')}</p>
          )}
        </aside>

        <main className="profile-main">
          <p className="enterprise-access-note">{t('enterprise.accessSeparation')}</p>
          <div className="tabs enterprise-tabs" role="tablist" aria-label={t('enterprise.tabsAria')}>
            <EnterpriseTabButton active={tab === 'workspaces'} onClick={() => setTab('workspaces')}>
              {t('common.workspaces')}
            </EnterpriseTabButton>
            {joined && (
              <EnterpriseTabButton active={tab === 'people'} onClick={() => setTab('people')}>
                {t('enterprise.people')}
              </EnterpriseTabButton>
            )}
            {joined && (
              <EnterpriseTabButton active={tab === 'policies'} onClick={() => setTab('policies')}>
                {t('enterprise.policies')}
              </EnterpriseTabButton>
            )}
            {canAdmin && (
              <EnterpriseTabButton active={tab === 'audit'} onClick={() => setTab('audit')}>
                {t('enterprise.audit')}
              </EnterpriseTabButton>
            )}
            {canAdmin && (
              <EnterpriseTabButton active={tab === 'settings'} onClick={() => setTab('settings')}>
                {t('enterprise.settings')}
              </EnterpriseTabButton>
            )}
          </div>

          {tab === 'workspaces' && (
            <EnterpriseWorkspaces
              publicWorkspaces={data.workspaces ?? []}
              enterpriseId={joined?.id ?? null}
              role={role}
            />
          )}
          {tab === 'people' && joined && (
            <EnterprisePeople enterprise={enterprise as Enterprise} members={members.data ?? []} role={role} />
          )}
          {tab === 'policies' && joined && <EnterprisePolicies enterpriseId={joined.id} canAdmin={canAdmin} />}
          {tab === 'audit' && joined && canAdmin && <EnterpriseAudit enterpriseId={joined.id} />}
          {tab === 'settings' && joined && canAdmin && (
            <EnterpriseSettings enterprise={enterprise as Enterprise} />
          )}
        </main>
      </div>
    </div>
  );
}

function EnterpriseTabButton({
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

export function MyEnterprises() {
  const t = useT();
  const enterprises = useEnterprises();
  const create = useCreateEnterprise();
  const [name, setName] = useState('');
  const [slug, setSlug] = useState('');

  return (
    <section className="profile-enterprises">
      <div className="profile-section-head">
        <h2 className="profile-section">{t('enterprise.plural')}</h2>
        <span className="count-badge">{(enterprises.data ?? []).length}</span>
      </div>
      <div className="enterprise-list">
        {(enterprises.data ?? []).map((enterprise) => (
          <button key={enterprise.id} className="enterprise-list-card" onClick={() => navigate(`/${enterprise.slug}`)}>
            {safeAvatarUrl(enterprise.logo) ? (
              <img src={safeAvatarUrl(enterprise.logo)} alt="" />
            ) : (
              <span style={{ background: avatarColor(enterprise.slug) }}>{enterprise.name.charAt(0).toUpperCase()}</span>
            )}
            <span>
              <strong>{enterprise.name}</strong>
              <code>/{enterprise.slug}</code>
            </span>
          </button>
        ))}
      </div>
      <form
        className="enterprise-create-form"
        onSubmit={(event) => {
          event.preventDefault();
          if (!name.trim()) return;
          create.mutate(
            { name: name.trim(), slug: slug.trim() },
            {
              onSuccess: (enterprise) => {
                setName('');
                setSlug('');
                navigate(`/${enterprise.slug}`);
              },
            },
          );
        }}
      >
        <h3>{t('enterprise.createTitle')}</h3>
        <div className="enterprise-inline-form">
          <input value={name} onChange={(event) => setName(event.target.value)} placeholder={t('enterprise.displayName')} />
          <input value={slug} onChange={(event) => setSlug(event.target.value.toLowerCase())} placeholder={t('enterprise.namespace')} spellCheck={false} />
          <button className="primary" disabled={!name.trim() || create.isPending}>
            {create.isPending ? t('common.creating') : t('common.create')}
          </button>
        </div>
        <p className="hint">{t('enterprise.createHint')}</p>
        {create.isError && <p className="err">{create.error.message}</p>}
      </form>
    </section>
  );
}

function EnterpriseWorkspaces({
  publicWorkspaces,
  enterpriseId,
  role,
}: {
  publicWorkspaces: PublicWorkspace[];
  enterpriseId: string | null;
  role?: EnterpriseRole;
}) {
  const t = useT();
  const policy = useEnterprisePolicy(enterpriseId);
  const privateWorkspaces = useEnterpriseWorkspaces(enterpriseId);
  const personalAccess = useWorkspaces();
  const create = useCreateEnterpriseWorkspace();
  const grant = useCreateBreakGlassGrant();
  const [name, setName] = useState('');
  const [emergencyWorkspace, setEmergencyWorkspace] = useState<string | null>(null);
  const [reason, setReason] = useState('');
  const [minutes, setMinutes] = useState(15);
  const workspaces: Array<Workspace | PublicWorkspace> = enterpriseId
    ? privateWorkspaces.data ?? []
    : publicWorkspaces;
  const accessible = useMemo(
    () => new Set((personalAccess.data ?? []).map((workspace) => workspace.id)),
    [personalAccess.data],
  );
  const canCreate =
    Boolean(enterpriseId) &&
    (roleRank(role) >= roleRank('admin') ||
      (role === 'member' && policy.data?.workspace_creation === 'members'));

  function submitWorkspace(event: FormEvent) {
    event.preventDefault();
    if (!enterpriseId || !name.trim()) return;
    create.mutate(
      { enterpriseId, name: name.trim() },
      { onSuccess: () => setName('') },
    );
  }

  function openWorkspace(workspace: Workspace | PublicWorkspace) {
    if (workspace.visibility === 'public' || accessible.has(workspace.id)) navigate(wsPath(workspace));
  }

  return (
    <section className="enterprise-panel">
      <div className="profile-section-head">
        <h2 className="profile-section">{t('common.workspaces')}</h2>
        <span className="count-badge">{workspaces.length}</span>
      </div>
      {canCreate && (
        <form className="enterprise-inline-form" onSubmit={submitWorkspace}>
          <input
            value={name}
            onChange={(event) => setName(event.target.value)}
            placeholder={t('enterprise.workspaceName')}
            aria-label={t('enterprise.workspaceName')}
          />
          <button className="primary" disabled={!name.trim() || create.isPending}>
            {create.isPending ? t('common.creating') : t('enterprise.createWorkspace')}
          </button>
        </form>
      )}
      {create.isError && <p className="err">{create.error.message}</p>}
      {workspaces.length === 0 ? (
        <div className="empty-box">{t('enterprise.noWorkspaces')}</div>
      ) : (
        <div className="ws-cards">
          {workspaces.map((workspace) => {
            const canOpen = workspace.visibility === 'public' || accessible.has(workspace.id);
            const canBreakGlass = enterpriseId && role === 'owner' && !canOpen && policy.data?.break_glass_enabled;
            return (
              <div className="enterprise-ws-card" key={workspace.id}>
                <button className="ws-card" disabled={!canOpen} onClick={() => openWorkspace(workspace)}>
                  <span className="ws-card-top">
                    <span className="ws-card-name">{workspace.name}</span>
                    <span className="ws-card-vis">
                      {workspace.visibility === 'public' ? (
                        t('common.public')
                      ) : (
                        <><LockIcon /> {t('common.private')}</>
                      )}
                    </span>
                  </span>
                  <span className="ws-card-path">{workspace.owner_username}/{workspace.slug}</span>
                  {!canOpen && <span className="ws-card-access">{t('enterprise.explicitWorkspaceRole')}</span>}
                </button>
                {canBreakGlass && emergencyWorkspace !== workspace.id && (
                  <button className="ghost mini emergency-btn" onClick={() => setEmergencyWorkspace(workspace.id)}>
                    {t('enterprise.emergencyRead')}
                  </button>
                )}
                {canBreakGlass && emergencyWorkspace === workspace.id && (
                  <form
                    className="emergency-form"
                    onSubmit={(event) => {
                      event.preventDefault();
                      if (!enterpriseId) return;
                      grant.mutate(
                        { enterpriseId, workspaceId: workspace.id, reason: reason.trim(), minutes },
                        { onSuccess: () => navigate(wsPath(workspace)) },
                      );
                    }}
                  >
                    <strong>{t('enterprise.emergencyTitle')}</strong>
                    <textarea
                      value={reason}
                      onChange={(event) => setReason(event.target.value)}
                      placeholder={t('enterprise.emergencyReason')}
                    />
                    <label>
                      {t('enterprise.emergencyMinutes')}
                      <input
                        type="number"
                        min={5}
                        max={policy.data?.break_glass_max_minutes ?? 60}
                        value={minutes}
                        onChange={(event) => setMinutes(Number(event.target.value))}
                      />
                    </label>
                    <div className="settings-row">
                      <button className="danger-btn" disabled={reason.trim().length < 3 || grant.isPending}>
                        {t('enterprise.grantEmergencyRead')}
                      </button>
                      <button type="button" className="ghost mini" onClick={() => setEmergencyWorkspace(null)}>
                        {t('common.cancel')}
                      </button>
                    </div>
                    {grant.isError && <p className="err">{grant.error.message}</p>}
                  </form>
                )}
              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}

function EnterprisePeople({
  enterprise,
  members,
  role,
}: {
  enterprise: Enterprise;
  members: EnterpriseMembership[];
  role?: EnterpriseRole;
}) {
  const t = useT();
  const update = useUpdateEnterpriseMember();
  const remove = useRemoveEnterpriseMember();
  const [userId, setUserId] = useState('');
  const [newRole, setNewRole] = useState<EnterpriseRole>('member');
  const canAdmin = roleRank(role) >= roleRank('admin');
  const canAssignOwner = role === 'owner';
  const ownerCount = members.filter((member) => member.role === 'owner').length;

  return (
    <section className="enterprise-panel">
      <div className="profile-section-head">
        <h2 className="profile-section">{t('enterprise.people')}</h2>
        <span className="count-badge">{members.length}</span>
      </div>
      {canAdmin && (
        <form
          className="enterprise-inline-form"
          onSubmit={(event) => {
            event.preventDefault();
            if (!userId.trim()) return;
            update.mutate(
              { enterpriseId: enterprise.id, userId: userId.trim(), role: newRole },
              { onSuccess: () => setUserId('') },
            );
          }}
        >
          <input
            value={userId}
            onChange={(event) => setUserId(event.target.value)}
            placeholder={t('enterprise.userId')}
            aria-label={t('enterprise.userId')}
          />
          <select value={newRole} onChange={(event) => setNewRole(event.target.value as EnterpriseRole)}>
            <option value="member">member</option>
            {canAssignOwner && <option value="admin">admin</option>}
            {canAssignOwner && <option value="owner">owner</option>}
          </select>
          <button className="primary" disabled={!userId.trim() || update.isPending}>
            {t('enterprise.addMember')}
          </button>
        </form>
      )}
      <p className="hint">{t('enterprise.memberAccessNote')}</p>
      {(update.isError || remove.isError) && <p className="err">{update.error?.message ?? remove.error?.message}</p>}
      <div className="enterprise-member-list">
        {members.map((member) => {
          const display = member.user?.nickname || member.user?.name || member.user?.username || member.user_id;
          const isLastOwner = member.role === 'owner' && ownerCount <= 1;
          const canRemove = canAssignOwner ? !isLastOwner : member.role === 'member';
          return (
            <div className="enterprise-member-row" key={member.user_id}>
              <div>
                <strong>{display}</strong>
                <code>{member.user?.username ? `@${member.user.username}` : member.user_id}</code>
              </div>
              {canAdmin ? (
                <div className="settings-row">
                  {canAssignOwner ? (
                    <select
                      aria-label={t('enterprise.changeRole', { name: display })}
                      value={member.role}
                      disabled={isLastOwner}
                      title={isLastOwner ? t('enterprise.lastOwnerRequired') : undefined}
                      onChange={(event) =>
                        update.mutate({
                          enterpriseId: enterprise.id,
                          userId: member.user_id,
                          role: event.target.value as EnterpriseRole,
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
                      onClick={() => remove.mutate({ enterpriseId: enterprise.id, userId: member.user_id })}
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

function EnterprisePolicies({ enterpriseId, canAdmin }: { enterpriseId: string; canAdmin: boolean }) {
  const t = useT();
  const query = useEnterprisePolicy(enterpriseId);
  const update = useUpdateEnterprisePolicy();
  const [draft, setDraft] = useState<EnterprisePolicy | null>(null);
  useEffect(() => {
    if (query.data) setDraft(query.data);
  }, [query.data]);
  if (!draft) return <div className="loading">…</div>;

  return (
    <section className="enterprise-panel">
      <h2 className="profile-section">{t('enterprise.policies')}</h2>
      <p className="hint">{t('enterprise.policyEnforcedOnly')}</p>
      <div className="enterprise-policy-list">
        <label className="policy-row">
          <span>{t('enterprise.workspaceCreation')}</span>
          <select
            disabled={!canAdmin}
            value={draft.workspace_creation}
            onChange={(event) => setDraft({ ...draft, workspace_creation: event.target.value as 'admins' | 'members' })}
          >
            <option value="admins">{t('enterprise.adminsOnly')}</option>
            <option value="members">{t('enterprise.allMembers')}</option>
          </select>
        </label>
        <label className="policy-row">
          <span>{t('enterprise.defaultVisibility')}</span>
          <select
            disabled={!canAdmin}
            value={draft.default_workspace_visibility}
            onChange={(event) =>
              setDraft({ ...draft, default_workspace_visibility: event.target.value as 'private' | 'public' })
            }
          >
            <option value="private">{t('common.private')}</option>
            <option value="public" disabled={!draft.allow_public_workspaces}>{t('common.public')}</option>
          </select>
        </label>
        <label className="policy-row">
          <span>{t('enterprise.allowPublic')}</span>
          <input
            type="checkbox"
            disabled={!canAdmin}
            checked={draft.allow_public_workspaces}
            onChange={(event) =>
              setDraft({
                ...draft,
                allow_public_workspaces: event.target.checked,
                default_workspace_visibility: event.target.checked ? draft.default_workspace_visibility : 'private',
              })
            }
          />
        </label>
        <label className="policy-row">
          <span>{t('enterprise.breakGlass')}</span>
          <input
            type="checkbox"
            disabled={!canAdmin}
            checked={draft.break_glass_enabled}
            onChange={(event) => setDraft({ ...draft, break_glass_enabled: event.target.checked })}
          />
        </label>
        <label className="policy-row">
          <span>{t('enterprise.breakGlassMax')}</span>
          <input
            type="number"
            min={5}
            max={240}
            disabled={!canAdmin || !draft.break_glass_enabled}
            value={draft.break_glass_max_minutes}
            onChange={(event) => setDraft({ ...draft, break_glass_max_minutes: Number(event.target.value) })}
          />
        </label>
      </div>
      {canAdmin && (
        <button
          className="primary enterprise-save"
          disabled={update.isPending}
          onClick={() =>
            update.mutate({
              enterpriseId,
              patch: {
                workspace_creation: draft.workspace_creation,
                default_workspace_visibility: draft.default_workspace_visibility,
                allow_public_workspaces: draft.allow_public_workspaces,
                break_glass_enabled: draft.break_glass_enabled,
                break_glass_max_minutes: draft.break_glass_max_minutes,
              },
            })
          }
        >
          {update.isPending ? t('common.saving') : t('common.save')}
        </button>
      )}
      {update.isError && <p className="err">{update.error.message}</p>}
    </section>
  );
}

function EnterpriseAudit({ enterpriseId }: { enterpriseId: string }) {
  const t = useT();
  const audit = useEnterpriseAudit(enterpriseId, true);
  return (
    <section className="enterprise-panel">
      <h2 className="profile-section">{t('enterprise.audit')}</h2>
      {audit.isLoading ? (
        <div className="loading">…</div>
      ) : (audit.data ?? []).length === 0 ? (
        <div className="empty-box">{t('enterprise.noAudit')}</div>
      ) : (
        <div className="enterprise-audit-list">
          {(audit.data ?? []).map((event) => (
            <div className="enterprise-audit-row" key={event.id}>
              <strong>{event.action}</strong>
              <code>{event.actor_id}</code>
              {(event.target_type || event.target_id) && (
                <span>{[event.target_type, event.target_id].filter(Boolean).join(' · ')}</span>
              )}
              {event.reason && <p>{event.reason}</p>}
              <time dateTime={event.created_at}>{new Date(event.created_at).toLocaleString()}</time>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}

function EnterpriseSettings({ enterprise }: { enterprise: Enterprise }) {
  const t = useT();
  const update = useUpdateEnterprise();
  const [name, setName] = useState(enterprise.name);
  const [logo, setLogo] = useState(safeAvatarUrl(enterprise.logo));
  const [imageError, setImageError] = useState('');

  useEffect(() => {
    setName(enterprise.name);
    setLogo(safeAvatarUrl(enterprise.logo));
  }, [enterprise.name, enterprise.logo]);

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
    <section className="enterprise-panel">
      <h2 className="profile-section">{t('enterprise.settings')}</h2>
      <form
        className="form enterprise-settings-form"
        onSubmit={(event) => {
          event.preventDefault();
          const patch: { name?: string; logo?: string } = {};
          if (name.trim() !== enterprise.name) patch.name = name.trim();
          if (logo !== (enterprise.logo ?? '')) patch.logo = logo;
          if (Object.keys(patch).length > 0) update.mutate({ enterpriseId: enterprise.id, patch });
        }}
      >
        <div className="avatar-field">
          <div className="avatar-preview enterprise-logo">
            {logo ? <img src={logo} alt={enterprise.name} /> : <span className="avatar-preview-empty">{enterprise.name.charAt(0)}</span>}
          </div>
          <div className="avatar-actions">
            <label className="file-btn">
              {t('enterprise.uploadLogo')}
              <input type="file" accept="image/*" onChange={pickLogo} hidden />
            </label>
            {logo && <button type="button" className="ghost mini" onClick={() => setLogo('')}>{t('common.remove')}</button>}
            {imageError && <p className="err">{imageError}</p>}
          </div>
        </div>
        <label>
          {t('enterprise.displayName')}
          <input value={name} onChange={(event) => setName(event.target.value)} maxLength={128} />
        </label>
        <label>
          {t('enterprise.namespace')}
          <input value={enterprise.slug} disabled />
        </label>
        <p className="hint">{t('enterprise.namespaceImmutable')}</p>
        <button className="primary" disabled={!name.trim() || update.isPending}>
          {update.isPending ? t('common.saving') : t('common.save')}
        </button>
        {update.isError && <p className="err">{update.error.message}</p>}
      </form>
    </section>
  );
}
