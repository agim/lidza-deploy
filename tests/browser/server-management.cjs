const { chromium } = require('playwright');
const assert = require('node:assert/strict');

const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8880';

(async () => {
  const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_BIN || '/usr/bin/chromium', args: ['--no-sandbox'] });
  try {
    for (const width of [390, 1440]) {
      const page = await browser.newPage({ viewport: { width, height: 844 } });
      const errors = [];
      const calls = [];
      page.on('pageerror', error => errors.push(error.message));
      await page.route('**/api/control/**', async route => {
        const request = route.request();
        const path = new URL(request.url()).pathname;
        const method = request.method();
        calls.push({ path, method, body: request.postDataJSON() });
        if (path.endsWith('/servers/local/containers') && method === 'GET') {
          await route.fulfill({ json: {
            checked_at: '2026-10-14T12:00:00Z',
            containers: [
              { id: 'web123', name: 'lidza-site-current', app_id: 'site', role: 'Web · current release', state: 'running', status: 'Up 2 hours (healthy)', health: 'healthy', cpu: '1.25%', memory: '80MiB / 512MiB', memory_percent: '15.63%', network_io: '2MB / 4MB', block_io: '8MB / 1MB', pids: '11', restartable: true },
              { id: 'old123', name: 'lidza-site-previous', app_id: 'site', role: 'Web · rollback release', state: 'exited', status: 'Exited (0) 1 hour ago', restartable: false },
            ],
          } });
          return;
        }
        if (path.endsWith('/servers/local/containers/restart') && method === 'POST') {
          await route.fulfill({ json: { status: 'restarted' } });
          return;
        }
        if (path.endsWith('/status')) return route.fulfill({ json: { roles: ['admin'] } });
        if (path.endsWith('/servers')) return route.fulfill({ json: [{ id: 'local', name: 'This server', url: 'http://127.0.0.1:9090' }] });
        if (path.endsWith('/deployments')) return route.fulfill({ json: { deployments: [], unavailable_servers: [] } });
        await route.fulfill({ json: [] });
      });

      await page.goto(`${base}/console.html#page/containers/local`);
      await page.getByRole('heading', { name: 'Containers & resource usage' }).waitFor();
      const current = page.getByRole('row').filter({ hasText: 'lidza-site-current' });
      assert.match(await current.textContent(), /site · Web · current release/);
      assert.match(await current.textContent(), /1\.25%/);
      assert.match(await current.textContent(), /80MiB \/ 512MiB/);
      assert.doesNotMatch(await current.textContent(), /0\.0%|0 B \/ 0 B/);
      assert.equal(await current.locator('.progress span').nth(0).getAttribute('style'), 'width: 1.25%;');
      assert.equal(await current.locator('.progress span').nth(1).getAttribute('style'), 'width: 15.63%;');

      const previous = page.getByRole('row').filter({ hasText: 'lidza-site-previous' });
      assert.match(await previous.textContent(), /Protected/);
      assert.equal(await previous.getByRole('button', { name: /Restart/ }).count(), 0);

      await current.getByRole('button', { name: 'Restart' }).click();
      await page.getByRole('button', { name: 'Restart container' }).click();
      await page.getByText('lidza-site-current restart requested.').waitFor();
      assert(calls.some(call => call.method === 'POST' && call.path.endsWith('/containers/restart') && call.body.name === 'lidza-site-current'));
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth), true);
      assert.deepEqual(errors, []);
      await page.close();
    }
    console.log('Container CPU and memory values, progress bars, guarded restarts and responsive layout passed.');
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exit(1); });
