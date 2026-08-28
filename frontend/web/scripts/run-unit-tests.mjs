import { mkdtemp, readdir, rm } from 'node:fs/promises';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { tmpdir } from 'node:os';
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

const outputDir = await mkdtemp(path.join(tmpdir(), 'cxt-web-tests-'));
try {
  for (const [index, test] of tests.entries()) {
    const outfile = path.join(outputDir, `${index}-${path.basename(test, '.test.ts')}.cjs`);
    await build({
      absWorkingDir: projectRoot,
      entryPoints: [test],
      bundle: true,
      format: 'cjs',
      logLevel: 'silent',
      outfile,
      platform: 'node',
      target: 'node20',
    });
    await import(pathToFileURL(outfile).href);
  }
} finally {
  await rm(outputDir, { force: true, recursive: true });
}

console.log(`frontend unit tests: ${tests.length} passed`);
