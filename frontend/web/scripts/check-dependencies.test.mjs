import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { dependencyCycles } from './check-dependencies.mjs';

function check(t, files) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cxt-dependencies-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  fs.writeFileSync(path.join(dir, 'tsconfig.json'), JSON.stringify({ compilerOptions: {
    module: 'ESNext', moduleResolution: 'Bundler', baseUrl: '.', paths: { '@/*': ['*'] },
  }, include: ['**/*.ts', '**/*.tsx'] }));
  for (const [name, value] of Object.entries(files)) fs.writeFileSync(path.join(dir, name), value);
  return dependencyCycles(path.join(dir, 'tsconfig.json')).cycles;
}
test('reports aliased barrel/runtime cycle', t => {
  const cycles = check(t, { 'a.tsx': 'import { b } from "@/index"; export const a = b;',
    'index.ts': 'export { b } from "./b";', 'b.ts': 'import { a } from "./a"; export const b = a;' });
  assert.equal(cycles.length, 1);
  const loop = cycles[0].map(f => path.basename(f));
  assert.equal(loop[0], loop.at(-1));
  assert.deepEqual(new Set(loop.slice(0, -1).map((f, i) => `${f}->${loop[i + 1]}`)),
    new Set(['a.tsx->index.ts', 'index.ts->b.ts', 'b.ts->a.tsx']));
});
test('permits type-only reverse edges but rejects mixed imports', t => {
  assert.equal(check(t, { 'a.ts': 'import { type B } from "./b"; export type A = B;',
    'b.ts': 'export { a } from "./a";' }).length, 0);
  assert.equal(check(t, { 'a.ts': 'import { type B, b } from "./b"; export const a = b;',
    'b.ts': 'import { a } from "./a"; export const b = a;' }).length, 1);
});
test('includes side effects and dynamic imports', t => {
  assert.equal(check(t, { 'a.ts': 'import "./b";', 'b.ts': 'void import("./a");' }).length, 1);
});
