// About mirrors GitHub's repository About panel at the top of the right rail. It shows
// description, website, and topics, with an edit modal. TeamSettings and SecretsPanel
// are independent sections below it, each with its own settings modal.
import { useEffect, useState, type FormEvent } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import type { Repo } from '../types';
import { useUpdateAbout, usePutSettings, useSettingsBundle, useSecretsEnvelope } from '../hooks';
import { api } from '../api';
import { encryptSecrets, decryptSecrets, fingerprint } from '../secretscrypto';
import { generatePassphrase, passphraseError } from '../passphrase';
import { buildZip, b64ToBytes, saveBlob } from '../zip';
import { useT, Rich } from '../i18n';
import { Portal } from './Portal';

// Chromium File System Access API support for writing .cxtsecrets directly to a selected local folder.
declare global {
  interface Window {
    showDirectoryPicker?: (opts?: { mode?: 'read' | 'readwrite' }) => Promise<FileSystemDirectoryHandle>;
  }
}

// GearBtn is the shared settings control used across repository, team, secrets, account, and workspace panels.
export function GearBtn({ label, onClick }: { label: string; onClick: () => void }) {
  return (
    <button className="gear" aria-label={label} title={label} onClick={onClick}>
      <svg width="15" height="15" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
        <path d="M8 5.3a2.7 2.7 0 1 0 0 5.4 2.7 2.7 0 0 0 0-5.4Zm0 4a1.3 1.3 0 1 1 0-2.6 1.3 1.3 0 0 1 0 2.6Z" />
        <path d="M9.1 1l.4 1.6c.4.14.77.33 1.12.56l1.55-.55 1.1 1.9-1.16 1.06c.04.28.06.57.06.86l1.17 1.05-1.1 1.9-1.55-.54c-.35.23-.73.42-1.12.56L9.1 15H6.9l-.4-1.6a5.5 5.5 0 0 1-1.12-.56l-1.55.55-1.1-1.9 1.16-1.06A5.7 5.7 0 0 1 3.83 8L2.66 6.95l1.1-1.9 1.55.54c.35-.23.73-.42 1.12-.56L6.9 1h2.2ZM8 3.5 7.7 2.4h-.02l-.28 1.1a4.2 4.2 0 0 0-2.1 1.05l-1.07-.38-.01.02.8.73A4.4 4.4 0 0 0 4.63 8c0 .38.05.75.14 1.1l-.8.72.01.02 1.07-.37a4.2 4.2 0 0 0 2.1 1.04l.28 1.1h.02l.28-1.1a4.2 4.2 0 0 0 2.1-1.04l1.07.37.01-.02-.8-.72c.1-.35.14-.72.14-1.1 0-.37-.05-.74-.14-1.09l.8-.73-.01-.02-1.07.38a4.2 4.2 0 0 0-2.1-1.05L8.02 2.4 8 3.5Z" />
      </svg>
    </button>
  );
}

function normalizeWebsiteInput(raw: string): string | null {
  const value = raw.trim();
  if (!value) return '';
  if (value.length > 2048 || /[\u0000-\u0020\u007f\\]/.test(value)) return null;
  const withScheme = value.includes('://') ? value : `https://${value}`;
  try {
    const url = new URL(withScheme);
    if (url.protocol === 'http:' || url.protocol === 'https:') {
      if (!url.hostname || url.username || url.password) return null;
      const normalized = url.pathname === '/' && !url.search && !url.hash ? url.origin : url.href;
      return normalized.length <= 2048 ? normalized : null;
    }
  } catch {
    return null;
  }
  return null;
}

