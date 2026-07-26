// AIBar — GitHub "Languages" widget's AI version: Displays a single bar (segmented color bar) + percentage legend for the context of this repo, indicating which AI was used.
// Color is fixed for entities (Claude/Codex — not ranked), Other is neutral (remaining bucket, legend as label).
import { useMemo } from 'react';
import type { Snapshot } from '../types';
import claudeLogo from '../assets/claude.webp';
import codexLogo from '../assets/codex.webp';

export const PROVIDER_META: Record<string, { name: string; color: string }> = {
  claude: { name: 'Claude', color: '#D97757' },
  codex: { name: 'Codex', color: '#10a37f' },
};
const OTHER = { name: 'Other', color: '#b6bcc6' };

// Provider logo (square crop asset for circular icon). Single source of truth — use color dots as fallback if unknown.
export const PROVIDER_LOGOS: Record<string, string> = { claude: claudeLogo, codex: codexLogo };

// Assistant text ink — provider main color (Claude terra cotta, Codex is logo's blue violet).
export const PROVIDER_INK: Record<string, string> = { claude: '#D97757', codex: '#5E68F0' };

// Model name → provider color (fixed for entities — same palette as AIBar). Use neutral color if unknown.
export function modelColor(model: string): string {
  const m = model.toLowerCase();
  if (m.includes('claude')) return PROVIDER_META.claude.color;
  if (m.includes('gpt') || m.includes('codex')) return PROVIDER_META.codex.color;
  return OTHER.color;
}

// Model name → provider logo (same mapping as modelColor). Use null — fallback to color dots at call site.
export function modelLogo(model: string): string | null {
  const m = model.toLowerCase();
  if (m.includes('claude')) return PROVIDER_LOGOS.claude;
  if (m.includes('gpt') || m.includes('codex')) return PROVIDER_LOGOS.codex;
  return null;
}

// Single AI icon — circular badge image if logo exists, neutral color dot (legacy/unknown model).
export function AIIcon({ logo, color, title, style }: { logo: string | null; color: string; title: string; style?: React.CSSProperties }) {
  return logo ? (
    <img className="ai-logo" src={logo} alt={title} title={title} style={style} />
  ) : (
    <span className="ai-dot" title={title} style={{ background: color, ...style }} />
  );
}

export function AIBar({ snapshots }: { snapshots: Snapshot[] }) {
  const shares = useMemo(() => {
    if (snapshots.length === 0) return [];
    const counts = new Map<string, number>();
    for (const s of snapshots) {
      const key = PROVIDER_META[s.provider] ? s.provider : 'other';
      counts.set(key, (counts.get(key) ?? 0) + 1);
    }
    // Fixed order: claude → codex → other (not by rank or size; color follows the entity).
    const order = ['claude', 'codex', 'other'];
    return order
      .filter((k) => counts.has(k))
      .map((k) => {
        const meta = PROVIDER_META[k] ?? OTHER;
        const pct = ((counts.get(k) ?? 0) / snapshots.length) * 100;
        return { key: k, name: meta.name, color: meta.color, pct };
      });
  }, [snapshots]);

  if (shares.length === 0) return null;
  const fmt = (p: number) => `${p.toFixed(1).replace(/\.0$/, '')}%`;

  return (
    <div className="aibar">
      <span className="label">AI</span>
      <div className="aibar-track" role="img" aria-label={shares.map((s) => `${s.name} ${fmt(s.pct)}`).join(', ')}>
        {shares.map((s) => (
          <span
            key={s.key}
            className="aibar-seg"
            style={{ width: `${s.pct}%`, background: s.color }}
            title={`${s.name} ${fmt(s.pct)}`}
          />
        ))}
      </div>
      <ul className="aibar-legend">
        {shares.map((s) => (
          <li key={s.key}>
            <AIIcon logo={PROVIDER_LOGOS[s.key] ?? null} color={s.color} title={s.name} />
            <strong>{s.name}</strong> <em>{fmt(s.pct)}</em>
          </li>
        ))}
      </ul>
    </div>
  );
}
