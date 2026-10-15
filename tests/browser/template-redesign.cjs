const { chromium } = require('playwright');
const assert = require('node:assert/strict');
const { mockConsole } = require('./fixtures/console.cjs');
const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8880';
const routes = ['applications', 'deployments', 'servers', 'databases', 'remote-backups', 'integrations', 'defaults', 'updates', 'team', 'audit', 'errors', 'security', 'help', 'new-application', 'page/settings/portal', 'page/server/local', 'page/server/new', 'page/containers/local', 'page/console/local', 'page/database/portal', 'page/cache/portal', 'page/build-logs/build-one', 'page/runtime-logs/portal', 'page/ssh/local', 'page/upgrade/local', 'page/two-factor/account'];
async function noOverflow(page, context) {
  const overflow = await page.evaluate(() => ({ viewport: innerWidth, page: document.documentElement.scrollWidth, controls: [...document.querySelectorAll('#main-content input:not([type=hidden]), #main-content select, #main-content textarea, #main-content button')].filter(e => e.getClientRects().length && getComputedStyle(e).visibility !== 'hidden').map(e => ({ label: e.getAttribute('aria-label') || e.name || e.textContent.trim().slice(0, 40), x: e.getBoundingClientRect().x, right: e.getBoundingClientRect().right })).filter(r => r.x < -1 || r.right > innerWidth + 1) }));
  assert(overflow.page <= overflow.viewport + 1, `${context}: page overflow ${JSON.stringify(overflow)}`);
  assert.equal(overflow.controls.length, 0, `${context}: clipped controls ${JSON.stringify(overflow.controls)}`);
}
(async () => {
  const browser = await chromium.launch({ executablePath: '/usr/bin/chromium', args: ['--no-sandbox'] });
  try {
    for (const width of [320, 390, 768, 1440]) {
      const page = await browser.newPage({ viewport: { width, height: 900 } });
      const errors = [];
      page.on('pageerror', e => errors.push(e.message));
      page.on('dialog', dialog => { errors.push(`Unexpected native dialog: ${dialog.message()}`); void dialog.dismiss(); });
      const state = await mockConsole(page);
      for (const route of routes) {
        await page.goto(`${base}/console.html#${route}`);
        await page.locator('#main-content h1').waitFor();
        await page.waitForTimeout(80);
        assert.equal(await page.locator('#main-content').innerText().then(text => /undefined|NaN/.test(text)), false, `${width} ${route}: missing dynamic values`);
        await noOverflow(page, `${width} ${route}`);
        assert.deepEqual(errors, [], `${width} ${route}: browser errors`);
      }
      await page.goto(`${base}/console.html#applications`);
      await page.locator('[data-app-card]').first().waitFor();
      assert.equal(await page.locator('[data-app-card]').count(), 2);
      await page.getByPlaceholder('Search applications…').fill('portal');
      assert.equal(await page.locator('[data-app-card]').count(), 1);
      await page.getByPlaceholder('Search applications…').fill('');
      await page.getByRole('combobox', { name: 'Filter by application status' }).click();
      await page.getByRole('option', { name: 'Stopped', exact: true }).click();
      assert.equal(await page.locator('[data-app-card]').count(), 1);
      assert.equal(await page.locator('[data-app-card]').first().getAttribute('data-app-card'), 'api');
      await page.getByRole('button', { name: 'List view' }).click();
      await noOverflow(page, `${width} list view`);
      await page.getByRole('combobox', { name: 'Filter by application status' }).click();
      await page.getByRole('option', { name: 'All statuses', exact: true }).click();
      await page.locator('[data-app-card="portal"]').getByRole('button', { name: 'More', exact: true }).click();
      await page.getByRole('menuitem', { name: 'Runtime logs', exact: true }).click();
      await page.waitForURL(url => url.hash === '#page/runtime-logs/portal');
      if (width < 861) {
        const trigger = page.getByRole('button', { name: 'Open menu' }); await trigger.click();
        const menu = page.getByRole('dialog', { name: 'Workspace menu' });
        await menu.getByRole('link', { name: 'Servers', exact: true }).click();
        await menu.waitFor({ state: 'hidden' });
        await page.waitForURL(url => url.hash === '#servers');
        await trigger.click();
        await menu.getByRole('link', { name: 'Servers', exact: true }).click();
        await menu.waitFor({ state: 'hidden' });
      }
      assert.deepEqual(state.unknown, [], 'Unmocked production API reads');
      assert.deepEqual(errors, []);
      await page.close();
      console.log(`PASS: 26 routes, filters, grid/list, menus and navigation at ${width}px`);
    }
  } finally { await browser.close(); }
})().catch(e => { console.error(e); process.exit(1); });
