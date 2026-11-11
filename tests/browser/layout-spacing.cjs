// Structure and layout regression check. Structure: every index uses the
// index template (no tabs, no detail sections, names link to resources),
// every tab of every resource uses the detail template with the tab the URL
// names selected, breadcrumbs link every segment but the current one (and
// collapse on phones), breadcrumb links open real views, earlier URLs
// redirect (docs/console-structure.md). Layout: the spacing system (ui/src/tokens.css,
// ui/src/layout.css, ui/src/components/layout). Opens every console page with
// fixture data at desktop and phone width and measures the rendered boxes:
//   - no horizontal overflow;
//   - a section heading block starts at least 16px below the block before it;
//   - sibling cards are --page-gap (24px ±1) apart, in a column or a grid;
//   - blocks in a layout container (page, section, stack, card, form) never touch;
//   - a Section's header sits --section-gap (12px ±1) above its content;
//   - buttons in a row are at least --control-gap (8px) apart;
//   - an icon shares a row with the text beside it: its vertical centre lies in
//     the first line box of that text (check lists, inline icon + label);
//   - key-value summaries are the shared SummaryList (dl.summary-list), their
//     values stay inside the list and use the --text-body font size;
//   - buttons are the standard height (--control-height, --control-height-sm)
//     unless their label wraps;
//   - a field hint starts at the left edge of its control, right below it;
//   - a detail tab list shows no scrollbar, and fades an edge when it overflows;
//   - an Actions menu item's highlight and focus ring stay inside the menu's
//     content box, and keyboard focus stays visible (application and server);
//   - the sidebar's panel identity links to the Control panel, its server
//     status links to Servers, and its account menu holds Account and Sign out
//     inside the menu (desktop sidebar and phone drawer).
// TEST_SCREENSHOTS=<dir> saves each open Actions menu with a focused item, the
// sidebar and its open account menu.
// Runs against TEST_WEB_URL, or serves web/static itself (build first).
const assert = require('node:assert/strict');
const { mockConsole, app } = require('./fixtures/console.cjs');
const { mockServerHost } = require('./fixtures/server-host.cjs');
const { launch, serve } = require('./fixtures/harness.cjs');
const { views, redirects } = require('./fixtures/views.cjs');
const widths = [1440, 390];

/**
 * menuFocus focuses every enabled item of the open menu (after a keyboard
 * open, so :focus-visible applies) and reports items whose highlight or
 * focus ring (outline plus outset box-shadow) leaves the menu's content box,
 * or whose keyboard focus is not visible.
 */
