const { chromium } = require('playwright');
const assert = require('node:assert/strict');
const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8880';
const branches = ['main', 'codex/kulla-rails-port', 'dependabot/github_actions/actions/checkout-7', 'dependabot/github_actions/actions/setup-go-7', 'feature/' + 'a'.repeat(160)];
(async () => {
 const browser = await chromium.launch({ executablePath: '/usr/bin/chromium', args: ['--no-sandbox'] });
 try {
  for (const width of [320, 390, 600, 960, 1440]) {
   const page = await browser.newPage({ viewport: { width, height: 844 } }), errors = [], saved = [];
   page.on('pageerror', e => errors.push(e.message));
   const app = { id: 'kulla-lidza', server_id: 'local', repository: 'agim/kulla-lidza', branch: 'main', domain: 'kulla.example.com' };
   await page.route('**/api/control/**', async route => {
    const request = route.request(), path = new URL(request.url()).pathname;
    let json = [];
    if (path.endsWith('/status')) json = { roles: ['admin'] };
    else if (path.endsWith('/apps')) json = [app];
    else if (path.endsWith('/servers')) json = [{ id: 'local', name: 'Production' }];
    else if (path.endsWith('/deployments')) json = { deployments: [] };
    else if (path.endsWith('/github/branches')) json = branches.map(name => ({ name }));
    else if (path.endsWith('/settings')) {
     if (request.method() === 'PATCH') { saved.push(request.postDataJSON()); json = { status: 'accepted' }; }
     else json = { ...app, environment: [], database_bindings: {} };
    }
    await route.fulfill({ json });
   });
   const fits = async label => {
    const bounds = await page.locator('input[name=branch]').boundingBox();
    assert(bounds.x >= 0 && bounds.x + bounds.width <= width, `${label}: branch input overflows at ${width}px`);
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${label}: page overflows`);
   };
   await page.goto(`${base}/console.html#page/settings/kulla-lidza`);
   await page.getByRole('button', { name: 'Load', exact: true }).click();
   await page.locator('#settings-branches option').last().waitFor({ state: 'attached' });
   assert.equal(await page.locator('#settings-branches option').count(), branches.length);
   await page.locator('input[name=branch]').fill(branches[4]);
   await fits('settings');
   await page.getByRole('button', { name: 'Save changes' }).click();
   await page.waitForURL(url => url.hash === '#applications');
   assert.equal(saved[0].branch, branches[4], 'Saving truncated the exact branch name');
   await page.goto(`${base}/console.html#new-application`);
   await page.locator('input[name=repository]').fill(app.repository);
   await page.locator('input[name=id]').fill('branch-fixture');
   await page.locator('input[name=branch]').fill(branches[3]);
   await fits('create');
   await page.getByRole('button', { name: 'Continue' }).click();
   await page.getByRole('button', { name: 'Back', exact: true }).click();
   assert.equal(await page.locator('input[name=branch]').inputValue(), branches[3], 'Wizard navigation discarded the selected branch');
   assert.deepEqual(errors, []); await page.close();
  }
  console.log('PASS: exact long branch values load, fit, save and survive wizard navigation at 320/390/600/960/1440px');
 } finally { await browser.close(); }
})().catch(e => { console.error(e); process.exit(1); });
