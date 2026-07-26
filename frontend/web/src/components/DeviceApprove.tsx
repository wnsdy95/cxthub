// DeviceApprove — CLI pairing approval page (/login/device?code=…).
// When cxt login confirms the code in this browser, the CLI polls for the token.
// The secret is not passed through this page — only an approval signal is sent.
import { useState } from 'react';
import { api } from '../api';
import { useT } from '../i18n';

export function DeviceApprove({ code }: { code: string }) {
  const t = useT();
  const [state, setState] = useState<'idle' | 'busy' | 'done' | 'error'>('idle');
  const [err, setErr] = useState('');

  async function approve() {
    setState('busy');
    try {
      await api.approveDevice(code);
      setState('done');
    } catch (x) {
      setErr(x instanceof Error ? x.message : String(x));
      setState('error');
    }
  }

  return (
    <div className="auth-wrap">
      <div className="auth-card device-card">
        <div className="brand">
          cxthub<span>/</span>
        </div>
        <p className="sub">{t('device.title')}</p>
        {state === 'done' ? (
          <>
            <p className="device-done">{t('device.approved')}</p>
            <p className="hint">{t('device.approvedHint')}</p>
          </>
        ) : (
          <>
            <div className="device-code">{code || t('device.noCode')}</div>
            <p className="warn-red">
              {t('device.warnPre')}
              <strong>{t('device.warnStrong')}</strong>
              {t('device.warnPost')}
            </p>
            <button onClick={approve} disabled={state === 'busy' || !code} style={{ width: '100%' }}>
              {state === 'busy' ? t('device.approving') : t('device.approve')}
            </button>
            {state === 'error' && <p className="err">{err}</p>}
          </>
        )}
      </div>
    </div>
  );
}
