const {chromium} = require('playwright');
const {spawn, execFileSync} = require('node:child_process');
const fs = require('node:fs'), path = require('node:path'), os = require('node:os'), crypto = require('node:crypto'), net = require('node:net');

(async () => {
  const root = path.resolve(__dirname, '../..'), base = process.env.TEST_WEB_URL || 'http://127.0.0.1:3000';
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lidza-errors-browser-'));
  const key = crypto.randomBytes(32).toString('hex'), id = 'errors-' + crypto.randomBytes(5).toString('hex'), host = 'errorhost-' + crypto.randomBytes(5).toString('hex');
  const network = 'lidza-db-' + id;
  const listener = net.createServer();
  await new Promise(resolve => listener.listen(0, '127.0.0.1', resolve));
  const port = listener.address().port;
  await new Promise(resolve => listener.close(resolve));
  const config = path.join(dir, 'agent.json');
  fs.writeFileSync(config, JSON.stringify({listen: '127.0.0.1:' + port, proxy_listen: '127.0.0.1:0', data_dir: dir, api_key: key}), {mode: 0o600});
  const agent = spawn(path.join(root, 'bin/lidza-agent'), ['-config', config], {cwd: root, stdio: 'ignore'});
  let browser, page;
  try {
    for (let i = 0; i < 100; i++) {
      try { if ((await fetch('http://127.0.0.1:' + port + '/health')).ok) break; } catch {}
      await new Promise(resolve => setTimeout(resolve, 100));
    }
    browser = await chromium.launch({executablePath: '/usr/bin/chromium', headless: true, args: ['--no-sandbox']});
    page = await browser.newPage({viewport: {width: 1440, height: 1000}});
    const browserErrors = [];
    page.on('pageerror', e => browserErrors.push(e.message));
    const cfg = {};
    for (const line of fs.readFileSync(path.join(root, '.local/dev.env'), 'utf8').split('\n')) {
      const match = line.match(/^export ([A-Z_]+)=(.*)$/);
      if (match) cfg[match[1]] = match[2].replace(/^'|'$/g, '');
    }
    await page.goto(base + '/login.html');
    await page.fill('[name=email]', cfg.CONTROL_USER);
    await page.fill('[name=password]', cfg.CONTROL_PASSWORD);
    await page.getByRole('button', {name: 'Open workspace'}).click();
    await page.waitForURL('**/console.html');
    const headers = {Origin: base};
    let response = await page.request.post(base + '/api/control/servers', {headers, data: {id: host, name: 'Errors fixture', url: 'http://127.0.0.1:' + port, token: key}});
    if (!response.ok()) throw Error('Could not connect errors fixture agent');
    response = await page.request.post(base + '/api/control/apps', {headers, data: {id, server_id: host, repository: 'acme/errors-fixture', branch: 'main', domain: id + '.example.com', env: {APP_SECRET: 'browser-private-value'}, database: {mode: 'local', backup: {hours: 0, keep: 1}}}});
    if (!response.ok()) throw Error('Could not create errors fixture app');
    for (let i = 0; i < 100; i++) {
      const data = await (await page.request.get(base + '/api/control/databases')).json();
      if (data.databases?.find(d => d.app_id === id)?.ready) break;
      await page.waitForTimeout(300);
    }
    await page.reload();
    await page.locator('[data-errors="' + id + '"]').click();
    await page.getByRole('heading', {name: 'Enable error reporting in this app'}).waitFor();
    const frameworkVersion = fs.readFileSync(path.join(root, 'go.mod'), 'utf8').match(/github\.com\/agim\/lidza\s+(v\S+)/)[1];
    const framework = fs.readFileSync(path.join(process.env.GOMODCACHE || '/workspace/.cache/gomod', 'github.com/agim/lidza@' + frameworkVersion + '/packs/analytics/reflect.go'), 'utf8');
    const tables = framework.match(/const Tables = `([\s\S]+?)`/)[1];
    const sql = tables + `
      ALTER TABLE app_error OWNER TO app;
      INSERT INTO app_error (source,message,stack,route,method,request_id,fingerprint,created_at) VALUES
      ('server','Checkout <failed> browser-private-value','handlers.Checkout browser-private-value','POST /checkout','POST','request-one','checkout-group',now()),
      ('server','Checkout <failed> browser-private-value','handlers.Checkout browser-private-value','POST /checkout','POST','request-two','checkout-group',now()-interval '1 minute'),
      ('client','Browser render failed','at page.js:12','/account','GET','request-three','render-group',now()-interval '2 minutes');`;
    execFileSync('docker', ['exec', '-i', network, 'psql', '-X', '-U', 'postgres', '-d', 'app', '-v', 'ON_ERROR_STOP=1'], {input: sql, stdio: ['pipe', 'ignore', 'pipe']});
    await page.locator('#errors-refresh').click();
    await page.getByRole('heading', {name: 'Captured failures'}).waitFor();
    if (await page.locator('[data-error-detail]').count() !== 2) throw Error('Repeated errors were not grouped');
    const result = await page.locator('#errors-result').textContent();
    if (!result.includes('Checkout <failed> [redacted]') || result.includes('browser-private-value')) throw Error('Error message escaping/redaction failed');
    await page.fill('#errors-search', 'request-two');
    if (await page.locator('[data-error-detail]').count() !== 1) throw Error('Request correlation search failed');
    await page.locator('[data-error-detail]').click();
    await page.locator('#error-detail-dialog').waitFor();
    if (!(await page.locator('#error-detail-dialog').textContent()).includes('handlers.Checkout [redacted]')) throw Error('Stack trace was lost or leaked a secret');
    await page.locator('#error-detail-close').click();
    await page.fill('#errors-search', '');
    await page.selectOption('#errors-source', 'client');
    if (await page.locator('[data-error-detail]').count() !== 1 || !(await page.locator('#errors-result').textContent()).includes('Browser render failed')) throw Error('Frontend source filter failed');
    await page.selectOption('#errors-source', '');
    fs.mkdirSync(path.join(root, '.local/screenshots/errors'), {recursive: true});
    await page.screenshot({path: path.join(root, '.local/screenshots/errors/dashboard.png'), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1)) throw Error('Mobile errors dashboard overflows');
    // Database failures must not turn into a successful empty dashboard.
    execFileSync('docker', ['stop', network], {stdio: 'ignore'});
    await page.locator('#errors-refresh').click();
    await page.locator('#errors-result [role=alert]').waitFor();
    if (!(await page.locator('#errors-result').textContent()).includes('unavailable')) throw Error('Database outage not shown');
    if (browserErrors.length) throw Error(browserErrors.join('\n'));
    console.log('PASS: real app database/agent/control errors dashboard, setup guidance, grouping/search/source filters, masked stack traces, mobile layout and database outage');
  } finally {
    if (page) {
      await page.request.delete(base + '/api/control/apps/' + id, {headers: {Origin: base}, data: {confirm: id}});
      await page.request.delete(base + '/api/control/servers/' + host, {headers: {Origin: base}});
    }
    if (browser) await browser.close();
    agent.kill('SIGTERM');
    await new Promise(resolve => agent.exitCode !== null ? resolve() : agent.once('exit', resolve));
    for (const args of [['rm', '-f', network], ['volume', 'rm', network + '-data'], ['network', 'rm', network]]) {
      try { execFileSync('docker', args, {stdio: 'ignore'}); } catch {}
    }
    fs.rmSync(dir, {recursive: true, force: true});
  }
})().catch(e => { console.error(e.message); process.exitCode = 1; });
