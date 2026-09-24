// Authentication (IDP) — Firebase or dev. Each function returns an IDP token (string),
// and the upper function (useLogin) exchanges that token with POST /auth/session to receive a server session token.
//
// Use Firebase setting (VITE_FIREBASE_API_KEY) for Email/Password + Google, or dev token otherwise.
import { initializeApp } from 'firebase/app';
import {
  getAuth,
  signInWithEmailAndPassword,
  createUserWithEmailAndPassword,
  sendEmailVerification,
  GoogleAuthProvider,
  GithubAuthProvider,
  linkWithCredential,
  linkWithPopup,
  reload,
  verifyBeforeUpdateEmail,
  signInWithPopup,
  signOut as fbSignOut,
  type Auth,
  type User as FirebaseUser,
  type AuthCredential,
} from 'firebase/auth';
import type { FirebaseError } from 'firebase/app';
import type { MsgKey, Vars } from './i18n';
import { AuthFlow, AuthStepError } from './auth-flow';

export { AuthStepError } from './auth-flow';

const cfg = {
  apiKey: import.meta.env.VITE_FIREBASE_API_KEY as string | undefined,
  authDomain: import.meta.env.VITE_FIREBASE_AUTH_DOMAIN as string | undefined,
  projectId: import.meta.env.VITE_FIREBASE_PROJECT_ID as string | undefined,
};

export const firebaseEnabled = Boolean(cfg.apiKey);
export const githubLoginEnabled = firebaseEnabled && import.meta.env.VITE_GITHUB_LOGIN === 'true';

let auth: Auth | null = null;
if (firebaseEnabled) {
  auth = getAuth(initializeApp({ apiKey: cfg.apiKey, authDomain: cfg.authDomain, projectId: cfg.projectId }));
}

const flow = new AuthFlow<FirebaseUser, AuthCredential>({
  link: async (user, credential) => (await linkWithCredential(user, credential)).user,
  refresh: async (user) => { await reload(user); return user; },
  verify: sendEmailVerification,
  verifyEmail: verifyBeforeUpdateEmail,
  token: (user) => user.getIdToken(true),
});

function translateAuthError(error: unknown, t: (k: MsgKey, v?: Vars) => string): Error {
  if (error instanceof AuthStepError) {
    const keys = { 'link-required': 'auth.linkRequired', 'link-expired': 'auth.linkExpired',
      'account-mismatch': 'auth.accountMismatch', 'missing-email': 'auth.missingEmail',
      'verify-email': error.sent ? 'auth.verifySent' : 'auth.verifyRequired' } as const;
    error.message = t(keys[error.step], { email: error.email });
    return error;
  }
  return new Error(fbErrMessage(error, t));
}

// dev IDP token ("dev:<email>:<name>"). Compatible with local cxtd(auth=dev).
export function devIdpToken(email: string, name: string): string {
  return `dev:${email}:${name || email}`;
}

// Map Firebase error codes (auth/…) to user messages. Otherwise, return the original message.
function fbErrMessage(e: unknown, t: (k: MsgKey, v?: Vars) => string): string {
  const code = e && typeof e === 'object' && 'code' in e ? String((e as { code: unknown }).code) : '';
  switch (code) {
    case 'auth/email-already-in-use':
      return t('auth.errEmailInUse');
    case 'auth/invalid-email':
      return t('auth.errInvalidEmail');
    case 'auth/weak-password':
      return t('auth.errWeakPassword');
    case 'auth/invalid-credential':
    case 'auth/wrong-password':
    case 'auth/user-not-found':
      return t('auth.errBadCredentials');
    case 'auth/too-many-requests':
      return t('auth.errTooMany');
    case 'auth/popup-closed-by-user':
    case 'auth/cancelled-popup-request':
      return t('auth.signInCancelled');
    case 'auth/popup-blocked':
      return t('auth.popupBlocked');
    case 'auth/credential-already-in-use':
    case 'auth/account-exists-with-different-credential':
      return t('auth.linkRequired');
    case 'auth/operation-not-allowed':
      return t('auth.providerUnavailable');
    default:
      return e instanceof Error ? e.message : String(e);
  }
}

