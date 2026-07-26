// Settings — Account settings (top bar ⚙) and workspace settings (title bar ⚙, owner-only).
//
// Account: nickname (light alias, free to change) / username (part of URL — heavy change, red warning).
// Workspace: public status (default private, red warning and toggle). GitHub public status sync planned.
import { useEffect, useMemo, useState, type ChangeEvent, type FormEvent } from 'react';
import type { Repo, User, Workspace, WorkspacePatch } from '../types';
import { LocaleSwitcher } from './LocaleSwitcher';
import {
  useUpdateMe,
  useUpdateWorkspace,
  useCreateCliToken,
  useCliTokens,
  useRevokeCliToken,
  useWebSessions,
  useRevokeWebSession,
  useMembers,
  useTransferWorkspace,
  useSyncVisibility,
  useRepos,
  useRefs,
  useSecretsEnvelope,
  useUpdateAbout,
} from '../hooks';
import { GearBtn } from './About';
import { Portal } from './Portal';
import { useT, Rich } from '../i18n';
import { safeAvatarUrl } from '../urls';

// resizeToDataURL reduces uploaded images to a 256px square JPEG data URL (center crop).
// Stores the record as is, keeping it small (server limit ~700KB).
async function resizeToDataURL(file: File, t: ReturnType<typeof useT>, size = 256): Promise<string> {
  const url = URL.createObjectURL(file);
  try {
    const img = await new Promise<HTMLImageElement>((res, rej) => {
      const im = new Image();
      im.onload = () => res(im);
      im.onerror = () => rej(new Error(t('settings.imgReadFail')));
      im.src = url;
    });
    const canvas = document.createElement('canvas');
    canvas.width = size;
    canvas.height = size;
    const ctx = canvas.getContext('2d');
    if (!ctx) throw new Error(t('settings.canvasUnsupported'));
    const s = Math.min(img.width, img.height);
    ctx.drawImage(img, (img.width - s) / 2, (img.height - s) / 2, s, s, 0, 0, size, size);
    return canvas.toDataURL('image/jpeg', 0.85);
  } finally {
    URL.revokeObjectURL(url);
  }
}

