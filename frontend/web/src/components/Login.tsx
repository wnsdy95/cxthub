import { useState, type FormEvent } from 'react';
import { firebaseEnabled } from '../auth';
import { useLogin, useSignUp } from '../hooks';
import { useT, Rich } from '../i18n';

export function Login() {
  const t = useT();
  const login = useLogin();
  const signUp = useSignUp();
  const [authMode, setAuthMode] = useState<'login' | 'signup'>('login');
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [name, setName] = useState('');
  const [signedUp, setSignedUp] = useState(false); // show the email-verification notice after sign-up

  const busy = login.isPending || signUp.isPending;
  const err = login.error || signUp.error;

  function switchMode(mode: 'login' | 'signup') {
    setAuthMode(mode);
    setSignedUp(false);
    login.reset();
    signUp.reset();
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    if (!firebaseEnabled) {
      login.mutate({ mode: 'dev', email, name });
      return;
    }
    if (authMode === 'signup') {
      signUp.mutate(
        { email, password },
        {
          // No session exists — Guide user to log in after clicking the verification link and switch to the login tab.
          onSuccess: () => {
            setSignedUp(true);
            setAuthMode('login');
            setPassword('');
          },
        },
      );
    } else {
      login.mutate({ mode: 'email', email, password });
    }
  }

  return (
    <div className="auth-wrap">
      <div className="auth-card">
        <div className="auth-head">
          <div className="brand">
            cxthub<span>/</span>
          </div>
          <span className="mode-chip">{firebaseEnabled ? 'FIREBASE' : 'DEV'}</span>
        </div>
        <p className="sub">{t('auth.tagline')}</p>

        {firebaseEnabled && (
          <div className="auth-tabs" role="tablist" aria-label={t('auth.tabLogin')}>
            <button
              type="button"
              role="tab"
              aria-selected={authMode === 'login'}
              className={authMode === 'login' ? 'on' : ''}
              onClick={() => switchMode('login')}
            >
              {t('auth.tabLogin')}
            </button>
            <button
              type="button"
              role="tab"
              aria-selected={authMode === 'signup'}
              className={authMode === 'signup' ? 'on' : ''}
              onClick={() => switchMode('signup')}
            >
              {t('common.signUp')}
            </button>
          </div>
        )}

        {signedUp && (
          <p className="hint ok-msg" role="status">
            {t('auth.verifySent')}
          </p>
        )}

        <form onSubmit={submit} className="form">
          <input
            type="email"
            placeholder="you@team.com"
            aria-label={t('auth.emailAria')}
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            required
            autoFocus
          />
          {firebaseEnabled ? (
            <input
              type="password"
              placeholder={t('auth.password')}
              aria-label={t('auth.password')}
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
              autoComplete={authMode === 'signup' ? 'new-password' : 'current-password'}
            />
          ) : (
            <input
              type="text"
              placeholder={t('auth.namePlaceholder')}
              aria-label={t('auth.nameDevAria')}
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          )}
          <button type="submit" disabled={busy}>
            {busy
              ? t('auth.signingIn')
              : !firebaseEnabled
                ? t('auth.submitDev')
                : authMode === 'signup'
                  ? t('common.signUp')
                  : t('auth.tabLogin')}
          </button>
        </form>

        {firebaseEnabled && (
          <button className="google" type="button" disabled={busy} onClick={() => login.mutate({ mode: 'google' })}>
            {t('auth.google')}
          </button>
        )}
        {!firebaseEnabled && (
          <p className="hint">
            <Rich>{t('auth.firebaseHint')}</Rich>
          </p>
        )}
        {err && (
          <p className="err" role="alert">
            {err.message}
          </p>
        )}
      </div>
    </div>
  );
}
