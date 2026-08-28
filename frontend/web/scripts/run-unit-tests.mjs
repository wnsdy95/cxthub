import { readdir } from 'node:fs/promises';
import { fileURLToPath, pathToFileURL } from 'node:url';
import path from 'node:path';
import { build } from 'esbuild';

const projectRoot = fileURLToPath(new URL('..', import.meta.url));
const testsRoot = path.join(projectRoot, 'tests');

async function collectTests(dir) {
  const entries = await readdir(dir, { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const target = path.join(dir, entry.name);
    if (entry.isDirectory()) files.push(...(await collectTests(target)));
    else if (entry.name.endsWith('.test.ts')) files.push(target);
  }
  return files;
}

const tests = (await collectTests(testsRoot)).sort();
if (tests.length === 0) throw new Error(`no frontend unit tests found under ${testsRoot}`);

for (const test of tests) {
  const result = await build({
    absWorkingDir: projectRoot,
    entryPoints: [test],
    bundle: true,
    format: 'esm',
    logLevel: 'silent',
    platform: 'node',
    target: 'node20',
    write: false,
  });
  const source = result.outputFiles[0]?.text;
  if (!source) throw new Error(`esbuild produced no output for ${test}`);
  const encoded = Buffer.from(`${source}\n//# sourceURL=${pathToFileURL(test).href}`).toString('base64');
  await import(`data:text/javascript;base64,${encoded}`);
}

console.log(`frontend unit tests: ${tests.length} passed`);
