const {chromium} = require('playwright');
const {spawn, execFileSync} = require('node:child_process');
const fs = require('node:fs'), path = require('node:path'), os = require('node:os'), crypto = require('node:crypto'), net = require('node:net');

(async () => {
  const root = path.resolve(__dirname, '../..'), base = process.env.TEST_WEB_URL || 'http://127.0.0.1:3000';
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lidza-errors-browser-'));
  const key = crypto.randomBytes(32).toString('hex'), id = 'errors-' + crypto.randomBytes(5).toString('hex'), host = 'errorhost-' + crypto.randomBytes(5).toString('hex');
  const container = "lidza-" + id + "-console-fixture";
  const listener = net.createServer();
  await new Promise(resolve => listener.listen(0, '127.0.0.1', resolve));
  const port = listener.address().port;
  await new Promise(resolve => listener.close(resolve));
  const config = path.join(dir, 'agent.json');
  fs.writeFileSync(config, JSON.stringify({listen: '127.0.0.1:' + port, proxy_listen: '127.0.0.1:0', data_dir: dir, api_key: key}), {mode: 0o600});
  const startAgent = () => spawn(path.join(root, 'bin/lidza-agent'), ['-config', config], {cwd: root, stdio: 'ignore'});
  const messages = [
    {level:'ERROR',msg:'Checkout <failed> browser-private-value',stack:'handlers.Checkout browser-private-value',path:'/checkout?token=private-query',request_id:'request-one'},
    {level:'ERROR',msg:'Checkout <failed> browser-private-value',stack:'handlers.Checkout browser-private-value',path:'/checkout',request_id:'request-two'},
    {level:'INFO',msg:'request',status:500,path:'/account',method:'GET',request_id:'request-three'}
  ];
  execFileSync('docker',['run','-d','--name',container,'--network','none','postgres:17-alpine','sh','-c',`printf '%s\\n' "$1" "$2" "$3"; sleep 300`,'fixture',...messages.map(m=>JSON.stringify(m))],{stdio:'ignore'});
  fs.writeFileSync(path.join(dir,'state.json'),JSON.stringify({apps:{[id]:{id,repository:'acme/errors-fixture',branch:'main',domain:id+'.example.com',env:{APP_SECRET:'browser-private-value'},current:{id:'console-release',container,commit:'abc123'}}},deployments:[]}),{mode:0o600});
  let agent = startAgent();
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
    response = await page.request.post(base + '/api/control/apps', {headers, data: {id, server_id: host, repository: 'acme/errors-fixture', branch: 'main', domain: id + '.example.com', env: {APP_SECRET: 'browser-private-value'}, database: {mode: 'none'}}});
    if (!response.ok()) throw Error('Could not create errors fixture app');
    await page.reload();
    await page.locator('[data-errors="' + id + '"]').click();
    for (let i = 0; i < 60; i++) {
      const data=await (await page.request.get(base+'/api/control/apps/'+id+'/errors')).json();
      if(data.errors?.length===3)break;
      await page.waitForTimeout(250);
    }
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
    if (await page.locator('[data-error-detail]').count() !== 0) throw Error('Console errors incorrectly attributed to frontend');
    await page.selectOption('#errors-source', '');
    fs.mkdirSync(path.join(root, '.local/screenshots/errors'), {recursive: true});
    await page.screenshot({path: path.join(root, '.local/screenshots/errors/dashboard.png'), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1)) throw Error('Mobile errors dashboard overflows');
    // Stored reports remain available when the hosting agent goes offline.
    agent.kill('SIGTERM');
    await new Promise(resolve=>agent.exitCode!==null?resolve():agent.once('exit',resolve));
    await page.locator('#errors-refresh').click();
    await page.locator('#errors-result [role=status]').waitFor();
    if(!(await page.locator('#errors-result').textContent()).includes('Stored errors remain available') || await page.locator('[data-error-detail]').count()!==2)throw Error('Offline agent lost central reports');
    agent=startAgent();
    for(let i=0;i<50;i++){try{if((await fetch('http://127.0.0.1:'+port+'/health')).ok)break}catch{}await page.waitForTimeout(100)}
    await page.selectOption('#errors-mode','analytics');
    await page.getByRole('heading',{name:'Enable error reporting in this app'}).waitFor();
    await page.selectOption('#errors-mode','console');
    await page.getByRole('heading',{name:'Captured failures'}).waitFor();
    if (browserErrors.length) throw Error(browserErrors.join('\n'));
    console.log('PASS: real Docker console -> durable agent outbox -> authenticated central ingestion -> Errors dashboard, no app database/analytics, grouping/search/redaction/mobile and offline retention');
  } finally {
    if (page) {
      await page.request.delete(base + '/api/control/apps/' + id, {headers: {Origin: base}, data: {confirm: id}});
      await page.request.delete(base + '/api/control/servers/' + host, {headers: {Origin: base}});
    }
    if (browser) await browser.close();
    agent.kill('SIGTERM');
    await new Promise(resolve => agent.exitCode !== null ? resolve() : agent.once('exit', resolve));
    try {execFileSync('docker',['rm','-f',container],{stdio:'ignore'});} catch {}
    fs.rmSync(dir, {recursive: true, force: true});
  }
})().catch(e => { console.error(e.message); process.exitCode = 1; });
