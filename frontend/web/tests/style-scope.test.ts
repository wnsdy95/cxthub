import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const source = (path: string) => readFileSync(resolve(process.cwd(), path), 'utf8');
const styles = source('src/styles.css');
const contextView = source('src/components/ContextView.tsx');
const dashboard = source('src/components/Dashboard.tsx');

assert.doesNotMatch(
  styles,
  /(^|\n)\.side(?=[\s,{])/,
  'generic .side layout selectors can collide with row state classes',
);
assert.match(styles, /\.app-side\s*\{[^}]*height:\s*calc\(100vh - 52px\)/s);
assert.match(dashboard, /<aside className="app-side">/);

assert.match(contextView, /' off-mainline'/);
assert.match(styles, /\.commit-row\.off-mainline\s*\{/);
assert.doesNotMatch(contextView, /' side'/, 'off-mainline rows must not inherit the app sidebar layout');
