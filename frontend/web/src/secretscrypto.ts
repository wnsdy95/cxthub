// .cxtsecrets inter-machine encryption — CLI(Go secretscrypto) and byte-compatible.
// PBKDF2-SHA256(600k) + AES-256-GCM, AAD = "cxtsecrets:v1:<repoID>".
// Password/plaintext/key does not leave the browser — server only handles this envelope(ciphertext).
import type { MsgKey, Vars } from './i18n';

export interface SecretsEnvelope {
  version: number;
  kdf: string;
  iterations: number;
  salt_b64: string;
  cipher: string;
  nonce_b64: string;
  ciphertext_b64: string;
/** Password fingerprint (first 12 hex) — for comparison/blocking, plaintext not exposed. */
  fingerprint?: string;
  updated_at?: string;
  updated_by?: string;
}

export const ITERATIONS = 600_000;

const enc = new TextEncoder();
const dec = new TextDecoder();
const b64 = (buf: ArrayBuffer | Uint8Array) => btoa(String.fromCharCode(...new Uint8Array(buf)));
const unb64 = (s: string) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));

async function deriveKey(passphrase: string, salt: Uint8Array, iterations: number): Promise<CryptoKey> {
  const base = await crypto.subtle.importKey('raw', enc.encode(passphrase), 'PBKDF2', false, ['deriveKey']);
  return crypto.subtle.deriveKey(
    { name: 'PBKDF2', hash: 'SHA-256', salt: salt as BufferSource, iterations },
    base,
    { name: 'AES-GCM', length: 256 },
    false,
    ['encrypt', 'decrypt'],
  );
}

const aad = (repoId: string) => enc.encode(`cxtsecrets:v1:${repoId}`);
const hexstr = (buf: ArrayBuffer) => [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, '0')).join('');

function validEnvelope(env: SecretsEnvelope): boolean {
  if (
    env.version !== 1 ||
    env.kdf !== 'PBKDF2-SHA256' ||
    env.cipher !== 'AES-256-GCM' ||
    env.iterations !== ITERATIONS
  ) {
    return false;
  }
  try {
    if (unb64(env.salt_b64).byteLength !== 16) return false;
    if (unb64(env.nonce_b64).byteLength !== 12) return false;
    if (unb64(env.ciphertext_b64).byteLength < 16) return false;
  } catch {
    return false;
  }
  return !env.fingerprint || /^[0-9a-f]{12}$/.test(env.fingerprint);
}

// fingerprint — password fingerprint(SHA-256(PBKDF2(pass, fixed salt, 600k)) first 12 hex).
// Fixed salt(repo binding) is deterministic, KDF cost is the same as encryption, not increasing attack surface,
// and does not expose the password plaintext. Go(secretscrypto.Fingerprint) and byte-compatible.
export async function fingerprint(passphrase: string, repoId: string): Promise<string> {
  const base = await crypto.subtle.importKey('raw', enc.encode(passphrase), 'PBKDF2', false, ['deriveBits']);
  const bits = await crypto.subtle.deriveBits(
    { name: 'PBKDF2', hash: 'SHA-256', salt: enc.encode(`cxtsecrets-fp:v1:${repoId}`) as BufferSource, iterations: ITERATIONS },
    base,
    256,
  );
  return hexstr(await crypto.subtle.digest('SHA-256', bits)).slice(0, 12);
}

export async function encryptSecrets(passphrase: string, plaintext: string, repoId: string): Promise<SecretsEnvelope> {
  const salt = crypto.getRandomValues(new Uint8Array(16));
  const nonce = crypto.getRandomValues(new Uint8Array(12));
  const key = await deriveKey(passphrase, salt, ITERATIONS);
  const ct = await crypto.subtle.encrypt(
    { name: 'AES-GCM', iv: nonce as BufferSource, additionalData: aad(repoId) },
    key,
    enc.encode(plaintext),
  );
  return {
    version: 1,
    kdf: 'PBKDF2-SHA256',
    iterations: ITERATIONS,
    salt_b64: b64(salt),
    cipher: 'AES-256-GCM',
    nonce_b64: b64(nonce),
    ciphertext_b64: b64(ct),
    fingerprint: await fingerprint(passphrase, repoId),
  };
}

export async function decryptSecrets(
  passphrase: string,
  env: SecretsEnvelope,
  repoId: string,
  t: (k: MsgKey, v?: Vars) => string,
): Promise<string> {
  if (!validEnvelope(env)) {
    throw new Error(t('secrets.envUnsupported'));
  }
  const key = await deriveKey(passphrase, unb64(env.salt_b64), env.iterations);
  try {
    const pt = await crypto.subtle.decrypt(
      { name: 'AES-GCM', iv: unb64(env.nonce_b64) as BufferSource, additionalData: aad(repoId) },
      key,
      unb64(env.ciphertext_b64) as BufferSource,
    );
    return dec.decode(pt);
  } catch {
    throw new Error(t('secrets.decryptFail'));
  }
}
