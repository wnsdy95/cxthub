// UI language switcher (ko/en). Immediate application + localStorage storage, sync to account (PATCH /me) if logged in.
import { useLocale, type Locale } from '../i18n';
import { useMe, useUpdateMe } from '../hooks';

export function LocaleSwitcher({ className }: { className?: string }) {
  const [locale, setLocale] = useLocale();
  const me = useMe().data;
  const updateMe = useUpdateMe();

  function change(next: Locale) {
    if (next === locale) return;
    setLocale(next); // Immediate application locally (including non-logged-in users)
    if (me) updateMe.mutate({ locale: next }); // Sync to account if logged in (local remains unchanged on failure)
  }

  return (
    <select
      className={className ? `locale-switch ${className}` : 'locale-switch'}
      value={locale}
      onChange={(e) => change(e.target.value as Locale)}
      aria-label="Language"
    >
      <option value="ko">Korean</option>
      <option value="en">English</option>
    </select>
  );
}
