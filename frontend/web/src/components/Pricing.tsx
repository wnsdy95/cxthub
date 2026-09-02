import { useEffect, useState } from 'react';
import { useMe } from '../hooks';
import { useT, Rich } from '../i18n';
import { navigate } from '../route';
import {
  STORAGE_PRICING,
  billableStorageGiB,
  estimateMonthlyStorageUsd,
  normalizedAverageStorageGiB,
} from '../pricing';
import { MarketingFooter, MarketingHeader, MarketingLink } from './MarketingChrome';

function displayGiB(value: number): string {
  return Number.isInteger(value) ? String(value) : value.toFixed(2).replace(/0+$/, '').replace(/\.$/, '');
}

export function Pricing({ onSignIn }: { onSignIn?: () => void }) {
  const t = useT();
  const me = useMe().data;
  const [storage, setStorage] = useState('25');
  const averageStorage = normalizedAverageStorageGiB(Number.parseFloat(storage));
  const billableStorage = billableStorageGiB(averageStorage);
  const estimate = estimateMonthlyStorageUsd(averageStorage);

  useEffect(() => {
    window.scrollTo({ top: 0, left: 0 });
  }, []);

  return (
    <div className="landing pricing-page">
      <MarketingHeader onSignIn={onSignIn}>
        <MarketingLink to="/">{t('landing.navProduct')}</MarketingLink>
        <MarketingLink to="/pricing" current>{t('landing.navPricing')}</MarketingLink>
        <a href={STORAGE_PRICING.sourceDocs} target="_blank" rel="noreferrer">
          {t('pricing.navSource')} ↗
        </a>
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
              <span className="pricing-parity">{t('pricing.parityBadge')}</span>
            </div>
            <div className="pricing-free">
              <strong>$0</strong>
              <span><b>{STORAGE_PRICING.includedGiB} GiB</b>{t('pricing.includedSuffix')}</span>
            </div>
            <div className="pricing-overage">
              <span>{t('pricing.then')}</span>
              <strong>${STORAGE_PRICING.overageUsdPerGiBMonth.toFixed(2)}</strong>
              <span>{t('pricing.unit')}</span>
            </div>
            <ul className="pricing-includes">
              <li>{t('pricing.includeAccountPool')}</li>
              <li>{t('pricing.includeMembers')}</li>
              <li>{t('pricing.includeAgents')}</li>
              <li>{t('pricing.includeBandwidth')}</li>
            </ul>
            <button className="btn-primary lg" onClick={me ? () => navigate('/') : onSignIn}>
              {me ? t('pricing.ctaAuthed') : t('pricing.ctaGuest')}
            </button>
          </article>

          <aside className="pricing-calculator" aria-labelledby="pricing-calculator-title">
            <div className="pricing-calc-head">
              <span className="feat-kicker">{t('pricing.calculatorKicker')}</span>
              <h2 id="pricing-calculator-title">{t('pricing.calculatorTitle')}</h2>
              <p>{t('pricing.calculatorHint')}</p>
            </div>
            <label htmlFor="pricing-storage">{t('pricing.averageStorage')}</label>
            <div className="pricing-input">
              <input
                id="pricing-storage"
                type="number"
                min="0"
                step="1"
                inputMode="decimal"
                value={storage}
                onChange={(e) => setStorage(e.target.value)}
              />
              <span>GiB</span>
            </div>
            <dl className="pricing-calc-lines">
              <div><dt>{t('pricing.included')}</dt><dd>− {STORAGE_PRICING.includedGiB} GiB</dd></div>
              <div><dt>{t('pricing.billable')}</dt><dd>{displayGiB(billableStorage)} GiB</dd></div>
              <div className="total"><dt>{t('pricing.estimate')}</dt><dd>${estimate.toFixed(2)} <small>USD / mo</small></dd></div>
            </dl>
            <p className="pricing-formula">
              {displayGiB(billableStorage)} GiB × ${STORAGE_PRICING.overageUsdPerGiBMonth.toFixed(2)} = ${estimate.toFixed(2)}
            </p>
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

        <section className="pricing-source-card">
          <div>
            <span className="feat-kicker">{t('pricing.sourceKicker')}</span>
            <h2>{t('pricing.sourceTitle')}</h2>
            <p><Rich>{t('pricing.sourceBody', { date: STORAGE_PRICING.verifiedAt })}</Rich></p>
          </div>
          <div className="pricing-source-links">
            <a href={STORAGE_PRICING.sourceDocs} target="_blank" rel="noreferrer">{t('pricing.sourceDocs')} ↗</a>
            <a href={STORAGE_PRICING.sourceCalculator} target="_blank" rel="noreferrer">{t('pricing.sourceCalculator')} ↗</a>
          </div>
        </section>
      </main>

      <MarketingFooter />
    </div>
  );
}
