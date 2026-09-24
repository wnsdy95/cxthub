import { test } from 'node:test';
import assert from 'node:assert/strict';
import { AuthFlow, AuthStepError, type AuthUser } from '../src/auth-flow';

function setup() {
  let now = 0;
  let linked = 0, tokens = 0, sent = 0;
  let refreshed: AuthUser = { uid: 'existing-uid', email: 'a@example.test', emailVerified: true };
  const flow = new AuthFlow<AuthUser, string>({
    link: async (user, credential) => { assert.equal(credential, 'pending'); linked++; return user; },
    refresh: async () => refreshed,
    verify: async () => { sent++; },
    verifyEmail: async () => { sent++; },
    token: async (user) => { tokens++; return user.uid; },
  }, () => now);
  return { flow, user: refreshed, counts: () => ({ linked, tokens, sent }),
    advance: () => { now += 600_001; }, refresh: (u: AuthUser) => { refreshed = u; } };
}
const step = (name: string) => (e: unknown) => e instanceof AuthStepError && e.step === name;

test('link requires matching authenticated identity, preserves UID and consumes credential once', async () => {
  const s = setup();
  assert.throws(() => s.flow.requireLink('pending', s.user.email!), step('link-required'));
  await assert.rejects(s.flow.finish({ ...s.user, uid: 'other', email: 'b@example.test' }), step('account-mismatch'));
  assert.equal(s.counts().linked, 0);
  assert.equal(await s.flow.finish(s.user), 'existing-uid');
  assert.equal(await s.flow.finish(s.user), 'existing-uid');
  assert.equal(s.counts().linked, 1);
});
test('expired linking cannot exchange or link credentials', async () => {
  const s = setup();
  assert.throws(() => s.flow.requireLink('pending', s.user.email!), step('link-required'));
  s.advance();
  await assert.rejects(s.flow.finish(s.user), step('link-expired'));
  assert.deepEqual(s.counts(), { linked: 0, tokens: 0, sent: 0 });
});
test('unverified email never exchanges a token; return reloads verification', async () => {
  const s = setup();
  await assert.rejects(s.flow.finish({ ...s.user, emailVerified: false }), step('verify-email'));
  assert.equal(s.counts().tokens, 0);
  assert.equal(s.counts().sent, 1);
  assert.equal(await s.flow.check(), s.user.uid);
});
test('missing email requires verification and logout discards pending state', async () => {
  const s = setup();
  await assert.rejects(s.flow.finish({ ...s.user, email: null, emailVerified: false }), step('missing-email'));
  await assert.rejects(s.flow.setEmail('a@example.test'), step('verify-email'));
  assert.equal(s.counts().tokens, 0);
  s.flow.clear();
  await assert.rejects(s.flow.check(), step('link-expired'));
});
