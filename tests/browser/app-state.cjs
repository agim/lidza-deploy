const {chromium}=require('playwright');const assert=require('node:assert/strict');
(async()=>{const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});try{
for(const width of [390,1440]){
 const page=await browser.newPage({viewport:{width,height:844}}),errors=[],calls=[];page.on('pageerror',e=>errors.push(e.message));
 const app={id:'portal',repository:'acme/portal',branch:'master',domain:'portal.example.com',server_id:'local',current:{commit:'abcdefg'},stopped:false};
 await page.route('**/api/control/**',async route=>{const r=route.request(),p=new URL(r.url()).pathname;let json=[];
 if(p.endsWith('/status'))json={roles:['admin']};else if(p.endsWith('/servers'))json=[{id:'local',name:'Hosting server'}];else if(p.endsWith('/apps'))json=[app];else if(p.endsWith('/deployments'))json={deployments:[]};else if(p.endsWith('/state')){calls.push(r.postDataJSON());app.stopped=r.postDataJSON().stopped;json={status:'accepted'}}
 await route.fulfill({json});});
 await page.goto((process.env.TEST_WEB_URL||'http://127.0.0.1:8880')+'/console.html#applications');await page.locator('[data-app-menu] summary').click();
 await page.locator('[data-app-state]').click();await page.locator('#confirm-app-state').waitFor();assert.match(await page.locator('#feature-dialog').textContent(),/database, backups, files/);assert.equal(calls.length,0,'stopped without confirmation');
 await page.locator('#confirm-app-state').click();await page.waitForFunction(()=>document.querySelector('[data-app-state]')?.textContent==='Start application');
 assert.equal(await page.locator('[data-action=deploy]').isDisabled(),true);assert.equal(await page.locator('[data-action=reload]').isDisabled(),true);assert.match(await page.locator('.card-head .pill').textContent(),/stopped/);
 await page.evaluate(()=>refresh({background:true}));assert.match(await page.locator('[data-app-state]').textContent(),/Start application/);
 await page.locator('[data-app-state]').click();await page.locator('#confirm-app-state').click();await page.waitForFunction(()=>document.querySelector('[data-app-state]')?.textContent==='Stop application');assert.equal(await page.locator('[data-action=deploy]').isDisabled(),false);
 assert.deepEqual(calls,[{stopped:true},{stopped:false}]);assert.deepEqual(errors,[]);await page.close();
}
console.log('PASS: mobile/desktop stop/start confirmation, persisted stopped badge, disabled deploy/reload and retained state on live refresh');
}finally{await browser.close()}})().catch(e=>{console.error(e);process.exit(1)});