function menuFocus(name) {
  const menu = document.querySelector('[role=menu][data-state=open]');
  if (!menu) return [`${name}: no open menu`];
  const style = getComputedStyle(menu), box = menu.getBoundingClientRect();
  const px = value => parseFloat(value) || 0;
  const content = { left: box.left + px(style.borderLeftWidth) + px(style.paddingLeft), right: box.right - px(style.borderRightWidth) - px(style.paddingRight) };
  const issues = [];
  const items = [...menu.querySelectorAll('[role^=menuitem]')].filter(item => !item.matches('[data-disabled], [aria-disabled=true]'));
  if (items.length < 2) issues.push(`${name}: only ${items.length} enabled menu items`);
  for (const item of items) {
    item.focus();
    const own = getComputedStyle(item), rect = item.getBoundingClientRect();
    const text = item.textContent.trim().slice(0, 30);
    if (!item.matches(':focus-visible')) { issues.push(`${name} "${text}": keyboard focus is not :focus-visible`); continue; }
    const outline = own.outlineStyle !== 'none' ? px(own.outlineWidth) : 0;
    if (outline < 1) issues.push(`${name} "${text}": keyboard focus shows no ring`);
    let ring = Math.max(0, outline + px(own.outlineOffset));
    for (const shadow of own.boxShadow === 'none' ? [] : own.boxShadow.split(/,(?![^(]*\))/)) {
      if (/inset/.test(shadow)) continue;
      const [x = 0, y = 0, blur = 0, spread = 0] = (shadow.replace(/rgba?\([^)]*\)|#[0-9a-f]+|[a-z]+\([^)]*\)/gi, '').match(/-?[\d.]+px/g) || []).map(parseFloat);
      if (blur + spread + Math.abs(x) + Math.abs(y) > 0) ring = Math.max(ring, Math.abs(x) + blur + spread);
    }
    if (rect.left - ring < content.left - .5 || rect.right + ring > content.right + .5) issues.push(`${name} "${text}": focus ring spans ${(rect.left - ring).toFixed(1)}–${(rect.right + ring).toFixed(1)}, outside the menu content ${content.left.toFixed(1)}–${content.right.toFixed(1)}`);
  }
  return issues;
}

/** structure checks the page against the template its route promises. */
function structure([path, template, tab]) {
  const issues = [];
  const root = document.querySelector('[data-template]');
  if (!root) return [`no template root ([data-template]) on ${path}`];
  if (root.dataset.template !== template) issues.push(`template is ${root.dataset.template}, want ${template}`);
  // Breadcrumbs: every segment but the last is a real link; the last is the current page.
  const items = [...document.querySelectorAll('nav[data-breadcrumbs] ol > li:not(.breadcrumb-collapse)')];
  if (items.length < 2) issues.push(`breadcrumbs have ${items.length} segments (want at least 2)`);
  items.forEach((item, i) => {
    const link = item.querySelector(':scope > a[href]');
    if (i < items.length - 1) { if (!link || !link.getAttribute('href').startsWith('/console/')) issues.push(`breadcrumb ${i + 1} "${item.textContent.trim()}" is not a console link`); }
    else if (link || !item.querySelector('[aria-current=page]')) issues.push('the last breadcrumb must be the current page, not a link');
  });
  if (innerWidth < 640 && items.length > 2) {
    const collapse = document.querySelector('.breadcrumb-collapse');
    if (!collapse || !collapse.getClientRects().length) issues.push('phone breadcrumbs do not collapse the middle segments into a menu');
    if (items.slice(1, -1).some(item => item.getClientRects().length)) issues.push('phone breadcrumbs still show middle segments');
  }
  const tablist = root.querySelector('[role=tablist]');
  if (template === 'index') {
    if (tablist) issues.push('an index has a tab list');
    if (root.querySelector('[data-danger-zone], .tab-panel, [role=tabpanel], .migration-plan, pre')) issues.push('an index holds detail sections');
    if (!root.querySelector('[data-resource-list]')) issues.push('an index has no resource list');
    const links = [...root.querySelectorAll('.resource-row td:first-child')];
    if (path !== '/console/audit' && links.some(cell => !cell.querySelector('a[href^="/console/"]'))) issues.push('an index row name does not link to its resource');
  }
  if (template === 'detail') {
    if (!tablist) issues.push('a detail page has no tab list');
    else {
      const selected = tablist.querySelector('[role=tab][aria-selected=true]');
      if (!selected || selected.dataset.tab !== tab) issues.push(`selected tab is ${selected?.dataset.tab}, want ${tab}`);
      if (root.dataset.tab !== tab) issues.push(`template tab is ${root.dataset.tab}, want ${tab}`);
      if ([...tablist.querySelectorAll('[role=tab]')].some(item => !item.getAttribute('href'))) issues.push('a tab is not a link');
      // The tab list scrolls without a visible scrollbar (no gutter on either axis).
      const style = getComputedStyle(tablist);
      const gutterY = tablist.offsetHeight - tablist.clientHeight - parseFloat(style.borderTopWidth) - parseFloat(style.borderBottomWidth);
      const gutterX = tablist.offsetWidth - tablist.clientWidth - parseFloat(style.borderLeftWidth) - parseFloat(style.borderRightWidth);
      if (style.scrollbarWidth !== 'none' || Math.round(gutterY) !== 0 || Math.round(gutterX) !== 0) issues.push(`the tab list shows a scrollbar (scrollbar-width ${style.scrollbarWidth}, gutters ${gutterX}px / ${gutterY}px)`);
      if (tablist.scrollWidth > tablist.clientWidth + 1 && (style.overflowX !== 'auto' || !tablist.dataset.overflow)) issues.push(`an overflowing tab list does not scroll with a faded edge (overflow-x ${style.overflowX}, data-overflow ${tablist.dataset.overflow})`);
    }
    if (!root.querySelector(`[data-tab-panel="${tab}"]`)) issues.push(`no panel for tab ${tab}`);
  }
  return issues;
}

// A private build with a bound update channel: the channel card shows the
// verified-release box, then the repository field and its actions.
const boundChannel = { control_version: 'v0.2.83', latest: 'v0.2.84', automatic_updates_supported: true, settings: { latest: 'v0.2.84', automatic: false, checked: '2026-10-15T14:00:00Z' }, channel: { configured: true, repository: 'acme/deploy-private', installation: 42, keys: ['release-2026'], keys_pinned: true, bundle_version: 'v0.2.84', key_id: 'release-2026' }, servers: [{ server_id: 'local', name: 'Production', current: 'v0.2.83', state: 'idle', supported: true }] };

// An application move plan with a long hostname, IPv6 and reverse DNS checks.
const appMovePlan = {
  app_id: 'portal', source_server: 'local', target_server: 'edge', source_runtime: 'container', runtime: 'rails', target_runtime: 'native', runtime_choices: ['native', 'container'],
  repository: 'acme/portal', branch: 'main', commit: '0123456789abcdef0123456789abcdef01234567', hostnames: ['portal.' + 'very-long-subdomain-'.repeat(3) + 'example.com'],
  environment_keys: ['DATABASE_URL'], copied_keys: ['RAILS_MASTER_KEY'], destination_keys: ['DATABASE_URL'], redis: false,
  databases: [{ id: 'portal', engine: 'postgresql', mode: 'local', tables: 12, size_bytes: 4 * 2 ** 20, action: 'copy' }], volumes: [],
  addresses: ['203.0.113.9', '2001:0db8:85a3:0000:0000:8a2e:0370:7334'], warnings: [], notes: [], blockers: [],
  mail: { ports: [{ port: 25, protocol: 'tcp' }], ptr: [{ address: '203.0.113.9', names: ['edge.example.com'], want: 'portal.example.com', ok: true }, { address: '2001:0db8:85a3:0000:0000:8a2e:0370:7334', names: [], want: 'portal.example.com', ok: false }], records: [] },
};

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

  // Icons beside text share its first line. A text node or inline element
  // after the icon (or a row flex/grid) means "icon + label"; an icon stacked
  // above a block heading (empty states, tiles) is a separate block.
  const firstLine = node => {
    const walker = document.createTreeWalker(node, NodeFilter.SHOW_TEXT, { acceptNode: text => text.textContent.trim() ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_REJECT });
    const text = node.nodeType === Node.TEXT_NODE ? (node.textContent.trim() ? node : null) : walker.nextNode();
    if (!text) return undefined;
    const range = document.createRange();
    range.selectNodeContents(text);
    return [...range.getClientRects()].find(box => box.width > 0 && box.height > 0);
  };
  for (const icon of main.querySelectorAll('svg')) {
    if (!visible(icon) || icon.closest('[aria-hidden=true]:not(svg), .sr-only, button, a.button, [role=tab], .progress, .badge, .status-dot')) continue;
    const parent = icon.parentElement, style = getComputedStyle(parent);
    const row = (style.display.includes('flex') && !style.flexDirection.startsWith('column')) || style.display.includes('inline');
    let next = icon.nextSibling;
    while (next && ((next.nodeType === Node.TEXT_NODE && !next.textContent.trim()) || (next.nodeType === Node.ELEMENT_NODE && (!visible(next) || !next.textContent.trim())))) next = next.nextSibling;
    if (!next) continue;
    const inline = next.nodeType === Node.TEXT_NODE || getComputedStyle(next).display.startsWith('inline');
    if (!row && !inline) continue;
    const line = firstLine(next);
    if (!line) continue;
    const box = rect(icon), middle = (box.top + box.bottom) / 2;
    if (middle < line.top - 1 || middle > line.bottom + 1) issues.push(`icon in ${label(parent)} is not on the row of its text (icon centre ${middle.toFixed(1)}, text line ${line.top.toFixed(1)}–${line.bottom.toFixed(1)})`);
  }

  // Check lists: the status icon and the text share a row.
  for (const item of main.querySelectorAll('.check-list-item')) {
    const icon = item.querySelector('.check-list-icon'), text = item.querySelector('.check-list-text');
    if (!icon || !text || !visible(item)) { issues.push(`check list item ${label(item)} lacks its icon or text`); continue; }
    const line = firstLine(text), box = rect(icon), middle = (box.top + box.bottom) / 2;
    if (line && (middle < line.top - 1 || middle > line.bottom + 1)) issues.push(`check list item ${label(item)}: icon centre ${middle.toFixed(1)} outside the text's first line ${line.top.toFixed(1)}–${line.bottom.toFixed(1)}`);
    if (line && box.right > line.left + 1) issues.push(`check list item ${label(item)}: icon is not left of its text`);
  }

  // Key-value summaries: the shared SummaryList, values inside it, body font size.
  const root = getComputedStyle(document.documentElement);
  const bodySize = parseFloat(root.getPropertyValue('--text-body'));
  for (const list of main.querySelectorAll('dl')) {
    if (!visible(list)) continue;
    if (!list.matches('.summary-list')) { issues.push(`${label(list)} is a hand-rolled dl; use SummaryList`); continue; }
    const box = rect(list);
    for (const value of list.querySelectorAll(':scope > div > dd')) {
      if (!visible(value)) continue;
      const own = rect(value);
      if (value.scrollWidth > value.clientWidth + 1 || own.right > box.right + 1) issues.push(`summary value ${label(value)} overflows its list (${own.right.toFixed(1)} > ${box.right.toFixed(1)}, scroll ${value.scrollWidth} > ${value.clientWidth})`);
      const size = parseFloat(getComputedStyle(value).fontSize);
      if (!(Math.abs(size - bodySize) < .5)) issues.push(`summary value ${label(value)} is ${size}px; want --text-body (${bodySize}px)`);
    }
  }

  // Buttons are the standard height unless the label wraps onto more lines.
  const standard = { md: parseFloat(root.getPropertyValue('--control-height')), sm: parseFloat(root.getPropertyValue('--control-height-sm')) };
  for (const button of main.querySelectorAll('.button-md, .button-sm')) {
    if (!visible(button) || button.closest('.view-toggle, [role=tablist]')) continue;
    const want = standard[button.matches('.button-sm') ? 'sm' : 'md'];
    if (!want) { issues.push('--control-height tokens are missing'); break; }
    const height = rect(button).height;
    const range = document.createRange();
    range.selectNodeContents(button);
    const tops = new Set([...range.getClientRects()].filter(box => box.height > 0 && box.width > 0 && box.height < height).map(box => Math.round(box.top)));
    const wraps = [...tops].sort((a, b) => a - b).some((top, index, all) => index > 0 && top - all[0] > 6);
    if (height < want - 1 || (height > want + 1 && !wraps)) issues.push(`button ${label(button)} is ${height.toFixed(1)}px high; want ${want}px`);
  }

  // Field hints sit under their control, flush with its left edge.
  for (const hint of main.querySelectorAll('.field > .field-hint')) {
    if (!visible(hint)) continue;
    const field = hint.parentElement;
    const control = [...field.children].filter(child => visible(child) && !child.matches('.field-label, .field-hint, .field-error')).pop();
    if (!control) continue;
    const a = rect(control), b = rect(hint);
    if (Math.abs(b.left - a.left) > 1) issues.push(`hint ${label(hint)} starts ${(b.left - a.left).toFixed(1)}px from its control's left edge`);
    if (b.top < a.bottom - 1 || b.top - a.bottom > 12.5) issues.push(`hint ${label(hint)} is ${(b.top - a.bottom).toFixed(1)}px below its control (0–12)`);
  }
  return issues;
}

