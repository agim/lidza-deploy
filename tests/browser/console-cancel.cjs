const { chromium } = require('playwright');
const assert = require('node:assert/strict');
const { mockConsole } = require('./fixtures/console.cjs');
const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8880';
(async()=>{
 const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});
 try { for(const width of [390,1440]) {
  const page=await browser.newPage({viewport:{width,height:950}}),errors=[];
  page.on('pageerror',e=>errors.push(e.message));
  const fixture=await mockConsole(page);fixture.consoleGranted=true;
  let id='',stopped=false,starts=0,cancels=0,deny=false,holdStop=false;
  await page.route('**/api/control/servers/local/console/jobs**',async route=>{
   const req=route.request(),url=new URL(req.url());
   if(url.pathname.endsWith('/cancel')) {
    cancels++; assert.equal(req.method(),'POST'); assert(url.pathname.includes(id));
    if(deny) { deny=false;return route.fulfill({status:403,json:{error:'Verify again.'}}); }
    stopped=true;await new Promise(resolve=>setTimeout(resolve,100));return route.fulfill({status:202,json:{status:'cancellation requested'}});
   }
   if(req.method()==='POST') { starts++;id=req.postDataJSON().id;stopped=false;return route.fulfill({status:202,json:{id,state:'running'}}); }
   const offset=Number(url.searchParams.get('offset'));
   return route.fulfill({json:{id,state:stopped&&!holdStop?'cancelled':'running',output:offset?'':'partial output',next_offset:14,output_size:14,exit_code:stopped?130:0,duration_seconds:1,truncated:false}});
  });
  await page.goto(`${base}/console.html#page/console/local`);
  const input=page.locator('#console-command textarea');
  await input.fill('sleep 30');await page.getByRole('button',{name:'Run command',exact:true}).click();
  await page.waitForFunction(()=>document.querySelector('.console-output')?.textContent==='partial output');
  await page.locator('.console-output').evaluate(el=>{const range=document.createRange();range.selectNodeContents(el);const selection=getSelection();selection.removeAllRanges();selection.addRange(range);});
  await page.keyboard.press('Control+c');assert.equal(cancels,0,'Ctrl+C should copy selected output');
  await page.evaluate(()=>getSelection().removeAllRanges());
  await input.fill('next command');await input.selectText();await page.keyboard.press('Control+c');
  assert.equal(cancels,0,'Ctrl+C should copy selected input');
  await input.press('ArrowRight');await page.keyboard.press('Control+c');
  await page.getByRole('status').filter({hasText:'Command cancelled.'}).waitFor();
  assert.equal(cancels,1);assert.match(await page.locator('.console-output').innerText(),/partial output[\s\S]*exit 130/);
  assert.equal(await input.inputValue(),'next command','Cancellation erased next command draft');
  await page.waitForFunction(()=>!document.querySelector('#console-command button[type=submit]').disabled);
  // A mobile user can use the button; expired MFA pauses cancellation until renewed.
  await page.getByRole('button',{name:'Run command',exact:true}).click();
  deny=true;
  await page.getByRole('button',{name:'Cancel command (Ctrl+C)',exact:true}).click();
  const verify=page.getByRole('alertdialog',{name:'Unlock to cancel command'});
  await verify.waitFor();
  await verify.getByLabel('Authenticator or recovery code').fill('123456');
  await verify.getByRole('button',{name:'Verify & cancel command',exact:true}).click();
  await page.getByRole('status').filter({hasText:'Command cancelled.'}).waitFor();
  assert.equal(starts,2);assert.equal(cancels,3);
  await page.waitForFunction(()=>!document.querySelector('#console-command button[type=submit]').disabled);
  await input.fill('slow to stop');await page.getByRole('button',{name:'Run command',exact:true}).click();holdStop=true;
  await page.getByRole('button',{name:'Cancel command (Ctrl+C)',exact:true}).click();
  const retry=page.getByRole('button',{name:'Retry cancel (Ctrl+C)',exact:true});
  await retry.waitFor();assert.equal(await retry.isEnabled(),true,'Acknowledgement left Cancel disabled');
  assert.equal(await retry.getAttribute('data-variant'),'outline');
  holdStop=false;await retry.click();
  await page.getByRole('status').filter({hasText:'Command cancelled.'}).waitFor();
  assert.equal(starts,3);assert.equal(cancels,5);
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth+1),false);
  assert.deepEqual(errors,[]);await page.close();
  console.log(`PASS: Cancel button, Ctrl+C, normal copy, partial output, next draft, renewed MFA and responsive layout at ${width}px`);
 }}finally{await browser.close();}
})().catch(error=>{console.error(error);process.exit(1)});
