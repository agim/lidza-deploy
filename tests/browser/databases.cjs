// Databases and backup storage: the databases index filter, backups (back
// up now, restore into a new database), the backup schedule, remote backup
// objects and the S3-compatible storage integration.
const { suite, assert, callsTo, until } = require('./fixtures/harness.cjs');

suite('databases', [
  {
    name: 'index filters by application and links each database',
    async run({ page, open }) {
      await open('/console/databases');
      const rows = page.locator('#main-content table tbody tr');
      await rows.first().waitFor();
      await page.getByRole('combobox', { name: 'Application' }).selectOption('api');
      await page.getByText(/No (matching )?databases?/i).first().waitFor();
      await page.getByRole('combobox', { name: 'Application' }).selectOption('portal');
      await rows.getByRole('link', { name: 'portal' }).click();
      await page.waitForURL(url => url.pathname === '/console/databases/portal');
      await page.getByRole('tab', { name: 'Overview', selected: true }).waitFor();
    },
  },
  {
    name: 'backups: back up now; restore goes into a new database after confirmation',
    async run({ page, state, open }) {
      await open('/console/databases/portal/backups');
      await page.getByRole('button', { name: 'Back up now' }).click();
      await until(() => callsTo(state, 'control/servers/local/databases/portal/backup', 'POST').length === 1, 'the backup request');
      const row = page.getByRole('row').filter({ hasText: 'Local · upload failed' });
      await row.getByText('Storage unavailable').waitFor();
      await row.getByRole('button', { name: 'Restore', exact: true }).click();
      const dialog = page.getByRole('alertdialog', { name: 'Restore backup into a new database' }).or(page.getByRole('dialog', { name: 'Restore backup into a new database' }));
      await dialog.getByRole('textbox').fill('restored-portal');
      assert.equal(callsTo(state, 'control/apps/portal/restore').length, 0, 'restored before confirmation');
      await dialog.getByRole('button', { name: 'Start restore' }).click();
      await dialog.waitFor({ state: 'hidden' });
      assert.deepEqual(callsTo(state, 'control/apps/portal/restore', 'POST')[0].body, { source: 'portal', backup: 'backup-one', target: 'restored-portal', env_key: 'DATABASE_URL' });
    },
  },
  {
    name: 'schedule: frequency and off-site copy save',
    async run({ page, state, open }) {
      await open('/console/databases/portal/schedule');
      await page.getByRole('combobox', { name: 'Frequency' }).selectOption({ label: 'Hourly' });
      await page.getByRole('checkbox', { name: 'Copy to off-site storage' }).uncheck();
      await page.getByRole('button', { name: 'Save backup schedule' }).click();
      await page.getByText('Backup schedule saved.').first().waitFor();
      assert.deepEqual(callsTo(state, 'control/servers/local/databases/portal/backups', 'PATCH')[0].body, { hours: 1, keep: 1, offsite: false });
    },
  },
  {
    name: 'remote backups: objects filter and long keys stay inside the page',
    widths: [320, 1440],
    async run({ page, state, open }) {
      await page.route('**/api/control/storage/backups', route => route.fulfill({ json: { bucket: 'private-backups', prefix: 'lidza', checked: '2026-10-15T14:00:00Z', objects: ['portal', 'api', 'shop'].map(id => ({ key: `databases/${id}/${'long-backup-name-'.repeat(8)}.dump`, prefix: 'lidza', size: 4096, modifiedAt: '2026-10-15T14:00:00Z' })) } }));
      await open('/console/remote-backups');
      const rows = page.locator('[role=tabpanel] table tbody tr');
      await page.waitForFunction(() => document.querySelectorAll('[role=tabpanel] table tbody tr').length === 3);
      await page.getByRole('searchbox', { name: 'Filter backup objects' }).fill('databases/shop');
      await page.waitForFunction(() => document.querySelectorAll('[role=tabpanel] table tbody tr').length === 1);
      assert.match(await rows.first().innerText(), /databases\/shop/);
      await page.getByRole('tab', { name: 'Retention' }).click();
      await page.waitForURL(url => url.pathname === '/console/remote-backups/retention');
      assert.deepEqual(state.unknown, [], 'unmocked API reads');
    },
  },
  {
    name: 'storage integration never shows saved keys; blank keeps them',
    async run({ page, state, open }) {
      await open('/console/integrations/storage');
      const access = page.getByRole('textbox', { name: /^Access key/ }), secret = page.getByRole('textbox', { name: /^Secret key/ });
      assert.equal(await access.inputValue(), '');
      assert.equal(await secret.inputValue(), '');
      assert.equal(await secret.getAttribute('type'), 'password');
      await page.getByRole('textbox', { name: 'Bucket', exact: true }).fill('other-backups');
      await page.getByRole('button', { name: 'Test & save storage' }).click();
      await page.getByText('Storage verified and saved.').first().waitFor();
      const saved = callsTo(state, 'control/storage', 'PUT')[0].body;
      assert.equal(saved.bucket, 'other-backups');
      assert.equal(saved.endpoint, 'https://nyc3.digitaloceanspaces.com');
      assert(!saved.access_key && !saved.secret_key, 'blank key fields replaced the saved credentials');
    },
  },
]).catch(error => { console.error(error); process.exit(1); });