(async () => {
  const started = Date.now();
  const server = await serve();
  const base = server.url;
  const browser = await launch();
  const failures = [];
  try {
    for (const width of widths) {
      const page = await browser.newPage({ viewport: { width, height: 900 } });
      const errors = [];
      page.on('pageerror', error => errors.push(error.message));
      const state = await mockConsole(page);
      await page.route('**/api/control/updates', route => route.request().method() === 'GET' ? route.fulfill({ json: boundChannel }) : route.fallback());
      await mockServerHost(page, 'local');
      const hrefs = new Set();
      for (const view of views) {
        const route = view[0];
        await page.goto(`${base}${route}`);
        await page.locator('#main-content h1').first().waitFor();
        if (view[1] === 'detail') await page.locator(`[data-tab-panel="${view[2]}"]`).waitFor({ timeout: 5000 }).catch(() => {});
        await page.waitForTimeout(150);
        for (const issue of await page.evaluate(structure, view)) failures.push(`${width}px ${route}: ${issue}`);
        for (const issue of await page.evaluate(measure)) failures.push(`${width}px ${route}: ${issue}`);
        for (const href of await page.$$eval('nav[data-breadcrumbs] a[href]', links => links.map(link => link.getAttribute('href')))) hrefs.add(href);
        if (errors.length) failures.push(`${width}px ${route}: browser errors ${JSON.stringify(errors.splice(0))}`);
      }
      // Every breadcrumb link opens a real view.
      for (const href of hrefs) {
        await page.goto(`${base}${href}`);
        await page.locator('#main-content h1').first().waitFor();
        if (await page.locator('[data-template=not-found]').count()) failures.push(`${width}px breadcrumb link ${href} opens Not found`);
      }
      for (const [from, to] of redirects) {
        await page.goto(`${base}${from}`);
        await page.locator('#main-content h1').first().waitFor();
        const at = await page.evaluate(() => location.pathname + location.search);
        if (at !== to) failures.push(`${width}px ${from} redirects to ${at}, want ${to}`);
      }

      // Server tabs: the tab list is one control on one row; the open tab's
      // panel shows its content; the tuning tab shows failed and waiting rows.
      for (const [route] of views.filter(([path, template]) => template === 'detail' && path.startsWith('/console/servers/local'))) {
        await page.goto(`${base}${route}`);
        const tabs = page.getByRole('tablist', { name: 'Sections' });
        await tabs.waitFor();
        const selected = await tabs.getByRole('tab', { selected: true }).getAttribute('id');
        const panel = page.locator(`[role=tabpanel][aria-labelledby="${selected}"]`);
        await panel.locator('.card').first().waitFor();
        const row = await tabs.evaluate(list => { const tops = new Set([...list.querySelectorAll('[role=tab]')].map(tab => Math.round(tab.getBoundingClientRect().top))); return { rows: tops.size, inside: list.getBoundingClientRect().right <= innerWidth + 1 }; });
        if (row.rows !== 1) failures.push(`${width}px ${route}: the tab list wraps onto ${row.rows} rows`);
        if (!row.inside) failures.push(`${width}px ${route}: the tab list runs past the viewport`);
        const active = await tabs.getByRole('tab', { selected: true }).evaluate(tab => { const a = tab.getBoundingClientRect(), b = tab.parentElement.getBoundingClientRect(); return a.left >= b.left - 1 && a.right <= b.right + 1; });
        if (!active) failures.push(`${width}px ${route}: the selected tab is scrolled out of view`);
      }
      await page.goto(`${base}/console/servers/local/tuning`);
      const tuning = page.locator('[data-server-tuning]');
      await tuning.waitFor();
      if (await tuning.locator('.check-list-failed').count() < 1) failures.push(`${width}px server tuning: the failed change is not a failed check list row`);
      if (await tuning.locator('.check-list-waiting').count() < 2) failures.push(`${width}px server tuning: deferred and next-restart changes are not waiting rows`);
      if (await tuning.locator('[data-redis]').count() !== 2) failures.push(`${width}px server tuning: both Redis instances should be listed`);
      if (await tuning.getByText('No application uses this instance.').count() !== 1) failures.push(`${width}px server tuning: the Redis instance without applications says so`);
      if (await tuning.locator('dl.summary-list').count() < 3) failures.push(`${width}px server tuning: status and instances use SummaryList`);

      // Control panel move review: readiness checks, the summary and the
      // action row, measured with the plan open.
      await page.goto(`${base}/console/control-panel/move`);
      await page.getByLabel('Destination server').selectOption('combined');
      await page.getByRole('button', { name: 'Review the move' }).click();
      const review = page.getByLabel('Move plan');
      await review.waitFor();
      await page.waitForTimeout(150);
      if (await review.locator('.check-list-item').count() < 3) failures.push(`${width}px /console/control-panel: the plan shows no check list`);
      if (await review.locator('dl.summary-list > div').count() < 7) failures.push(`${width}px /console/control-panel: the plan shows no summary`);
      for (const issue of await page.evaluate(measure)) failures.push(`${width}px /console/control-panel (plan): ${issue}`);

      // Application move plan: the same summary and check list components.
      await page.route('**/api/control/servers', route => route.fulfill({ json: [{ id: 'local', name: 'Production', url: 'http://127.0.0.1:9090' }, { id: 'edge', name: 'Edge', url: 'http://127.0.0.1:9091' }] }));
      await page.route('**/api/control/apps/*/move-plan?*', route => route.fulfill({ json: appMovePlan }));
      await page.goto(`${base}/console/applications/portal/move`);
      const move = page.locator('.migration-plan').first();
      await move.getByRole('button', { name: 'Review move' }).click();
      await move.getByLabel('Move summary').waitFor();
      await page.waitForTimeout(150);
      if (await move.locator('.check-list-item').count() < 2) failures.push(`${width}px /console/applications/portal/move: the move plan shows no reverse DNS checks`);
      for (const issue of await page.evaluate(measure)) failures.push(`${width}px /console/applications/portal/move (plan): ${issue}`);
      await page.unroute('**/api/control/servers');

      // Updates: the bound repository is a value, not a placeholder, and the
      // field group sits clear of the verified-release box above it.
      await page.goto(`${base}/console/updates/channel`);
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
      // Actions menus: focus and highlight stay inside the menu, keyboard
      // focus stays visible, and a disabled lifecycle item carries a short
      // reason with a link to the server setting that allows it.
      state.apps.push({ ...app, id: 'ledger', runtime: 'rails', domain: 'ledger.example.com', native: { id: 'ledger', name: 'Ledger', services: ['ledger-server.service'] }, management_capabilities: { deploy: true, remove: true, lifecycle: false, lifecycle_reason: 'Service control is off for this server.', lifecycle_setting: 'service_control', adopted: true } });
      for (const [route, name] of [['/console/applications/ledger', 'application Actions'], ['/console/servers/local', 'server Actions']]) {
        await page.goto(`${base}${route}`);
        const trigger = page.getByRole('button', { name: 'Actions' });
        await trigger.waitFor();
        await trigger.focus();
        await page.keyboard.press('Enter');
        await page.getByRole('menu').waitFor();
        await page.waitForTimeout(150);
        for (const issue of await page.evaluate(menuFocus, name)) failures.push(`${width}px ${route}: ${issue}`);
        if (route.includes('ledger')) {
          for (const label of ['Restart', 'Stop']) {
            const reason = await page.getByRole('menuitem', { name: new RegExp(`^${label}`) }).locator('.menu-reason').textContent();
            if (reason !== 'Service control is off for this server.') failures.push(`${width}px ${route}: disabled ${label} reason is ${JSON.stringify(reason)}`);
          }
          if (await page.getByRole('menuitem', { name: 'Service control settings' }).getAttribute('href') !== '/console/servers/local/agent') failures.push(`${width}px ${route}: no link to the service control setting`);
          await page.getByRole('menuitem', { name: 'Service control settings' }).focus();
        } else await page.getByRole('menuitem').nth(1).focus();
        if (process.env.TEST_SCREENSHOTS) await page.screenshot({ path: require('node:path').join(process.env.TEST_SCREENSHOTS, `${name.split(' ')[0]}-actions-${width}.png`) });
        await page.keyboard.press('Escape');
      }
      // Sidebar: the panel's identity links to the Control panel, the server
      // status links to Servers, and the person's block opens a menu with
      // Account and Sign out inside its bounds. Phones open the drawer first.
      await page.goto(`${base}/console/applications`);
      await page.locator('#main-content h1').first().waitFor();
      let sidebar = page.locator('.desktop-sidebar');
      if (width < 900) {
        await page.getByRole('button', { name: 'Open menu' }).click();
        sidebar = page.getByRole('dialog', { name: 'Navigation menu' });
        await sidebar.waitFor();
        await page.waitForTimeout(400); // the drawer's slide-in
      }
      const identity = sidebar.getByRole('link', { name: /deploy\.example\.com/ });
      if (await identity.getAttribute('href') !== '/console/control-panel') failures.push(`${width}px sidebar: the panel identity does not link to the Control panel`);
      if (!/Release v0\.2\.83/.test(await identity.textContent())) failures.push(`${width}px sidebar: the panel identity does not show the release`);
      if (await sidebar.locator('.panel-identity svg, .panel-identity button').count()) failures.push(`${width}px sidebar: the panel identity looks like a dropdown`);
      const serverLink = sidebar.getByRole('link', { name: 'All servers connected' });
      if (await serverLink.count() !== 1 || await serverLink.getAttribute('href') !== '/console/servers') failures.push(`${width}px sidebar: the server status is not a link to /console/servers`);
      if (await sidebar.getByText(/workspace/i).count()) failures.push(`${width}px sidebar: still says workspace`);
      const groupLabels = await sidebar.locator('.nav-group > p').allTextContents();
      if (!groupLabels.includes('Settings')) failures.push(`${width}px sidebar: no Settings group (${groupLabels.join(', ')})`);
      const person = sidebar.getByRole('button', { name: 'Account menu for Ada Lovelace' });
      if (!/Admin/.test(await person.textContent()) || (await person.locator('.user-avatar').textContent()) !== 'A') failures.push(`${width}px sidebar: the account block does not show the person's role and initial`);
      if (process.env.TEST_SCREENSHOTS) { // tall enough to show the whole sidebar
        await page.setViewportSize({ width, height: 1200 });
        await page.screenshot({ path: require('node:path').join(process.env.TEST_SCREENSHOTS, `sidebar-${width}.png`) });
      }
      await person.focus();
      await page.keyboard.press('Enter');
      const accountMenu = page.getByRole('menu');
      await accountMenu.waitFor();
      await page.waitForTimeout(150);
      const inside = await accountMenu.evaluate(menu => {
        const box = menu.getBoundingClientRect();
        const items = [...menu.querySelectorAll('[role=menuitem]')].map(item => ({ text: item.textContent.trim(), href: item.getAttribute('href'), rect: item.getBoundingClientRect() }));
        return { labels: items.map(item => item.text), account: items.find(item => item.text === 'Account')?.href, outside: items.filter(item => item.rect.left < box.left - .5 || item.rect.right > box.right + .5 || item.rect.top < box.top - .5 || item.rect.bottom > box.bottom + .5).map(item => item.text), inViewport: box.left >= 0 && box.top >= 0 && box.right <= innerWidth + .5 && box.bottom <= innerHeight + .5 };
      });
      if (inside.labels.join('|') !== 'Account|Sign out') failures.push(`${width}px account menu: items are ${inside.labels.join(', ')}`);
      if (inside.account !== '/console/account') failures.push(`${width}px account menu: Account links to ${inside.account}`);
      if (inside.outside.length) failures.push(`${width}px account menu: ${inside.outside.join(', ')} outside the menu`);
      if (!inside.inViewport) failures.push(`${width}px account menu: runs past the viewport`);
      for (const issue of await page.evaluate(menuFocus, 'account menu')) failures.push(`${width}px sidebar: ${issue}`);
      if (process.env.TEST_SCREENSHOTS) await page.screenshot({ path: require('node:path').join(process.env.TEST_SCREENSHOTS, `account-menu-${width}.png`) });
      await page.keyboard.press('Escape');
      await page.close();
    }
  } finally {
    await browser.close();
    await server.close();
  }
  if (failures.length) {
    console.error(`FAIL: ${failures.length} layout spacing problem(s)\n${failures.join('\n')}`);
    process.exit(1);
  }
  console.log(`PASS: ${views.length} console views (${views.filter(view => view[1] === 'index').length} indexes, ${views.filter(view => view[1] === 'detail').length} detail tabs) and ${redirects.length} redirects at ${widths.join(' and ')}px use their templates, linked breadcrumbs and token spacing (${((Date.now() - started) / 1000).toFixed(1)}s)`);
})().catch(error => { console.error(error.message || error); process.exit(1); });
