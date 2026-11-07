import { expect, test } from 'vitest';
import { checkConsole, cssViolations, tsxViolations } from '../../../scripts/check-spacing.mjs';

// The spacing lint (scripts/check-spacing.mjs, `npm run lint`) also runs in the
// UI suite, so a raw pixel margin, padding or gap fails `vitest run`.
test('console styles use spacing tokens only', () => {
  const { files, problems } = checkConsole();
  expect(files).toBeGreaterThan(10);
  expect(problems.map(problem => `${problem.file}:${problem.line}: ${problem.message}`)).toEqual([]);
});

test('the spacing lint rejects raw pixel spacing and accepts tokens', () => {
  expect(cssViolations('.a { margin: 0 0 18px; gap: var(--space-2); }')).toHaveLength(1);
  expect(cssViolations('.a { padding: 7px 12px; }')).toHaveLength(2);
  expect(cssViolations('.a { row-gap: 3px; margin-top: calc(var(--space-2) * -1); }')).toHaveLength(1);
  expect(cssViolations('.a { margin: 0; padding: 1px; border: 2px solid; width: 18px; scroll-margin-top: 84px; }')).toHaveLength(0);
  expect(cssViolations('/* margin: 12px */ .a { gap: var(--control-gap); }')).toHaveLength(0);
  expect(tsxViolations('<div style={{ marginTop: 12 }} className="mt-[7px] gap-2" />')).toHaveLength(2);
});
