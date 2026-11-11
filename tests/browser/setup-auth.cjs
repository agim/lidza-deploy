// First-run setup and sign-in: setup.html (claim with the setup key, the
// interview, completion), login.html (errors, one-time codes, the safe
// return address) and the console sending a signed-out visitor to sign in.
// The setup and auth endpoints are browser fixtures.
const { suite, assert } = require('./fixtures/harness.cjs');

async function mockSetup(page, { publicURL = '', paired = 0 } = {}) {
  const calls = [];
  await page.route('**/api/setup/**', route => {
    const request = route.request(), name = new URL(request.url()).pathname.split('/').pop();
    calls.push({ name, authorization: request.headers().authorization, body: request.postData() ? request.postDataJSON() : undefined });
    if (request.headers().authorization !== 'Bearer fixture-setup-key') return route.fulfill({ status: 401, json: { error: 'The setup key is not valid.' } });
    if (name === 'check') return route.fulfill({ json: { public_url: publicURL, managed_database: true, paired_agent_count: paired } });
    if (name === 'complete') return route.fulfill({ json: { redirect: '/login.html' } });
    return route.fulfill({ status: 404, json: { error: 'not found' } });
  });
  return calls;
}

suite('setup and sign-in', [
  {
    name: 'setup: a wrong key is refused; the interview completes',
    mock: false,
    async run({ page, base }) {
      const calls = await mockSetup(page);
      await page.goto(`${base}/setup.html`);
      await page.locator('#setup-key').fill('wrong-key');
      await page.getByRole('button', { name: 'Unlock setup' }).click();
      await page.getByRole('alert').filter({ hasText: 'The setup key is not valid.' }).waitFor();
      assert.equal(await page.locator('#setup-form').isVisible(), false, 'a wrong key opened the interview');
      await page.locator('#setup-key').fill('fixture-setup-key');
      await page.getByRole('button', { name: 'Unlock setup' }).click();
      await page.locator('#setup-form').waitFor();
      assert.equal(await page.locator('#setup-key').inputValue(), '', 'the setup key stays in the page');
      // An external database needs its URL; managed PostgreSQL does not.
      await page.locator('#database-mode').selectOption('external');
      assert.equal(await page.locator('#database-url-label').isVisible(), true);
      await page.locator('#database-mode').selectOption('managed');
      assert.equal(await page.locator('#database-url-label').isVisible(), false);
      // A local address needs no DNS or HTTPS questions.
      await page.locator('#public-url').fill('http://127.0.0.1:8080');
      await page.locator('#public-url').dispatchEvent('input');
      assert.equal(await page.locator('#public-network').isVisible(), false, 'a localhost install asks for DNS');
      await page.locator('[name=email]').fill('owner@example.com');
      await page.locator('[name=password]').fill('fixture-first-run-password');
      await page.getByRole('button', { name: 'Validate and finish setup' }).click();
      await page.waitForURL(url => url.pathname === '/login.html');
      const complete = calls.find(call => call.name === 'complete').body;
      assert.equal(complete.email, 'owner@example.com');
      assert.equal(complete.public_url, 'http://127.0.0.1:8080');
      assert.equal(complete.database_mode, 'managed');
      assert.equal(complete.agent, undefined, 'an empty agent section was sent');
    },
  },
  {
    name: 'setup: an installed address is locked and a paired agent needs no details',
    mock: false,
    async run({ page, base }) {
      await mockSetup(page, { publicURL: 'https://deploy.example.com', paired: 1 });
      await page.goto(`${base}/setup.html`);
      await page.locator('#setup-key').fill('fixture-setup-key');
      await page.getByRole('button', { name: 'Unlock setup' }).click();
      await page.locator('#setup-form').waitFor();
      assert.equal(await page.locator('#public-url').inputValue(), 'https://deploy.example.com');
      assert.equal(await page.locator('#public-url').evaluate(input => input.readOnly), true, 'the installed address can be changed');
      assert.match(await page.locator('#paired-agent').textContent(), /already paired/);
      assert.equal(await page.locator('#extra-agent').evaluate(details => details.open), false, 'the paired agent still asks for connection details');
      assert.equal(await page.locator('[name=agent_token]').isDisabled(), true);
    },
  },
  {
    name: 'sign-in: errors show; the code is a header; only console return addresses are followed',
    mock: false,
    async run({ page, base }) {
      const logins = [];
      await page.route('**/api/v1/auth/login', route => {
        const request = route.request();
        logins.push({ otp: request.headers()['x-lidza-otp'], body: request.postDataJSON() });
        if (request.postDataJSON().password !== 'correct-password') return route.fulfill({ status: 401, json: { error: { message: 'Email or password is incorrect.' } } });
        return route.fulfill({ json: { ok: true } });
      });
      await page.route('**/api/control/**', route => route.fulfill({ json: [] }));
      await page.route('**/api/v1/**', route => route.request().url().endsWith('/auth/login') ? route.fallback() : route.fulfill({ json: {} }));
      const signIn = async (next, password) => {
        await page.goto(`${base}/login.html${next ? `?next=${encodeURIComponent(next)}` : ''}`);
        await page.locator('[name=email]').fill('owner@example.com');
        await page.locator('[name=password]').fill(password);
        await page.locator('[name=otp]').fill('123456');
        await page.getByRole('button', { name: /^Sign in/ }).click();
      };
      await signIn('', 'wrong-password');
      await page.locator('#login-error').filter({ hasText: 'Email or password is incorrect.' }).waitFor();
      assert.equal(logins[0].otp, '123456');
      assert.equal(logins[0].body.otp, undefined, 'the one-time code was sent in the body');
      await signIn('/console/servers/local/console?tab=1', 'correct-password');
      await page.waitForURL(url => url.pathname === '/console/servers/local/console' && url.search === '?tab=1');
      await signIn('https://evil.example.com/console/applications', 'correct-password');
      await page.waitForURL(url => url.origin === base && url.pathname === '/console/applications');
      await signIn('/setup.html', 'correct-password');
      await page.waitForURL(url => url.pathname === '/console/applications');
    },
  },
  {
    name: 'a signed-out console visit returns to the same view after sign-in',
    mock: false,
    async run({ page, base }) {
      await page.route('**/api/**', route => route.fulfill({ status: 401, json: { error: 'Please sign in.' } }));
      await page.goto(`${base}/console/databases/portal/schedule`);
      await page.waitForURL(url => url.pathname === '/login.html');
      assert.equal(new URL(page.url()).searchParams.get('next'), '/console/databases/portal/schedule');
    },
  },
]).catch(error => { console.error(error); process.exit(1); });
