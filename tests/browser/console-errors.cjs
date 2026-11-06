// Every console route, opened in demo mode with the fixture API, must render and
// be exercised (details, tabs, menus, selects, the move panel, the new-application
// wizard) without a browser console error or warning, a page error, a failed or
// 4xx/5xx request, a Chrome [DOM] form hint, or an inline-style CSP violation.
// Run against the Vite dev server (TEST_WEB_URL=http://127.0.0.1:5173) or the
// built console served by the control panel.
const { chromium } = require('playwright');
const { mockConsole } = require('./fixtures/console.cjs');
const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8880';
const routes = ['/console/applications', '/console/deployments', '/console/errors', '/console/security', '/console/servers', '/console/databases', '/console/remote-backups', '/console/integrations', '/console/defaults', '/console/updates', '/console/team', '/console/audit', '/console/help', '/console/new-application',
  '/console/applications/portal/settings', '/console/applications/portal/settings/environment', '/console/applications/portal/settings/resources', '/console/applications/portal/settings/processes', '/console/applications/portal/logs', '/console/applications/portal/cache',
  '/console/deployments/build-one', '/console/databases/portal', '/console/servers/local', '/console/servers/new', '/console/servers/local/containers', '/console/servers/local/cleanup', '/console/servers/local/console', '/console/servers/local/ssh', '/console/servers/local/upgrade',
  '/console/account/two-factor', '/console/applications?move=portal', '/console/not-a-page'];
const demo = route => route + (route.includes('?') ? '&' : '?') + 'demo=1';
const click = async (page, locator, settle = 300) => { try { await locator.click({ timeout: 1500 }); await page.waitForTimeout(settle); return true; } catch { return false; } };

async function visit(browser, route, drive) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
  const page = await context.newPage();
  const problems = [];
  page.on('console', message => {
    const type = message.type(), text = message.text();
    // Chrome's form hints ([DOM] missing autocomplete, password outside a form) arrive as verbose entries.
    // The sign-in heuristic asking for a username field is the one exception: the wizard and the
    // environment editors mask secrets with password fields but are not credential forms.
    if (type === 'verbose' && text.includes('Password forms should have (optionally hidden) username fields')) return;
    if (type === 'error' || type === 'warning' || (type === 'verbose' && text.startsWith('[DOM]'))) problems.push(`console.${type}: ${text.split('\n')[0].slice(0, 200)}`);
  });
  page.on('pageerror', error => problems.push(`pageerror: ${error.message}`));
  page.on('requestfailed', request => { const reason = request.failure()?.errorText || ''; if (reason !== 'net::ERR_ABORTED') problems.push(`request failed: ${request.method()} ${request.url()} ${reason}`); });
  page.on('response', response => { if (response.status() >= 400) problems.push(`HTTP ${response.status()}: ${response.request().method()} ${response.url()}`); });
  page.on('dialog', dialog => dialog.dismiss().catch(() => {}));
  await mockConsole(page);
  await page.goto(base + demo(route), { waitUntil: 'networkidle' });
  await page.waitForTimeout(500);
  await drive(page);
  await page.waitForTimeout(1200);
  const inputs = await page.evaluate(() => [...document.querySelectorAll('input')].filter(input => !['checkbox', 'radio', 'hidden', 'file', 'submit', 'button', 'reset', 'image'].includes(input.type) && !input.hasAttribute('autocomplete')).map(input => `${input.name || input.id || input.getAttribute('aria-label') || input.placeholder || input.type}`));
  if (inputs.length) problems.push(`inputs without autocomplete: ${inputs.join(', ')}`);
  // A JavaScript error caught by a component and shown as its message is still a bug.
  const shown = await page.evaluate(() => (document.body.innerText.match(/[^\n]*(Cannot read properties of|is not a function|is not iterable|undefined is not)[^\n]*/g) || []));
  for (const line of shown) problems.push(`script error shown on the page: ${line.slice(0, 200)}`);
  await context.close();
  return [...new Set(problems)];
}

// Generic interactions: expand every details element, visit every tab, open every menu and select, open the mobile and account menus.
async function exercise(page) {
  for (let n = 0; n < 30; n++) { const summary = page.locator('details:not([open]) > summary').first(); if (!await summary.count() || !await click(page, summary)) break; }
  const tabs = page.locator('[role=tab]'); for (let i = 0; i < await tabs.count(); i++) await click(page, tabs.nth(i), 400);
  const menus = page.locator('[aria-haspopup="menu"], [role=combobox]'); for (let i = 0; i < Math.min(await menus.count(), 10); i++) { await click(page, menus.nth(i), 400); await page.keyboard.press('Escape'); await page.waitForTimeout(150); }
  await page.setViewportSize({ width: 390, height: 844 }); await click(page, page.getByRole('button', { name: 'Open menu' }), 400); await page.keyboard.press('Escape'); await page.setViewportSize({ width: 1440, height: 1000 });
}

// The wizard with the demo detection, start to finish.
async function wizard(page) {
  await page.locator('#wizard-id').fill('console-sweep');
  await page.getByRole('button', { name: 'Continue' }).click();
  await page.getByRole('button', { name: /acme\/storefront/ }).click();
  await page.locator('#wizard-repository').waitFor();
  await page.getByRole('button', { name: 'Detect' }).click().catch(() => {});
  await page.waitForTimeout(400);
  for (const label of ['Continue', 'Continue', 'Continue', 'Continue', 'Continue']) { await page.getByRole('button', { name: label }).click(); await page.waitForTimeout(300); }
  await page.locator('#wizard-domain').fill('sweep.example.com').catch(() => {});
  while (await page.getByRole('button', { name: 'Continue' }).count()) { await page.getByRole('button', { name: 'Continue' }).click(); await page.waitForTimeout(300); }
  await exercise(page);
}

(async () => {
  const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_BIN || '/usr/bin/chromium', headless: true, args: ['--no-sandbox'] });
  const failures = [];
  try {
    for (const route of routes) {
      const problems = await visit(browser, route, route === '/console/new-application' ? wizard : exercise);
      if (problems.length) failures.push(`${route}\n  ${problems.join('\n  ')}`);
    }
  } finally { await browser.close(); }
  if (failures.length) { console.error(`Console problems on ${failures.length} route(s):\n${failures.join('\n')}`); process.exit(1); }
  console.log(`PASS: ${routes.length} console routes render and respond without console errors, warnings, failed requests or form hints`);
})().catch(error => { console.error(error); process.exit(1); });
