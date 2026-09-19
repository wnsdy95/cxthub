import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import ts from 'typescript';

function runtimeImports(file) {
  const imports = [];
  function visit(node) {
    if (ts.isImportDeclaration(node)) {
      const clause = node.importClause;
      const named = clause?.namedBindings;
      const onlyTypes = clause?.isTypeOnly || (!clause?.name && named && ts.isNamedImports(named)
        && named.elements.length > 0 && named.elements.every(e => e.isTypeOnly));
      if (!onlyTypes && ts.isStringLiteral(node.moduleSpecifier)) imports.push(node.moduleSpecifier.text);
    } else if (ts.isExportDeclaration(node) && node.moduleSpecifier) {
      const named = node.exportClause;
      const onlyTypes = node.isTypeOnly || (named && ts.isNamedExports(named)
        && named.elements.length > 0 && named.elements.every(e => e.isTypeOnly));
      if (!onlyTypes && ts.isStringLiteral(node.moduleSpecifier)) imports.push(node.moduleSpecifier.text);
    } else if (ts.isImportEqualsDeclaration(node) && !node.isTypeOnly && ts.isExternalModuleReference(node.moduleReference)) {
      const value = node.moduleReference.expression;
      if (value && ts.isStringLiteral(value)) imports.push(value.text);
    } else if (ts.isCallExpression(node) && (node.expression.kind === ts.SyntaxKind.ImportKeyword
      || (ts.isIdentifier(node.expression) && node.expression.text === 'require'))) {
      const arg = node.arguments[0];
      if (arg && ts.isStringLiteral(arg)) imports.push(arg.text);
    }
    ts.forEachChild(node, visit);
  }
  visit(file);
  return imports;
}

// Resolve with the same TypeScript options as the app, including path aliases
// and index/barrel modules. Type-only edges do not create runtime cycles.
export function dependencyCycles(configPath) {
  const config = ts.readConfigFile(configPath, ts.sys.readFile);
  if (config.error) throw new Error(ts.flattenDiagnosticMessageText(config.error.messageText, '\n'));
  const parsed = ts.parseJsonConfigFileContent(config.config, ts.sys, path.dirname(configPath));
  if (parsed.errors.length) throw new Error(parsed.errors.map(e => ts.flattenDiagnosticMessageText(e.messageText, '\n')).join('\n'));
  const graph = new Map();
  function collect(filename) {
    filename = path.resolve(filename);
    if (graph.has(filename) || filename.endsWith('.d.ts') || filename.includes(`${path.sep}node_modules${path.sep}`)) return;
    const source = ts.createSourceFile(filename, fs.readFileSync(filename, 'utf8'), ts.ScriptTarget.Latest, true);
    const targets = [];
    graph.set(filename, targets);
    for (const specifier of runtimeImports(source)) {
      const resolved = ts.resolveModuleName(specifier, filename, parsed.options, ts.sys).resolvedModule;
      if (!resolved || resolved.isExternalLibraryImport || resolved.resolvedFileName.endsWith('.d.ts')) continue;
      const target = path.resolve(resolved.resolvedFileName);
      targets.push(target);
      collect(target);
    }
  }
  parsed.fileNames.forEach(collect);
  const visited = new Set(), active = new Map(), stack = [], cycles = [];
  function walk(file) {
    if (active.has(file)) { cycles.push([...stack.slice(active.get(file)), file]); return; }
    if (visited.has(file)) return;
    visited.add(file); active.set(file, stack.length); stack.push(file);
    for (const target of graph.get(file) ?? []) walk(target);
    stack.pop(); active.delete(file);
  }
  for (const file of graph.keys()) walk(file);
  return { files: graph.size, cycles };
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const { files, cycles } = dependencyCycles(path.resolve('tsconfig.json'));
  if (cycles.length) {
    for (const cycle of cycles) console.error(cycle.map(f => path.relative(process.cwd(), f)).join(' -> '));
    process.exitCode = 1;
  } else console.log(`Runtime dependency graph: ${files} modules, no cycles`);
}
