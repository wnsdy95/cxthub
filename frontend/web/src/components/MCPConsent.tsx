import { useEffect, useState } from 'react';
import { api, type OAuthConsentRequest } from '../api';
import { useT } from '../i18n';

export function MCPConsent({ requestId }: { requestId: string }) {
  const t = useT();
  const [request, setRequest] = useState<OAuthConsentRequest | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let live = true;
    if (!requestId) {
      setError(t('oauth.invalidRequest'));
      return () => {
        live = false;
      };
    }
    api
      .getOAuthConsent(requestId)
      .then((value) => {
        if (live) setRequest(value);
      })
      .catch((cause: Error) => {
        if (live) setError(cause.message);
      });
    return () => {
      live = false;
    };
  }, [requestId, t]);

  async function decide(approve: boolean) {
    setBusy(true);
    setError('');
    try {
      const result = await api.decideOAuthConsent(requestId, approve);
      location.assign(result.redirect_url);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
      setBusy(false);
    }
  }

  return (
    <div className="auth-wrap">
      <main className="auth-card mcp-consent-card">
        <div className="auth-head">
          <div className="brand">
            cxthub<span>/</span>
          </div>
          <span className="mode-chip">MCP · READ ONLY</span>
        </div>
        <h1>{t('oauth.title')}</h1>
        {!request && !error && <p className="sub">{t('oauth.loading')}</p>}
        {request && (
          <>
            <p className="sub">{t('oauth.description', { client: request.client_name })}</p>
            <dl className="mcp-consent-scope">
              <div>
                <dt>{t('oauth.permission')}</dt>
                <dd>{t('oauth.readContext')}</dd>
              </div>
              <div>
                <dt>{t('oauth.resource')}</dt>
                <dd>
                  <code>{request.resource}</code>
                </dd>
              </div>
              <div>
                <dt>{t('oauth.returnDestination')}</dt>
                <dd>
                  <code>{request.redirect_uri}</code>
                </dd>
              </div>
            </dl>
            <p className="hint">{t('oauth.boundary')}</p>
            <p className="hint mcp-consent-warning">{t('oauth.verifyDestination')}</p>
            <div className="mcp-consent-actions">
              <button type="button" className="ghost" disabled={busy} onClick={() => void decide(false)}>
                {t('common.cancel')}
              </button>
              <button type="button" disabled={busy} onClick={() => void decide(true)}>
                {busy ? t('oauth.connecting') : t('oauth.approve')}
              </button>
            </div>
          </>
        )}
        {error && (
          <p className="err" role="alert">
            {error}
          </p>
        )}
      </main>
    </div>
  );
}