export function AccountSettings({ user, trigger = 'gear' }: { user: User; trigger?: 'gear' | 'button' }) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const [nickname, setNickname] = useState('');
  const [username, setUsername] = useState('');
  const [loadMode, setLoadMode] = useState('');
  const [avatar, setAvatar] = useState('');
  const [avatarErr, setAvatarErr] = useState('');
  const save = useUpdateMe();

  function openModal() {
    setNickname(user.nickname ?? '');
    setUsername(user.username);
    setLoadMode(user.load_mode ?? '');
    setAvatar(safeAvatarUrl(user.avatar));
    setAvatarErr('');
    save.reset();
    setOpen(true);
  }

  async function onPickAvatar(e: ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0];
    e.target.value = ''; // Allows reselecting the same file
    if (!file) return;
    setAvatarErr('');
    try {
      setAvatar(await resizeToDataURL(file, t));
    } catch (err) {
      setAvatarErr(err instanceof Error ? err.message : t('settings.imgProcessFail'));
    }
  }

  const usernameChanged = username.trim() !== user.username;

  function submit(e: FormEvent) {
    e.preventDefault();
    const patch: { username?: string; nickname?: string; load_mode?: string; avatar?: string } = {};
    if (nickname.trim() !== (user.nickname ?? '')) patch.nickname = nickname.trim();
    if (usernameChanged) patch.username = username.trim();
    if (loadMode !== (user.load_mode ?? '')) patch.load_mode = loadMode;
    if (avatar !== (user.avatar ?? '')) patch.avatar = avatar;
    if (Object.keys(patch).length === 0) {
      setOpen(false);
      return;
    }
    save.mutate(patch, { onSuccess: () => setOpen(false) });
  }

  return (
    <>
      {trigger === 'button' ? (
        <button type="button" className="edit-profile-btn" onClick={openModal}>
          {t('settings.editProfile')}
        </button>
      ) : (
        <GearBtn label={t('settings.account')} onClick={openModal} />
      )}
      {open && (
        <Portal>
        <div className="modal-back" onClick={() => setOpen(false)}>
          <div className="modal" role="dialog" aria-label={t('settings.account')} onClick={(e) => e.stopPropagation()}>
            <h3>{t('settings.account')}</h3>
            <form onSubmit={submit} className="form">
              <div className="avatar-field">
                <div className="avatar-preview">
                  {avatar ? (
                    <img src={avatar} alt={t('settings.avatarPreview')} />
                  ) : (
                    <span className="avatar-preview-empty">
                      {(nickname || username || '?').trim().charAt(0).toUpperCase()}
                    </span>
                  )}
                </div>
                <div className="avatar-actions">
                  <label className="file-btn">
                    {t('settings.uploadPhoto')}
                    <input type="file" accept="image/*" onChange={onPickAvatar} hidden />
                  </label>
                  {avatar && (
                    <button type="button" className="ghost mini" onClick={() => setAvatar('')}>
                      {t('common.remove')}
                    </button>
                  )}
                  {avatarErr && <p className="err">{avatarErr}</p>}
                </div>
              </div>

              <label>
                {t('settings.nicknameLabel')}
                <input
                  value={nickname}
                  onChange={(e) => setNickname(e.target.value)}
                  placeholder={t('settings.nicknamePlaceholder')}
                />
              </label>
              <p className="hint">{t('settings.nicknameHint')}</p>

              <label>
                {t('settings.usernameLabel')}
                <input
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                  placeholder="url-handle"
                  spellCheck={false}
                />
              </label>
              {usernameChanged ? (
                <p className="warn-red">
                  <Rich>{t('settings.usernameWarn', { username: user.username })}</Rich>
                </p>
              ) : (
                <p className="hint">{t('settings.usernameHint')}</p>
              )}

              <label>
                {t('common.language')}
                <LocaleSwitcher className="in-form" />
              </label>

              <label>
                {t('settings.loadModeLabel')}
                <select value={loadMode} onChange={(e) => setLoadMode(e.target.value)}>
                  <option value="">{t('settings.loadFull')}</option>
                  <option value="reconstructed">{t('settings.loadReconstructed')}</option>
                  <option value="memory">{t('settings.loadMemory')}</option>
                </select>
              </label>
              <p className="hint">
                <Rich>{t('settings.loadModeHint')}</Rich>
              </p>

              <CliTokenSection />
              <WebSessionSection />

              <div className="modal-actions">
                <button type="button" className="ghost" onClick={() => setOpen(false)}>
                  {t('common.cancel')}
                </button>
                <button type="submit" disabled={save.isPending}>
                  {save.isPending ? t('common.saving') : t('common.save')}
                </button>
              </div>
              {save.error && (
                <p className="err">
                  {save.error.message.includes('conflict') ? t('settings.usernameTaken') : save.error.message}
                </p>
              )}
            </form>
          </div>
        </div>
        </Portal>
      )}
    </>
  );
}

// ArchiveSection — Archive toggle (read-only). Deletion in P1 is not possible, so archiving is the endpoint.
function ArchiveSection({ ws }: { ws: Workspace }) {
  const t = useT();
  const save = useUpdateWorkspace();
  const archived = ws.archived ?? false;
  return (
    <div className={archived ? 'danger-zone' : 'settings-upload archive-divider'}>
      <span className={archived ? 'warn-red-label' : 'label'}>{t('settings.archive')}</span>
      <p className={archived ? 'warn-red' : 'hint'}>
        {archived ? t('settings.archivedDesc') : t('settings.archiveDesc')}
      </p>
      <button
        type="button"
        className={archived ? 'ghost mini' : 'danger-btn'}
        onClick={() => save.mutate({ wsId: ws.id, patch: { archived: !archived } })}
        disabled={save.isPending}
      >
        {save.isPending ? '…' : archived ? t('settings.unarchive') : t('settings.doArchive')}
      </button>
      {save.error && <p className="err">{save.error.message}</p>}
    </div>
  );
}

// SlugSection — Danger Zone: Manual URL slug change (Korean possible — Unicode slug support).
function SlugSection({ ws }: { ws: Workspace }) {
  const t = useT();
  const [slug, setSlug] = useState(ws.slug);
  const save = useUpdateWorkspace();
  const changed = slug.trim() !== ws.slug;
  return (
    <div className="danger-zone">
      <span className="warn-red-label">{t('settings.slugChange')}</span>
      <p className="warn-red">
        <Rich>{t('settings.slugWarn', { owner: ws.owner_username, slug: ws.slug })}</Rich>
      </p>
      <div className="settings-row">
        <input value={slug} onChange={(e) => setSlug(e.target.value)} spellCheck={false} />
        <button
          type="button"
          className="danger-btn"
          disabled={!changed || save.isPending}
          onClick={() => save.mutate({ wsId: ws.id, patch: { slug: slug.trim() } })}
        >
          {save.isPending ? '…' : t('settings.change')}
        </button>
      </div>
      {save.error && (
        <p className="err">{save.error.message.includes('conflict') ? t('settings.slugTaken') : save.error.message}</p>
      )}
    </div>
  );
}

