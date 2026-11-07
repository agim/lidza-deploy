// Layout regression check for the spacing system (ui/src/tokens.css,
// ui/src/layout.css, ui/src/components/layout). Opens every console page with
// fixture data at desktop and phone width and measures the rendered boxes:
//   - no horizontal overflow;
//   - a section heading block starts at least 16px below the block before it;
//   - sibling cards are --page-gap (24px ±1) apart, in a column or a grid;
//   - blocks in a layout container (page, section, stack, card, form) never touch;
//   - a Section's header sits --section-gap (12px ±1) above its content;
//   - buttons in a row are at least --control-gap (8px) apart.
// Runs against TEST_WEB_URL, or serves web/static itself (build first).
const { chromium } = require('playwright');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { mockConsole } = require('./fixtures/console.cjs');
const { startStaticServer } = require('./fixtures/static-server.cjs');

const routes = ['/console/applications', '/console/deployments', '/console/servers', '/console/databases', '/console/remote-backups', '/console/integrations', '/console/defaults', '/console/updates', '/console/team', '/console/audit', '/console/errors', '/console/security', '/console/help', '/console/new-application', '/console/applications/portal/settings', '/console/servers/local', '/console/servers/new', '/console/servers/local/containers', '/console/servers/local/cleanup', '/console/servers/local/console', '/console/databases/portal', '/console/applications/portal/cache', '/console/deployments/build-one', '/console/applications/portal/logs', '/console/servers/local/ssh', '/console/servers/local/upgrade', '/console/account/two-factor', '/console/deleted-apps', '/console/control-panel'];
const widths = [1440, 390];

// A private build with a bound update channel: the channel card shows the
// verified-release box, then the repository field and its actions.
const boundChannel = { control_version: 'v0.2.83', latest: 'v0.2.84', automatic_updates_supported: true, settings: { latest: 'v0.2.84', automatic: false, checked: '2026-10-15T14:00:00Z' }, channel: { configured: true, repository: 'acme/deploy-private', installation: 42, keys: ['release-2026'], keys_pinned: true, bundle_version: 'v0.2.84', key_id: 'release-2026' }, servers: [{ server_id: 'local', name: 'Production', current: 'v0.2.83', state: 'idle', supported: true }] };

function measure() {
  const visible = element => element.getClientRects().length > 0 && getComputedStyle(element).visibility !== 'hidden' && getComputedStyle(element).position !== 'absolute' && getComputedStyle(element).position !== 'fixed';
  const rect = element => element.getBoundingClientRect();
  const label = element => (element.className && typeof element.className === 'string' ? `.${element.className.trim().split(/\s+/).slice(0, 2).join('.')}` : element.tagName.toLowerCase()) + ` "${(element.textContent || '').trim().slice(0, 32)}"`;
  const children = element => [...element.children].filter(visible);
  const main = document.querySelector('#main-content');
  const issues = [];
  if (!main) return ['#main-content is missing'];

  if (document.documentElement.scrollWidth > innerWidth + 1) issues.push(`horizontal overflow: ${document.documentElement.scrollWidth}px > ${innerWidth}px`);

  // Section heading blocks: climb to the outermost block the heading starts.
  for (const heading of main.querySelectorAll('.section-header, section.section')) {
    if (!visible(heading)) continue;
    let block = heading;
    while (block.parentElement && block.parentElement !== main && !block.parentElement.matches('.page') && children(block.parentElement)[0] === block) block = block.parentElement;
    const siblings = children(block.parentElement || main);
    const previous = siblings[siblings.indexOf(block) - 1];
    if (!previous) continue;
    const space = rect(block).top - rect(previous).bottom;
    if (rect(block).top >= rect(previous).top && space < 15.5) issues.push(`heading ${label(heading)} is ${space.toFixed(1)}px below ${label(previous)} (min 16)`);
  }

  // Sibling cards: nearest card above (overlapping columns) and to the left (overlapping rows).
  for (const card of main.querySelectorAll('.card')) {
    if (!visible(card) || card.closest('.stats-grid')) continue;
    const box = rect(card);
    const siblings = children(card.parentElement).filter(other => other !== card);
    const above = siblings.filter(other => rect(other).bottom <= box.top + 1 && rect(other).left < box.right - 1 && rect(other).right > box.left + 1).sort((a, b) => rect(b).bottom - rect(a).bottom)[0];
    if (above && above.matches('.card')) {
      const space = box.top - rect(above).bottom;
      if (Math.abs(space - 24) > 1) issues.push(`cards ${label(above)} / ${label(card)} are ${space.toFixed(1)}px apart vertically (want 24)`);
    }
    const left = siblings.filter(other => rect(other).right <= box.left + 1 && rect(other).top < box.bottom - 1 && rect(other).bottom > box.top + 1).sort((a, b) => rect(b).right - rect(a).right)[0];
    if (left && left.matches('.card')) {
      const space = box.left - rect(left).right;
      if (Math.abs(space - 24) > 1) issues.push(`cards ${label(left)} / ${label(card)} are ${space.toFixed(1)}px apart horizontally (want 24)`);
    }
  }

  // Blocks inside layout containers never touch.
  for (const container of [main, ...main.querySelectorAll('.page, .section, .section-body, .stack:not(.stack-row), .card, form.form-stack, .form-card > form, .settings-stack, .card fieldset, form fieldset')]) {
    if (!visible(container)) continue;
    const style = getComputedStyle(container);
    if (!['flex', 'grid'].includes(style.display) || (style.display === 'flex' && !style.flexDirection.startsWith('column'))) continue;
    const blocks = children(container).filter(element => rect(element).height > 0);
    for (let i = 1; i < blocks.length; i++) {
      const a = rect(blocks[i - 1]), b = rect(blocks[i]);
      if (b.left >= a.right - 1 || b.right <= a.left + 1) continue; // side by side in a grid row
      const before = getComputedStyle(blocks[i - 1]), after = getComputedStyle(blocks[i]);
      if (parseFloat(before.borderBottomWidth) > 0 || parseFloat(after.borderTopWidth) > 0) {
        if (Math.abs(b.top - a.bottom) <= 1 && (parseFloat(before.borderBottomWidth) > 0) !== (parseFloat(after.borderTopWidth) > 0) && !blocks[i].matches('.card') && !blocks[i - 1].matches('.card')) continue; // bands of one box divided by a rule
      }
      const space = b.top - a.bottom;
      if (space < 7.5) issues.push(`${label(blocks[i - 1])} and ${label(blocks[i])} are ${space.toFixed(1)}px apart in ${label(container)} (min 8)`);
    }
  }

  // Section primitive: header to content is --section-gap.
  for (const section of main.querySelectorAll('section.section')) {
    const [header, content] = children(section);
    if (!header || !content) continue;
    const space = rect(content).top - rect(header).bottom;
    if (Math.abs(space - 12) > 1) issues.push(`section ${label(header)} header is ${space.toFixed(1)}px above its content (want 12)`);
  }

  // Buttons in a row: siblings at least 8px apart. Segmented toggles and tab lists are one control.
  const buttons = [...main.querySelectorAll('button, a.button')].filter(element => visible(element) && !element.closest('.view-toggle, [role=tablist], [data-slot=select-trigger], .search-field, .env-value-control, .copy-control') && !element.matches('[data-slot=switch], [role=checkbox], [role=radio], .step'));
  const byParent = new Map();
  for (const button of buttons) byParent.set(button.parentElement, [...(byParent.get(button.parentElement) || []), button]);
  for (const group of byParent.values()) {
    for (let i = 0; i < group.length; i++) for (let j = i + 1; j < group.length; j++) {
      const a = rect(group[i]), b = rect(group[j]);
      const space = Math.max(b.left - a.right, a.left - b.right, b.top - a.bottom, a.top - b.bottom);
      if (space < 7.5) issues.push(`buttons ${label(group[i])} / ${label(group[j])} are ${space.toFixed(1)}px apart (min 8)`);
    }
  }
  return issues;
}

