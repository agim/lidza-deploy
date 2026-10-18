const { chromium } = require('playwright');

const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8879';
function assert(value, message) { if (!value) throw new Error(message); }

(async () => {
  const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_BIN || '/usr/bin/chromium', args: ['--no-sandbox'] });
  try {
    const page = await browser.newPage({ viewport: { width: 390, height: 844 } });
    let phase = 0;
    let offline = false;
    let calls = 0;
    let finished = false;
    const errors = [];
    const created = new Date().toISOString();
    page.on('pageerror', error => errors.push(error.message));

    const row = () => ({
      id: 'fixture-build', app_id: 'fixture-app', status: finished ? 'live' : 'building', created,
      duration_seconds: 10 + phase,
      log: phase ? `${Array.from({ length: 160 }, (_, index) => `line ${index}`).join('\n')}\nphase-${phase} <literal>` : '',
    });
    await page.route('**/api/control/**', route => {
      const path = new URL(route.request().url()).pathname;
      if (path.endsWith('/deployments')) {
        calls += 1;
        return route.fulfill(offline ? { status: 503, json: { error: 'Agent offline' } } : { json: { deployments: [row()], unavailable_servers: [] } });
      }
      if (path.endsWith('/status')) return route.fulfill({ json: { roles: ['admin'] } });
      return route.fulfill({ json: [] });
    });

    await page.goto(`${base}/console.html#page/build-logs/fixture-build`);
    await page.waitForSelector('#deployment-output');
    assert(await page.locator('#deployment-output').textContent().then(text => text.includes('Waiting for build output')), 'Queued build did not show a waiting state');

    phase = 1;
    await page.waitForFunction(() => document.querySelector('#deployment-output')?.textContent.includes('phase-1'));
    assert(await page.locator('#deployment-state').textContent() === 'Live · building', 'Missing live build status');

    await page.locator('#deployment-output').evaluate(element => { element.scrollTop = 30; element.dispatchEvent(new Event('scroll', { bubbles: true })); });
    await page.waitForFunction(() => !document.querySelector('#log-follow').checked);
    phase = 2;
    await page.waitForFunction(() => document.querySelector('#deployment-output')?.textContent.includes('phase-2'));
    assert(await page.locator('#deployment-output').evaluate(element => element.scrollTop) === 30, 'Reading position jumped when new output arrived');

    await page.locator('#log-pause').click();
    await page.waitForTimeout(250);
    let count = calls;
    await page.waitForTimeout(1_900);
    assert(calls === count, 'Paused logs still requested updates');

    await page.locator('#log-pause').click();
    offline = true;
    await page.waitForSelector('#deployment-refresh-error');
    assert(await page.locator('#deployment-output').textContent().then(text => text.includes('phase-2')), 'Offline response erased previous output');

    offline = false;
    phase = 3;
    finished = true;
    await page.waitForFunction(() => document.querySelector('#deployment-state')?.textContent === 'Finished · live');
    assert(await page.locator('#deployment-refresh-error').count() === 0, 'Recovery error did not clear');
    assert(await page.locator('#deployment-output').textContent().then(text => text.includes('phase-3 <literal>')), 'Final output missing or parsed as HTML');

    count = calls;
    await page.waitForTimeout(1_900);
    assert(calls - count <= 1, 'Completed deployment kept fast-polling');
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth), 'Live logs page overflows on mobile');

    // The finished state from the reported screenshot must retain separation too.
    for (const width of [320, 390, 768, 1440, 2560]) {
      await page.setViewportSize({ width, height: 900 });
      const gap = await page.locator('.log-status-card').evaluate(card => {
        const status = card.firstElementChild.getBoundingClientRect();
        const follow = card.querySelector('.log-follow-control').getBoundingClientRect();
        return Math.max(follow.top - status.bottom, follow.left - status.right);
      });
      assert(gap >= 16, `Finished status touches Follow latest output at ${width}px: ${gap}px`);
      await page.screenshot({ path: `/tmp/lidza-log-spacing-${width}.png`, fullPage: true });
    }

    const locationHash = await page.evaluate(() => { location.hash = 'applications'; return location.hash; });
    assert(locationHash === '#applications', 'Could not leave build logs');
    count = calls;
    await page.waitForTimeout(1_900);
    assert(calls - count <= 1, 'Closed logs kept fast-polling');
    assert(!errors.length, errors.join('\n'));
    console.log('Live build output, stable scrolling, pause/resume, offline recovery, completion and cleanup passed.');
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exit(1); });
