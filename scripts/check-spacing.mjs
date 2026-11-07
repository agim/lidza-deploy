#!/usr/bin/env node
// Fails when console code writes a raw pixel length for spacing instead of a
// token from ui/src/tokens.css. Checked:
//   - CSS: margin*, padding*, gap, row-gap, column-gap declarations in every
//     ui/src stylesheet except tokens.css (0, 1px and -1px are allowed: borders
//     and the visually-hidden pattern).
//   - TSX: inline style spacing ({ margin: 12 } / '12px') and Tailwind arbitrary
//     spacing utilities such as mt-[7px] or gap-[10px].
// Usage: node scripts/check-spacing.mjs [--count]
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const source = join(root, 'ui/src');
const tokenFile = join(source, 'tokens.css');

function walk(dir) {
  return readdirSync(dir).flatMap(name => {
    const path = join(dir, name);
    return statSync(path).isDirectory() ? walk(path) : [path];
  });
}

const spacingProperty = /^(margin|padding)(-(top|right|bottom|left|block|inline|block-start|block-end|inline-start|inline-end))?$|^(row-|column-)?gap$/;
const allowedPx = new Set(['0px', '1px', '-1px']);

// Returns [line, column] for an offset.
function position(text, offset) {
  const before = text.slice(0, offset).split('\n');
  return [before.length, before[before.length - 1].length + 1];
}

export function cssViolations(text) {
  const problems = [];
  // Blank out comments while keeping offsets, so line numbers stay correct.
  const code = text.replace(/\/\*[\s\S]*?\*\//g, comment => comment.replace(/[^\n]/g, ' '));
  const declaration = /(^|[;{\s])([a-z-]+)\s*:\s*([^;{}]+)/g;
  for (const match of code.matchAll(declaration)) {
    const property = match[2];
    if (!spacingProperty.test(property)) continue;
    const value = match[3];
    for (const length of value.matchAll(/-?\d*\.?\d+px\b/g)) {
      if (allowedPx.has(length[0])) continue;
      const [line] = position(code, match.index + match[0].indexOf(value) + length.index);
      problems.push({ line, message: `${property}: ${value.trim()} uses ${length[0]}; use a spacing token (var(--space-*) or a semantic token)` });
    }
  }
  return problems;
}

export function tsxViolations(text) {
  const problems = [];
  const inline = /\b(margin|padding|gap|rowGap|columnGap)(Top|Right|Bottom|Left|Block|Inline)?\s*:\s*(['"`]?-?\d*\.?\d+(px)?['"`]?)/g;
  for (const match of text.matchAll(inline)) {
    if (/^['"`]?-?0(px)?['"`]?$/.test(match[3])) continue;
    problems.push({ line: position(text, match.index)[0], message: `inline style ${match[0]}; use a layout primitive or spacing class` });
  }
  const arbitrary = /\b-?(m|p|gap|space)(x|y|t|r|b|l|s|e)?-\[-?\d*\.?\d+px\]/g;
  for (const match of text.matchAll(arbitrary)) {
    problems.push({ line: position(text, match.index)[0], message: `arbitrary spacing utility ${match[0]}; use a scale step or a token` });
  }
  return problems;
}

export function checkConsole() {
  const files = walk(source).filter(path => (path.endsWith('.css') && path !== tokenFile) || (path.endsWith('.tsx') && !path.endsWith('.test.tsx')));
  const problems = files.flatMap(path => {
    const text = readFileSync(path, 'utf8');
    return (path.endsWith('.css') ? cssViolations(text) : tsxViolations(text)).map(problem => ({ ...problem, file: relative(root, path) }));
  });
  return { files: files.length, problems };
}

function main() {
  const { files, problems } = checkConsole();
  if (process.argv.includes('--count')) { console.log(problems.length); return; }
  for (const problem of problems) console.error(`${problem.file}:${problem.line}: ${problem.message}`);
  if (problems.length) {
    console.error(`\n${problems.length} raw spacing value(s). Spacing tokens live in ui/src/tokens.css; layout primitives in ui/src/components/layout.`);
    process.exit(1);
  }
  console.log(`Spacing check passed (${files} files).`);
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) main();
