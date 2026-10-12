const {chromium}=require('playwright');const assert=require('node:assert/strict');
(async()=>{const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});try{
for(const width of [390,1440]){
const page=await browser.newPage({viewport:{width,height:844}}),errors=[],calls=[];page.on('pageerror',e=>errors.push(e.message));
let data={control_version:'v0.2.14',settings:{automatic:false,latest:'v0.2.15',checked:'2026-10-10T03:00:00Z',targets:[]},servers:[{server_id:'local',name:'This server',local:true,current:'v0.2.14',supported:true,state:'idle'},{server_id:'remote',name:'Remote host',local:false,current:'v0.2.14',supported:true,state:'idle'}]},offline=false,checkGate=null,finishCheck;
const go=async section=>{if(width<761)await page.locator('#menu-toggle').click();await page.locator('[data-tab='+section+']').click();};
await page.route('**/api/control/**',async route=>{const path=new URL(route.request().url()).pathname,method=route.request().method();
 if(path.includes('/updates')){calls.push({path,method,body:route.request().postDataJSON()});if(offline){await route.fulfill({status:503,json:{error:'Updates unavailable'}});return;}if(path.endsWith('/check')){if(checkGate)await checkGate;data.settings.checked='2026-10-11T22:06:24Z';}if(method==='PATCH')data.settings.automatic=route.request().postDataJSON().automatic;if(path.endsWith('/install'))data.settings.targets=[{server_id:'remote',state:'waiting',message:'Queued remote first'},{server_id:'local',state:'waiting',message:'Control panel last'}];await route.fulfill({json:path.endsWith('/updates')&&method==='GET'?data:data.settings});return;}
 const name=path.split('/').pop();await route.fulfill({json:name==='server-health'?{servers:[]}:name==='status'?{roles:['admin']}:name==='deployments'?{deployments:[],unavailable_servers:[]}:name==='servers'?data.servers.map(s=>({id:s.server_id,name:s.name,url:s.local?'http://127.0.0.1:9090':'https://agent.example'})):[]});});
await page.goto((process.env.TEST_WEB_URL||'http://127.0.0.1:8880')+'/console.html#updates');await page.locator('#updates-check').waitFor();assert(await page.getByText('Remote host',{exact:true}).isVisible());assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth-innerWidth),0);
await page.evaluate(()=>window.stableUpdate=document.querySelector('#updates-check'));await page.evaluate(()=>refresh({background:true}));assert(await page.evaluate(()=>stableUpdate===document.querySelector('#updates-check')),'Unchanged update status replaced DOM');
await page.locator('#updates-check').click();assert(calls.some(c=>c.path.endsWith('/updates/check')&&c.method==='POST'));await page.waitForFunction(()=>updateView.settings.checked==='2026-10-11T22:06:24Z');assert.equal(await page.locator('#title').textContent(),'Updates.');
await page.locator('#updates-auto').check();await page.waitForFunction(()=>document.querySelector('#updates-auto').checked);assert(calls.some(c=>c.method==='PATCH'&&c.body.automatic===true));
await page.locator('#updates-install').click();await page.getByText('Queued remote first',{exact:true}).waitFor();assert(await page.locator('#updates-install').isDisabled());
offline=true;await page.evaluate(()=>loadUpdates());assert(await page.getByText('Queued remote first',{exact:true}).isVisible(),'Offline poll cleared update progress');
assert.match(await page.locator('#error').textContent(),/Updates unavailable/);await page.locator('#error .notice-dismiss').click();
await go('apps');offline=false;await go('updates');await page.locator('#updates-check').waitFor();
checkGate=new Promise(resolve=>finishCheck=resolve);const beforeChecks=calls.filter(c=>c.path.endsWith('/check')).length;
await page.locator('#updates-check').click();await page.waitForFunction(()=>document.querySelector('#updates-check').disabled);
assert.equal(calls.filter(c=>c.path.endsWith('/check')).length,beforeChecks+1);
await go('servers');await page.locator('[data-server-inventory]').waitFor();assert.equal(await page.locator('#updates-check').count(),0);assert.equal(await page.locator('#title').textContent(),'Servers.');
await page.evaluate(()=>{window.inventory=document.querySelector('[data-server-inventory]');window.hostButton=inventory.querySelector('[data-manage-server]');hostButton.focus({preventScroll:true});window.serverY=scrollY;});
finishCheck();checkGate=null;await page.waitForFunction(()=>document.querySelector('#toast').textContent.includes('Release check complete.'));await page.waitForFunction(()=>!updatesLoading);
assert(await page.evaluate(()=>tab==='servers'&&inventory===document.querySelector('[data-server-inventory]')&&document.activeElement===hostButton&&scrollY===serverY));assert.equal(await page.locator('[data-updates-panel]').count(),0);
await page.evaluate(()=>refresh({background:true}));assert.equal(await page.locator('[data-server-card]').count(),2);assert.equal(await page.locator('#updates-check').count(),0);
await go('updates');await page.locator('[data-update-hosts]').waitFor();assert.equal(await page.locator('[data-server-inventory]').count(),0);assert.equal(await page.locator('#title').textContent(),'Updates.');
await page.locator('#updates-check').click();await page.waitForFunction(()=>!document.querySelector('#updates-check').disabled);assert.equal(errors.length,0,errors.join('\n'));assert.equal(await page.locator('#error').isVisible(),false);await page.close();
}console.log('PASS: desktop/mobile release checks, automatic policy, fleet queue, offline retention, Updates→Servers→Updates and in-flight check navigation preserve the correct view, focus and scroll');
}finally{await browser.close()}})().catch(e=>{console.error(e);process.exit(1)});