function displayWebsite(raw: string): { href: string; label: string } | null {
  const href = normalizeWebsiteInput(raw);
  if (!href) return null;
  const label = href.replace(/^https?:\/\//, '');
  return { href, label };
}

// Convert a folder upload into bundle files. The top-level folder must match
// kind (claude|agents|codex); remove that segment to produce each relative path.
async function readFolder(
  files: FileList,
  kind: string,
  t: ReturnType<typeof useT>,
): Promise<{ path: string; content_b64: string }[]> {
  const out: { path: string; content_b64: string }[] = [];
  for (const f of Array.from(files)) {
    const rel = (f as File & { webkitRelativePath?: string }).webkitRelativePath || f.name;
    const segs = rel.split('/');
    if (segs[0] !== kind) {
      throw new Error(t('about.folderNameErr', { kind, got: segs[0] }));
    }
    const path = segs.slice(1).join('/');
    if (!path || path.startsWith('.git/')) continue;
    const b64 = await new Promise<string>((res, rej) => {
      const r = new FileReader();
      r.onload = () => res(String(r.result).split(',')[1] ?? '');
      r.onerror = rej;
      r.readAsDataURL(f);
    });
    out.push({ path, content_b64: b64 });
  }
  if (out.length === 0) throw new Error(t('about.noFiles'));
  return out;
}

export function About({ repo, canEdit }: { repo: Repo; canEdit: boolean }) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const [desc, setDesc] = useState(repo.description ?? '');
  const [website, setWebsite] = useState(repo.website ?? '');
  const [topics, setTopics] = useState((repo.topics ?? []).join(' '));
  const [websiteErr, setWebsiteErr] = useState('');
  const save = useUpdateAbout();

  useEffect(() => {
    setDesc(repo.description ?? '');
    setWebsite(repo.website ?? '');
    setTopics((repo.topics ?? []).join(' '));
  }, [repo.id, repo.description, repo.website, repo.topics]);

  // Default branch protection is managed by RepoBranchSettings on the Workspace Settings tab.
  function submit(e: FormEvent) {
    e.preventDefault();
    const normalizedWebsite = normalizeWebsiteInput(website);
    if (normalizedWebsite === null) {
      setWebsiteErr(t('about.urlInvalid'));
      return;
    }
    setWebsiteErr('');
    save.mutate(
      {
        repoId: repo.id,
        description: desc,
        website: normalizedWebsite,
        topics: topics.split(/\s+/).filter(Boolean),
      },
      { onSuccess: () => setOpen(false) },
    );
  }

  const empty = !repo.description && !repo.website && !(repo.topics ?? []).length;
  const websiteDisplay = repo.website ? displayWebsite(repo.website) : null;

  return (
    <div className="about">
      <div className="about-head">
        <span className="label">About</span>
        {canEdit && <GearBtn label={t('about.editDetails')} onClick={() => setOpen(true)} />}
      </div>

      {empty ? (
        <p className="about-empty">{t('about.empty')}</p>
      ) : (
        <div className="about-body">
          {repo.description && <p>{repo.description}</p>}
          {repo.website && websiteDisplay ? (
            <a href={websiteDisplay.href} target="_blank" rel="noopener noreferrer" className="about-link">
              {websiteDisplay.label} ↗
            </a>
          ) : repo.website ? (
            <span className="about-link about-link-disabled">{repo.website}</span>
          ) : null}
          {(repo.topics ?? []).length > 0 && (
            <div className="about-topics">
              {(repo.topics ?? []).map((t) => (
                <span key={t} className="topic-chip">
                  {t}
                </span>
              ))}
            </div>
          )}
        </div>
      )}

      {open && (
        <Portal>
        <div className="modal-back" onClick={() => setOpen(false)}>
          <div className="modal" role="dialog" aria-label={t('about.editDetails')} onClick={(e) => e.stopPropagation()}>
            <h3>{t('about.editDetails')}</h3>
            <form onSubmit={submit} className="form">
              <label>
                {t('about.description')}
                <input value={desc} onChange={(e) => setDesc(e.target.value)} placeholder={t('about.descPlaceholder')} />
              </label>
              <label>
                {t('about.website')}
                <input
                  value={website}
                  onChange={(e) => {
                    setWebsite(e.target.value);
                    if (websiteErr) setWebsiteErr('');
                  }}
                  placeholder={t('about.urlPlaceholder')}
                />
              </label>
              <label>
                {t('about.topics')}
                <input value={topics} onChange={(e) => setTopics(e.target.value)} placeholder="context ai team" />
              </label>
              <div className="modal-actions">
                <button type="button" className="ghost" onClick={() => setOpen(false)}>
                  {t('common.cancel')}
                </button>
                <button type="submit" disabled={save.isPending}>
                  {save.isPending ? t('common.saving') : t('about.saveChanges')}
                </button>
              </div>
              {websiteErr && <p className="err">{websiteErr}</p>}
              {save.error && <p className="err">{save.error.message}</p>}
            </form>
          </div>
        </div>
        </Portal>
      )}
    </div>
  );
}


