// Home (Landing) — Login entry point (/). Top CLI installation, top-right Sign in/Sign up (account on login),
// below, "What is it? Why?" descriptions + screenshot placeholders. For logged-in users, / goes to App Dashboard,
// so this page is mainly for non-logged-in users but handles login states in the header.
import { useState } from 'react';
import { useMe, useLogout } from '../hooks';
import { navigate } from '../route';
import { Logo } from './Logo';
import { Avatar } from './Avatar';
import { LocaleSwitcher } from './LocaleSwitcher';
import { AccountSettings } from './Settings';
import { useT, Rich } from '../i18n';

const INSTALL =
  'curl -fsSL https://raw.githubusercontent.com/wnsdy95/cxthub/main/distrib/install | sh';

// Screenshot placeholder — Location for user to insert an image.
function Shot({ label }: { label: string }) {
  const t = useT();
  return (
    <div className="shot-placeholder" role="img" aria-label={label}>
      <span>{t('landing.shotPlaceholder', { label })}</span>
    </div>
  );
}

export function Landing({ onSignIn }: { onSignIn?: () => void }) {
  const t = useT();
  const me = useMe().data;
  const logout = useLogout();
  const [copied, setCopied] = useState(false);

  function copyInstall() {
    navigator.clipboard?.writeText(INSTALL).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    });
  }

  return (
    <div className="landing">
      <header className="landing-header">
        <button className="linkish-logo" onClick={() => navigate('/')} aria-label={t('common.home')}>
          <div className="brand sm">
            <Logo />
          </div>
        </button>
        <div className="who">
          <LocaleSwitcher />
          {me ? (
            <>
              <Avatar user={me} link />
              <span>{me.nickname || me.name || me.email}</span>
              <AccountSettings user={me} />
              <button className="ghost" onClick={() => logout.mutate()} disabled={logout.isPending}>
                {t('common.logout')}
              </button>
            </>
          ) : (
            <>
              <button className="ghost" onClick={onSignIn}>
                {t('common.signIn')}
              </button>
              <button className="btn-primary" onClick={onSignIn}>
                {t('common.signUp')}
              </button>
            </>
          )}
        </div>
      </header>

      {/* 1) Top — CLI installation */}
      <section className="landing-hero">
        <h1>
          {t('landing.heroTitle1')} <br />
          {t('landing.heroTitle2')}
        </h1>
        <p className="hero-sub">{t('landing.heroSub')}</p>

        <div className="install-box">
          <span className="install-label">{t('landing.installLabel')}</span>
          <code>{INSTALL}</code>
          <button className="btn-primary" onClick={copyInstall}>
            {copied ? t('common.copied') : t('common.copy')}
          </button>
        </div>
        <p className="install-next">
          <Rich>{t('landing.installNext1')}</Rich>
          <br />
          <Rich>{t('landing.installNext2')}</Rich>
        </p>
      </section>

      {/* 2) Feature descriptions */}
      <section className="landing-feat">
        <div className="feat-text">
          <span className="feat-kicker">{t('landing.f1Kicker')}</span>
          <h2>{t('landing.f1Title')}</h2>
          <p>
            <Rich>{t('landing.f1Body')}</Rich>
          </p>
          <p className="feat-why">
            <Rich>{t('landing.f1Why')}</Rich>
          </p>
        </div>
        <Shot label={t('landing.f1Shot')} />
      </section>

      <section className="landing-feat reverse">
        <div className="feat-text">
          <span className="feat-kicker">{t('landing.f2Kicker')}</span>
          <h2>{t('landing.f2Title')}</h2>
          <p>
            <Rich>{t('landing.f2Body')}</Rich>
          </p>
          <p className="feat-why">
            <Rich>{t('landing.f2Why')}</Rich>
          </p>
        </div>
        <Shot label={t('landing.f2Shot')} />
      </section>

      <section className="landing-feat">
        <div className="feat-text">
          <span className="feat-kicker">{t('landing.f3Kicker')}</span>
          <h2>{t('landing.f3Title')}</h2>
          <p>
            <Rich>{t('landing.f3Body')}</Rich>
          </p>
          <p className="feat-why">
            <Rich>{t('landing.f3Why')}</Rich>
          </p>
        </div>
        <Shot label={t('landing.f3Shot')} />
      </section>

      <section className="landing-feat reverse">
        <div className="feat-text">
          <span className="feat-kicker">{t('landing.f4Kicker')}</span>
          <h2>{t('landing.f4Title')}</h2>
          <p>
            <Rich>{t('landing.f4Body')}</Rich>
          </p>
          <p className="feat-why">
            <Rich>{t('landing.f4Why')}</Rich>
          </p>
        </div>
        <Shot label={t('landing.f4Shot')} />
      </section>

      <section className="landing-feat">
        <div className="feat-text">
          <span className="feat-kicker">{t('landing.f5Kicker')}</span>
          <h2>{t('landing.f5Title')}</h2>
          <p>
            <Rich>{t('landing.f5Body')}</Rich>
          </p>
          <p className="feat-why">
            <Rich>{t('landing.f5Why')}</Rich>
          </p>
        </div>
        <Shot label={t('landing.f5Shot')} />
      </section>

      <section className="landing-feat reverse">
        <div className="feat-text">
          <span className="feat-kicker">{t('landing.f6Kicker')}</span>
          <h2>{t('landing.f6Title')}</h2>
          <p>
            <Rich>{t('landing.f6Body')}</Rich>
          </p>
          <p className="feat-why">
            <Rich>{t('landing.f6Why')}</Rich>
          </p>
        </div>
        <Shot label={t('landing.f6Shot')} />
      </section>

      <section className="landing-cta">
        <h2>{t('landing.ctaTitle')}</h2>
        <div className="install-box">
          <code>{INSTALL}</code>
          <button className="btn-primary" onClick={copyInstall}>
            {copied ? t('common.copied') : t('common.copy')}
          </button>
        </div>
        {!me && (
          <button className="btn-primary lg" onClick={onSignIn}>
            {t('landing.ctaSignUp')}
          </button>
        )}
      </section>

      <footer className="landing-footer">
        <span className="brand sm">
          <Logo />
        </span>
        <span>cxthub — coding agent context, on git.</span>
      </footer>
    </div>
  );
}
