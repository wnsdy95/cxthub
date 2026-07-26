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
  signInWithPopup,
  signOut as fbSignOut,
  type Auth,
} from 'firebase/auth';
import type { MsgKey, Vars } from './i18n';

const cfg = {
  apiKey: import.meta.env.VITE_FIREBASE_API_KEY as string | undefined,
  authDomain: import.meta.env.VITE_FIREBASE_AUTH_DOMAIN as string | undefined,
  projectId: import.meta.env.VITE_FIREBASE_PROJECT_ID as string | undefined,
};

export const firebaseEnabled = Boolean(cfg.apiKey);

let auth: Auth | null = null;
if (firebaseEnabled) {
  auth = getAuth(initializeApp({ apiKey: cfg.apiKey, authDomain: cfg.authDomain, projectId: cfg.projectId }));
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
  let cred;
  try {
    cred = await signInWithEmailAndPassword(auth, email, password);
  } catch (e) {
    throw new Error(fbErrMessage(e, t));
  }
  if (!cred.user.emailVerified) {
    try {
      await sendEmailVerification(cred.user);
    } catch {
/* Ignore too-many-requests resend limit — message is the same */
    }
    await fbSignOut(auth);
    throw new Error(t('auth.emailNotVerified'));
  }
  return cred.user.getIdToken();
}

// Firebase Google popup → ID token.
export async function firebaseGoogleIdToken(t: (k: MsgKey, v?: Vars) => string): Promise<string> {
  if (!auth) throw new Error(t('auth.firebaseNotConfigured'));
  const cred = await signInWithPopup(auth, new GoogleAuthProvider());
  return cred.user.getIdToken();
}

export async function firebaseSignOut(): Promise<void> {
  if (auth) await fbSignOut(auth);
}