// TransferSection — Danger Zone: Ownership transfer (creator exclusive, workspace name typing confirmation).
// Transferring changes the URL (/<owner>/<slug>) to the new owner's base — existing links·CLI remote reconfiguration required.
function TransferSection({
  ws,
  members,
  onDone,
}: {
  ws: Workspace;
  members: import('../types').Membership[];
  onDone: () => void;
}) {
  const t = useT();
  const [target, setTarget] = useState('');
  const [confirm, setConfirm] = useState('');
  const transfer = useTransferWorkspace();
  const candidates = members.filter((m) => m.user_id !== ws.owner_id);
  const ready = target !== '' && confirm === ws.name;

  return (
    <div className="danger-zone">
      <span className="warn-red-label">{t('settings.transferTitle')}</span>
      <p className="warn-red">
        <Rich>{t('settings.transferWarn', { owner: ws.owner_username, slug: ws.slug })}</Rich>
      </p>
      <div className="settings-row">
        <select value={target} onChange={(e) => setTarget(e.target.value)} aria-label={t('settings.transferToAria')}>
          <option value="">{t('settings.transferToPlaceholder')}</option>
          {candidates.map((m) => (
            <option key={m.user_id} value={m.user_id}>
              {m.user?.nickname || m.user?.name || m.user_id}
            </option>
          ))}
        </select>
      </div>
      <div className="settings-row">
        <input
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          placeholder={t('settings.transferConfirm', { name: ws.name })}
          spellCheck={false}
        />
        <button
          type="button"
          className="danger-btn"
          disabled={!ready || transfer.isPending}
          onClick={() => transfer.mutate({ wsId: ws.id, toUserId: target }, { onSuccess: onDone })}
        >
          {transfer.isPending ? t('settings.transferring') : t('settings.transfer')}
        </button>
      </div>
      {transfer.error && <p className="err">{transfer.error.message}</p>}
    </div>
  );
}

// PolicyControl — Permission policy item: maintainer or higher (role-based) / Owner only.
// User unit segmentation is handled by the 5-tier role ladder (member tab).
function PolicyControl({
  label,
  policy,
  setPolicy,
}: {
  label: string;
  policy: Policy;
  setPolicy: (p: Policy) => void;
}) {
  const t = useT();
  return (
    <div className="policy-row">
      <span>{label}</span>
      <select value={policy} onChange={(e) => setPolicy(e.target.value as Policy)}>
        <option value="members">{t('settings.policyMaintainer')}</option>
        <option value="owner">{t('settings.policyOwner')}</option>
      </select>
    </div>
  );
}

// WebSessionSection — List of logged-in devices (web sessions) and logout. Discard current session = immediate logout.
function WebSessionSection() {
  const tr = useT();
  const sessions = useWebSessions(true).data ?? [];
  const revoke = useRevokeWebSession();
  return (
    <div className="settings-upload">
      <span className="label">{tr('settings.webSessions')}</span>
      <p className="hint">{tr('settings.webSessionsHint')}</p>
      {(sessions ?? []).map((t) => (
        <div key={t.suffix} className="settings-slot">
          <div className="settings-slot-info">
            <code>…{t.suffix}</code>
            {t.label && <span className="dev-label">{t.label}</span>}
            {t.current && <span className="vis-chip cur">{tr('settings.currentSession')}</span>}
            <em>{tr('settings.loginExpiry', { login: t.created_at.slice(0, 10), expiry: t.expires_at.slice(0, 10) })}</em>
          </div>
          <button type="button" className="ghost mini" onClick={() => revoke.mutate(t.suffix)} disabled={revoke.isPending}>
            {t.current ? tr('common.logout') : tr('settings.endSession')}
          </button>
        </div>
      ))}
      {(sessions ?? []).length === 0 && <p className="hint">{tr('settings.noSessions')}</p>}
      {revoke.error && <p className="err">{revoke.error.message}</p>}
    </div>
  );
}

