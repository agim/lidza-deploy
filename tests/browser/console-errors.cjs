// Every console view (fixtures/views.cjs) and every earlier URL, opened with
// the fixture API and exercised (details expanded, menus and selects opened,
// the phone menu opened), must render without a browser console error or
// warning, a page error, a failed or 4xx/5xx request, a Chrome [DOM] form
// hint, an input without autocomplete, an unmocked API read, or a script
// error shown as text. Views run in parallel pages to keep the suite short.
const { launch, serve } = require('./fixtures/harness.cjs');
const { mockConsole } = require('./fixtures/console.cjs');
const { mockServerHost } = require('./fixtures/server-host.cjs');
const { views, redirects } = require('./fixtures/views.cjs');

const paths = [...new Set([...views.map(view => view[0]), ...redirects.map(([from]) => from)])];
const parallel = Number(process.env.TEST_PARALLEL || 4);
const click = async (page, locator) => { try { await locator.click({ timeout: 1500 }); await page.waitForTimeout(150); return true; } catch { return false; } };

async function visit(browser, base, path) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
  const page = await context.newPage();
  const problems = [];
  page.on('console', message => {
    const type = message.type(), text = message.text();
    // The sign-in heuristic asking for a username field: the environment
    // editors mask secrets with password fields but are not credential forms.
    if (type === 'verbose' && text.includes('Password forms should have (optionally hidden) username fields')) return;
    if (type === 'error' || type === 'warning' || (type === 'verbose' && text.startsWith('[DOM]'))) problems.push(`console.${type}: ${text.split('\n')[0].slice(0, 200)}`);
  });
  page.on('pageerror', error => problems.push(`page error: ${error.message}`));
  page.on('requestfailed', request => { const reason = request.failure()?.errorText || ''; if (reason !== 'net::ERR_ABORTED') problems.push(`request failed: ${request.method()} ${request.url()} ${reason}`); });
  page.on('response', response => { if (response.status() >= 400) problems.push(`HTTP ${response.status()}: ${response.request().method()} ${response.url()}`); });
  page.on('dialog', dialog => { problems.push(`native dialog: ${dialog.message()}`); void dialog.dismiss(); });
  const state = await mockConsole(page);
  await mockServerHost(page, 'local');
  try {
    await page.goto(base + path, { waitUntil: 'networkidle' });
    await page.locator('#main-content h1').first().waitFor({ timeout: 5000 });
    for (let n = 0; n < 20; n++) { const summary = page.locator('#main-content details:not([open]) > summary').first(); if (!await summary.count() || !await click(page, summary)) break; }
    const menus = page.locator('#main-content [aria-haspopup="menu"], #main-content [role=combobox]');
    for (let i = 0; i < Math.min(await menus.count(), 6); i++) { if (await click(page, menus.nth(i))) { await page.keyboard.press('Escape'); await page.waitForTimeout(100); } }
    await page.setViewportSize({ width: 390, height: 844 });
    if (await click(page, page.getByRole('button', { name: 'Open menu' }))) await page.keyboard.press('Escape');
    await page.waitForTimeout(400);
    const inputs = await page.evaluate(() => [...document.querySelectorAll('input')].filter(input => !['checkbox', 'radio', 'hidden', 'file', 'submit', 'button', 'reset', 'image'].includes(input.type) && !input.hasAttribute('autocomplete')).map(input => input.name || input.id || input.getAttribute('aria-label') || input.placeholder || input.type));
    if (inputs.length) problems.push(`inputs without autocomplete: ${inputs.join(', ')}`);
    // A JavaScript error caught by a component and shown as its message is still a bug.
    for (const line of await page.evaluate(() => document.body.innerText.match(/[^\n]*(Cannot read properties of|is not a function|is not iterable|undefined is not)[^\n]*/g) || [])) problems.push(`script error shown on the page: ${line.slice(0, 200)}`);
    for (const unknown of new Set(state.unknown)) problems.push(`unmocked API read: ${unknown} (add it to fixtures/console.cjs)`);
  } catch (error) {
    problems.push(`did not render: ${String(error.message || error).split('\n')[0]}`);
  } finally { await context.close(); }
  return [...new Set(problems)];
}

(async () => {
  const started = Date.now();
  const server = await serve();
  const browser = await launch();
  const failures = [];
  try {
    const queue = [...paths];
    await Promise.all(Array.from({ length: parallel }, async () => {
      for (let path = queue.shift(); path; path = queue.shift()) {
        const problems = await visit(browser, server.url, path);
        if (problems.length) failures.push(`${path}\n    ${problems.join('\n    ')}`);
      }
    }));
  } finally { await browser.close(); await server.close(); }
  const seconds = ((Date.now() - started) / 1000).toFixed(1);
  if (failures.length) { console.log(`FAIL console errors: ${failures.length} of ${paths.length} views (${seconds}s)\n  ${failures.sort().join('\n  ')}`); process.exit(1); }
  console.log(`PASS console errors: ${paths.length} views and earlier URLs render and respond without console errors, failed requests or form hints (${seconds}s)`);
})().catch(error => { console.error(error); process.exit(1); });
