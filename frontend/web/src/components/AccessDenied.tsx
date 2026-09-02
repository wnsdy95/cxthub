import { useT } from '../i18n';

export function AccessDenied({ message }: { message: string }) {
  const t = useT();

  return (
    <div className="access-denied" role="alert">
      <span className="access-denied-mark" aria-hidden="true">
        ⊘
      </span>
      <div>
        <strong>{t('common.accessDenied')}</strong>
        <p>{message}</p>
      </div>
    </div>
  );
}
