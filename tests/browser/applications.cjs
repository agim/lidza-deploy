// Applications: the index (search, status filter, an empty or offline
// control panel), the detail header (Deploy, lifecycle actions, setup link), the
// new-application wizard, and the Environment, Domains, Database & cache and
// Settings tabs.
const { suite, assert, callsTo, until } = require('./fixtures/harness.cjs');
const { app } = require('./fixtures/console.cjs');

const rows = page => page.locator('#main-content table tbody tr');
const continueButton = page => page.getByRole('button', { name: 'Continue', exact: true });
const settingsPatches = state => callsTo(state, 'control/apps/portal/settings', 'PATCH').map(call => call.body);
const envRow = (page, key) => page.locator('.env-row').filter({ has: page.locator(`input.env-name[value="${key}"]`) });

suite('applications', [
  {
    name: 'index search and status filter',
    async run({ page, open }) {
      await open('/console/applications');
      await rows(page).first().waitFor();
      assert.equal(await rows(page).count(), 2);
      await page.getByRole('searchbox', { name: 'Search applications' }).fill('portal');
      await page.waitForFunction(() => document.querySelectorAll('#main-content table tbody tr').length === 1);
      assert.match(await rows(page).first().innerText(), /portal\.example\.com/);
      await page.getByRole('button', { name: 'Clear filters' }).click();
      await page.getByRole('combobox', { name: 'Filter by application status' }).click();
      await page.getByRole('option', { name: 'Stopped', exact: true }).click();
      await page.waitForFunction(() => document.querySelectorAll('#main-content table tbody tr').length === 1);
      assert.match(await rows(page).first().innerText(), /api\.example\.com/);
      assert.match(page.url(), /status=stopped/, 'the filter is not kept in the URL');
      await rows(page).first().getByRole('link', { name: 'api', exact: true }).click();
      await page.waitForURL(url => url.pathname === '/console/applications/api');
      await page.getByRole('heading', { level: 1, name: 'api' }).waitFor();
    },
  },
  {
    name: 'an empty or offline control panel stays usable',
    async run({ page, open }) {
      await page.route('**/api/control/apps', route => route.request().method() === 'GET' ? route.fulfill({ json: [] }) : route.fallback());
      await page.route('**/api/control/servers', route => route.fulfill({ json: [] }));
      await open('/console/applications');
      await page.getByRole('heading', { name: 'Connect your first server' }).waitFor();
      assert.equal(await page.getByRole('button', { name: 'New application' }).count(), 0, 'New application offered without a server');
      await page.getByRole('button', { name: 'Connect server', exact: true }).click();
      await page.waitForURL(url => url.pathname === '/console/servers/new');
      await page.unroute('**/api/control/apps');
      await page.route('**/api/control/apps', route => route.fulfill({ status: 503, json: { error: 'Fixture control offline' } }));
      await open('/console/applications');
      await page.getByRole('alert').filter({ hasText: 'Fixture control offline' }).waitFor();
    },
  },
  {
    name: 'Deploy queues a release and shows a rejection',
    async run({ page, state, open }) {
      state.apps.push({ ...app, id: 'fresh', current: undefined });
      let fail = false;
      await page.route('**/api/control/apps/fresh/deploy', route => fail
        ? route.fulfill({ status: 409, json: { error: 'GitHub installation unavailable' } })
        : route.fulfill({ status: 202, json: { id: 'fresh-build', status: 'queued', created: new Date().toISOString() } }));
      await open('/console/applications/fresh');
      await page.getByRole('button', { name: 'Deploy', exact: true }).click();
      await page.getByText('Deployment queued.', { exact: true }).first().waitFor();
      await page.getByRole('link', { name: 'Follow the build log' }).waitFor();
      assert.equal(await page.getByRole('link', { name: 'Follow the build log' }).getAttribute('href'), '/console/deployments/fresh-build/log');
      fail = true;
      await page.getByRole('button', { name: 'Deploy', exact: true }).click();
      await page.getByRole('alert').filter({ hasText: 'GitHub installation unavailable' }).waitFor();
    },
  },
  {
    name: 'stop needs confirmation; a stopped app offers Start',
    async run({ page, state, open }) {
      state.apps.push({ ...app, id: 'idle', current: { commit: 'abcdef1' } });
      await open('/console/applications/idle');
      await page.getByRole('button', { name: 'Actions' }).click();
      await page.getByRole('menuitem', { name: 'Stop' }).click();
      const dialog = page.getByRole('alertdialog', { name: 'Stop idle?' });
      await dialog.waitFor();
      assert.equal(callsTo(state, 'control/apps/idle/stop').length, 0, 'stopped before confirmation');
      await dialog.getByRole('button', { name: 'Stop application' }).click();
      await dialog.waitFor({ state: 'hidden' });
      assert.equal(callsTo(state, 'control/apps/idle/stop', 'POST').length, 1);
      await open('/console/applications/api');
      await page.getByRole('button', { name: 'Start', exact: true }).click();
      await page.getByText('api is starting.', { exact: true }).first().waitFor();
      assert.equal(callsTo(state, 'control/apps/api/start', 'POST').length, 1);
      assert.equal(await page.getByRole('button', { name: 'Deploy', exact: true }).count(), 0, 'a stopped app still offers Deploy');
    },
  },
  {
    name: 'a stack waiting for setup links to its setup page',
    async run({ page, state, open }) {
      state.apps[0].current = { ...state.apps[0].current, stack: { bootstrap: true, manifest: { bootstrap_readiness: { setup_path: '/setup' } } } };
      await open('/console/applications/portal');
      assert.equal(await page.getByRole('link', { name: 'Open setup' }).getAttribute('href'), 'https://portal.example.com/setup');
    },
  },
  {
    name: 'wizard validates, keeps drafts and creates what it reviewed',
    widths: [320, 1440],
    async run({ page, state, open }) {
      await open('/console/applications/new');
      await continueButton(page).click();
      await page.getByRole('heading', { name: 'Application name', level: 2 }).waitFor();
      assert.equal(await page.getByRole('heading', { name: 'Repository', level: 2 }).count(), 0, 'an empty name advanced the wizard');
      await page.getByRole('textbox', { name: /^Application name/ }).fill('new-portal');
      await continueButton(page).click();
      await page.getByRole('button', { name: 'acme/portal Private · main' }).click();
      // Detection lists the repository's branches; the default one is chosen.
      await page.getByRole('combobox', { name: /^Branch/ }).waitFor();
      assert.equal(await page.locator('#wizard-branch').inputValue(), 'main', 'the repository default branch was not selected');
      await page.getByRole('switch', { name: 'Deploy automatically on branch pushes' }).click();
      await continueButton(page).click();
      await page.getByRole('heading', { name: 'Setup', level: 2 }).waitFor();
      await page.getByRole('combobox', { name: 'Runtime profile' }).selectOption({ label: 'Minimum · 4 GiB / 2 CPU' });
      await continueButton(page).click();
      await page.getByRole('radio', { name: /^Production/ }).waitFor();
      await continueButton(page).click();
      await page.getByLabel('Application hostname', { exact: true }).fill('new.example.com');
      await continueButton(page).click();
      await page.getByRole('radio', { name: /^Create PostgreSQL on the server/ }).check();
      await continueButton(page).click();
      const key = page.getByLabel('Application master key', { exact: true });
      assert.equal(await key.getAttribute('type'), 'password');
      await key.fill('not-a-key');
      await continueButton(page).click();
      assert.equal(await page.getByRole('heading', { name: 'Review', level: 2 }).count(), 0, 'an invalid master key advanced the wizard');
      await key.fill('ab'.repeat(32));
      await page.getByRole('button', { name: 'Add variable', exact: true }).click();
      await page.getByRole('textbox', { name: 'Variable 2 name' }).fill('MAIL_API_KEY');
      await page.getByRole('combobox', { name: 'Variable 2 type' }).selectOption('credential');
      await page.getByRole('textbox', { name: 'Variable 2 value' }).fill('fixture-only-secret');
      await page.getByRole('button', { name: 'Back', exact: true }).click();
      await page.getByRole('button', { name: 'Back', exact: true }).click();
      assert.equal(await page.getByLabel('Application hostname', { exact: true }).inputValue(), 'new.example.com', 'Back lost the hostname');
      await continueButton(page).click();
      await continueButton(page).click();
      assert.equal(await page.getByRole('textbox', { name: 'Variable 2 value' }).inputValue(), 'fixture-only-secret', 'Back lost the environment');
      await continueButton(page).click();
      await page.getByRole('heading', { name: 'Review', level: 2 }).waitFor();
      assert.equal(callsTo(state, 'control/apps', 'POST').length, 0, 'created before the final confirmation');
      await page.getByRole('button', { name: 'Create application', exact: true }).click();
      await page.getByRole('heading', { level: 1, name: 'new-portal is ready' }).waitFor();
      const created = callsTo(state, 'control/apps', 'POST')[0].body;
      assert.equal(created.id, 'new-portal');
      assert.equal(created.repository, 'acme/portal');
      assert.equal(created.branch, 'main');
      assert.equal(created.github_installation, 42);
      assert.equal(created.server_id, 'local');
      assert.equal(created.domain, 'new.example.com');
      assert.equal(created.auto_deploy, true);
      assert.equal(created.runtime_profile, 'minimum');
      assert.equal(created.database.mode, 'local');
      assert.equal(created.env.LIDZA_MASTER_KEY, 'ab'.repeat(32));
      assert.equal(created.env.MAIL_API_KEY, 'fixture-only-secret');
      assert.equal(created.env_kinds.MAIL_API_KEY, 'credential');
      assert.equal(created.env_kinds.LIDZA_MASTER_KEY, 'credential');
    },
  },
  {
    name: 'environment: credentials reveal after step-up and saves keep them',
    async run({ page, state, open }) {
      await open('/console/applications/portal/environment');
      const credential = envRow(page, 'API_TOKEN');
      const value = credential.locator('.env-value');
      await value.waitFor();
      assert.equal(await value.inputValue(), '');
      assert.match(await value.getAttribute('placeholder'), /tok… · 18 characters/);
      assert.equal((await page.content()).includes('fixture-secret-value'), false, 'the credential is in the page before reveal');
      await credential.getByRole('button', { name: 'Reveal credential' }).click();
      const factor = page.getByRole('dialog', { name: 'Verify to reveal credential' }).or(page.getByRole('alertdialog', { name: 'Verify to reveal credential' }));
      await factor.getByLabel('Authenticator or recovery code').fill('123456');
      await factor.getByRole('button', { name: 'Verify & reveal' }).click();
      await page.waitForFunction(() => [...document.querySelectorAll('.env-value')].some(input => input.value === 'fixture-secret-value'));
      assert(callsTo(state, 'v1/auth/step-up').some(call => call.body.purpose === 'credential.reveal' && call.body.target === 'portal/API_TOKEN'));
      await credential.getByRole('button', { name: 'Hide credential' }).click();
      assert.equal(await value.inputValue(), '');
      await envRow(page, 'LOG_LEVEL').locator('.env-value').fill('debug');
      await page.getByRole('button', { name: 'Save variables' }).click();
      // A variable is read at process start: the save restarts the application.
      await page.locator('.alert[role=status]').filter({ hasText: 'Settings saved. Restarting…' }).waitFor();
      await page.locator('.alert[role=status]').filter({ hasText: 'Restarted.' }).waitFor();
      assert.match(await page.getByRole('link', { name: 'View restart' }).getAttribute('href'), /\/console\/deployments\/reload-settings/);
      assert(callsTo(state, 'control/deployments/reload-settings').length > 0, 'the restart was not followed');
      assert.equal((await page.content()).includes('next deployment'), false, 'the page still defers the change to the next deployment');
      const saved = settingsPatches(state)[0];
      assert.deepEqual(saved.env_changes, { LOG_LEVEL: 'debug' }, 'the save touched another variable');
      assert.equal(saved.domain, 'portal.example.com');
    },
  },
  {
    name: 'environment: a failed save keeps the edit for a retry',
    async run({ page, state, open }) {
      let reject = true;
      await page.route('**/api/control/apps/portal/settings', route => route.request().method() === 'PATCH' && reject ? route.fulfill({ status: 422, json: { error: 'Fixture save rejected; retry is safe.' } }) : route.fallback());
      await open('/console/applications/portal/environment');
      const setting = envRow(page, 'LOG_LEVEL').locator('.env-value');
      await setting.fill('trace');
      await page.getByRole('button', { name: 'Save variables' }).click();
      await page.getByRole('alert').filter({ hasText: 'Fixture save rejected' }).waitFor();
      assert.equal(await setting.inputValue(), 'trace', 'a failed save discarded the edit');
      reject = false;
      await page.getByRole('button', { name: 'Save variables' }).click();
      await page.getByText('Settings saved.', { exact: false }).first().waitFor();
      assert.equal(settingsPatches(state).at(-1).env_changes.LOG_LEVEL, 'trace');
    },
  },
  {
    name: 'environment: the master key is generated once, after confirmation',
    async run({ page, state, open }) {
      await open('/console/applications/portal/environment');
      await page.getByRole('button', { name: 'Generate master key' }).click();
      const dialog = page.getByRole('alertdialog', { name: 'Generate application master key' });
      await dialog.getByRole('button', { name: 'Cancel' }).click();
      assert.equal(callsTo(state, 'control/apps/portal/master-key').length, 0, 'generated without confirmation');
      await page.getByRole('button', { name: 'Generate master key' }).click();
      await dialog.getByRole('button', { name: 'Generate & save key' }).click();
      await dialog.waitFor({ state: 'hidden' });
      assert.equal(callsTo(state, 'control/apps/portal/master-key', 'POST').length, 1);
      const key = envRow(page, 'LIDZA_MASTER_KEY');
      await key.waitFor();
      assert.equal(await key.locator('.env-value').inputValue(), '');
      assert.match(await key.locator('.env-value').getAttribute('placeholder'), /64 characters/);
      assert.equal(await page.getByRole('button', { name: 'Generate master key' }).count(), 0, 'the key can be rotated by a second click');
    },
  },
  {
    name: 'domains: additional hostnames and customer hosts save',
    async run({ page, state, open }) {
      state.aliases = ['saved.example.com'];
      state.domainStatuses = [{ domain: 'saved.example.com', state: 'waiting_dns', message: 'DNS check pending: ' + 'long-dns-response'.repeat(15) }];
      await page.route('**/api/control/apps/portal/settings', route => {
        if (route.request().method() !== 'PATCH') return route.fallback();
        state.aliases = route.request().postDataJSON().additional_domains;
        return route.fallback();
      });
      await open('/console/applications/portal/domains');
      await page.getByLabel('Additional hostname 1', { exact: true }).waitFor();
      await page.getByRole('button', { name: 'Add hostname', exact: true }).click();
      await page.getByLabel('Additional hostname 2', { exact: true }).fill('WWW.EXAMPLE.ORG');
      await page.getByRole('checkbox', { name: /Serve approved customer hostnames/ }).check();
      await page.getByRole('button', { name: 'Save hostnames' }).click();
      await page.getByText('Settings saved.', { exact: false }).first().waitFor();
      let saved = settingsPatches(state)[0];
      assert.deepEqual(saved.additional_domains, ['saved.example.com', 'www.example.org']);
      assert.equal(saved.customer_hosts, true);
      assert.equal(saved.domain, 'portal.example.com');
      await page.getByLabel('Additional hostname 2', { exact: true }).waitFor();
      await page.getByRole('button', { name: 'Remove additional hostname 1', exact: true }).click();
      assert.equal(await page.getByLabel('Additional hostname 1', { exact: true }).inputValue(), 'www.example.org');
      await page.getByRole('button', { name: 'Save hostnames' }).click();
      await until(() => settingsPatches(state).length === 2, 'the second hostname save');
      saved = settingsPatches(state)[1];
      assert.deepEqual(saved.additional_domains, ['www.example.org']);
    },
  },
  {
    name: 'data tab: the cache is automatic or an external URL kept out of view',
    async run({ page, state, open }) {
      await open('/console/applications/portal/data');
      await page.getByRole('radio', { name: /^External Redis \/ Valkey/ }).check();
      const url = page.locator('[data-cache-settings] input[name=url]');
      assert.equal(await url.getAttribute('type'), 'password', 'the cache URL with its password is shown in clear');
      await url.fill('rediss://:fixture-secret@cache.example.com:6379/0');
      await page.getByRole('button', { name: 'Save cache settings' }).click();
      await page.getByText('Cache configuration saved.').first().waitFor();
      await page.getByRole('radio', { name: /^Automatic private cache/ }).check();
      await page.getByRole('button', { name: 'Save cache settings' }).click();
      await until(() => callsTo(state, 'control/apps/portal/cache', 'POST').length === 2, 'the second cache save');
      assert.deepEqual(callsTo(state, 'control/apps/portal/cache', 'POST').map(call => call.body), [{ mode: 'external', url: 'rediss://:fixture-secret@cache.example.com:6379/0' }, { mode: 'automatic' }]);
    },
  },
  {
    name: 'settings: a long branch from the loaded list and the runtime profile save and deploy',
    widths: [320, 1440],
    async run({ page, state, open }) {
      const long = 'feature/' + 'long-branch-'.repeat(18);
      await open('/console/applications/portal/settings');
      await page.getByRole('button', { name: 'Load', exact: true }).click();
      await page.waitForFunction(() => document.querySelectorAll('#settings-branches option').length === 2);
      const branch = page.locator('input[name=branch]');
      await branch.fill(long);
      const box = await branch.boundingBox();
      assert(box.x >= 0 && box.x + box.width <= page.viewportSize().width, 'the branch field runs past the viewport');
      await page.getByRole('combobox', { name: 'Runtime profile' }).selectOption({ label: 'Standard · 8 GiB / 4 CPU' });
      // Branch and runtime profile are build changes: the save deploys them.
      assert.equal(await page.getByRole('button', { name: 'Save changes' }).count(), 0, 'a build change still reads Save changes');
      await page.getByRole('button', { name: 'Save and deploy' }).click();
      await page.locator('.alert[role=status]').filter({ hasText: 'Settings saved. Deploying…' }).waitFor();
      assert.match(await page.getByRole('link', { name: 'View deployment' }).getAttribute('href'), /\/console\/deployments\/deploy-settings/);
      const saved = settingsPatches(state)[0];
      assert.equal(saved.branch, long, 'saving changed the branch name');
      assert.equal(saved.runtime_profile, 'standard');
      assert.equal(saved.domain, 'portal.example.com');
    },
  },
  {
    name: 'move: a move stopped at Prepare keeps its actions readable on a phone',
    widths: [390, 1440],
    async run({ page, state, open }) {
      const step = (key, status) => ({ key, title: key, status, log: [] });
      state.move = {
        id: 'm1', app_id: 'portal', source_server: 'local', target_server: 'local', target_runtime: 'native', runtime: 'go', state: 'failed', hostnames: ['portal.example.com'], addresses: ['203.0.113.9'],
        dns: [], tls_ready: false, records: [], switched: false, actions: ['continue', 'remove-retained', 'abort'],
        steps: [step('plan', 'done'), step('prepare', 'failed'), step('environment', 'pending'), step('data', 'pending'), step('deploy', 'pending'), step('cutover', 'pending'), step('final_sync', 'skipped'), step('finish', 'pending')],
        failure: { step: 'prepare', server: 'local', class: 'operator', code: 'retained_data', message: 'Production keeps retained data of portal (r-old): an application with this id ran there before.', attempt: 1, max_attempts: 6, time: new Date().toISOString(),
          remediation: 'Remove the retained data of portal on Production from this move, or remove it under the server\'s retained resources; the move then continues.',
          retained: { id: 'r-old', server: 'local', app_id: 'portal', databases: 1, last_backup: '2026-11-05T10:00:00Z', offsite: true, final_backup_required: false, label: 'Remove retained data of portal on Production (last backup 2026-11-05 10:00 UTC, off-site)' } },
      };
      await open('/console/applications/portal/move');
      const remove = page.getByRole('button', { name: 'Remove retained data', exact: true });
      await remove.waitFor();
      const width = page.viewportSize().width;
      const heights = [];
      for (const name of ['Remove retained data', 'Retry', 'Abort move']) {
        const box = await page.getByRole('button', { name, exact: true }).boundingBox();
        assert(box, `${name} is not shown`);
        assert(box.x >= 0 && box.x + box.width <= width, `${name} runs past the viewport at ${width}px`);
        heights.push(Math.round(box.height));
      }
      assert(heights.every(height => height === heights[0]), `a label wraps at ${width}px: the buttons are ${heights.join(', ')}px tall`);
      if (process.env.TEST_SHOT_DIR) await page.locator('.move-actions').screenshot({ path: `${process.env.TEST_SHOT_DIR}/move-actions-${width}.png` });
      // The details moved out of the label: they are in the confirmation.
      await remove.click();
      await page.getByRole('alertdialog').getByText('Remove retained data of portal on Production?').waitFor();
    },
  },
  {
    name: 'move: a move stopped at the ClickHouse copy can leave that data behind',
    widths: [390, 1440],
    async run({ page, state, open }) {
      const step = (key, status) => ({ key, title: key, status, log: [] });
      state.move = {
        id: 'm1', app_id: 'portal', source_server: 'local', target_server: 'local', target_runtime: 'native', runtime: 'go', state: 'failed', hostnames: ['portal.example.com'], addresses: ['203.0.113.9'],
        dns: [], tls_ready: false, records: [], switched: false, actions: ['continue', 'abort', 'leave-clickhouse'],
        steps: [step('plan', 'done'), step('prepare', 'done'), step('environment', 'done'), step('data', 'failed'), step('deploy', 'pending'), step('cutover', 'pending'), step('final_sync', 'pending'), step('finish', 'pending')],
        failure: { step: 'data', engine: 'ClickHouse telemetry', server: 'local', class: 'capacity', code: 'memory', message: 'Copy data failed (ClickHouse telemetry) on Production: memory limit exceeded.', attempt: 1, max_attempts: 6, time: new Date().toISOString() },
      };
      await open('/console/applications/portal/move');
      const leave = page.getByRole('button', { name: 'Leave ClickHouse behind', exact: true });
      await leave.waitFor();
      const width = page.viewportSize().width;
      const heights = [];
      for (const name of ['Leave ClickHouse behind', 'Retry', 'Abort move']) {
        const box = await page.getByRole('button', { name, exact: true }).boundingBox();
        assert(box, `${name} is not shown`);
        assert(box.x >= 0 && box.x + box.width <= width, `${name} runs past the viewport at ${width}px`);
        heights.push(Math.round(box.height));
      }
      assert(heights.every(height => height === heights[0]), `a label wraps at ${width}px: the buttons are ${heights.join(', ')}px tall`);
      if (process.env.TEST_SHOT_DIR) await page.locator('.move-actions').screenshot({ path: `${process.env.TEST_SHOT_DIR}/move-leave-clickhouse-${width}.png` });
      await leave.click();
      const dialog = page.getByRole('alertdialog');
      await dialog.getByText('Leave the ClickHouse data behind?').waitFor();
      await dialog.getByText(/starts there with an empty ClickHouse database/).waitFor();
    },
  },
  {
    name: 'settings: the access log shows its file and switches off at once',
    widths: [320, 1440],
    async run({ page, state, open }) {
      await open('/console/applications/portal/settings');
      const card = page.locator('[data-access-log=portal]');
      await card.getByText('/var/log/lidza/apps/portal/access.log').waitFor();
      assert.match(await card.innerText(), /LIDZA_ACCESS_LOG/);
      assert.match(await card.innerText(), /Writing/);
      const box = await card.boundingBox();
      assert(box.x >= 0 && box.x + box.width <= page.viewportSize().width, 'the access log card runs past the viewport');
      const toggle = card.getByRole('switch', { name: 'Write an access log for this application' });
      assert.equal(await toggle.getAttribute('aria-checked'), 'true');
      await toggle.click();
      await until(() => callsTo(state, 'control/apps/portal/access-log', 'PUT').length === 1, 'the switch did not save the setting');
      assert.deepEqual(callsTo(state, 'control/apps/portal/access-log', 'PUT')[0].body, { enabled: false });
      await card.getByText('Off', { exact: true }).waitFor();
      assert.equal(await card.getByText('/var/log/lidza/apps/portal/access.log').count(), 0, 'a switched-off log still shows its file');
      assert.equal(settingsPatches(state).length, 0, 'the access log must not save the settings form');
    },
  },
]).catch(error => { console.error(error); process.exit(1); });
