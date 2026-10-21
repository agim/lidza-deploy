const {chromium}=require('playwright');
const assert=require('node:assert/strict');
const {mockConsole}=require('./fixtures/console.cjs');
const base=process.env.TEST_WEB_URL||'http://127.0.0.1:34225';
(async()=>{const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});try{
 for(const width of [320,390,768,1440]){
  const page=await browser.newPage({viewport:{width,height:900}}),errors=[];
  page.on('pageerror',e=>errors.push(e.message));page.on('dialog',d=>{errors.push('Native dialog');d.dismiss()});
  const state=await mockConsole(page);
  await page.goto(`${base}/console.html#page/settings/portal`);
  await page.locator('[name=branch]').fill('release');await page.locator('[name=domain]').fill('draft.example.com');
  await page.getByRole('tab',{name:'Environment',exact:true}).click();
  const setting=page.locator('.env-row').filter({has:page.locator('input[value="LOG_LEVEL"]')});await setting.locator('.env-value').fill('debug');
  await page.getByRole('button',{name:'Generate master key',exact:true}).click();
  const modal=page.getByRole('alertdialog',{name:'Generate application master key'});
  await modal.getByRole('button',{name:'Cancel',exact:true}).click();
  assert.equal(state.calls.filter(c=>c.path.endsWith('/master-key')).length,0);
  await page.getByRole('button',{name:'Generate master key',exact:true}).click();
  await modal.getByRole('button',{name:'Generate & save key',exact:true}).click();await modal.waitFor({state:'hidden'});
  assert.equal(state.calls.filter(c=>c.path.endsWith('/master-key')).length,1);
  const key=page.locator('.env-row').filter({has:page.locator('input[value="LIDZA_MASTER_KEY"]')});
  await key.waitFor();assert.equal(await key.locator('.env-value').inputValue(),'');assert.equal(await key.locator('.env-kind').inputValue(),'credential');
  assert.match(await key.locator('.env-value').getAttribute('placeholder'),/64 characters/);
  assert.equal(await page.getByRole('button',{name:'Generate master key',exact:true}).count(),0);
  assert.equal(await setting.locator('.env-value').inputValue(),'debug');
  await page.getByRole('button',{name:'Save changes',exact:true}).click();await page.waitForURL(url=>url.hash==='#applications');
  const patch=state.calls.find(c=>c.path.endsWith('/settings')&&c.method==='PATCH').body;
  assert.equal(patch.branch,'release');assert.equal(patch.domain,'draft.example.com');assert.equal(patch.env_changes.LOG_LEVEL,'debug');assert.equal(patch.env_changes.LIDZA_MASTER_KEY,undefined);
  await page.goto(`${base}/console.html#page/settings/portal`);await page.getByRole('tab',{name:'Environment',exact:true}).click();
  assert.equal(await page.getByRole('button',{name:'Generate master key',exact:true}).count(),0);
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);assert.deepEqual(errors,[]);assert.deepEqual(state.unknown,[]);
  await page.close();console.log(`PASS: confirmed server generation, masked key, no rotation and unsaved draft preservation at ${width}px`);
 }
}finally{await browser.close()}})().catch(e=>{console.error(e);process.exit(1)});