// CliTokenSection — CLI login token issuance, listing, and revocation. Token value is shown only once in the issuance response.
function CliTokenSection() {
  const tr = useT();
  const create = useCreateCliToken();
  const revoke = useRevokeCliToken();
  const tokens = useCliTokens(true).data ?? [];
  const [copied, setCopied] = useState(false);
  const tok = create.data?.token;

  function copy() {
    if (!tok) return;
    navigator.clipboard?.writeText(`cxt login ${tok}`);
    setCopied(true);
    setTimeout(() => setCopied(false), 1600);
  }

  return (
    <div className="settings-upload">
      <span className="label">{tr('settings.cliTokens')}</span>
      <p className="hint">
        <Rich>{tr('settings.cliTokensHint')}</Rich>
      </p>
      {(tokens ?? []).map((t) => (
        <div key={t.suffix} className="settings-slot">
          <div className="settings-slot-info">
            <code>…{t.suffix}</code>
            {t.label && <span className="dev-label">{t.label}</span>}
            <em>{tr('settings.issuedExpiry', { issued: t.created_at.slice(0, 10), expiry: t.expires_at.slice(0, 10) })}</em>
          </div>
          <button type="button" className="ghost mini" onClick={() => revoke.mutate(t.suffix)} disabled={revoke.isPending}>
            {tr('settings.revokeToken')}
          </button>
        </div>
      ))}
      {tok ? (
        <div className="invite-row">
          <code>cxt login {tok}</code>
          <button type="button" className={`copy${copied ? ' done' : ''}`} onClick={copy}>
            {copied ? tr('common.copied') : tr('common.copy')}
          </button>
        </div>
      ) : (
        <button type="button" className="ghost" onClick={() => create.mutate()} disabled={create.isPending}>
          {create.isPending ? tr('settings.issuing') : tr('settings.issueToken')}
        </button>
      )}
      {tok && <p className="warn-red">{tr('settings.tokenOnce')}</p>}
      {(create.error || revoke.error) && <p className="err">{(create.error ?? revoke.error)!.message}</p>}
    </div>
  );
}

type Policy = 'members' | 'owner';
const asPolicy = (v?: string): Policy => (v === 'owner' ? 'owner' : 'members');

// WorkspaceSettings is an inline workspace settings form rendered in the 'Settings' tab body.
// (Not a modal — centered content area). Owner-only calls are gated by the caller (Dashboard).
// TeamPassphraseStatus — Displays team passphrase status only in the workspace settings tab (read-only).
// Fingerprint (id) is stored in an envelope and can be read instantly without calculation — the passphrase text is not exposed.
function TeamPassphraseStatus({ repoId, label }: { repoId: string; label: string | null }) {
  const t = useT();
  const env = useSecretsEnvelope(repoId).data;
  return (
    <div className="tp-status">
      {label && <code>{label}</code>}
      {env ? (
        <span className="ok-msg">
          {t('secrets.tpConfigured')}
          {env.fingerprint ? ` · id ${env.fingerprint}` : ''}
          {env.updated_by ? ` · ${t('secrets.tpBy', { by: env.updated_by })}` : ''}
        </span>
      ) : (
        <em className="hint">{t('common.unset')}</em>
      )}
    </div>
  );
}

