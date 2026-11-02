import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { build } from 'esbuild';

const input = process.argv[2];
if (!input) throw new Error('Pass the synthetic JSON produced by TestPGDocVisibleBrowserContract with CXT_VISIBLE_WIRE_OUTPUT.');
const temporary = await mkdtemp(path.join(tmpdir(), 'cxt-doc-visible-contract-'));
try {
  const output = path.join(temporary, 'validator.mjs');
  await build({ entryPoints: [fileURLToPath(new URL('../src/documentVisible.ts', import.meta.url))], outfile: output,
    bundle: true, platform: 'node', format: 'esm', target: 'node20', logLevel: 'silent' });
  const { validateVisibleUnits, validateVisibleFold } = await import(pathToFileURL(output).href);
  const bundle = JSON.parse(await readFile(input, 'utf8'));
  assert.equal(bundle.cases.length, 9);
  let mainCount = 0;
  let foldCount = 0;
  for (const item of bundle.cases) {
    let previous;
    const mainCursors = [];
    const seen = new Set();
    const mark = ordinal => { assert.equal(seen.has(ordinal), false); seen.add(ordinal); };
    for (const wire of item.pages) {
      if (previous?.next_cursor) mainCursors.push(previous.next_cursor);
      const page = validateVisibleUnits(wire.main, item.selection, previous, mainCursors);
      mainCount++;
      for (const unit of page.units) {
        if (unit.event) mark(unit.event.event_index);
        if (!unit.fold) continue;
        const foldWire = wire.folds.find(fold => fold.unit_from === unit.from);
        assert.ok(foldWire);
        let priorFold;
        let rendered = 0;
        const foldCursors = [unit.fold.cursor];
        for (const value of foldWire.pages) {
          if (priorFold?.next_cursor) foldCursors.push(priorFold.next_cursor);
          const fold = validateVisibleFold(value, item.selection, page, unit.fold, priorFold, rendered, foldCursors);
          for (const event of fold.events) {
            mark(event.event_index);
            if (!event.empty_reasoning) rendered++;
          }
          priorFold = fold;
          foldCount++;
        }
        assert.equal(priorFold?.next_cursor, null);
      }
      previous = page;
    }
    assert.equal(previous?.next_cursor, null);
    assert.equal(seen.size, previous.total - previous.start);
  }
  console.log(`Actual PostgreSQL → Go wire → Web validator: ${bundle.cases.length} selections, ${mainCount} main pages, ${foldCount} fold pages passed.`);
} finally {
  await rm(temporary, { recursive: true, force: true });
}
