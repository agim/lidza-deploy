const {chromium}=require('playwright');
const assert=require('node:assert/strict');
const {mockConsole}=require('./fixtures/console.cjs');
const base=process.env.TEST_WEB_URL||'http://127.0.0.1:8880';
(async()=>{
 const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});
 try {for(const width of [390,1440]){
  const page=await browser.newPage({viewport:{width,height:950}}),errors=[];
  page.on('pageerror',e=>errors.push(e.message));
  const fixture=await mockConsole(page);
  await page.addInitScript(()=>sessionStorage.setItem('lidza-console-job:local','saved-command-00001'));
  let reads=0,cancels=0,stopped=false;
  await page.route('**/api/v1/auth/step-up',route=>{
   const body=route.request().postDataJSON();
   assert.equal(body.purpose,'server.console');assert.equal(body.target,'local');
   if(body.code!=='654321')return route.fulfill({status:403,json:{error:'Invalid verification code'}});
   fixture.consoleGranted=true;
   return route.fulfill({json:{expiresAt:'2099-10-16T12:00:00Z'}});
  });
  await page.route('**/api/control/servers/local/console/jobs/**',route=>{
   assert(fixture.consoleGranted,'Job accessed without verification');
   if(route.request().method()==='POST'){
    assert(new URL(route.request().url()).pathname.endsWith('/saved-command-00001/cancel'));
    cancels++;stopped=true;
    return route.fulfill({status:202,json:{status:'cancellation requested'}});
   }
   reads++;
   return route.fulfill({json:{id:'saved-command-00001',state:stopped?'cancelled':'running',output:'saved output',next_offset:12,output_size:12,exit_code:stopped?130:0,duration_seconds:5}});
  });
  await page.goto(`${base}/console.html#page/console/local`);
  const unlock=page.getByRole('button',{name:'Unlock to cancel',exact:true});
  await unlock.waitFor();assert.equal(await unlock.isEnabled(),true);
  assert.equal(await unlock.getAttribute('data-variant'),'outline');
  assert.equal(await unlock.evaluate(el=>el.classList.contains('button-danger')),false);
  assert.equal(reads,0);assert.equal(cancels,0);
  assert.equal(await page.getByRole('button',{name:'Command running',exact:true}).count(),0);
  assert.match(await page.getByRole('status').filter({hasText:'Saved command found'}).innerText(),/Saved command found/);
  assert.notEqual(await page.locator('.console-output').innerText(),'Locked.');
  await page.screenshot({path:`/tmp/lidza-console-locked-${width}.png`,fullPage:true});
  await unlock.click();
  const modal=page.getByRole('alertdialog',{name:'Unlock to cancel command'});
  await modal.waitFor();
  await modal.getByRole('button',{name:'Cancel',exact:true}).click();
  assert.equal(cancels,0,'Dismissing verification cancelled the command');
  await page.keyboard.press('Control+c');await modal.waitFor();
  await modal.getByLabel('Authenticator or recovery code').fill('bad-code');
  await modal.getByRole('button',{name:'Verify & cancel command',exact:true}).click();
  await modal.getByText('Invalid verification code').waitFor();assert.equal(cancels,0);assert.equal(reads,0);
  await modal.getByLabel('Authenticator or recovery code').fill('654321');
  await modal.getByRole('button',{name:'Verify & cancel command',exact:true}).click();
  await page.getByRole('status').filter({hasText:'Command cancelled.'}).waitFor();
  assert.equal(cancels,1);assert.match(await page.locator('.console-output').innerText(),/saved output[\s\S]*exit 130/);
  // A failed access-status read must not turn Cancel into a setup redirect.
  fixture.consoleGranted=false;stopped=false;
  await page.route('**/api/control/servers/local/console',route=>route.fulfill({status:503,json:{error:'Console access temporarily unavailable'}}));
  await page.reload();
  await page.getByRole('button',{name:'Unlock to cancel',exact:true}).click();
  await modal.getByLabel('Authenticator or recovery code').fill('654321');
  await modal.getByRole('button',{name:'Verify & cancel command',exact:true}).click();
  await page.getByRole('status').filter({hasText:'Command cancelled.'}).waitFor();
  assert.equal(cancels,2);
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth+1),false);
  assert.deepEqual(errors,[]);await page.close();
  console.log(`PASS: locked restored job, enabled neutral cancel, no false running, Ctrl+C verification, invalid code, dismissal and automatic cancellation at ${width}px`);
 }}finally{await browser.close();}
})().catch(e=>{console.error(e);process.exit(1)});
