const { chromium } = require('playwright');
const assert = require('node:assert/strict');
const { mockConsole } = require('./fixtures/console.cjs');
const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8880';
(async () => {
 const browser = await chromium.launch({ executablePath: '/usr/bin/chromium', args: ['--no-sandbox'] });
 try {
  for (const width of [390, 1440]) {
   const page = await browser.newPage({ viewport: { width, height: 900 } }), errors = [];
   page.on('pageerror', e => errors.push(e.message));
   await mockConsole(page);
   let rejectSave = true;
   await page.route('**/api/control/apps/portal/settings', route => {
    if (route.request().method() === 'PATCH' && rejectSave) return route.fulfill({ status: 422, json: { error: 'Fixture save rejected; retry is safe.' } });
    return route.fallback();
   });
   await page.goto(`${base}/console.html#page/settings/portal`);
   await page.getByRole('tab', { name: 'Environment' }).click();
   const setting = page.locator('.env-row').filter({ has: page.locator('input[value=LOG_LEVEL]') }).locator('.env-value');
   await setting.fill('trace');
   await page.getByRole('button', { name: 'Save changes' }).click();
   await page.getByRole('alert').filter({ hasText: 'Fixture save rejected' }).waitFor();
   assert.equal(await setting.inputValue(), 'trace', 'A failed save discarded the edit');
   rejectSave = false;
   await page.getByRole('button', { name: 'Save changes' }).click();
   await page.waitForURL(url => url.hash === '#applications');
   await page.route('**/api/control/apps', async route => { if (route.request().method() === 'GET') return route.fulfill({ json: [] }); return route.fallback(); });
   await page.route('**/api/control/servers', route => route.fulfill({ json: [] }));
   await page.goto(`${base}/console.html#applications`);
   await page.getByRole('heading', { name: 'Connect your first server' }).waitFor();
   assert.equal(await page.getByRole('button', { name: 'New application', exact: true }).isDisabled(), true);
   await page.getByRole('button', { name: 'Connect server', exact: true }).click();
   await page.waitForURL(url => url.hash === '#servers');
   await page.getByRole('heading', { name: 'Connect your first server' }).waitFor();
   await page.route('**/api/control/apps', route => route.fulfill({ status: 503, json: { error: 'Fixture control offline' } }));
   await page.goto(`${base}/console.html#applications`);
   await page.getByRole('alert').filter({ hasText: 'Fixture control offline' }).waitFor();
   assert.deepEqual(errors, []);
   await page.close(); console.log(`PASS: failed saves retain edits and retry; empty/offline workspaces remain usable at ${width}px`);
  }
 } finally { await browser.close(); }
})().catch(e => { console.error(e); process.exit(1); });