// Registration — Account creation → Authentication email sent → Logout (session not exchanged). Log in required after clicking the authentication link to issue a session.
export async function firebaseEmailSignUp(email: string, password: string, t: (k: MsgKey, v?: Vars) => string): Promise<void> {
  if (!auth) throw new Error(t('auth.firebaseNotConfigured'));
  try {
    const cred = await createUserWithEmailAndPassword(auth, email, password);
    await sendEmailVerification(cred.user);
    await fbSignOut(auth); // Do not keep login state before authentication
  } catch (e) {
    throw new Error(fbErrMessage(e, t));
  }
}

// Login — Email/Password → ID token. Resend authentication email and block if email is unverified (session not issued).
export async function firebaseEmailIdToken(email: string, password: string, t: (k: MsgKey, v?: Vars) => string): Promise<string> {
  if (!auth) throw new Error(t('auth.firebaseNotConfigured'));
  try {
    const cred = await signInWithEmailAndPassword(auth, email, password);
    return await flow.finish(cred.user);
  } catch (e) {
    throw translateAuthError(e, t);
  }
}

// Firebase Google popup → ID token.
export async function firebaseGoogleIdToken(t: (k: MsgKey, v?: Vars) => string): Promise<string> {
  return firebaseSocialIdToken('google', t);
}

export async function firebaseSocialIdToken(provider: 'google' | 'github', t: (k: MsgKey, v?: Vars) => string): Promise<string> {
  if (!auth) throw new Error(t('auth.firebaseNotConfigured'));
  if (provider === 'github' && !githubLoginEnabled) throw new Error(t('auth.providerUnavailable'));
  const oauth = provider === 'github' ? new GithubAuthProvider() : new GoogleAuthProvider();
  if (provider === 'github') oauth.addScope('user:email');
  try {
    let cred;
    try { cred = await signInWithPopup(auth, oauth); }
    catch (e) {
      const error = e as FirebaseError;
      if (error.code === 'auth/account-exists-with-different-credential') {
        const credential = provider === 'github' ? GithubAuthProvider.credentialFromError(error) : GoogleAuthProvider.credentialFromError(error);
        if (credential) flow.requireLink(credential, String(error.customData?.email ?? ''));
      }
      throw e;
    }
    return await flow.finish(cred.user);
  } catch (e) { throw translateAuthError(e, t); }
}

export async function firebaseVerification(action: 'check' | 'resend' | 'email', email: string, t: (k: MsgKey, v?: Vars) => string): Promise<string> {
  try { return await (action === 'check' ? flow.check() : action === 'resend' ? flow.resend() : flow.setEmail(email)); }
  catch (e) { throw translateAuthError(e, t); }
}

export async function firebaseSignOut(): Promise<void> {
  flow.clear();
  if (auth) await fbSignOut(auth);
}

// A server cookie alone is not proof for linking an external identity. Require
// Firebase's matching current user; SDK linking preserves that user's UID.
export async function firebaseLinkGitHub(uid: string, t: (k: MsgKey, v?: Vars) => string): Promise<void> {
  if (!auth || !githubLoginEnabled) throw new Error(t('auth.providerUnavailable'));
  try {
    await auth.authStateReady();
    const user = auth.currentUser;
    if (!user || user.uid !== uid) throw new AuthStepError('link-expired');
    if (user.providerData.some(p => p.providerId === 'github.com')) return;
    const provider = new GithubAuthProvider();
    provider.addScope('user:email');
    const linked = await linkWithPopup(user, provider);
    if (linked.user.uid !== uid) throw new AuthStepError('account-mismatch');
  } catch (e) { throw translateAuthError(e, t); }
}
