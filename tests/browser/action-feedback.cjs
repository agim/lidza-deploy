const { chromium } = require('playwright');
const assert = require('node:assert/strict');

(async () => {
  const browser = await chromium.launch({ executablePath: process.env.TEST_CHROMIUM_PATH || '/usr/bin/chromium', args: ['--no-sandbox'] });
  try {
    for (const width of [390, 1440]) {
      const page = await browser.newPage({ viewport: { width, height: 900 } });
      const errors = [];
      let logRequests = 0;
      let unlockRequests = 0;
      page.on('pageerror', error => errors.push(error.message));
      await page.route('**/api/**', async route => {
        const path = new URL(route.request().url()).pathname;
        if (path.endsWith('/control/status')) return route.fulfill({ json: { roles: ['admin'] } });
        if (path.endsWith('/control/apps')) return route.fulfill({ json: [{ id: 'portal', server_id: 'local', repository: 'acme/portal', branch: 'main', domain: 'portal.example.com', current: { commit: 'abc123' } }] });
        if (path.endsWith('/control/servers')) return route.fulfill({ json: [{ id: 'local', name: 'This server', url: 'http://127.0.0.1:9090' }] });
        if (path.endsWith('/control/deployments')) return route.fulfill({ json: { deployments: [{ id: 'build-one', app_id: 'portal', server_id: 'local', status: 'building', created: new Date().toISOString(), log: `Build output ${++logRequests}`, duration_seconds: 80 }], unavailable_servers: [] } });
        if (path.endsWith('/control/servers/local/console')) return route.fulfill({ json: { two_factor_enabled: true, unlocked: false } });
        if (path.endsWith('/v1/auth/step-up')) { unlockRequests++; return route.fulfill({ json: { expiresAt: new Date(Date.now() + 900000).toISOString() } }); }
        return route.fulfill({ json: {} });
      });
      const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8880';
      await page.goto(`${base}/console.html#page/build-logs/build-one`);
      await page.locator('#deployment-output').waitFor();
      const refresh = page.getByRole('button', { name: 'Refresh', exact: true });
      const before = await refresh.boundingBox();
      const started = Date.now();
      await refresh.click();
      assert.equal(await refresh.getAttribute('aria-busy'), 'true', 'Refresh did not acknowledge the click');
      assert(await refresh.locator('.button-progress').isVisible(), 'Loading bar is not visible');
      await page.waitForTimeout(200);
      assert.equal(await refresh.getAttribute('aria-busy'), 'true', 'Fast response hid feedback too soon');
      const after = await refresh.boundingBox();
      assert.equal(before.width, after.width, 'Feedback changed button width');
      assert.equal(before.height, after.height, 'Feedback changed button height');
      const count = logRequests;
      await refresh.evaluate(button => button.click());
      assert.equal(logRequests, count, 'Busy refresh sent a duplicate request');
      await page.waitForFunction(() => !document.querySelector('.sticky-actions button:last-child').hasAttribute('aria-busy'));
      assert(Date.now() - started >= 600, 'Feedback did not remain perceptible');
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, 'Action buttons overflow the page');

      await page.goto(`${base}/console.html#applications`);
      await page.getByRole('button', { name: 'More', exact: true }).click();
      const reload = page.getByRole('menuitem', { name: 'Reload', exact: true });
      await reload.click();
      assert.equal(await reload.getAttribute('aria-busy'), 'true', 'Async menu action has no busy feedback');
      await reload.waitFor({ state: 'hidden' });

      await page.goto(`${base}/console.html#page/console/local`);
      await page.locator('#console-unlock input').fill('123456');
      const unlock = page.getByRole('button', { name: 'Unlock for 15 minutes' });
      await unlock.click();
      const extend = page.getByRole('button', { name: 'Extend access' });
      await extend.waitFor();
      assert.equal(await extend.getAttribute('aria-busy'), 'true', 'Native form submission has no busy feedback');
      assert.equal(await page.locator('#console-unlock input').inputValue(), '', 'Async form no longer clears the code');
      await page.locator('#console-unlock').evaluate(form => form.requestSubmit());
      assert.equal(unlockRequests, 1, 'Form submitted the same action twice');
      assert.deepEqual(errors, []);
      await page.close();
    }
    console.log('PASS: refresh feedback persists for fast responses; busy actions do not repeat; native submits still work; no layout shift or mobile overflow');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
