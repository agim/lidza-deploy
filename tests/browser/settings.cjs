// Settings and navigation: phone navigation and the sidebar's identity, status and account menu, the GitHub connection, application defaults,
// updates, team, errors and audit, the account's two-factor setup and the
// help gallery.
const { suite, assert, callsTo } = require('./fixtures/harness.cjs');

suite('settings', [
  {
    name: 'phone navigation drawer opens views and closes',
    widths: [360, 390],
    context: { isMobile: true, hasTouch: true },
    height: 844,
    async run({ page, open }) {
      await open('/console/applications');
      const trigger = page.getByRole('button', { name: 'Open menu' });
      const drawer = page.getByRole('dialog', { name: 'Navigation menu' });
      for (const [label, path] of [['Servers', '/console/servers'], ['Databases', '/console/databases'], [/^Applications/, '/console/applications']]) {
        await trigger.click();
        const box = await drawer.boundingBox();
        assert(box.width <= page.viewportSize().width - 48, `the drawer covers the whole page (${box.width}px)`);
        await drawer.getByRole('link', typeof label === 'string' ? { name: label, exact: true } : { name: label }).click();
        await drawer.waitFor({ state: 'hidden' });
        await page.waitForURL(url => url.pathname === path);
        assert.equal(await page.evaluate(() => scrollX), 0, 'the drawer pushed the page sideways');
      }
      // Touch targets in the page and top bar are at least 40px tall.
      const small = await page.evaluate(() => [...document.querySelectorAll('#main-content button, #main-content a.button, header button')]
        .filter(element => element.getClientRects().length && !element.matches('[role=switch]'))
        .map(element => ({ label: element.getAttribute('aria-label') || element.textContent.trim().slice(0, 30), height: Math.round(element.getBoundingClientRect().height) }))
        .filter(item => item.height < 40));
      assert.deepEqual(small, [], 'touch targets under 40px');
    },
  },
  {
    name: 'sidebar: unreachable servers are named, and the account menu signs out',
    widths: [1440],
    async run({ page, open }) {
      await page.route('**/api/control/servers', route => route.fulfill({ json: [{ id: 'local', name: 'Production', url: 'http://127.0.0.1:9090' }, { id: 'edge', name: 'Edge', url: 'http://127.0.0.1:9091' }] }));
      await page.route('**/api/control/deployments', route => route.fulfill({ json: { deployments: [], unavailable_servers: ['edge'] } }));
      await open('/console/applications');
      const sidebar = page.locator('.desktop-sidebar');
      const status = sidebar.getByRole('link', { name: '1 server unreachable' });
      await status.waitFor();
      assert.equal(await status.getAttribute('title'), 'Unreachable: Edge');
      await status.click();
      await page.waitForURL(url => url.pathname === '/console/servers');
      let signedOut = false;
      await page.route('**/api/v1/auth/logout', route => { signedOut = route.request().method() === 'POST'; return route.fulfill({ json: {} }); });
      await sidebar.getByRole('button', { name: 'Account menu for Ada Lovelace' }).click();
      await page.getByRole('menuitem', { name: 'Sign out' }).click();
      await page.waitForURL(url => url.pathname === '/login.html');
      assert.equal(signedOut, true, 'Sign out did not end the session');
    },
  },
  {
    name: 'GitHub connect posts the app manifest to GitHub',
    async run({ page, open }) {
      await page.route('**/api/control/status', route => route.fulfill({ json: { roles: ['admin'], github_app_connected: false, github_app_configured: false, github_public_https: true } }));
      await page.route('**/api/control/github/app/register', route => route.fulfill({ json: { action: 'https://github.example.com/settings/apps/new?state=fixture-state', manifest: { name: 'Fixture Deploy', url: 'https://deploy.example.com', hook_attributes: { url: 'https://deploy.example.com/api/github/webhook' } } } }));
      let posted;
      await page.route('https://github.example.com/**', route => { posted = { method: route.request().method(), body: route.request().postData() }; return route.fulfill({ contentType: 'text/html', body: '<!doctype html><title>GitHub fixture</title><h1>GitHub fixture</h1>' }); });
      await open('/console/integrations');
      await page.getByRole('heading', { name: 'Connect your GitHub App' }).waitFor();
      assert.equal(await page.locator('[name=github_id], [name=github_secret], [name=client_secret]').count(), 0, 'the page asks for OAuth credentials');
      await page.getByRole('button', { name: 'Connect GitHub' }).click();
      await page.waitForURL(url => url.hostname === 'github.example.com');
      assert.equal(posted.method, 'POST');
      const manifest = JSON.parse(new URLSearchParams(posted.body).get('manifest'));
      assert.equal(manifest.hook_attributes.url, 'https://deploy.example.com/api/github/webhook');
    },
    overflow: false,
  },
  {
    name: 'application defaults save',
    async run({ page, state, open }) {
      await open('/console/defaults');
      await page.getByRole('textbox', { name: 'LOG_LEVEL value' }).fill('debug');
      await page.getByRole('button', { name: 'Add variable' }).click();
      await page.getByRole('textbox', { name: 'VARIABLE value' }).fill('shared@example.com');
      await page.getByRole('button', { name: /^Save/ }).first().click();
      await page.getByText('Application defaults saved.', { exact: false }).first().waitFor();
      assert.deepEqual(callsTo(state, 'control/application-defaults', 'PUT')[0].body, { values: { LOG_LEVEL: 'debug', VARIABLE: 'shared@example.com' } });
    },
  },
  {
    name: 'updates queue durably; a busy host waits without an error',
    async run({ page, open }) {
      const calls = [];
      const data = { control_version: 'v0.2.83', automatic_updates_supported: true, settings: { automatic: false, latest: 'v0.2.84', checked: '2026-10-13T20:00:00Z', targets: [] }, servers: [{ server_id: 'local', name: 'Production', current: 'v0.2.83', supported: true, state: 'succeeded' }] };
      await page.route('**/api/control/updates**', route => {
        const request = route.request(), path = new URL(request.url()).pathname;
        calls.push({ path, method: request.method(), body: request.postData() ? request.postDataJSON() : undefined });
        if (path.endsWith('/updates/install')) { data.settings.targets = [{ server_id: 'local', name: 'Production', state: 'waiting', message: 'Server busy; waiting for active operations' }]; return route.fulfill({ status: 202, json: data.settings }); }
        if (path.endsWith('/updates/check')) return route.fulfill({ json: data.settings });
        if (request.method() === 'PATCH') { data.settings.automatic = request.postDataJSON().automatic; return route.fulfill({ json: data.settings }); }
        return route.fulfill({ json: data });
      });
      await page.route('**/api/control/servers/local/upgrade', route => { calls.push({ path: 'immediate upgrade' }); return route.fulfill({ status: 502, json: { error: 'agent returned 409' } }); });
      await open('/console/updates');
      await page.getByText('Installed v0.2.83 · Latest stable v0.2.84', { exact: true }).waitFor();
      await page.getByRole('button', { name: 'Install update' }).click();
      await page.getByText('Server busy; waiting for active operations', { exact: false }).first().waitFor();
      assert(calls.some(call => call.path.endsWith('/updates/install') && call.method === 'POST'), 'the durable installer was not called');
      assert(!calls.some(call => call.path === 'immediate upgrade'), 'the page called the immediate agent upgrade');
      assert.equal(await page.locator('#main-content .alert-danger').count(), 0, 'a queued busy host is shown as an error');
      assert.equal(await page.getByRole('button', { name: 'Update queued' }).isDisabled(), true);
      data.settings.targets = [];
      await page.getByRole('button', { name: 'Check now' }).click();
      await page.getByText('Release check complete.').first().waitFor();
      await page.getByRole('checkbox', { name: /^Automatically install stable releases/ }).check();
      await page.getByText('Automatic stable updates enabled.').first().waitFor();
      assert(calls.some(call => call.method === 'PATCH' && call.body.automatic === true), 'the automatic policy was not saved');
    },
  },
  {
    name: 'team: adding a member sends the chosen role',
    async run({ page, state, open }) {
      await open('/console/team/new');
      await page.getByRole('textbox', { name: 'Email' }).fill('dev@example.com');
      await page.getByRole('textbox', { name: 'Name' }).fill('Developer');
      await page.getByRole('combobox', { name: /^Role/ }).selectOption({ label: 'Deployer' });
      await page.getByRole('button', { name: 'Save member' }).click();
      await page.waitForURL(url => url.pathname.startsWith('/console/team'));
      const created = callsTo(state, 'control/team', 'POST')[0]?.body;
      assert.equal(created?.email, 'dev@example.com');
      assert.equal(created?.role, 'deployer');
    },
  },
  {
    name: 'errors open a group and copy its details; the audit log copies',
    async run({ page, open }) {
      await open('/console/errors?app=portal');
      await page.getByRole('link', { name: 'Fixture request rejected' }).click();
      await page.waitForURL(url => url.pathname.startsWith('/console/errors/') && url.searchParams.get('app') === 'portal');
      await page.getByRole('button', { name: 'Copy stack trace' }).click();
      assert.equal(await page.evaluate(() => window.consoleCopied), 'Error: fixture diagnostic\n  at handler.go:42');
      await page.getByRole('button', { name: 'Copy event details' }).click();
      const event = JSON.parse(await page.evaluate(() => window.consoleCopied));
      assert.equal(event.container, 'lidza-portal');
      assert.equal(event.recent_occurrences[0].request_id, 'request-fixture');
      await open('/console/audit');
      await page.getByRole('button', { name: 'Copy audit log' }).click();
      assert.match(await page.evaluate(() => window.consoleCopied), /ops@example\.com[\s\S]*server\.console\.unlock[\s\S]*local/);
    },
  },
  {
    name: 'account: two-factor setup shows a QR code',
    async run({ page, open }) {
      await open('/console/account');
      await page.getByRole('button', { name: 'Set up authenticator' }).click();
      const qr = page.getByRole('img', { name: 'QR code for Līdza Deploy two-factor setup' });
      await qr.waitFor();
      assert.equal(await qr.evaluate(image => image.complete && image.naturalWidth > 0), true, 'the QR code did not load');
    },
  },
  {
    name: 'help: example dialogs open, close with Escape and act',
    async run({ page, open }) {
      await open('/console/help/components');
      for (const [button, dialog] of [['Open example dialog', 'A focused conversation'], ['Open example panel', 'Room for the details']]) {
        await page.getByRole('button', { name: button }).click();
        await page.getByRole('dialog', { name: dialog }).waitFor();
        await page.keyboard.press('Escape');
        await page.getByRole('dialog', { name: dialog }).waitFor({ state: 'hidden' });
      }
      await page.getByRole('button', { name: 'Primary action' }).click();
      await page.getByText('Example action completed.').first().waitFor();
    },
  },
]).catch(error => { console.error(error); process.exit(1); });
