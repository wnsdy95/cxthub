// i18n locale consistency check for two error classes that tsc cannot detect:
//   1) mismatched interpolation variables between ko and en;
//   2) duplicate values under multiple keys in both locales, which may belong in common.
// The Messages type already enforces key parity, but this script verifies it defensively.
// esbuild (already a Vite dependency) loads the TypeScript locale files directly, so no
// separate test runner is required.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { transformSync } from 'esbuild';

const localesDir = join(dirname(fileURLToPath(import.meta.url)), '..', 'src', 'i18n', 'locales');

function load(file, name) {
  const src = readFileSync(join(localesDir, file), 'utf8').replace(/^\s*import\s.*$/gm, ''); // Remove type-only imports.
  const js = transformSync(src, { loader: 'ts', format: 'cjs' }).code;
  const mod = { exports: {} };
  new Function('module', 'exports', js)(mod, mod.exports);
  return mod.exports[name];
}

function flatten(obj, prefix = '', out = {}) {
  for (const [k, v] of Object.entries(obj)) {
    const key = prefix ? `${prefix}.${k}` : k;
    if (v && typeof v === 'object') flatten(v, key, out);
    else out[key] = v;
  }
  return out;
}

// Unique interpolation-variable set. Count a repeated {var} only once across plural forms.
const ph = (s) => [...new Set([...String(s).matchAll(/\{(\w+)\}/g)].map((m) => m[1]))].sort().join(',');

const ko = flatten(load('ko.ts', 'ko'));
const en = flatten(load('en.ts', 'en'));

let fail = 0;
const err = (m) => { console.error('✗ ' + m); fail++; };

// 1) Key parity.
for (const k of Object.keys(ko)) if (!(k in en)) err(`key missing from en: ${k}`);
for (const k of Object.keys(en)) if (!(k in ko)) err(`key missing from ko: ${k}`);

// 2) Matching interpolation-variable sets for every key.
for (const k of Object.keys(ko)) {
  if (k in en && ph(ko[k]) !== ph(en[k])) err(`interpolation mismatch ${k}: ko{${ph(ko[k])}} ≠ en{${ph(en[k])}}`);
}

// 3) Duplicate values. A repeated (ko, en) pair under two or more keys is a candidate
//    for consolidation into common. Ignore accidental duplication in only one locale.
let warn = 0;
const byPair = new Map();
for (const k of Object.keys(ko)) {
  if (String(ko[k]).trim().length <= 1) continue; // Ignore trivial values such as an ellipsis.
  const pair = `${ko[k]}\u0000${en[k]}`;
  (byPair.get(pair) ?? byPair.set(pair, []).get(pair)).push(k);
}
for (const [pair, keys] of byPair) {
  if (keys.length > 1) { console.warn(`⚠ duplicate value "${pair.split('\u0000')[0]}" → ${keys.join(', ')} (consider consolidating into common)`); warn++; }
}

console.log(`\nchecked ${Object.keys(ko).length} keys — ${fail} errors, ${warn} duplicate warnings`);
if (fail) { console.error('i18n check failed'); process.exit(1); }
console.log('✓ i18n check passed (key parity and interpolation variables match)');