// TeamSettings manages shared .claude/.agents/.codex defaults in the side rail.
// Pullers can view status and download. Public workspaces keep the management
// affordance visible, but deny it before mounting the dialog when canWrite is false.
export function TeamSettings({
  repoId,
  canWrite,
  showLockedControl = false,
}: {
  repoId: string;
  canWrite: boolean;
  showLockedControl?: boolean;
}) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const [denied, setDenied] = useState(false);
  return (
    <div className="side-sec">
      <div className="about-head">
        <span className="label">{t('about.teamSettings')}</span>
        {(canWrite || showLockedControl) && (
          <GearBtn
            label={t('about.uploadTeamSettings')}
            onClick={() => {
              if (!canWrite) {
                setDenied(true);
                return;
              }
              setDenied(false);
              setOpen(true);
            }}
          />
        )}
      </div>
      <SettingsRow repoId={repoId} kind="claude" />
      <SettingsRow repoId={repoId} kind="agents" />
      <SettingsRow repoId={repoId} kind="codex" />
      {denied && (
        <p className="err slot-msg" role="alert">
          {t('about.teamSettingsAccessDenied')}
        </p>
      )}

      {open && (
        <Portal>
        <div className="modal-back" onClick={() => setOpen(false)}>
          <div className="modal" role="dialog" aria-label={t('about.uploadTeamSettings')} onClick={(e) => e.stopPropagation()}>
            <h3>{t('about.uploadTeamSettings')}</h3>
            <div className="guide">
              <p>
                <Rich>{t('about.teamGuideIntro')}</Rich>
              </p>
              <ul>
                <li>
                  <Rich>{t('about.teamGuide1')}</Rich>
                </li>
                <li>
                  <Rich>{t('about.teamGuide2')}</Rich>
                </li>
                <li>
                  <Rich>{t('about.teamGuide3')}</Rich>
                </li>
                <li>
                  <Rich>{t('about.teamGuide4')}</Rich>
                </li>
              </ul>
            </div>
            <SettingsSlot repoId={repoId} kind="claude" />
            <SettingsSlot repoId={repoId} kind="agents" />
            <SettingsSlot repoId={repoId} kind="codex" />
            <div className="modal-actions">
              <button type="button" className="ghost" onClick={() => setOpen(false)}>
                {t('common.close')}
              </button>
            </div>
          </div>
        </div>
        </Portal>
      )}
    </div>
  );
}

// SettingsRow — Row for displaying the rail: current status (file count, uploader) and download button (zip).
function SettingsRow({ repoId, kind }: { repoId: string; kind: 'claude' | 'agents' | 'codex' }) {
  const t = useT();
  const current = useSettingsBundle(repoId, kind, true);
  function download() {
    const b = current.data;
    if (!b) return;
    const zip = buildZip(b.files.map((f) => ({ path: `${kind}/${f.path}`, bytes: b64ToBytes(f.content_b64) })));
    saveBlob(zip, `${kind}-settings.zip`);
  }
  return (
    <div className="settings-slot">
      <div className="settings-slot-info">
        <code>{kind}</code> <span className="arrow">→ .{kind}/</span>
        <em>
          {current.data
            ? `${t('about.filesN', { count: current.data.files.length })}${current.data.updated_by ? ` · by ${current.data.updated_by}` : ''}`
            : t('common.unset')}
        </em>
      </div>
      {current.data && (
        <button type="button" className="ghost mini" onClick={download}>
          {t('about.download')}
        </button>
      )}
    </div>
  );
}

