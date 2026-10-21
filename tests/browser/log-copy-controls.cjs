const {chromium}=require('playwright');
const assert=require('node:assert/strict');
const {mockConsole}=require('./fixtures/console.cjs');
const base=process.env.TEST_WEB_URL||'http://127.0.0.1:34225';
(async()=>{const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});try{
 for(const width of [320,390,768,1440]){
  const page=await browser.newPage({viewport:{width,height:900}});
  const errors=[];page.on('pageerror',e=>errors.push(e.message));
  const state=await mockConsole(page);
  for(const [route,label] of [['page/build-logs/build-one','Copy build log'],['page/runtime-logs/portal','Copy runtime log']]){
   await page.goto(`${base}/console.html#${route}`);await page.locator('#deployment-output').filter({hasText:route.includes('build-logs')?'[fixture output]':'Serving requests'}).waitFor();
   const expected=await page.locator('#deployment-output').textContent();
   await page.locator('.log-card').getByRole('button',{name:label,exact:true}).click();
   await page.getByRole('button',{name:'Copied',exact:true}).waitFor();
   assert.equal(await page.evaluate(()=>window.consoleCopied),expected);
   await page.locator('#deployment-output').evaluate(el=>{el.scrollTop=el.scrollHeight;});
   assert(await page.locator('.log-card .log-output-heading').evaluate(el=>{const r=el.getBoundingClientRect();return r.top>=0&&r.bottom<=innerHeight}));
  }
  for(const route of ['errors','security']){
   await page.goto(`${base}/console.html#${route}`);await page.locator('.activity-details summary').first().click();
   for(const [label,expected] of [['Copy message','Fixture request rejected'],['Copy stack trace','Error: fixture diagnostic\n  at handler.go:42']]){
    await page.getByRole('button',{name:label,exact:true}).first().click();
    await page.waitForFunction(text=>window.consoleCopied===text,expected);
   }
   await page.getByRole('button',{name:'Copy event details',exact:true}).first().click();
   const event=JSON.parse(await page.evaluate(()=>window.consoleCopied));assert.equal(event.container,'lidza-portal');assert(event.last_seen);assert.equal(event.recent_occurrences[0].request_id,'request-fixture');
  }
  await page.goto(`${base}/console.html#audit`);await page.getByRole('button',{name:'Copy audit log'}).click();
  assert.match(await page.evaluate(()=>window.consoleCopied),/ops@example.com.*server.console.unlock.*local/);
  await page.goto(`${base}/console.html#page/cleanup/local`);await page.getByRole('button',{name:'Copy cleanup report'}).waitFor();
  await page.goto(`${base}/console.html#page/console/local`);await page.getByRole('button',{name:'Copy output',exact:true}).waitFor();
  await page.goto(`${base}/console.html#page/build-logs/build-one`);await page.locator('#deployment-output').waitFor();
  await page.evaluate(()=>navigator.clipboard.writeText=async()=>{throw Error('denied')});
  await page.getByRole('button',{name:'Copy build log',exact:true}).click();
  await page.getByRole('alert').filter({hasText:'Could not copy'}).waitFor();
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
  await page.route('**/api/control/deployments/build-one',route=>route.fulfill({json:{id:'build-one',app_id:'portal',status:'failed',created:'2026-10-20T12:00:00Z',log:'git\ncheckout completed',error:'Fixture runtime preflight rejected'}}));
  await page.route('**/api/control/deployments', route=>route.fulfill({json:{deployments:[{id:'build-one',app_id:'portal',server_id:'local',status:'failed',created:'2026-10-20T12:00:00Z',log:'git\ncheckout completed',error:'Fixture runtime preflight rejected'}],unavailable_servers:[]}}));
  await page.reload();
  await page.locator('#deployment-failure').filter({hasText:'Fixture runtime preflight rejected'}).waitFor();
  const failedOutput=await page.locator('#deployment-output').textContent();
  assert.match(failedOutput,/checkout completed/);assert.match(failedOutput,/Failure: Fixture runtime preflight rejected/);
  await page.getByRole('button',{name:'Copy build log',exact:true}).click();
  assert.equal(await page.evaluate(()=>window.consoleCopied),failedOutput);
  assert.deepEqual(errors,[]);assert.deepEqual(state.unknown,[]);
  await page.close();console.log(`PASS: log, event and audit clipboard; existing console/cleanup controls; clipboard denial at ${width}px`);
 }
}finally{await browser.close()}})().catch(e=>{console.error(e);process.exit(1)});
