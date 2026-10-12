const {chromium}=require('playwright');
const assert=require('node:assert/strict');
(async()=>{const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});try{for(const width of [390,1440]){
 const page=await browser.newPage({viewport:{width,height:900}}),errors=[];page.on('pageerror',e=>errors.push(e.message));
 await page.goto((process.env.TEST_WEB_URL||'http://127.0.0.1:8880')+'/console.html?demo=1#applications');
 await page.evaluate(()=>{status.control_version='v0.2.41';checkUIVersion()});assert.equal(await page.locator('#ui-update-notice').count(),0);
 await page.evaluate(()=>openSettings(apps[0].id));await page.locator('#settings-form [name=domain]').fill('unsaved.example.com');
 const url=page.url();await page.evaluate(()=>{window.form=document.querySelector('#settings-form');window.input=form.elements.domain;input.focus({preventScroll:true});window.savedY=scrollY;status.control_version='v0.2.42';checkUIVersion();checkUIVersion()});
 assert.equal(await page.locator('#ui-update-notice').count(),1);assert(await page.locator('#ui-update-notice').isVisible());assert.match(await page.locator('#ui-update-notice').textContent(),/v0.2.42/);
 assert(await page.evaluate(()=>form===document.querySelector('#settings-form')&&document.activeElement===input&&input.value==='unsaved.example.com'&&scrollY===savedY));
 page.once('dialog',d=>d.dismiss());await page.locator('#ui-update-notice button').click();assert.equal(page.url(),url);assert.equal(await page.locator('#settings-form [name=domain]').inputValue(),'unsaved.example.com');
 await page.evaluate(()=>{status.control_version='v0.2.41';checkUIVersion()});assert.equal(await page.locator('#ui-update-notice').isVisible(),false);
 assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);assert.deepEqual(errors,[]);await page.close();
}console.log('PASS: release-change notice preserves drafts, focused fields, route and scroll; canceled reload retains edits; rollback clears notice on desktop/mobile')}finally{await browser.close()}})().catch(e=>{console.error(e);process.exit(1)});