// SettingsSlot — Upload row in the ⚙ modal: status + folder selection.
function SettingsSlot({ repoId, kind }: { repoId: string; kind: 'claude' | 'agents' | 'codex' }) {
  const t = useT();
  const put = usePutSettings();
  const current = useSettingsBundle(repoId, kind, true);
  const [msg, setMsg] = useState<string | null>(null);

  async function onFolder(files: FileList | null) {
    if (!files || files.length === 0) return;
    setMsg(null);
    try {
      const bundle = await readFolder(files, kind, t);
      put.mutate(
        { repoId, kind, files: bundle },
        {
          onSuccess: (r) => setMsg(t('about.uploaded', { count: r.files })),
          onError: (x) => setMsg(t('about.failed', { msg: x.message })),
        },
      );
    } catch (x) {
      setMsg(x instanceof Error ? x.message : String(x));
    }
  }

  return (
    <div className="settings-slot">
      <div className="settings-slot-info">
        <code>{kind}</code> <span className="arrow">→ .{kind}/</span>
        <em>
          {current.data
            ? `${t('about.filesN', { count: current.data.files.length })}${current.data.updated_by ? ` · by ${current.data.updated_by}` : ''}`
            : t('common.unset')}
        </em>
      </div>
      <label className="file-btn">
        {put.isPending ? t('about.uploading') : current.data ? t('about.replace') : t('about.pickFolder')}
        <input
          type="file"
          // @ts-expect-error Non-standard folder selection attribute
          webkitdirectory=""
          multiple
          style={{ display: 'none' }}
          onChange={(e) => onFolder(e.target.files)}
        />
      </label>
      {msg && <p className={msg.startsWith('✓') ? 'hint ok-msg slot-msg' : 'err slot-msg'}>{msg}</p>}
    </div>
  );
}


