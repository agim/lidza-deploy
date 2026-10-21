const {chromium}=require('playwright');
const assert=require('node:assert/strict');
const {mockConsole}=require('./fixtures/console.cjs');
const base=process.env.TEST_WEB_URL||'http://127.0.0.1:34225';
(async()=>{const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});try{
 for(const width of [320,390,768,1440]){
  for(const imported of [false,true]){
   const page=await browser.newPage({viewport:{width,height:900}}),errors=[];page.on('pageerror',e=>errors.push(e.message));
   const state=await mockConsole(page);await page.goto(`${base}/console.html#new-application`);
   await page.getByRole('button',{name:'Browse',exact:true}).click();await page.getByRole('button',{name:'acme/portal Private · main'}).click();
   await page.locator('[name=id]').fill('master-fixture');await page.getByRole('button',{name:'Continue',exact:true}).click();
   await page.locator('[name=domain]').fill('fixture.example.com');await page.locator('[name=database_mode]').selectOption('none');
   await page.getByRole('button',{name:'Continue',exact:true}).click();
   const field=page.getByLabel('Application master key',{exact:true});assert.equal(await field.getAttribute('type'),'password');
   assert.equal(await field.inputValue(),'');const key='ab'.repeat(32);
   if(imported){
    await field.fill('not-a-key');await page.getByRole('button',{name:'Create application',exact:true}).click();
    assert.equal(state.calls.filter(c=>c.path==='control/apps'&&c.method==='POST').length,0);
    await field.fill(key);await page.getByRole('button',{name:'Back',exact:true}).click();await page.getByRole('button',{name:'Continue',exact:true}).click();
    assert.equal(await field.inputValue(),key);
   }
   await page.getByRole('button',{name:'Create application',exact:true}).click();await page.waitForURL(url=>url.hash==='#applications');
   const body=state.calls.find(c=>c.path==='control/apps'&&c.method==='POST').body;
   assert.equal(body.env.LIDZA_MASTER_KEY,imported?key:undefined);
   assert.equal(body.env_kinds.LIDZA_MASTER_KEY,imported?'credential':undefined);
   assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);assert.deepEqual(errors,[]);
   await page.close();console.log(`PASS: ${imported?'imported key, validation and draft retention':'server-generated key default'} at ${width}px`);
  }
 }
}finally{await browser.close()}})().catch(e=>{console.error(e);process.exit(1)});
