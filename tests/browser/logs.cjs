// Logs: the live build log of a deployment (streaming, reading position,
// pause, offline recovery, completion) and the runtime log of an
// application, their copy controls, and a failed deployment's log.
const { suite, assert } = require('./fixtures/harness.cjs');

const output = page => page.locator('#deployment-output');
const state = page => page.locator('#deployment-state');

suite('logs', [
  {
    name: 'build log streams, keeps the reading position, pauses and recovers',
    async run({ page, open }) {
      let phase = 0, offline = false, finished = false, reads = 0;
      const created = new Date().toISOString();
      const row = () => ({ id: 'fixture-build', app_id: 'portal', server_id: 'local', status: finished ? 'live' : 'building', created, duration_seconds: 10 + phase,
        log: phase ? `${Array.from({ length: 160 }, (_, index) => `line ${index}`).join('\n')}\nphase-${phase} <b>literal</b>` : '' });
      await page.route('**/api/control/deployments/fixture-build', route => { reads++; return route.fulfill(offline ? { status: 503, json: { error: 'Agent offline' } } : { json: row() }); });
      await open('/console/deployments/fixture-build/log');
      await output(page).getByText('Waiting for build output').waitFor();
      phase = 1;
      await output(page).getByText('phase-1', { exact: false }).waitFor();
      assert.equal(await state(page).textContent(), 'Live · building');
      assert.equal(await page.locator('#deployment-output b').count(), 0, 'build output was parsed as HTML');
      // Scrolling up stops following; new output keeps the reading position.
      await output(page).evaluate(element => { element.scrollTop = 30; element.dispatchEvent(new Event('scroll', { bubbles: true })); });
      await page.waitForFunction(() => !document.querySelector('#log-follow').checked);
      phase = 2;
      await output(page).getByText('phase-2', { exact: false }).waitFor();
      assert.equal(await output(page).evaluate(element => element.scrollTop), 30, 'the reading position jumped when output arrived');
      // Paused output does not change until it is resumed.
      await page.getByRole('button', { name: 'Pause live output' }).click();
      await page.waitForFunction(() => document.querySelector('#deployment-state')?.textContent === 'Paused · building');
      await page.waitForTimeout(300);
      phase = 3;
      await page.waitForTimeout(1900);
      assert.doesNotMatch(await output(page).textContent(), /phase-3/, 'paused output kept updating');
      await page.getByRole('button', { name: 'Resume live output' }).click();
      await output(page).getByText('phase-3', { exact: false }).waitFor();
      // An outage shows an error and keeps the output; recovery clears it.
      offline = true;
      await page.locator('#deployment-refresh-error').waitFor();
      assert.match(await output(page).textContent(), /phase-3/, 'an outage erased the output');
      offline = false; phase = 4; finished = true;
      await page.waitForFunction(() => document.querySelector('#deployment-state')?.textContent === 'Finished · live');
      assert.equal(await page.locator('#deployment-refresh-error').count(), 0, 'the recovered error stayed');
      assert.match(await output(page).textContent(), /phase-4 <b>literal<\/b>/);
      count = reads;
      await page.waitForTimeout(1900);
      assert(reads - count <= 1, 'a finished deployment kept polling');
    },
  },
  {
    name: 'build and runtime logs copy exactly what is shown',
    widths: [320, 1440],
    async run({ page, open }) {
      for (const [path, label, text] of [['/console/deployments/build-one/log', 'Copy build log', '[fixture output]'], ['/console/applications/portal/logs', 'Copy runtime log', 'Serving requests']]) {
        await open(path);
        await output(page).getByText(text, { exact: false }).waitFor();
        const shown = await output(page).textContent();
        await page.getByRole('button', { name: label, exact: true }).click();
        await page.getByRole('button', { name: 'Copied', exact: true }).waitFor();
        assert.equal(await page.evaluate(() => window.consoleCopied), shown, `${label} copied something else`);
      }
      await open('/console/deployments/build-one/log');
      await output(page).getByText('[fixture output]', { exact: false }).waitFor();
      await page.evaluate(() => { navigator.clipboard.writeText = async () => { throw new Error('denied'); }; });
      await page.getByRole('button', { name: 'Copy build log', exact: true }).click();
      await page.getByRole('alert').filter({ hasText: 'Could not copy' }).waitFor();
    },
  },
  {
    name: 'a failed deployment shows the failure in its log',
    async run({ page, open }) {
      const failed = { id: 'build-one', app_id: 'portal', server_id: 'local', status: 'failed', created: '2026-10-20T12:00:00Z', log: 'git\ncheckout completed', error: 'Fixture runtime preflight rejected' };
      await page.route('**/api/control/deployments/build-one', route => route.fulfill({ json: failed }));
      await page.route('**/api/control/deployments', route => route.fulfill({ json: { deployments: [failed], unavailable_servers: [] } }));
      await open('/console/deployments/build-one/log');
      await page.locator('#deployment-failure').filter({ hasText: 'Fixture runtime preflight rejected' }).waitFor();
      await page.waitForFunction(() => document.querySelector('#deployment-state')?.textContent === 'Finished · failed');
      const shown = await output(page).textContent();
      assert.match(shown, /checkout completed[\s\S]*Failure: Fixture runtime preflight rejected/);
      assert.equal(await page.getByRole('button', { name: 'Pause live output' }).isDisabled(), true, 'a finished log offers Pause');
      await page.getByRole('button', { name: 'Copy build log', exact: true }).click();
      assert.equal(await page.evaluate(() => window.consoleCopied), shown);
      await open('/console/applications/portal');
      await page.getByRole('alert').filter({ hasText: 'Last deployment failed: Fixture runtime preflight rejected' }).waitFor();
      assert.equal(await page.getByRole('link', { name: 'Open its log' }).getAttribute('href'), '/console/deployments/build-one/log');
    },
  },
]).catch(error => { console.error(error); process.exit(1); });
