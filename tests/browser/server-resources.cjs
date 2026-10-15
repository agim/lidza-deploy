const { chromium } = require('playwright');
const assert = require('node:assert/strict');

(async () => {
  const browser = await chromium.launch({ executablePath: process.env.TEST_CHROMIUM_PATH || '/usr/bin/chromium', args: ['--no-sandbox'] });
  try {
    for (const width of [360, 390, 1440]) {
      const page = await browser.newPage({ viewport: { width, height: 900 } });
      const errors = [];
      let offline = false;
      page.on('pageerror', error => errors.push(error.message));
      await page.route('**/api/**', async route => {
        const path = new URL(route.request().url()).pathname;
        if (path.endsWith('/control/server-health')) return route.fulfill({ json: {
          servers: offline ? [] : [{
            server_id: 'local', checked: '2026-10-15T13:00:00Z', cpus: 4, cpu_percent: 16.3, load1: .11,
            memory_total: 8 * 1024 ** 3, memory_available: 6 * 1024 ** 3,
            swap_total: 4 * 1024 ** 3, swap_free: 3 * 1024 ** 3,
            disks: [{ path: '/var/lib/docker', total: 100 * 1024 ** 3, available: 30 * 1024 ** 3, used_percent: 70 }],
            database_bytes: { 'portal-with-a-long-application-name': 20 * 1024 ** 2 },
          }], unavailable_servers: offline ? ['This server'] : [],
        } });
        if (path.endsWith('/control/status')) return route.fulfill({ json: { roles: ['admin'] } });
        if (path.endsWith('/control/apps')) return route.fulfill({ json: [] });
        if (path.endsWith('/control/servers')) return route.fulfill({ json: [{ id: 'local', name: 'This server', url: 'http://127.0.0.1:9090' }] });
        if (path.endsWith('/control/deployments')) return route.fulfill({ json: { deployments: [], unavailable_servers: [] } });
        return route.fulfill({ json: {} });
      });
      await page.goto(`${process.env.TEST_WEB_URL || 'http://127.0.0.1:8880'}/console.html#servers`);
      await page.getByText('16.3%', { exact: true }).waitFor();
      const resources = page.getByRole('region', { name: 'Server resources' });
      await resources.getByText('2.0 GiB used', { exact: true }).waitFor();
      await resources.getByText('70.0% used', { exact: true }).waitFor();
      await page.locator('.host-databases summary').click();
      await page.getByText('20.0 MiB', { exact: true }).waitFor();
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, `Host resources overflow at ${width}px`);
      assert(await resources.isVisible(), 'Resources are hidden on mobile');
      if (width === 390) { await page.evaluate(() => window.scrollTo(0, 0)); await page.screenshot({ path: '/tmp/lidza-server-resources-mobile.png', fullPage: true }); }
      offline = true;
      await page.getByRole('button', { name: 'Refresh resources' }).click();
      await page.getByText(/These measurements are stale/).waitFor();
      assert(await page.getByText('16.3%', { exact: true }).isVisible(), 'Last CPU sample was lost during an outage');
      assert.equal(await page.locator('.host-databases').evaluate(element => element.open), true, 'Refresh replaced the expanded details');
      assert.deepEqual(errors, []);
      await page.close();
    }
    console.log('PASS: host metrics visible at 360/390/1440px; no horizontal overflow; offline refresh preserves samples and expanded details');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
