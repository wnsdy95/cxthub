// Home (Landing) — product narrative, installation, and captured product views.
import { useState } from 'react';
import { useMe, useLogout } from '../hooks';
import { navigate } from '../route';
import { Logo } from './Logo';
import { Avatar } from './Avatar';
import { LocaleSwitcher } from './LocaleSwitcher';
import { AccountSettings } from './Settings';
import { useT, Rich } from '../i18n';
import codexLogo from '../assets/codex.webp';
import claudeLogo from '../assets/claude.webp';

const INSTALL =
  'curl -fsSL https://raw.githubusercontent.com/wnsdy95/cxthub/main/distrib/install | sh';

const SHOTS = {
  setup: '/landing/setup.jpg',
  context: '/landing/context.jpg',
  onhold: '/landing/onhold.jpg',
  profile: '/landing/profile.jpg',
  security: '/landing/security.jpg',
  permissions: '/landing/permissions.jpg',
} as const;

function ProductShot({ src, label }: { src: string; label: string }) {
  return (
    <figure className="product-shot">
      <a href={src} target="_blank" rel="noreferrer" className="product-shot-link">
        <img src={src} alt={label} width={1200} height={700} loading="lazy" decoding="async" />
        <span className="product-shot-open" aria-hidden="true">↗</span>
      </a>
      <figcaption>{label}</figcaption>
    </figure>
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
        <nav className="landing-nav" aria-label={t('landing.navLabel')}>
          <a href="#workflow">{t('landing.navWorkflow')}</a>
          <a href="#timeline">{t('landing.navProduct')}</a>
          <a href="#security">{t('landing.navSecurity')}</a>
        </nav>
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

      <section className="landing-hero">
        <span className="hero-eyebrow">{t('landing.eyebrow')}</span>
        <h1>
          {t('landing.heroTitle1')} <br />
          {t('landing.heroTitle2')}
        </h1>
        <p className="hero-sub">{t('landing.heroSub')}</p>

        <div className="hero-integrations" aria-label={t('landing.integrationsLabel')}>
          <span className="integration-pill">
            <img src={codexLogo} alt="" aria-hidden="true" /> Codex app
          </span>
          <span className="integration-pill">
            <img src={claudeLogo} alt="" aria-hidden="true" /> Claude Desktop
          </span>
          <span className="integration-pill cli"><code>&gt;_</code> CLI</span>
        </div>

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
        <a className="landing-source" href="https://github.com/wnsdy95/cxthub" target="_blank" rel="noreferrer">
          {t('landing.sourceLink')} <span aria-hidden="true">↗</span>
        </a>

        <div className="hero-proof">
          <span><b>01</b>{t('landing.proofCapture')}</span>
          <span><b>02</b>{t('landing.proofHistory')}</span>
          <span><b>03</b>{t('landing.proofSecurity')}</span>
        </div>
      </section>

      <section className="landing-story-head">
        <span className="feat-kicker">{t('landing.storyKicker')}</span>
        <h2>{t('landing.storyTitle')}</h2>
        <p>{t('landing.storySub')}</p>
      </section>

      <div className="landing-stories">
        <section className="landing-feat" id="workflow">
          <div className="feat-text">
            <div className="feat-label"><span className="feat-kicker">{t('landing.f1Kicker')}</span><span>01</span></div>
            <h2>{t('landing.f1Title')}</h2>
            <p>
              <Rich>{t('landing.f1Body')}</Rich>
            </p>
            <p className="feat-why">
              <Rich>{t('landing.f1Why')}</Rich>
            </p>
          </div>
          <ProductShot src={SHOTS.setup} label={t('landing.f1Shot')} />
        </section>

        <section className="landing-feat reverse" id="timeline">
          <div className="feat-text">
            <div className="feat-label"><span className="feat-kicker">{t('landing.f2Kicker')}</span><span>02</span></div>
            <h2>{t('landing.f2Title')}</h2>
            <p>
              <Rich>{t('landing.f2Body')}</Rich>
            </p>
            <p className="feat-why">
              <Rich>{t('landing.f2Why')}</Rich>
            </p>
          </div>
          <ProductShot src={SHOTS.context} label={t('landing.f2Shot')} />
        </section>

        <section className="landing-feat">
          <div className="feat-text">
            <div className="feat-label"><span className="feat-kicker">{t('landing.f3Kicker')}</span><span>03</span></div>
            <h2>{t('landing.f3Title')}</h2>
            <p>
              <Rich>{t('landing.f3Body')}</Rich>
            </p>
            <p className="feat-why">
              <Rich>{t('landing.f3Why')}</Rich>
            </p>
          </div>
          <ProductShot src={SHOTS.onhold} label={t('landing.f3Shot')} />
        </section>

        <section className="landing-feat reverse">
          <div className="feat-text">
            <div className="feat-label"><span className="feat-kicker">{t('landing.f4Kicker')}</span><span>04</span></div>
            <h2>{t('landing.f4Title')}</h2>
            <p>
              <Rich>{t('landing.f4Body')}</Rich>
            </p>
            <p className="feat-why">
              <Rich>{t('landing.f4Why')}</Rich>
            </p>
          </div>
          <ProductShot src={SHOTS.profile} label={t('landing.f4Shot')} />
        </section>

        <section className="landing-feat" id="security">
          <div className="feat-text">
            <div className="feat-label"><span className="feat-kicker">{t('landing.f5Kicker')}</span><span>05</span></div>
            <h2>{t('landing.f5Title')}</h2>
            <p>
              <Rich>{t('landing.f5Body')}</Rich>
            </p>
            <p className="feat-why">
              <Rich>{t('landing.f5Why')}</Rich>
            </p>
          </div>
          <ProductShot src={SHOTS.security} label={t('landing.f5Shot')} />
        </section>

        <section className="landing-feat reverse">
          <div className="feat-text">
            <div className="feat-label"><span className="feat-kicker">{t('landing.f6Kicker')}</span><span>06</span></div>
            <h2>{t('landing.f6Title')}</h2>
            <p>
              <Rich>{t('landing.f6Body')}</Rich>
            </p>
            <p className="feat-why">
              <Rich>{t('landing.f6Why')}</Rich>
            </p>
          </div>
          <ProductShot src={SHOTS.permissions} label={t('landing.f6Shot')} />
        </section>
      </div>

      <section className="landing-cta">
        <span className="hero-eyebrow">{t('landing.ctaKicker')}</span>
        <h2>{t('landing.ctaTitle')}</h2>
        <p>{t('landing.ctaBody')}</p>
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
        <a href="https://github.com/wnsdy95/cxthub" target="_blank" rel="noreferrer">GitHub ↗</a>
      </footer>
    </div>
  );
}
