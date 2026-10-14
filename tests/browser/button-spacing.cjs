const { chromium } = require('playwright');
const assert = require('node:assert/strict');

(async () => {
  const browser = await chromium.launch({ executablePath: process.env.TEST_CHROMIUM_PATH || '/usr/bin/chromium', args: ['--no-sandbox'] });
  try {
    for (const width of [390, 1440]) {
      const page = await browser.newPage({ viewport: { width, height: 900 } });
      const errors = [];
      page.on('pageerror', error => errors.push(error.message));
      await page.route('**/api/**', async route => {
        const path = new URL(route.request().url()).pathname;
        if (path.endsWith('/v1/auth/step-up')) return route.fulfill({ json: { expiresAt: new Date(Date.now() + 900000).toISOString() } });
        if (path.endsWith('/control/status')) return route.fulfill({ json: { roles: ['admin'] } });
        if (path.endsWith('/control/apps')) return route.fulfill({ json: [] });
        if (path.endsWith('/control/servers/local/console')) return route.fulfill({ json: { two_factor_enabled: true, unlocked: false } });
        if (path.endsWith('/control/servers')) return route.fulfill({ json: [{ id: 'local', name: 'Production server', url: 'https://agent.example.com' }] });
        if (path.endsWith('/control/deployments')) return route.fulfill({ json: { deployments: [], unavailable_servers: [] } });
        return route.fulfill({ json: [] });
      });

      const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8880';
      await page.goto(`${base}/console.html#page/console/local`);
      const form = page.locator('#console-unlock');
      await form.waitFor();
      const input = form.locator('input');
      const button = form.locator('button');
      const inputBox = await input.boundingBox();
      const buttonBox = await button.boundingBox();
      assert(inputBox && buttonBox);
      assert(buttonBox.y - (inputBox.y + inputBox.height) >= 15, `Security unlock controls are touching at ${width}px`);

      await input.fill('123456');
      await button.click();
      await page.getByRole('button', { name: 'Extend access' }).waitFor();
      assert.equal(await input.inputValue(), '', 'Unlock code was not cleared after submit');
      assert.equal(await page.locator('.alert-danger').count(), 0, 'Unlock displayed an error after succeeding');
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, 'Security unlock overflows the viewport');
      assert.deepEqual(errors, []);
      await page.close();
    }
    console.log('PASS: security unlock fields and buttons are separated, successful submit clears the code, and async reset does not throw');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