// RepoBranchSettings — One-line form for repo structure settings (default branch, protected branch). Moved from About modal:
// Not for description/topic info like intro — it's a management setting to change push rules, so it stays in the workspace settings tab.
// Independent storage from the workspace main form (partial PATCH — about body unchanged).
function RepoBranchSettings({ repo, label }: { repo: Repo; label: string | null }) {
  const t = useT();
  const [branch, setBranch] = useState(repo.default_branch);
  const [protect, setProtect] = useState(repo.protect_default ?? false);
  const save = useUpdateAbout();
  // Candidate = list of actual branch refs on the server — not free input; must be chosen from actual remote branches (prevents creating non-existent default branches by typo). Current setting is always included.
  const refs = useRefs(repo.id).data ?? [];
  const branches = useMemo(() => {
    const names = new Set<string>();
    for (const r of refs) if (r.kind === 'branch' && r.name) names.add(r.name);
    if (repo.default_branch) names.add(repo.default_branch);
    return [...names].sort();
  }, [refs, repo.default_branch]);
  useEffect(() => {
    setBranch(repo.default_branch);
    setProtect(repo.protect_default ?? false);
    save.reset();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [repo.id, repo.default_branch, repo.protect_default]);
  const dirty = branch !== repo.default_branch || protect !== (repo.protect_default ?? false);
  return (
    <div className="repo-branch-row">
      {label && <code>{label}</code>}
      <label>
        {t('about.defaultBranch')}
        <select value={branch} onChange={(e) => setBranch(e.target.value)}>
          {branches.map((b) => (
            <option key={b} value={b}>
              {b}
            </option>
          ))}
        </select>
      </label>
      <label className="vis-sync">
        <input type="checkbox" checked={protect} onChange={(e) => setProtect(e.target.checked)} />
        <Rich>{t('about.protectBranch')}</Rich>
      </label>
      <button
        type="button"
        className="ghost mini"
        disabled={!dirty || save.isPending}
        onClick={() => save.mutate({ repoId: repo.id, default_branch: branch || undefined, protect_default: protect })}
      >
        {save.isPending ? t('common.saving') : t('common.save')}
      </button>
      {save.isSuccess && !dirty && <span className="ok-msg">{t('settings.saved')}</span>}
      {save.error && <span className="err">{save.error.message}</span>}
    </div>
  );
}

export function WorkspaceSettings({ ws, isCreator }: { ws: Workspace; isCreator: boolean }) {
  const t = useT();
  const [pub, setPub] = useState(ws.visibility === 'public');
  const [ghSync, setGhSync] = useState(ws.gh_visibility_sync ?? false);
  const [secretsPolicy, setSecretsPolicy] = useState<Policy>(asPolicy(ws.secrets_policy));
  const [settingsPolicy, setSettingsPolicy] = useState<Policy>(asPolicy(ws.settings_policy));
  const [webhook, setWebhook] = useState(ws.webhook_url ?? '');
  const [publicRole, setPublicRole] = useState<'viewer' | 'puller'>(ws.public_role === 'puller' ? 'puller' : 'viewer');
  const save = useUpdateWorkspace();
  const syncNow = useSyncVisibility();
  const members = useMembers(ws.id).data ?? [];
  const repos = useRepos(ws.id).data ?? [];

  // Reinitialize form state with the values of the new workspace when switching workspaces.
  useEffect(() => {
    setPub(ws.visibility === 'public');
    setGhSync(ws.gh_visibility_sync ?? false);
    setSecretsPolicy(asPolicy(ws.secrets_policy));
    setSettingsPolicy(asPolicy(ws.settings_policy));
    setWebhook(ws.webhook_url ?? '');
    setPublicRole(ws.public_role === 'puller' ? 'puller' : 'viewer');
    save.reset();
    syncNow.reset();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ws.id]);

  // All items in this form (publicity, member role, permission policy, webhook, GH sync) only change local state,
  // and must be applied by pressing the save button — toggling or entering alone does not reflect on the server.
  function buildPatch(): WorkspacePatch {
    const patch: WorkspacePatch = {};
    if (ghSync !== (ws.gh_visibility_sync ?? false)) patch.gh_visibility_sync = ghSync;
    // If synchronization is enabled (including while enabling), manual visibility is not sent — the server also rejects it.
    if (!ghSync && pub !== (ws.visibility === 'public')) patch.visibility = pub ? 'public' : 'private';
    if (secretsPolicy !== asPolicy(ws.secrets_policy)) patch.secrets_policy = secretsPolicy;
    if (settingsPolicy !== asPolicy(ws.settings_policy)) patch.settings_policy = settingsPolicy;
    if (webhook.trim() !== (ws.webhook_url ?? '')) patch.webhook_url = webhook.trim();
    if (publicRole !== (ws.public_role === 'puller' ? 'puller' : 'viewer')) patch.public_role = publicRole;
    return patch;
  }
  const dirty = Object.keys(buildPatch()).length > 0; // Check if there are any unsaved changes.

  function submit(e: FormEvent) {
    e.preventDefault();
    const patch = buildPatch();
    if (Object.keys(patch).length === 0) return;
    save.mutate({ wsId: ws.id, patch });
  }

  return (
    <form onSubmit={submit} className="form ws-settings-form">
              <div className="vis-row">
                <span className="warn-red-label">{t('settings.visibility')}</span>
                <button
                  type="button"
                  role="switch"
                  aria-checked={pub}
                  className={`toggle${pub ? ' on' : ''}`}
                  onClick={() => !ghSync && setPub(!pub)}
                  disabled={ghSync}
                  title={ghSync ? t('settings.ghLockTitle') : undefined}
                >
                  <span className="knob" />
                </button>
                <code>{(ghSync ? ws.visibility === 'public' : pub) ? 'public' : 'private'}</code>
              </div>
              {ghSync ? (
                <p className="hint">{t('settings.ghManages')}</p>
              ) : pub ? (
                <p className="warn-red">{t('settings.publicWarn')}</p>
              ) : (
                <p className="hint">{t('settings.privateHint')}</p>
              )}

              {(ghSync ? ws.visibility === 'public' : pub) && (
                <div className="settings-upload">
                  <span className="label">{t('settings.nonMemberRole')}</span>
                  <p className="hint">
                    <Rich>{t('settings.nonMemberHint')}</Rich>
                  </p>
                  <select value={publicRole} onChange={(e) => setPublicRole(e.target.value as 'viewer' | 'puller')}>
                    <option value="viewer">{t('settings.roleViewerWeb')}</option>
                    <option value="puller">{t('settings.rolePullerWeb')}</option>
                  </select>
                </div>
              )}

              <label className="vis-sync">
                <input type="checkbox" checked={ghSync} onChange={(e) => setGhSync(e.target.checked)} />
                {t('settings.ghSync')}
              </label>
              {ghSync && (
                <div className="sync-row">
                  <p className="hint">
                    <Rich>{t('settings.ghSyncHint')}</Rich>
                    {ws.gh_synced_at && ` ${t('settings.lastSync', { when: ws.gh_synced_at.slice(0, 16).replace('T', ' ') })}`}
                  </p>
                  {(ws.gh_visibility_sync ?? false) && (
                    <button
                      type="button"
                      className="ghost mini"
                      onClick={() => syncNow.mutate(ws.id)}
                      disabled={syncNow.isPending}
                    >
                      {syncNow.isPending ? t('settings.syncing') : t('settings.syncNow')}
                    </button>
                  )}
                  {syncNow.data && <p className="hint ok-msg">{t('settings.synced', { vis: syncNow.data.visibility ?? 'private' })}</p>}
                  {syncNow.error && <p className="err">{syncNow.error.message}</p>}
                </div>
              )}

              <div className="settings-upload">
                <span className="label">{t('settings.permissions')}</span>
                <p className="hint">{t('settings.permissionsHint')}</p>
                <PolicyControl label={t('about.secretsSettings')} policy={secretsPolicy} setPolicy={setSecretsPolicy} />
                <PolicyControl label={t('settings.settingsPolicy')} policy={settingsPolicy} setPolicy={setSettingsPolicy} />
              </div>

              <div className="settings-upload">
                <span className="label">{t('settings.branchSection')}</span>
                <p className="hint">{t('settings.branchHint')}</p>
                {repos.length === 0 ? (
                  <em className="hint">{t('secrets.tpNoRepo')}</em>
                ) : (
                  repos.map((r) => (
                    <RepoBranchSettings key={r.id} repo={r} label={repos.length > 1 ? r.id.slice(0, 10) : null} />
                  ))
                )}
              </div>

              <div className="settings-upload">
                <span className="label">{t('secrets.tpLabel')}</span>
                <p className="hint">
                  <Rich>{t('secrets.tpHint')}</Rich>
                </p>
                {repos.length === 0 ? (
                  <em className="hint">{t('secrets.tpNoRepo')}</em>
                ) : (
                  repos.map((r) => <TeamPassphraseStatus key={r.id} repoId={r.id} label={repos.length > 1 ? r.id.slice(0, 10) : null} />)
                )}
              </div>

              <div className="settings-upload">
                <span className="label">{t('settings.webhook')}</span>
                <p className="hint">
                  <Rich>{t('settings.webhookHint')}</Rich>
                </p>
                <input
                  value={webhook}
                  onChange={(e) => setWebhook(e.target.value)}
                  placeholder="https://hooks.slack.com/services/…"
                  spellCheck={false}
                />
              </div>

              {/* The save button is placed immediately below the main form it applies to — the Danger Zone below
                  is clearly positioned to indicate that this save is independent of the button. */}
              <div className="modal-actions">
                {dirty && <span className="unsaved-note">{t('settings.unsaved')}</span>}
                {save.isSuccess && !dirty && <span className="ok-msg">{t('settings.saved')}</span>}
                <button type="submit" disabled={save.isPending || !dirty}>
                  {save.isPending ? t('common.saving') : t('common.save')}
                </button>
              </div>
              {save.error && <p className="err">{save.error.message}</p>}

              <ArchiveSection ws={ws} />
              {isCreator && <SlugSection ws={ws} />}
              {isCreator && <TransferSection ws={ws} members={members} onDone={() => {}} />}
    </form>
  );
}
