const { chromium } = require('playwright');
const assert = require('node:assert/strict');

(async () => {
  const browser = await chromium.launch({ executablePath: process.env.TEST_CHROMIUM_PATH || '/usr/bin/chromium', args: ['--no-sandbox'] });
  try {
    for (const width of [390, 1440]) {
      const page = await browser.newPage({ viewport: { width, height: 900 } });
      const errors = [], calls = [];
      page.on('pageerror', error => errors.push(error.message));
      const data = {
        control_version: 'v0.2.67',
        settings: { automatic: false, latest: 'v0.2.69', checked: '2026-10-13T20:00:00Z', targets: [] },
        servers: [{ server_id: 'local', name: 'This server', current: 'v0.2.67', supported: true, state: 'succeeded' }],
      };
      await page.route('**/api/**', async route => {
        const request = route.request(), path = new URL(request.url()).pathname, method = request.method();
        if (path.endsWith('/control/status')) return route.fulfill({ json: { roles: ['admin'] } });
        if (path.endsWith('/control/apps')) return route.fulfill({ json: [] });
        if (path.endsWith('/control/servers')) return route.fulfill({ json: [{ id: 'local', name: 'This server', url: 'http://127.0.0.1:9090' }] });
        if (path.endsWith('/control/deployments')) return route.fulfill({ json: { deployments: [], unavailable_servers: [] } });
        if (path.endsWith('/control/servers/local/upgrade')) {
          calls.push({ path, method });
          return route.fulfill({ status: 502, json: { error: 'agent returned 409: wait for deployments and restores before upgrading' } });
        }
        if (path.endsWith('/control/updates/install')) {
          calls.push({ path, method });
          data.settings.targets = [{ server_id: 'local', name: 'This server', state: 'waiting', message: 'Server busy; waiting for active operations' }];
          return route.fulfill({ status: 202, json: data.settings });
        }
        if (path.endsWith('/control/updates/check')) {
          calls.push({ path, method });
          return route.fulfill({ json: data.settings });
        }
        if (path.endsWith('/control/updates') && method === 'PATCH') {
          calls.push({ path, method, body: request.postDataJSON() });
          data.settings.automatic = request.postDataJSON().automatic;
          return route.fulfill({ json: data.settings });
        }
        if (path.endsWith('/control/updates')) return route.fulfill({ json: data });
        return route.fulfill({ json: [] });
      });

      const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8880';
      await page.goto(`${base}/console.html#updates`);
      const install = page.getByRole('button', { name: 'Install update' });
      await install.waitFor();
      assert(await page.getByText('Installed v0.2.67 · Latest stable v0.2.69', { exact: true }).isVisible());
      await install.click();
      await page.getByText('Server busy; waiting for active operations', { exact: false }).waitFor();
      assert(calls.some(call => call.path.endsWith('/updates/install') && call.method === 'POST'), 'Durable update installer was not called');
      assert(!calls.some(call => call.path.endsWith('/servers/local/upgrade')), 'UI called the immediate agent upgrade endpoint');
      assert.equal(await page.locator('.alert-danger').count(), 0, 'Queued busy host displayed as an error');
      assert(await page.getByRole('button', { name: 'Update queued' }).isDisabled());

      data.settings.targets = [];
      await page.getByRole('button', { name: 'Check now' }).click();
      await page.getByText('This server', { exact: true }).waitFor();
      assert(calls.some(call => call.path.endsWith('/updates/check')), 'Release check was not sent');
      const automatic = page.getByRole('checkbox', { name: 'Automatically install stable releases' });
      await page.waitForFunction(() => !document.querySelector('input[type="checkbox"]')?.disabled);
      await automatic.check();
      assert(calls.some(call => call.method === 'PATCH' && call.body.automatic === true), 'Automatic update policy was not saved');
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, `Updates page overflows at ${width}px`);
      assert.deepEqual(errors, []);
      await page.close();
    }
    console.log('PASS: safe durable update queue, busy-host deferral, release check, automatic policy and responsive layout');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
