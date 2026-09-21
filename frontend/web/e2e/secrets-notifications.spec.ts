import { test, expect, type Page } from '@playwright/test';
import { createCipheriv, createHash, pbkdf2Sync } from 'node:crypto';
import { installApiFixture, capturePageErrors, type ApiRequest, type ApiResponse } from './api-fixture';
const pass = 'amber orbit violet meadow', repo = 'repo-secrets';
function envelope(plain: string, revision: string) {
  const salt = Buffer.alloc(16, 1), nonce = Buffer.alloc(12, 2);
  const cipher = createCipheriv('aes-256-gcm', pbkdf2Sync(pass, salt, 600000, 32, 'sha256'), nonce);
  cipher.setAAD(Buffer.from(`cxtsecrets:v1:${repo}`));
  const ct = Buffer.concat([cipher.update(plain), cipher.final(), cipher.getAuthTag()]);
  return { revision, version: 1, kdf: 'PBKDF2-SHA256', iterations: 600000, salt_b64: salt.toString('base64'), cipher: 'AES-256-GCM', nonce_b64: nonce.toString('base64'), ciphertext_b64: ct.toString('base64'), fingerprint: createHash('sha256').update(pbkdf2Sync(pass, `cxtsecrets-fp:v1:${repo}`, 600000, 32, 'sha256')).digest('hex').slice(0, 12) };
}
const head = `sha256:${'a'.repeat(64)}`;
const snapshot = { id: head, doc_hash: head, repo_id: repo, branch: 'main', parents: [], provider: 'codex', created_at: '2026-09-18T00:00:00Z', message: 'Fixture context' };
const refs = [{ kind: 'branch', name: 'main', repo_id: repo, target: head }];
const repository = { id: 'repository-secrets', name: 'cxthub', slug: 'cxthub', owner_id: 'owner', effective_role: 'owner', owner_username: 'alice', visibility: 'private' };
function base({ method, pathname }: ApiRequest): ApiResponse | undefined {
  if (method !== 'GET') return undefined;
  if (pathname === '/api/v1/me') return { body: { id: 'owner', username: 'alice', locale: 'en', email: 'alice@example.test' } };
  if (pathname === '/api/v1/repositories') return { body: [repository] };
  if (pathname.endsWith('/members')) return { body: [{ repository_id: repository.id, user_id: 'owner', role: 'owner' }] };
  if (pathname.endsWith('/invites')) return { body: [] };
  if (pathname === '/api/v1/repos') return { body: [{ id: repo, remote_url: 'https://cxthub.com/alice/cxthub', default_branch: 'main' }] };
  if (pathname.endsWith('/view')) return { body: { refs, snapshots: [snapshot], history: [], pending: [], unsync: [], reflog: [] } };
  if (pathname.endsWith('/refs')) return { body: refs };
  if (pathname.endsWith('/snapshots')) return { body: [snapshot] };
  if (pathname.includes('/docs/')) return { body: { hash: head, envelope: { cir_version: '2', source_provider: 'codex' }, events: [], total: 0, offset: 0, next: -1, inherited: 0 } };
  if (pathname.includes('/memories/')) return { body: null };
  if (/\/(refs|snapshots|pending|unsync|history|reflog)$/.test(pathname)) return { body: [] };
  if (pathname.includes('/settings/')) return { body: null };
  if (pathname.endsWith('/prs/promotions')) return { body: [] };
  return undefined;
}
async function openSecrets(page: Page) {
  await page.goto('/alice/cxthub');
  await page.getByRole('button', { name: '.cxtsecrets settings', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: '.cxtsecrets settings' });
  await dialog.locator('input[type="password"]').first().fill(pass);
  await dialog.getByRole('button', { name: /load/i }).click();
  await expect(dialog.getByRole('textbox', { name: 'Secrets to encrypt' })).toHaveValue('original\n');
  return dialog;
}
test('same-key conflict preserves the draft and requires comparing a fresh baseline', async ({ page }) => {
  const errors = capturePageErrors(page);
  let current = envelope('original\n', 'revision-1');
  const revisions: string[] = [];
  const unexpected = await installApiFixture(page, r => {
    if (r.pathname.endsWith('/secrets')) {
      if (r.method === 'GET') return { body: current };
      const revision = r.searchParams.get('expected_revision') ?? ''; revisions.push(revision);
      return revision !== current.revision ? { status: 409, body: { error: { code: 'secrets_conflict', message: 'stale draft' } } } : { body: { status: 'stored', revision: 'revision-3' } };
    }
    return base(r);
  });
  const dialog = await openSecrets(page), editor = dialog.getByRole('textbox', { name: 'Secrets to encrypt' });
  await editor.fill('my draft'); current = envelope('teammate\n', 'revision-2');
  await dialog.getByRole('button', { name: /encrypt.*save/i }).click();
  await expect(dialog).toContainText('Someone saved a newer version');
  await expect(editor).toHaveValue('my draft'); expect(revisions).toEqual(['revision-1']);
  await expect(dialog.getByRole('button', { name: /encrypt.*save/i })).toBeDisabled();
  await dialog.getByRole('button', { name: 'Load latest and keep draft' }).click();
  await expect(editor).toHaveValue('teammate\n');
  await expect(dialog.getByRole('textbox', { name: /Your previous draft/ })).toHaveValue('my draft');
  await editor.fill('teammate\nmy draft\n'); await dialog.getByRole('button', { name: /encrypt.*save/i }).click();
  await expect(editor).toHaveValue(''); expect(revisions).toEqual(['revision-1', 'revision-2']);
  expect(errors).toEqual([]); expect(unexpected).toEqual([]);
});
test('rotation submits the decrypted revision instead of the passphrase generation alone', async ({ page }) => {
  let current = envelope('original\n', 'revision-1'), race = false, attempted = '';
  await installApiFixture(page, r => {
    if (r.pathname.endsWith('/secrets')) {
      if (r.method === 'GET') { const read = current; if (race) { current = { ...current, revision: 'revision-2' }; race = false; } return { body: read }; }
      attempted = r.searchParams.get('expected_revision') ?? ''; expect(r.searchParams.get('rotate')).toBe('true');
      return { status: 409, body: { error: { code: 'secrets_conflict', message: 'stale rotation' } } };
    }
    return base(r);
  });
  const dialog = await openSecrets(page);
  await dialog.locator('input[type="checkbox"]').check();
  await dialog.locator('input[type="password"]').nth(1).fill('quiet forest copper comet'); race = true;
  await dialog.getByRole('button', { name: /rotate/i }).click();
  await expect(dialog).toContainText('Someone saved a newer version'); expect(attempted).toBe('revision-1');
});
test('notification history shows safe failure status and retries against current settings', async ({ page }) => {
  let state = 'attention', attempts = 8;
  const unexpected = await installApiFixture(page, r => {
    if (r.pathname.endsWith('/notifications/event-1/retry')) { expect(r.method).toBe('POST'); state = 'pending'; attempts = 0; return { body: { status: 'queued' } }; }
    if (r.pathname.endsWith('/notifications')) return { body: [{ id: 'event-1', repository_id: repository.id, kind: 'secrets_updated', text: 'cxthub: secrets updated', state, reason: state === 'attention' ? 'attempts_exhausted' : '', http_status: 503, attempts, created_at: '2026-09-18T00:00:00Z', next_attempt: '2026-09-18T00:00:00Z' }] };
    if (r.pathname.endsWith('/secrets')) return { body: null };
    return base(r);
  });
  await page.goto('/alice/cxthub?tab=settings');
  const history = page.getByRole('region', { name: 'Notification delivery' });
  await expect(history).toContainText('Automatic retry limit reached'); await expect(history).toContainText('HTTP 503');
  await history.getByRole('button', { name: 'Retry with current webhook' }).click();
  await expect(history).toContainText('Queued'); await expect(history.getByRole('button')).toHaveCount(0); expect(unexpected).toEqual([]);
});
