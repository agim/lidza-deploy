// Phone and tablet layout of the console: no horizontal overflow on any main
// route at 390px, toolbars and stat grids laid out for narrow screens, notices
// instead of bare status paragraphs, overlays inside the viewport, touch
// targets of at least 40px, and a navigation drawer that closes on route change.
// Needs a console at TEST_WEB_URL (the Vite dev server or the Go server).
const { chromium } = require('playwright');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { mockConsole, app } = require('./fixtures/console.cjs');
const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8880';
const executablePath = process.env.CHROMIUM_BIN || (fs.existsSync('/usr/bin/chromium') ? '/usr/bin/chromium' : undefined);
const now = '2026-10-15T14:00:00Z';
const routes = ['/console/applications', '/console/deployments', '/console/servers', '/console/databases', '/console/remote-backups', '/console/integrations', '/console/defaults', '/console/updates', '/console/team', '/console/audit', '/console/errors', '/console/security', '/console/help', '/console/new-application', '/console/applications/portal/settings', '/console/servers/local', '/console/servers/new', '/console/servers/local/containers', '/console/servers/local/cleanup', '/console/servers/local/console', '/console/databases/portal', '/console/applications/portal/cache', '/console/deployments/build-one', '/console/applications/portal/logs', '/console/servers/local/ssh', '/console/servers/local/upgrade', '/console/account/two-factor'];

// Two servers and three runtimes show every toolbar filter; one host reports a
// failed inventory sync so the status notices render.
async function workspace(page) {
  await mockConsole(page);
  await page.route('**/api/control/status', route => route.fulfill({ json: { roles: ['admin'], github_app_connected: true, github_app_configured: true, github_public_https: true, native_inventory: [{ server_id: 'local', state: 'error', message: 'Agent native bindings do not match discovery; previous inventory retained.', checked: now }, { server_id: 'eu-1', state: 'synced', apps: 4, checked: now }] } }));
  await page.route('**/api/control/servers', route => route.fulfill({ json: [{ id: 'local', name: 'Production', url: 'http://127.0.0.1:9090' }, { id: 'eu-1', name: 'Frankfurt production (very-long-hostname-for-wrapping.example.internal)', url: 'https://agent-eu.example.com' }] }));
  await page.route('**/api/control/apps', route => route.request().method() === 'GET' ? route.fulfill({ json: [app, { ...app, id: 'api', repository: 'acme/api', domain: 'api.example.com', auto_deploy: false, stopped: true, runtime: 'rails', server_id: 'eu-1' }, { ...app, id: 'marketing-site-with-a-rather-long-identifier', repository: 'acme/marketing-site-with-a-rather-long-identifier', domain: 'www.marketing-site-with-a-rather-long-identifier.example.com', runtime: 'astro' }] }) : route.fallback());
}

async function open(page, route) {
  await page.goto(`${base}${route}`);
  await page.locator('#main-content h1').waitFor();
  await page.waitForTimeout(120);
}

// The page never scrolls sideways, and nothing in the main column reaches past
// the viewport unless it sits in a container that scrolls on its own.
async function noOverflow(page, context) {
  const result = await page.evaluate(() => {
    const scrolls = element => { for (let node = element.parentElement; node; node = node.parentElement) { const style = getComputedStyle(node); if (/(auto|scroll)/.test(style.overflowX) && node.scrollWidth > node.clientWidth) return true; } return false; };
    const clipped = [...document.querySelectorAll('#main-content *')].filter(element => element.getClientRects().length && !scrolls(element)).map(element => ({ element, rect: element.getBoundingClientRect() })).filter(({ rect }) => rect.right > innerWidth + 1 || rect.left < -1).map(({ element, rect }) => `${element.tagName.toLowerCase()}.${String(element.className).split(' ')[0]} ${Math.round(rect.left)}–${Math.round(rect.right)}`);
    return { viewport: innerWidth, page: document.documentElement.scrollWidth, clipped };
  });
  assert(result.page <= result.viewport, `${context}: page scrolls sideways (${result.page} > ${result.viewport})`);
  assert.deepEqual(result.clipped, [], `${context}: content past the viewport edge`);
}

const rect = locator => locator.evaluate(element => { const r = element.getBoundingClientRect(); return { left: r.left, right: r.right, top: r.top, bottom: r.bottom, width: r.width, height: r.height }; });
const near = (a, b, tolerance = 1.5) => Math.abs(a - b) <= tolerance;

