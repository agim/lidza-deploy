// Servers: the index with live resources, connecting a server, the detail
// header menu, and the Agent (connection, upgrade, system log access),
// Processes, Disk cleanup and Console tabs. The
// console checks cover just-in-time unlock, running and copying a command,
// cancelling (button, Ctrl+C, expired verification), a restored job while
// locked, and reconnecting to a job whose acknowledgement was lost.
const { suite, assert, callsTo } = require('./fixtures/harness.cjs');

const command = page => page.locator('#console-command textarea');
const output = page => page.locator('.console-output');
const statusText = (page, text) => page.getByRole('status').filter({ hasText: text });
const runEnabled = page => page.waitForFunction(() => !document.querySelector('#console-command button[type=submit]').disabled);

suite('servers', [
  {
    name: 'index shows live resources and links each server',
    async run({ page, open }) {
      await open('/console/servers');
      const row = page.locator('#main-content table tbody tr').filter({ hasText: 'Production' });
      await row.getByText('12.5%', { exact: true }).waitFor();
      await row.getByText('3.0 GiB of 8.0 GiB', { exact: true }).waitFor();
      await row.getByText('70.0% used', { exact: true }).waitFor();
      await row.getByRole('link', { name: 'Production' }).click();
      await page.waitForURL(url => url.pathname === '/console/servers/local');
      await page.getByRole('button', { name: 'Actions' }).click();
      await page.getByRole('menuitem', { name: 'Server console' }).click();
      await page.waitForURL(url => url.pathname === '/console/servers/local/console');
      await page.getByRole('tab', { name: 'Console', selected: true }).waitFor();
    },
  },
  {
    name: 'connecting a server opens it',
    async run({ page, state, open }) {
      await open('/console/servers/new');
      // A server that already runs an agent is added with its connection details.
      await page.getByRole('combobox', { name: 'How to connect' }).selectOption('existing');
      await page.getByRole('textbox', { name: /^Server ID/ }).fill('edge');
      await page.getByRole('textbox', { name: 'Display name' }).fill('Edge');
      await page.getByRole('textbox', { name: 'Agent URL' }).fill('https://agent.example.com');
      await page.getByRole('textbox', { name: /^Agent token/ }).fill('t'.repeat(40));
      await page.getByRole('button', { name: 'Save connection' }).click();
      await page.waitForURL(url => url.pathname === '/console/servers/edge');
      assert.deepEqual(callsTo(state, 'control/servers', 'POST')[0].body, { id: 'edge', name: 'Edge', url: 'https://agent.example.com', token: 't'.repeat(40) });
    },
  },
  {
    name: 'connect: the panel installs a new server over SSH, or gives one command',
    async run({ page, state, open }) {
      await open('/console/servers/new');
      // Installing over SSH is the default; the login is sent once with the request.
      await page.getByRole('textbox', { name: /^Server ID/ }).fill('edge');
      await page.getByRole('textbox', { name: 'Display name' }).fill('Edge');
      await page.getByRole('textbox', { name: /^Hostname/ }).fill('edge.example.com');
      await page.getByRole('textbox', { name: 'SSH password' }).fill('one-time-pass');
      await page.getByRole('button', { name: 'Install and connect' }).click();
      await page.getByRole('region', { name: 'Installer output' }).waitFor();
      assert.deepEqual(callsTo(state, 'control/server-enrollments', 'POST')[0].body, { id: 'edge', name: 'Edge', hostname: 'edge.example.com', email: '', ssh: { user: 'root', port: 22, address: '', host_key: '', password: 'one-time-pass' } });
      assert.equal(await page.getByRole('textbox', { name: 'SSH password' }).count(), 0, 'the password field stayed on screen after the request');
      // The page follows the installation and opens the server once it is connected.
      await page.waitForURL(url => url.pathname === '/console/servers/edge');
      // Without SSH access the panel gives one command to run on the server.
      await open('/console/servers/new');
      await page.getByRole('combobox', { name: 'How to connect' }).selectOption('command');
      assert.equal(await page.getByRole('textbox', { name: 'SSH password' }).count(), 0);
      await page.getByRole('textbox', { name: /^Server ID/ }).fill('edge2');
      await page.getByRole('textbox', { name: 'Display name' }).fill('Edge 2');
      await page.getByRole('textbox', { name: /^Hostname/ }).fill('edge2.example.com');
      await page.getByRole('button', { name: 'Create install command' }).click();
      assert.match(await page.getByRole('textbox', { name: 'Install command' }).inputValue(), /^curl -fsSL https:\/\/deploy\.example\.com\/enroll\/[a-f0-9]+\/install\.sh \| sudo sh$/);
      assert.equal(callsTo(state, 'control/server-enrollments', 'POST')[1].body.ssh, undefined);
    },
  },
  {
    name: 'agent tab: native applications are enabled after confirmation',
    async run({ page, state, open }) {
      await open('/console/servers/local/agent');
      await page.getByRole('button', { name: 'Enable native applications' }).click();
      assert.equal(callsTo(state, 'control/servers/local/host/native', 'POST').length, 0, 'enabled before the agent restart was confirmed');
      await page.getByRole('alertdialog').getByRole('button', { name: 'Enable' }).click();
      // The card follows the agent's restart and then shows the account and root.
      await page.locator('[data-native-apps="enabled"]').getByText('/home/deploy').waitFor();
      assert.equal(callsTo(state, 'control/servers/local/host/native', 'POST').length, 1);
    },
  },
  {
    name: 'agent tab: connection keeps the saved token; upgrade queues',
    async run({ page, state, open }) {
      await open('/console/servers/local/agent');
      const token = page.getByRole('textbox', { name: /^Agent token/ });
      assert.equal(await token.inputValue(), '', 'the saved token was sent to the browser');
      await page.getByRole('textbox', { name: 'Display name' }).fill('Production EU');
      await page.getByRole('button', { name: 'Save connection' }).click();
      await page.getByText('Server connection updated.').first().waitFor();
      const saved = callsTo(state, 'control/servers/local', 'PUT')[0].body;
      assert.equal(saved.name, 'Production EU');
      assert.equal(saved.url, 'http://127.0.0.1:9090');
      assert(!saved.token, 'an empty token field replaced the saved credential');
      await page.getByRole('button', { name: 'Upgrade agent' }).click();
      await page.getByText('Agent upgrade to v0.2.84 queued.').first().waitFor();
      assert.deepEqual(callsTo(state, 'control/servers/local/upgrade', 'POST')[0].body, { version: 'v0.2.84' });
    },
  },
  {
    name: 'agent tab: system log access switch sits with service control and shows membership',
    async run({ page, state, open }) {
      await open('/console/servers/local/agent');
      const card = page.locator('[data-server-log-access="local"]');
      const toggle = card.getByRole('switch', { name: 'System log access for the deployment account' });
      await toggle.waitFor();
      await page.locator('[data-server-service-control="local"]').waitFor();
      const order = await page.evaluate(() => {
        const control = document.querySelector('[data-server-service-control="local"]');
        const access = document.querySelector('[data-server-log-access="local"]');
        return Boolean(control.compareDocumentPosition(access) & Node.DOCUMENT_POSITION_FOLLOWING);
      });
      assert(order, 'system log access is not placed after service control');
      assert.equal(await toggle.getAttribute('aria-checked'), 'false');
      await card.getByText('Not in systemd-journal').waitFor();
      await card.getByText(/journalctl -u <unit>/).first().waitFor();
      await card.getByText(/may contain secrets/).waitFor();
      await toggle.click();
      await card.getByText('In systemd-journal', { exact: true }).waitFor();
      assert.equal(await toggle.getAttribute('aria-checked'), 'true');
      assert.deepEqual(callsTo(state, 'control/servers/local/host/log-access', 'PUT')[0].body, { enabled: true });
    },
  },
  {
    name: 'processes tab: restart needs confirmation',
    async run({ page, state, open }) {
      await open('/console/servers/local/processes');
      const row = page.getByRole('row').filter({ hasText: 'lidza-portal-' });
      await row.getByText('12.5%').first().waitFor();
      await row.getByRole('button', { name: 'Restart' }).click();
      const dialog = page.getByRole('alertdialog');
      assert.equal(callsTo(state, 'control/servers/local/containers/restart').length, 0, 'restarted before confirmation');
      await dialog.getByRole('button', { name: 'Restart container' }).click();
      await dialog.waitFor({ state: 'hidden' });
      await page.getByText(/restart requested\./).first().waitFor();
      assert.match(callsTo(state, 'control/servers/local/containers/restart', 'POST')[0].body.name, /^lidza-portal-/);
    },
  },
  {
    name: 'cleanup tab: schedule saves; a run is confirmed and reported',
    widths: [320, 1440],
    async run({ page, state, open }) {
      await open('/console/servers/local/cleanup');
      const backups = page.getByRole('checkbox', { name: /^Older local backups/ });
      await backups.uncheck();
      assert.equal(await backups.isChecked(), false);
      await backups.check();
      await page.getByRole('combobox', { name: 'Run every' }).selectOption({ label: 'Six hours' });
      await page.getByRole('spinbutton', { name: 'Minimum age (days)' }).fill('14');
      await page.getByRole('button', { name: 'Save cleanup schedule' }).click();
      await page.getByText('Server cleanup schedule saved.').first().waitFor();
      assert.deepEqual(callsTo(state, 'control/servers/local/maintenance', 'PATCH')[0].body, { enabled: true, interval_hours: 6, build_cache: true, logs: true, older_than_days: 14 });
      await page.getByRole('button', { name: 'Clean selected now' }).click();
      const dialog = page.getByRole('alertdialog', { name: 'Clean selected server data?' });
      await dialog.waitFor();
      assert.equal(callsTo(state, 'control/servers/local/maintenance/run').length, 0, 'cleanup ran before confirmation');
      await dialog.getByRole('button', { name: 'Start cleanup' }).click();
      await dialog.waitFor({ state: 'hidden' });
      await page.getByText('Total reclaimed space: 2 GB', { exact: false }).first().waitFor({ timeout: 15000 });
      assert.deepEqual(callsTo(state, 'control/servers/local/maintenance/run', 'POST')[0].body, { backups: true, build_cache: true, logs: true });
      await page.getByRole('button', { name: 'Copy cleanup report' }).click();
      assert.match(await page.evaluate(() => window.consoleCopied), /Removed 2 older local backups/);
    },
  },
  {
    name: 'console: unlock, run and copy a command',
    async run({ page, state, open }) {
      await open('/console/servers/local/console');
      assert.equal(await page.getByRole('button', { name: 'Run command' }).isDisabled(), true, 'a locked console runs commands');
      const code = page.getByRole('textbox', { name: 'Authenticator or recovery code' });
      await code.fill('654321');
      await page.getByRole('button', { name: 'Unlock for 15 minutes' }).click();
      await page.getByRole('button', { name: 'Extend access' }).waitFor();
      assert.equal(await code.inputValue(), '', 'the verification code stays in the field');
      assert(callsTo(state, 'v1/auth/step-up').some(call => call.body.purpose === 'server.console' && call.body.target === 'local'));
      await command(page).fill('docker ps');
      await page.getByRole('button', { name: 'Run command' }).click();
      assert.equal(await command(page).inputValue(), '', 'the command stays in the field after it started');
      await output(page).getByText('rows: 2', { exact: false }).waitFor();
      await page.getByRole('button', { name: 'Copy output' }).click();
      assert.match(await page.evaluate(() => window.consoleCopied), /rows: 2[\s\S]*exit 0/);
    },
  },
  {
    name: 'console: cancel by button and Ctrl+C, renewing verification',
    height: 950,
    async run({ page, state, open }) {
      state.consoleGranted = true;
      let id = '', stopped = false, starts = 0, cancels = 0, deny = false, holdStop = false;
      await page.route('**/api/control/servers/local/console/jobs**', async route => {
        const request = route.request(), url = new URL(request.url());
        if (url.pathname.endsWith('/cancel')) {
          cancels++;
          if (deny) { deny = false; return route.fulfill({ status: 403, json: { error: 'Verify again.' } }); }
          stopped = true;
          return route.fulfill({ status: 202, json: { status: 'cancellation requested' } });
        }
        if (request.method() === 'POST') { starts++; id = request.postDataJSON().id; stopped = false; return route.fulfill({ status: 202, json: { id, state: 'running' } }); }
        const offset = Number(url.searchParams.get('offset'));
        return route.fulfill({ json: { id, state: stopped && !holdStop ? 'cancelled' : 'running', output: offset ? '' : 'partial output', next_offset: 14, output_size: 14, exit_code: stopped ? 130 : 0, duration_seconds: 1, truncated: false } });
      });
      await open('/console/servers/local/console');
      await command(page).fill('sleep 30');
      await page.getByRole('button', { name: 'Run command', exact: true }).click();
      await page.waitForFunction(() => document.querySelector('.console-output')?.textContent === 'partial output');
      // Ctrl+C with selected output or a selected draft copies; otherwise it cancels.
      await output(page).evaluate(element => { const range = document.createRange(); range.selectNodeContents(element); getSelection().removeAllRanges(); getSelection().addRange(range); });
      await page.keyboard.press('Control+c');
      assert.equal(cancels, 0, 'Ctrl+C on selected output cancelled the command');
      await page.evaluate(() => getSelection().removeAllRanges());
      await command(page).fill('next command');
      await command(page).selectText();
      await page.keyboard.press('Control+c');
      assert.equal(cancels, 0, 'Ctrl+C on a selected draft cancelled the command');
      await command(page).press('ArrowRight');
      await page.keyboard.press('Control+c');
      await statusText(page, 'Command cancelled.').waitFor();
      assert.equal(cancels, 1);
      assert.match(await output(page).innerText(), /partial output[\s\S]*exit 130/);
      assert.equal(await command(page).inputValue(), 'next command', 'cancelling erased the next draft');
      await runEnabled(page);
      // Expired verification pauses the cancel until it is renewed.
      await page.getByRole('button', { name: 'Run command', exact: true }).click();
      deny = true;
      await page.getByRole('button', { name: 'Cancel command (Ctrl+C)', exact: true }).click();
      const verify = page.getByRole('alertdialog', { name: 'Unlock to cancel command' });
      await verify.getByLabel('Authenticator or recovery code').fill('123456');
      await verify.getByRole('button', { name: 'Verify & cancel command', exact: true }).click();
      await statusText(page, 'Command cancelled.').waitFor();
      assert.equal(starts, 2);
      assert.equal(cancels, 3);
      await runEnabled(page);
      // A slow stop leaves a retry that stays usable.
      await command(page).fill('slow to stop');
      await page.getByRole('button', { name: 'Run command', exact: true }).click();
      holdStop = true;
      await page.getByRole('button', { name: 'Cancel command (Ctrl+C)', exact: true }).click();
      const retry = page.getByRole('button', { name: 'Retry cancel (Ctrl+C)', exact: true });
      await retry.waitFor();
      assert.equal(await retry.isEnabled(), true, 'the acknowledgement left Cancel disabled');
      holdStop = false;
      await retry.click();
      await statusText(page, 'Command cancelled.').waitFor();
      assert.equal(starts, 3);
      assert.equal(cancels, 5);
    },
  },
  {
    name: 'console: a job restored while locked is cancelled only after verification',
    height: 950,
    async run({ page, state, open }) {
      await page.addInitScript(() => sessionStorage.setItem('lidza-console-job:local', 'saved-command-00001'));
      let reads = 0, cancels = 0, stopped = false, unverified = 0;
      await page.route('**/api/v1/auth/step-up', route => {
        const body = route.request().postDataJSON();
        if (body.purpose !== 'server.console' || body.target !== 'local' || body.code !== '654321') return route.fulfill({ status: 403, json: { error: 'Invalid verification code' } });
        state.consoleGranted = true;
        return route.fulfill({ json: { expiresAt: '2099-10-16T12:00:00Z' } });
      });
      await page.route('**/api/control/servers/local/console/jobs/**', route => {
        if (!state.consoleGranted) unverified++;
        if (route.request().method() === 'POST') { cancels++; stopped = true; return route.fulfill({ status: 202, json: { status: 'cancellation requested' } }); }
        reads++;
        return route.fulfill({ json: { id: 'saved-command-00001', state: stopped ? 'cancelled' : 'running', output: 'saved output', next_offset: 12, output_size: 12, exit_code: stopped ? 130 : 0, duration_seconds: 5 } });
      });
      await open('/console/servers/local/console');
      const unlock = page.getByRole('button', { name: 'Unlock to cancel', exact: true });
      await unlock.waitFor();
      assert.equal(await unlock.isEnabled(), true);
      await statusText(page, 'Saved command found').waitFor();
      assert.equal(await page.getByRole('button', { name: 'Command running', exact: true }).count(), 0, 'a locked console claims the job is running');
      assert.equal(reads + cancels, 0, 'the job was read before verification');
      await unlock.click();
      const modal = page.getByRole('alertdialog', { name: 'Unlock to cancel command' });
      await modal.getByRole('button', { name: 'Cancel', exact: true }).click();
      assert.equal(cancels, 0, 'dismissing the verification cancelled the command');
      await page.keyboard.press('Control+c');
      await modal.waitFor();
      await modal.getByLabel('Authenticator or recovery code').fill('bad-code');
      await modal.getByRole('button', { name: 'Verify & cancel command', exact: true }).click();
      await modal.getByText('Invalid verification code').waitFor();
      assert.equal(cancels, 0);
      await modal.getByLabel('Authenticator or recovery code').fill('654321');
      await modal.getByRole('button', { name: 'Verify & cancel command', exact: true }).click();
      await statusText(page, 'Command cancelled.').waitFor();
      assert.equal(cancels, 1);
      assert.match(await output(page).innerText(), /saved output[\s\S]*exit 130/);
      assert.equal(unverified, 0, 'the job was accessed without verification');
      // A failed access-status read still lets the operator cancel.
      state.consoleGranted = false; stopped = false;
      await page.route('**/api/control/servers/local/console', route => route.fulfill({ status: 503, json: { error: 'Console access temporarily unavailable' } }));
      await page.reload();
      await page.getByRole('button', { name: 'Unlock to cancel', exact: true }).click();
      await modal.getByLabel('Authenticator or recovery code').fill('654321');
      await modal.getByRole('button', { name: 'Verify & cancel command', exact: true }).click();
      await statusText(page, 'Command cancelled.').waitFor();
      assert.equal(cancels, 2);
      // The 503 above is expected; it is not a page error.
    },
  },
  {
    name: 'console: a lost acknowledgement reconnects without rerunning',
    async run({ page, state, open }) {
      state.consoleGranted = true;
      let starts = 0, id = '', dropped = false, expire = false, missing = false;
      await page.route('**/api/control/servers/local/console/jobs**', async route => {
        const request = route.request(), url = new URL(request.url());
        if (request.method() === 'POST') { starts++; id = request.postDataJSON().id; return route.fulfill({ status: 502, body: 'Bad Gateway' }); }
        if (missing) return route.fulfill({ status: 404, json: { error: 'Result unavailable; check outcome before rerunning.' } });
        if (expire) return route.fulfill({ status: 403, json: { error: 'Verify again.' } });
        const offset = Number(url.searchParams.get('offset'));
        if (offset && !dropped) { dropped = true; return route.abort('failed'); }
        return route.fulfill({ json: { id, state: offset ? 'finished' : 'running', output: offset ? 'second' : 'first', next_offset: offset ? 11 : 5, output_size: 11, exit_code: 7, duration_seconds: 2, truncated: false } });
      });
      await open('/console/servers/local/console');
      await command(page).fill('printf first; sleep 2; printf second; exit 7');
      await page.getByRole('button', { name: 'Run command', exact: true }).click();
      await statusText(page, 'Connection interrupted').waitFor();
      assert.equal(await output(page).innerText(), 'first');
      await page.reload();
      await statusText(page, 'Command finished.').waitFor();
      assert.match(await output(page).innerText(), /^firstsecond\n\n\[exit 7/);
      assert.equal(starts, 1, 'reconnecting ran the command twice');
      await page.getByRole('button', { name: 'Copy output', exact: true }).click();
      assert.match(await page.evaluate(() => window.consoleCopied), /firstsecond/);
      // Expired verification pauses reads; renewing resumes the same job.
      expire = true;
      await page.reload();
      await page.getByText('Verify again to reconnect to this command.', { exact: true }).waitFor();
      expire = false;
      await page.getByRole('textbox', { name: 'Authenticator or recovery code' }).fill('654321');
      await page.getByRole('button', { name: 'Unlock for 15 minutes', exact: true }).click();
      await statusText(page, 'Command finished.').waitFor();
      assert.equal(starts, 1, 'renewing verification ran the command again');
      // A result lost in an agent restart is reported as unknown, never rerun.
      missing = true;
      await page.reload();
      await statusText(page, 'Result unavailable.').waitFor();
      assert.equal(starts, 1);
      assert.equal(await page.getByRole('button', { name: 'Run command', exact: true }).isEnabled(), true);
    },
  },
]).catch(error => { console.error(error); process.exit(1); });
