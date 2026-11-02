import { existsSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';

export interface MarkdownDisplayFixture {
  version: number;
  cases: { name: string; source: string; text: string }[];
}

export function loadMarkdownDisplayFixture(root = process.cwd()): MarkdownDisplayFixture {
  return loadDisplayFixture('markdown-display-v1', root);
}

export function loadDisplayFixture(name: 'markdown-display-v1' | 'event-display-v1' | 'tool-display-v1', root = process.cwd()): MarkdownDisplayFixture {
  const paths = [resolve(root, `../../schemas/fixtures/${name}.json`),
    resolve(root, `tests/fixtures/${name}.json`)].filter(existsSync);
  if (!paths.length) throw new Error('Markdown display fixture is missing from the source or component export');
  const raw = readFileSync(paths[0]);
  if (paths.some(filename => !readFileSync(filename).equals(raw))) throw new Error('Markdown display fixture copies disagree');
  const fixture = JSON.parse(raw.toString('utf8')) as MarkdownDisplayFixture;
  if (fixture.version !== 1 || !Array.isArray(fixture.cases) || fixture.cases.length === 0 || fixture.cases.some(entry =>
    typeof entry.name !== 'string' || typeof entry.source !== 'string' || typeof entry.text !== 'string')) {
    throw new Error('Invalid markdown display v1 fixture');
  }
  return fixture;
}
