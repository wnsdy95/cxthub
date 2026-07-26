// Lightweight type-safe i18n — no dependencies. Infers key types from ko.ts tree, catches missing keys or typos at compile time.
//
// Detection priority: localStorage('cxt.locale') > Browser language (ko → ko) > en.
// Fallback (key missing): en. Persistence: localStorage + (login sync) handled by LocaleSwitcher/Root.
import { createContext, useCallback, useContext, useState, type ReactNode } from 'react';
import { ko, type Messages } from './locales/ko';
import { en } from './locales/en';

export type Locale = 'ko' | 'en';
const MESSAGES: Record<Locale, Messages> = { ko, en };
const FALLBACK: Locale = 'en';
const STORAGE_KEY = 'cxt.locale';

// Derives point path key union from ko tree — t('common.save') is OK, t('common.svae') is compile error.
type Leaves<T> = {
  [K in keyof T & string]: T[K] extends string ? K : T[K] extends object ? `${K}.${Leaves<T[K]>}` : never;
}[keyof T & string];
export type MsgKey = Leaves<Messages>;
export type Vars = Record<string, string | number>;

export function detectLocale(): Locale {
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    if (saved === 'ko' || saved === 'en') return saved;
  } catch {
/* Block environment — fallback to browser language */
  }
  return typeof navigator !== 'undefined' && navigator.language?.toLowerCase().startsWith('ko') ? 'ko' : FALLBACK;
}

function lookup(locale: Locale, key: string): string | undefined {
  let cur: unknown = MESSAGES[locale];
  for (const part of key.split('.')) {
    if (cur && typeof cur === 'object' && part in (cur as Record<string, unknown>)) {
      cur = (cur as Record<string, unknown>)[part];
    } else {
      return undefined;
    }
  }
  return typeof cur === 'string' ? cur : undefined;
}

// {name} interpolation + plurality ("singular|plural" selected by vars.count).
function format(tmpl: string, vars?: Vars): string {
  let s = tmpl;
  if (s.includes('|') && vars && typeof vars.count === 'number') {
    const [one, other] = s.split('|');
    s = vars.count === 1 ? one : other ?? one;
  }
  if (!vars) return s;
  return s.replace(/\{(\w+)\}/g, (_, k: string) => (k in vars ? String(vars[k]) : `{${k}}`));
}

export function translate(locale: Locale, key: MsgKey, vars?: Vars): string {
  const raw = lookup(locale, key) ?? lookup(FALLBACK, key) ?? key;
  return format(raw, vars);
}

interface Ctx {
  locale: Locale;
  setLocale: (l: Locale) => void;
  t: (key: MsgKey, vars?: Vars) => string;
}
const I18nContext = createContext<Ctx | null>(null);

export function I18nProvider({ children }: { children: ReactNode }) {
  const [locale, setLocaleState] = useState<Locale>(detectLocale);
  const setLocale = useCallback((l: Locale) => {
    setLocaleState(l);
    try {
      localStorage.setItem(STORAGE_KEY, l);
    } catch {
/* Ignore block environment — maintain state within session */
    }
    if (typeof document !== 'undefined') document.documentElement.lang = l;
  }, []);
  const t = useCallback((key: MsgKey, vars?: Vars) => translate(locale, key, vars), [locale]);
  return <I18nContext.Provider value={{ locale, setLocale, t }}>{children}</I18nContext.Provider>;
}

export function useT() {
  const c = useContext(I18nContext);
  if (!c) throw new Error('useT must be used within <I18nProvider>');
  return c.t;
}

export function useLocale(): [Locale, (l: Locale) => void] {
  const c = useContext(I18nContext);
  if (!c) throw new Error('useLocale must be used within <I18nProvider>');
  return [c.locale, c.setLocale];
}

// Rich — renders `code`·**bold** segments of i18n strings as <code>/<strong> (simple markdown).
// Helper for inline emphasis in sentences across languages with different word orders.
export function Rich({ children }: { children: string }) {
  const parts = children.split(/(`[^`]+`|\*\*[^*]+\*\*)/g);
  return (
    <>
      {parts.map((p, i) => {
        if (p.startsWith('`') && p.endsWith('`')) return <code key={i}>{p.slice(1, -1)}</code>;
        if (p.startsWith('**') && p.endsWith('**')) return <strong key={i}>{p.slice(2, -2)}</strong>;
        return <span key={i}>{p}</span>;
      })}
    </>
  );
}
