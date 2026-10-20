const { chromium } = require('playwright');
const assert = require('node:assert/strict');
const { mockConsole } = require('./fixtures/console.cjs');
const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8880';
(async () => {
 const browser = await chromium.launch({ executablePath: '/usr/bin/chromium', args: ['--no-sandbox'] });
 try {
  for (const width of [390, 1440]) {
   const page = await browser.newPage({ viewport: { width, height: 900 } });
   const errors = []; page.on('pageerror', e => errors.push(e.message));
   const state = await mockConsole(page);
   await page.route('**/api/control/deployments', route => route.fulfill({ json: { deployments: [{ id: 'bootstrap', app_id: 'portal', status: 'setup_required', created: new Date().toISOString() }], unavailable_servers: [] } }));
   state.apps[0].current.stack = { bootstrap: true, manifest: { bootstrap_readiness: { setup_path: '/setup' } } };
   await page.goto(`${base}/console.html#applications`);
   const setupCard = page.locator('[data-app-card="portal"]');
   await setupCard.getByText('Setup required', { exact: true }).waitFor();
   assert.equal(await setupCard.getByRole('link', { name: 'Open setup' }).getAttribute('href'), 'https://portal.example.com/setup');
   delete state.apps[0].current.stack;
   await page.goto(`${base}/console.html#new-application`);
   await page.getByRole('button', { name: 'Browse', exact: true }).click();
   await page.getByRole('button', { name: 'acme/portal Private · main' }).click();
   await page.locator('[name=id]').fill('stack-portal');
   await page.getByRole('button', { name: 'Continue', exact: true }).click();
   await page.getByRole('combobox', { name: 'Runtime profile' }).selectOption('minimum');
   await page.locator('[name=domain]').fill('stack.example.com');
   await page.locator('[name=database_mode]').selectOption('local');
   await page.getByRole('button', { name: 'Continue', exact: true }).click();
   await page.getByRole('button', { name: 'Create application', exact: true }).click();
   await page.waitForURL(url => url.hash === '#applications');
   const created = state.calls.find(c => c.path === 'control/apps' && c.method === 'POST')?.body;
   assert.equal(created?.runtime_profile, 'minimum');
   assert.equal(created?.database.mode, 'local');
   await page.goto(`${base}/console.html#page/settings/portal`);
   await page.getByRole('combobox', { name: 'Runtime profile' }).selectOption('standard');
   await page.getByRole('tab', { name: 'Environment', exact: true }).click();
   await page.getByRole('button', { name: 'Save changes' }).click();
   await page.waitForURL(url => url.hash === '#applications');
   const saved = state.calls.find(c => c.path === 'control/apps/portal/settings' && c.method === 'PATCH')?.body;
   assert.equal(saved?.runtime_profile, 'standard');
   assert.equal(saved?.domain, 'portal.example.com');
   assert.deepEqual(errors, []);
   await page.close();
   console.log(`PASS: stack creation with managed PostgreSQL and profile settings persistence at ${width}px`);
  }
 } finally { await browser.close(); }
})().catch(e => { console.error(e); process.exit(1); });
