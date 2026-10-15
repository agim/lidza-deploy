const { chromium } = require('playwright');
const assert = require('node:assert/strict');
const { mockConsole } = require('./fixtures/console.cjs');
const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8880';
(async () => {
 const browser = await chromium.launch({ executablePath: '/usr/bin/chromium', args: ['--no-sandbox'] });
 try { for (const width of [390,1440]) {
  const page = await browser.newPage({ viewport: { width, height: 900 } }), errors=[];
  page.on('pageerror', e=>errors.push(e.message));
  const state=await mockConsole(page); state.consoleGranted=true;
  let starts=0,id='',dropped=false,expire=false,missing=false;
  await page.route('**/api/control/servers/local/console/jobs**',async route=>{
   const request=route.request(), url=new URL(request.url());
   if(request.method()==='POST') {
    starts++; id=request.postDataJSON().id;
    return route.fulfill({status:502,body:'Bad Gateway'}); // Agent accepted; acknowledgement lost.
   }
   assert(url.pathname.endsWith(id));
   if(missing) return route.fulfill({status:404,json:{error:'Result unavailable; check outcome before rerunning.'}});
   if(expire) return route.fulfill({status:403,json:{error:'Verify again.'}});
   const offset=Number(url.searchParams.get('offset'));
   if(offset && !dropped) { dropped=true; return route.abort('failed'); }
   return route.fulfill({json:{id,state:offset?'finished':'running',output:offset?'second':'first',next_offset:offset?11:5,output_size:11,exit_code:7,duration_seconds:2,truncated:false}});
  });
  await page.goto(`${base}/console.html#page/console/local`);
  await page.locator('#console-command textarea').fill('printf first; sleep 2; printf second; exit 7');
  await page.getByRole('button',{name:'Run command',exact:true}).click();
  await page.getByRole('status').filter({hasText:'Connection interrupted'}).waitFor();
  assert.equal(await page.locator('.console-output').innerText(),'first');
  assert.equal(await page.locator('#console-command textarea').inputValue(),'');
  await page.reload();
  await page.getByRole('status').filter({hasText:'Command finished.'}).waitFor();
  assert.match(await page.locator('.console-output').innerText(),/^firstsecond\n\n\[exit 7/);
  assert.equal(starts,1,'Reconnect executed command twice');
  await page.getByRole('button',{name:'Copy output',exact:true}).click();
  assert.match(await page.evaluate(()=>window.consoleCopied),/firstsecond/);
  // An expired MFA unlock pauses reads and resumes the same job after verification.
  expire=true; await page.reload();
  await page.getByText('Verify again to reconnect to this command.',{exact:true}).waitFor();
  expire=false;
  await page.locator('#console-unlock input').fill('654321');
  await page.getByRole('button',{name:'Unlock for 15 minutes',exact:true}).click();
  await page.getByRole('status').filter({hasText:'Command finished.'}).waitFor();
  assert.match(await page.locator('.console-output').innerText(),/^firstsecond/);
  assert.equal(starts,1,'MFA renewal repeated the command');
  // A stale result after an agent restart must report unknown, never rerun it.
  missing=true; await page.reload();
  await page.getByRole('status').filter({hasText:'Result unavailable.'}).waitFor();
  assert.equal(starts,1);
  assert.equal(await page.getByRole('button',{name:'Run command',exact:true}).isEnabled(),true);
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth+1),false);
  assert.deepEqual(errors,[]);
  await page.close(); console.log(`PASS: lost acknowledgement, live chunks, reconnect, reload, exit status, clipboard and missing-result recovery at ${width}px`);
 }} finally { await browser.close(); }
})().catch(error=>{console.error(error);process.exit(1)});
