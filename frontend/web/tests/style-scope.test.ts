import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const source = (path: string) => readFileSync(resolve(process.cwd(), path), 'utf8');
const styles = source('src/styles.css');
const dashboard = source('src/components/Dashboard.tsx');

assert.doesNotMatch(
  styles,
  /(^|\n)\.side(?=[\s,{])/,
  'generic .side layout selectors can collide with row state classes',
);
assert.match(styles, /\.app-side\s*\{[^}]*height:\s*calc\(100vh - 52px\)/s);
assert.doesNotMatch(dashboard, /<aside/);
assert.match(source('src/components/RepositorySidebar.tsx'), /className="app-side app-side-right"/);
