// Provider-independent sign-in transitions. Credentials are intentionally held
// only in this page's memory, and linking requires a fresh matching sign-in.
export type AuthStep = 'link-required' | 'link-expired' | 'account-mismatch' | 'missing-email' | 'verify-email';
export class AuthStepError extends Error {
  constructor(readonly step: AuthStep, readonly email = '', readonly sent = false) { super(step); }
}
export interface AuthUser { uid: string; email: string | null; emailVerified: boolean }
export interface AuthFlowPort<U extends AuthUser, C> {
  link(user: U, credential: C): Promise<U>;
  refresh(user: U): Promise<U>;
  verify(user: U): Promise<void>;
  verifyEmail(user: U, email: string): Promise<void>;
  token(user: U): Promise<string>;
}
export class AuthFlow<U extends AuthUser, C> {
  private pending?: { credential: C; email: string; until: number };
  private verification?: U;
  constructor(private port: AuthFlowPort<U, C>, private now = Date.now) {}
  clear() { this.pending = undefined; this.verification = undefined; }
  requireLink(credential: C, email: string): never {
    this.verification = undefined;
    if (!email.trim()) { this.pending = undefined; throw new AuthStepError('account-mismatch'); }
    this.pending = { credential, email, until: this.now() + 10 * 60_000 };
    throw new AuthStepError('link-required', email);
  }
  async finish(user: U, send = true): Promise<string> {
    const pending = this.pending;
    if (pending) {
      if (this.now() >= pending.until) { this.pending = undefined; throw new AuthStepError('link-expired'); }
      if (user.email?.toLowerCase() !== pending.email.toLowerCase()) throw new AuthStepError('account-mismatch', pending.email);
      this.pending = undefined; // A consumed credential is never replayed.
      const linked = await this.port.link(user, pending.credential);
      if (linked.uid !== user.uid) throw new AuthStepError('account-mismatch');
      user = linked;
    }
    this.verification = user;
    if (!user.email) throw new AuthStepError('missing-email');
    if (!user.emailVerified) {
      let sent = false;
      if (send) { try { await this.port.verify(user); sent = true; } catch { /* Retry remains explicit. */ } }
      throw new AuthStepError('verify-email', user.email, sent);
    }
    const token = await this.port.token(user);
    this.verification = undefined;
    return token;
  }
  async check(): Promise<string> {
    if (!this.verification) throw new AuthStepError('link-expired');
    return this.finish(await this.port.refresh(this.verification), false);
  }
  async resend(): Promise<string> {
    if (!this.verification) throw new AuthStepError('link-expired');
    return this.finish(await this.port.refresh(this.verification));
  }
  async setEmail(email: string): Promise<string> {
    if (!this.verification) throw new AuthStepError('link-expired');
    await this.port.verifyEmail(this.verification, email.trim());
    throw new AuthStepError('verify-email', email.trim(), true);
  }
}