async function phoneApplications(page, width) {
  const context = `${width} applications`;
  await open(page, '/console/applications');
  const content = await rect(page.locator('#main-content .page > .page-header'));
  // Toolbar: search field on the first row with the view toggle, selects two
  // per row with the lone last one spanning the row, nothing centred.
  const search = await rect(page.locator('.toolbar .search-field'));
  const toggle = await rect(page.locator('.toolbar .view-toggle'));
  assert(near(search.left, content.left), `${context}: search field starts at the content edge`);
  assert(near(search.top, toggle.top, 4), `${context}: view toggle shares the search row`);
  assert(search.width >= content.width * .6, `${context}: search field is ${search.width}px wide of ${content.width}px`);
  const selects = page.locator('.toolbar [data-slot=select-trigger]');
  assert.equal(await selects.count(), 3, `${context}: three filters`);
  const [first, second, third] = await Promise.all([0, 1, 2].map(index => rect(selects.nth(index))));
  assert(near(first.left, content.left), `${context}: first filter starts at the content edge`);
  assert(near(first.top, second.top) && near(first.width, second.width), `${context}: filters share a two-column row`);
  assert(near(second.right, content.right), `${context}: second filter ends at the content edge`);
  assert(third.top > first.bottom && near(third.width, content.width), `${context}: a lone filter spans the row`);
  // Stat grid: one compact column under 480px.
  const cards = page.locator('.stats-grid > .card');
  assert.equal(await cards.count(), 3);
  for (const index of [0, 1, 2]) { const card = await rect(cards.nth(index)); assert(near(card.left, content.left) && near(card.width, content.width), `${context}: stat card ${index} spans the column`); assert(card.height < 110, `${context}: stat card ${index} is compact (${card.height}px)`); }
  // Inventory status lines are notice rows with an icon, not bare paragraphs.
  assert.equal(await page.locator('#main-content .page > p[role=status]').count(), 0, `${context}: no bare status paragraphs`);
  const notices = page.locator('#main-content .notice-stack [role=status]');
  assert.equal(await notices.count(), 2, `${context}: inventory notices`);
  assert.equal(await notices.locator('svg').count(), 2, `${context}: notice icons`);
  assert((await notices.first().getAttribute('class')).includes('alert-warning'), `${context}: failed sync is a warning`);
  // Primary page action spans the width; buttons meet the touch target size.
  const primary = await rect(page.locator('.page-actions .button-primary'));
  assert(near(primary.width, content.width), `${context}: primary action is full width`);
  assert.equal(await page.evaluate(() => matchMedia('(pointer: coarse)').matches), true, `${context}: coarse pointer emulated`);
  const small = await page.evaluate(() => [...document.querySelectorAll('#main-content button, #main-content a.button, .topbar button')].filter(element => element.getClientRects().length && !element.matches('[data-slot=switch], [role=switch]')).map(element => ({ label: element.getAttribute('aria-label') || element.textContent.trim().slice(0, 30), height: element.getBoundingClientRect().height })).filter(item => item.height < 39.5));
  assert.deepEqual(small, [], `${context}: touch targets under 40px`);
  // Overlays stay inside the viewport.
  await page.getByRole('combobox', { name: 'Filter by application status' }).click();
  const list = await rect(page.locator('[data-slot=select-content]'));
  assert(list.left >= 0 && list.right <= width && list.bottom <= 844, `${context}: select list inside the viewport`);
  await page.keyboard.press('Escape');
  await page.locator('[data-app-card]').first().getByRole('button', { name: 'More' }).click();
  const menu = await rect(page.locator('.menu-content'));
  assert(menu.left >= 0 && menu.right <= width && menu.top >= 0, `${context}: action menu inside the viewport`);
  await page.getByText('Delete application').click();
  const dialog = await rect(page.locator('.dialog'));
  assert(dialog.left >= 0 && dialog.right <= width && dialog.top >= 0 && dialog.bottom <= 844, `${context}: dialog inside the viewport`);
  await page.keyboard.press('Escape');
  await page.locator('.dialog').waitFor({ state: 'hidden' });
  await page.keyboard.press('Escape');
  // The navigation drawer fits the screen and closes when a route opens.
  await page.getByRole('button', { name: 'Open menu' }).click();
  const drawer = page.locator('.mobile-sidebar');
  const drawerRect = await rect(drawer);
  assert(drawerRect.width <= width - 48, `${context}: drawer leaves the page visible (${drawerRect.width}px)`);
  await drawer.getByRole('link', { name: 'Servers', exact: true }).click();
  await page.waitForURL('**/console/servers');
  await drawer.waitFor({ state: 'hidden' });
  assert.equal(await page.evaluate(() => scrollX), 0, `${context}: content not pushed sideways by the drawer`);
}

async function tabletApplications(page) {
  await open(page, '/console/applications');
  const search = await rect(page.locator('.toolbar .search-field'));
  const toggle = await rect(page.locator('.toolbar .view-toggle'));
  const last = await rect(page.locator('.toolbar [data-slot=select-trigger]').last());
  assert(near(search.top, toggle.top, 4) && near(search.top, last.top, 4), '768 applications: toolbar fits on one row');
  assert(search.width >= 180, `768 applications: search field keeps its placeholder (${search.width}px)`);
  const cards = page.locator('.stats-grid > .card');
  const [a, b, c] = await Promise.all([0, 1, 2].map(index => rect(cards.nth(index))));
  assert(near(a.top, b.top) && near(b.top, c.top), '768 applications: three stat columns');
  await open(page, '/console/errors');
  const title = await rect(page.locator('.section-header h2').first());
  const filters = await rect(page.locator('.activity-filters'));
  assert(filters.top >= title.bottom, '768 errors: filters sit under the section title');
}

(async () => {
  const browser = await chromium.launch({ executablePath, headless: true, args: ['--no-sandbox'] });
  try {
    for (const width of [390, 360]) {
      const page = await browser.newPage({ viewport: { width, height: 844 }, isMobile: true, hasTouch: true });
      const errors = [];
      page.on('pageerror', error => errors.push(error.message));
      await workspace(page);
      if (width === 390) for (const route of routes) { await open(page, route); await noOverflow(page, `${width} ${route}`); }
      await phoneApplications(page, width);
      assert.deepEqual(errors, [], `${width}: browser errors`);
      await page.close();
    }
    const tablet = await browser.newPage({ viewport: { width: 768, height: 1024 }, isMobile: true, hasTouch: true });
    await workspace(tablet);
    for (const route of routes) { await open(tablet, route); await noOverflow(tablet, `768 ${route}`); }
    await tabletApplications(tablet);
    await tablet.close();
    console.log(`PASS: mobile layout, ${routes.length} routes at 390 and 768, applications toolbar and overlays at 390/360`);
  } finally { await browser.close(); }
})().catch(error => { console.error(error.message); process.exitCode = 1; });