// SecretsPanel shares .cxtsecrets through end-to-end encryption. The rail shows only
// status and encrypted save/load actions; secret input, passphrase, and details stay in
// the settings modal. Plaintext, passphrases, and keys never leave the browser. Only a
// PBKDF2 (600k) + AES-256-GCM envelope reaches the server, and teammates decrypt it with
// `cxt secrets pull -p <pw>`.
export function SecretsPanel({
  repoId,
  canWrite,
  showLockedControl = false,
}: {
  repoId: string;
  canWrite: boolean;
  showLockedControl?: boolean;
}) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const [lines, setLines] = useState('');
  const [pass, setPass] = useState('');
  const [newPass, setNewPass] = useState('');
  const [showPass, setShowPass] = useState(false);
  const [rotate, setRotate] = useState(false);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const envelope = useSecretsEnvelope(repoId);
  const qc = useQueryClient();

  async function requireEnvelope() {
    const env = await api.getSecrets(repoId);
    if (!env) throw new Error(t('secrets.notConfigured'));
    return env;
  }

  async function save() {
    if (!pass || !lines.trim()) {
      setOpen(true);
      setMsg(!pass ? t('about.needPass') : t('about.needSecrets'));
      return;
    }
    const pe = passphraseError(pass); // Enforce 4+ word passphrase, 12+ characters
    if (pe) {
      setOpen(true);
      setMsg(t(pe));
      return;
    }
    setBusy(true);
    setMsg(null);
    try {
      // Team passphrase consistency: If a fingerprint already exists, the input passphrase's fingerprint must match to proceed (mismatched replaces the tab). The server also re-verifies with 409.
      const existing = envelope.data?.fingerprint;
      if (existing && (await fingerprint(pass, repoId)) !== existing) {
        setMsg(t('secrets.mismatch', { id: existing }));
        return;
      }
      const env = await encryptSecrets(pass, lines.endsWith('\n') ? lines : lines + '\n', repoId);
      await api.putSecrets(repoId, env);
      qc.invalidateQueries({ queryKey: ['secrets', repoId] });
      setMsg(t('about.encryptedSaved'));
      setLines('');
    } catch (x) {
      setMsg(x instanceof Error ? x.message : String(x));
    } finally {
      setBusy(false);
    }
  }

  // rotateSecrets — re-key: decrypts the server's current secret with 'current passphrase' and re-encrypts it with 'new passphrase'. Content comes from the server envelope; decryption fails if the current passphrase is unknown (forces old passphrase knowledge). Race condition between GET and PUT is resolved by server's 409 rejection if expect (based on envelope's fingerprint) CAS fails — without this, stale plaintext re-encryption overwrites team member's update silently.
  async function rotateSecrets() {
    if (!pass) {
      setMsg(t('about.needPassDecrypt'));
      return;
    }
    const pe = passphraseError(newPass); // enforces new passphrase format
    if (pe) {
      setMsg(t(pe));
      return;
    }
    setBusy(true);
    setMsg(null);
    try {
      const cur = await requireEnvelope();
      const plain = await decryptSecrets(pass, cur, repoId, t); // decrypts with current passphrase (throws if incorrect)
      const env = await encryptSecrets(newPass, plain, repoId); // re-encrypts with new passphrase
      await api.putSecrets(repoId, env, true, cur.fingerprint ?? ''); // rotate CAS — based envelope fingerprint
      qc.invalidateQueries({ queryKey: ['secrets', repoId] });
      setMsg(t('secrets.rotated'));
      setPass('');
      setNewPass('');
      setRotate(false);
    } catch (x) {
      setMsg(x instanceof Error ? x.message : String(x));
    } finally {
      setBusy(false);
    }
  }

  async function load() {
    if (!pass) {
      setOpen(true);
      setMsg(t('about.needPassDecrypt'));
      return;
    }
    setBusy(true);
    setMsg(null);
    try {
      const env = await requireEnvelope();
      setLines(await decryptSecrets(pass, env, repoId, t));
      setOpen(true); // decryption result confirmed and editable in modal
      setMsg(t('about.decrypted') + (env.updated_at ? ` (${env.updated_at.slice(0, 10)})` : ''));
    } catch (x) {
      setMsg(x instanceof Error ? x.message : String(x));
    } finally {
      setBusy(false);
    }
  }

  // Local storage — decrypts server envelope in browser and writes directly to the user-selected local connection folder (repo root) as .cxtsecrets. End-to-end: plaintext does not leave the browser network.
  // Browsers without File System Access API fallback to file download.
  async function saveLocal() {
    if (!pass) {
      setOpen(true);
      setMsg(t('about.needPassDecrypt'));
      return;
    }
    setBusy(true);
    setMsg(null);
    try {
      const env = await requireEnvelope();
      const text = await decryptSecrets(pass, env, repoId, t);
      if (window.showDirectoryPicker) {
        const dir = await window.showDirectoryPicker({ mode: 'readwrite' });
        let warn = '';
        try {
          await dir.getDirectoryHandle('.git');
        } catch {
          warn = t('about.noGitWarn');
        }
        const fh = await dir.getFileHandle('.cxtsecrets', { create: true });
        const w = await fh.createWritable();
        await w.write(text);
        await w.close();
        setMsg(t('about.savedTo', { dir: dir.name }) + warn);
      } else {
        saveBlob(new Blob([text], { type: 'text/plain' }), 'cxtsecrets');
        setMsg(t('about.noFsApi'));
      }
    } catch (x) {
      if (x instanceof DOMException && x.name === 'AbortError') return; // Folder selection canceled
      setMsg(x instanceof Error ? x.message : String(x));
    } finally {
      setBusy(false);
    }
  }

  const status = envelope.data
    ? `${t('about.configured')}${envelope.data.fingerprint ? ` · id ${envelope.data.fingerprint}` : ''}${envelope.data.updated_at ? ` · ${envelope.data.updated_at.slice(0, 10)}` : ''}${envelope.data.updated_by ? ` · by ${envelope.data.updated_by}` : ''}`
    : t('common.unset');

  return (
    <div className="side-sec">
      <div className="about-head">
        <span className="label">.cxtsecrets</span>
        {(canWrite || showLockedControl) && (
          <GearBtn
            label={t('about.secretsSettings')}
            onClick={() => {
              if (!canWrite) {
                setMsg(t('about.secretsSettingsAccessDenied'));
                return;
              }
              setMsg(null);
              setOpen(true);
            }}
          />
        )}
      </div>
      <div className="settings-slot">
        <div className="settings-slot-info">
          <span className="arrow">{t('about.e2e')}</span>
          <em>{status}</em>
        </div>
      </div>
      <div className="settings-row">
        <button type="button" className="mini" onClick={saveLocal} disabled={busy}>
          {busy ? '…' : t('about.saveLocal')}
        </button>
      </div>
      {!open && msg && (
        <p className={msg.startsWith('✓') ? 'hint ok-msg slot-msg' : 'err slot-msg'} role="alert">
          {msg}
        </p>
      )}

      {open && (
        <Portal>
        <div className="modal-back" onClick={() => setOpen(false)}>
          <div className="modal" role="dialog" aria-label={t('about.secretsSettings')} onClick={(e) => e.stopPropagation()}>
            <h3>{t('about.secretsSettings')}</h3>
            <div className="guide">
              <p>
                <Rich>{t('about.secretsGuideIntro')}</Rich>
              </p>
              <ul>
                <li>
                  <Rich>{t('about.secretsGuide1')}</Rich>
                </li>
                <li>
                  <Rich>{t('about.secretsGuide2')}</Rich>
                </li>
                <li>
                  <Rich>{t('about.secretsGuide3')}</Rich>
                </li>
                <li>
                  <Rich>{t('about.secretsGuide4')}</Rich>
                </li>
              </ul>
            </div>
            {!rotate && (
              <textarea
                className="secrets-area"
                rows={5}
                placeholder={'sk-…\nAKIA…'}
                value={lines}
                onChange={(e) => setLines(e.target.value)}
                spellCheck={false}
              />
            )}
            <div className="settings-row">
              <input
                type={showPass ? 'text' : 'password'}
                placeholder={rotate ? t('secrets.curPassPlaceholder') : t('about.passPlaceholder')}
                value={pass}
                onChange={(e) => setPass(e.target.value)}
                autoComplete="new-password"
                spellCheck={false}
              />
              <button
                type="button"
                className="mini"
                onClick={() => setShowPass((v) => !v)}
                aria-pressed={showPass}
                aria-label={t('secrets.togglePass')}
                title={t('secrets.togglePass')}
              >
                {showPass ? '🙈' : '👁'}
              </button>
              {canWrite && !rotate && (
                <button
                  type="button"
                  className="mini"
                  onClick={() => {
                    setPass(generatePassphrase());
                    setShowPass(true);
                  }}
                  aria-label={t('secrets.generateAria')}
                >
                  {t('secrets.generate')}
                </button>
              )}
            </div>
            {rotate && (
              <div className="settings-row">
                <input
                  type={showPass ? 'text' : 'password'}
                  placeholder={t('secrets.newPassPlaceholder')}
                  value={newPass}
                  onChange={(e) => setNewPass(e.target.value)}
                  autoComplete="new-password"
                  spellCheck={false}
                />
                {canWrite && (
                  <button
                    type="button"
                    className="mini"
                    onClick={() => {
                      setNewPass(generatePassphrase());
                      setShowPass(true);
                    }}
                    aria-label={t('secrets.generateAria')}
                  >
                    {t('secrets.generate')}
                  </button>
                )}
              </div>
            )}
            {canWrite && envelope.data?.fingerprint && (
              <label className="rotate-row">
                <input
                  type="checkbox"
                  checked={rotate}
                  onChange={(e) => {
                    setRotate(e.target.checked);
                    if (!e.target.checked) setNewPass('');
                  }}
                />
                <span>{t('secrets.rotateLabel')}</span>
                <em className="hint">{t('secrets.rotateHint')}</em>
              </label>
            )}
            {msg && <p className={msg.startsWith('✓') ? 'hint ok-msg slot-msg' : 'err slot-msg'}>{msg}</p>}
            <div className="modal-actions">
              <button type="button" className="ghost" onClick={() => setOpen(false)}>
                {t('common.close')}
              </button>
              {!rotate && (
                <button type="button" className="ghost" onClick={load} disabled={busy}>
                  {t('about.load')}
                </button>
              )}
              {!rotate && (
                <button type="button" className="ghost" onClick={saveLocal} disabled={busy}>
                  {t('about.saveLocal')}
                </button>
              )}
              {canWrite &&
                (rotate ? (
                  <button type="button" onClick={rotateSecrets} disabled={busy}>
                    {busy ? '…' : t('secrets.rotateAction')}
                  </button>
                ) : (
                  <button type="button" onClick={save} disabled={busy}>
                    {busy ? '…' : t('about.encryptSave')}
                  </button>
                ))}
            </div>
          </div>
        </div>
        </Portal>
      )}
    </div>
  );
}
