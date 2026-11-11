// Shared harness for the browser suites run by `npm run test:browser`
// (tests/browser/run.cjs). A suite is a list of named checks; each check runs
// once per viewport width on a fresh page with the fixture API (mockConsole),
// and fails on an assertion, a page error, an unexpected native dialog or a
// page that scrolls sideways. Every check runs even when an earlier one
// fails; the process exits non-zero when any did.
//
// Runs against TEST_WEB_URL, or serves the built console (web/static) itself.
// Chromium: TEST_CHROMIUM_PATH, else /usr/bin/chromium when present, else the
// browser Playwright installed (`npx playwright install chromium`).
const fs = require('node:fs');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const { mockConsole } = require('./console.cjs');
const { startStaticServer } = require('./static-server.cjs');

function chromiumPath() {
  if (process.env.TEST_CHROMIUM_PATH) return process.env.TEST_CHROMIUM_PATH;
  return fs.existsSync('/usr/bin/chromium') ? '/usr/bin/chromium' : undefined;
}

function launch(options = {}) {
  return chromium.launch({ executablePath: chromiumPath(), args: ['--no-sandbox'], ...options });
}

async function serve() {
  if (process.env.TEST_WEB_URL) return { url: process.env.TEST_WEB_URL.replace(/\/$/, ''), close: async () => {} };
  return startStaticServer();
}

/** noOverflow fails when the page scrolls sideways. */
async function noOverflow(page, context = '') {
  const sizes = await page.evaluate(() => ({ page: document.documentElement.scrollWidth, viewport: innerWidth }));
  assert(sizes.page <= sizes.viewport + 1, `${context} page scrolls sideways (${sizes.page}px > ${sizes.viewport}px)`.trim());
}

/** open loads a console path and waits for its heading. */
async function open(page, base, path) {
  await page.goto(`${base}${path}`);
  await page.locator('#main-content h1').first().waitFor();
}

/**
 * suite runs checks: [{ name, widths = [390, 1440], mock = true, run(ctx) }].
 * ctx: { page, base, width, state (mockConsole state), open(path), errors }.
 */
async function suite(title, checks) {
  const started = Date.now();
  const server = await serve();
  const browser = await launch();
  const failures = [];
  const only = process.env.TEST_ONLY;
  try {
    for (const check of checks) {
      if (only && !check.name.includes(only)) continue;
      for (const width of check.widths || [390, 1440]) {
        const label = `${check.name} @${width}px`;
        const context = await browser.newContext({ viewport: { width, height: check.height || 900 }, ...(check.context || {}) });
        const page = await context.newPage();
        page.setDefaultTimeout(Number(process.env.TEST_TIMEOUT || 8000));
        const errors = [];
        page.on('pageerror', error => errors.push(`page error: ${error.message}`));
        page.on('dialog', dialog => { errors.push(`unexpected native dialog: ${dialog.message()}`); void dialog.dismiss(); });
        const state = check.mock === false ? undefined : await mockConsole(page);
        const begun = Date.now();
        try {
          await check.run({ page, context, base: server.url, width, state, errors, open: path => open(page, server.url, path) });
          if (check.overflow !== false) await noOverflow(page, label);
          assert.deepEqual(errors, [], `${label}: browser errors`);
          console.log(`  ok   ${label} (${((Date.now() - begun) / 1000).toFixed(1)}s)`);
        } catch (error) {
          failures.push(label);
          console.log(`  FAIL ${label}\n       ${String(error.stack || error).split('\n').slice(0, 6).join('\n       ')}`);
        } finally {
          await context.close();
        }
      }
    }
  } finally {
    await browser.close();
    await server.close();
  }
  const seconds = ((Date.now() - started) / 1000).toFixed(1);
  if (failures.length) {
    console.log(`FAIL ${title}: ${failures.length} check(s) failed (${seconds}s): ${failures.join(', ')}`);
    process.exitCode = 1;
  } else console.log(`PASS ${title} (${seconds}s)`);
}

/** gate is a promise a route handler can wait on until the check releases it. */
function gate() { let release; const promise = new Promise(resolve => { release = resolve; }); return { promise, release }; }

/** calls filters the fixture's recorded API calls. */
const callsTo = (state, test, method) => state.calls.filter(call => (typeof test === 'string' ? call.path === test : test.test(call.path)) && (!method || call.method === method));

/** until polls a Node-side condition (such as recorded fixture calls) until it holds. */
async function until(condition, message = 'condition', timeout = 8000) {
  const deadline = Date.now() + timeout;
  while (!condition()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${message}`);
    await new Promise(resolve => setTimeout(resolve, 50));
  }
}

module.exports = { suite, launch, serve, chromiumPath, noOverflow, open, gate, callsTo, until, assert };
