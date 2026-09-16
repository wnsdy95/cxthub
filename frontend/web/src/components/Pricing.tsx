import { useEffect } from 'react';
import { useMe } from '../hooks';
import { useT } from '../i18n';
import { navigate } from '../route';
import { MarketingFooter, MarketingHeader, MarketingLink } from './MarketingChrome';

export function Pricing({ onSignIn }: { onSignIn?: () => void }) {
  const t = useT();
  const me = useMe().data;
  useEffect(() => { window.scrollTo({ top: 0, left: 0 }); }, []);

  return (
    <div className="landing pricing-page">
      <MarketingHeader onSignIn={onSignIn}>
        <MarketingLink to="/">{t('landing.navProduct')}</MarketingLink>
        <MarketingLink to="/pricing" current>{t('landing.navPricing')}</MarketingLink>
      </MarketingHeader>
      <main className="pricing-main">
        <section className="pricing-hero">
          <span className="hero-eyebrow">{t('pricing.eyebrow')}</span>
          <h1>{t('pricing.title')}</h1>
          <p>{t('pricing.subtitle')}</p>
          <div className="pricing-notice" role="note">
            <strong>{t('pricing.noticeTitle')}</strong>
            <span>{t('pricing.noticeBody')}</span>
          </div>
        </section>
        <section className="pricing-grid" aria-label={t('pricing.planAria')}>
          <article className="pricing-card">
            <div className="pricing-card-head">
              <div>
                <span className="feat-kicker">{t('pricing.planKicker')}</span>
                <h2>{t('pricing.planTitle')}</h2>
              </div>
            </div>
            <div className="pricing-free"><strong>$0</strong><span>{t('pricing.currentPeriod')}</span></div>
            <ul className="pricing-includes">
              <li>{t('pricing.includeMembers')}</li>
              <li>{t('pricing.includeAgents')}</li>
              <li>{t('pricing.includeStorage')}</li>
            </ul>
            <button className="btn-primary lg" onClick={me ? () => navigate('/') : onSignIn}>
              {me ? t('pricing.ctaAuthed') : t('pricing.ctaGuest')}
            </button>
          </article>
          <aside className="pricing-card" aria-labelledby="future-plans-title">
            <span className="feat-kicker">{t('pricing.futureKicker')}</span>
            <h2 id="future-plans-title">{t('pricing.futureTitle')}</h2>
            <p>{t('pricing.futureBody')}</p>
            <p>{t('pricing.futureNotice')}</p>
          </aside>
        </section>
        <section className="pricing-explainer">
          <div className="pricing-section-head">
            <span className="feat-kicker">{t('pricing.howKicker')}</span>
            <h2>{t('pricing.howTitle')}</h2>
            <p>{t('pricing.howSubtitle')}</p>
          </div>
          <ol className="pricing-steps">
            <li><b>01</b><strong>{t('pricing.step1Title')}</strong><span>{t('pricing.step1Body')}</span></li>
            <li><b>02</b><strong>{t('pricing.step2Title')}</strong><span>{t('pricing.step2Body')}</span></li>
            <li><b>03</b><strong>{t('pricing.step3Title')}</strong><span>{t('pricing.step3Body')}</span></li>
          </ol>
        </section>
      </main>
      <MarketingFooter />
    </div>
  );
}