(async () => {
  const started = Date.now();
  const server = process.env.TEST_WEB_URL ? undefined : await startStaticServer();
  const base = process.env.TEST_WEB_URL || server.url;
  const executablePath = process.env.TEST_CHROMIUM_PATH || (fs.existsSync('/usr/bin/chromium') ? '/usr/bin/chromium' : undefined);
  const browser = await chromium.launch({ executablePath, args: ['--no-sandbox'] });
  const failures = [];
  try {
    for (const width of widths) {
      const page = await browser.newPage({ viewport: { width, height: 900 } });
      const errors = [];
      page.on('pageerror', error => errors.push(error.message));
      await mockConsole(page);
      await page.route('**/api/control/updates', route => route.request().method() === 'GET' ? route.fulfill({ json: boundChannel }) : route.fallback());
      for (const route of routes) {
        await page.goto(`${base}${route}`);
        await page.locator('#main-content h1').first().waitFor();
        await page.waitForTimeout(150);
        for (const issue of await page.evaluate(measure)) failures.push(`${width}px ${route}: ${issue}`);
        if (errors.length) failures.push(`${width}px ${route}: browser errors ${JSON.stringify(errors.splice(0))}`);
      }

      // Updates: the bound repository is a value, not a placeholder, and the
      // field group sits clear of the verified-release box above it.
      await page.goto(`${base}/console/updates`);
      const field = page.locator('.field', { hasText: 'Release repository' });
      await field.waitFor();
      const input = field.locator('input');
      assert.equal(await input.inputValue(), 'acme/deploy-private', `${width}px updates: bound repository is not shown as the field value`);
      const gap = await field.evaluate(element => {
        const card = element.closest('.card');
        const list = card.querySelector('.simple-list');
        return element.getBoundingClientRect().top - list.getBoundingClientRect().bottom;
      });
      if (gap < 11.5) failures.push(`${width}px /console/updates: Release repository field is ${gap.toFixed(1)}px below the channel facts (min 12)`);
      await page.close();
    }
  } finally {
    await browser.close();
    await server?.close();
  }
  if (failures.length) {
    console.error(`FAIL: ${failures.length} layout spacing problem(s)\n${failures.join('\n')}`);
    process.exit(1);
  }
  console.log(`PASS: ${routes.length} console pages at ${widths.join(' and ')}px keep token spacing (${((Date.now() - started) / 1000).toFixed(1)}s)`);
})().catch(error => { console.error(error.message || error); process.exit(1); });
