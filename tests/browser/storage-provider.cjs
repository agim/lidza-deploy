const {chromium}=require('playwright');const assert=require('node:assert/strict');
(async()=>{const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});try{for(const width of [390,1440]){
 const page=await browser.newPage({viewport:{width,height:900}}),errors=[],patches=[];page.on('pageerror',error=>errors.push(error.message));
 let supported=true,override=true;
 const app={id:'portal',repository:'acme/portal',branch:'master',domain:'portal.example.com',server_id:'local'};
 await page.route('**/api/control/**',async route=>{const request=route.request(),path=new URL(request.url()).pathname;let json=[];
  if(path.endsWith('/status'))json={roles:['admin']};else if(path.endsWith('/apps'))json=[app];else if(path.endsWith('/servers'))json=[{id:'local',name:'Production'}];else if(path.endsWith('/deployments'))json={deployments:[]};else if(path.endsWith('/settings')){if(request.method()==='PATCH'){patches.push(request.postDataJSON());override=false;}json={...app,persistent_storage:true,app_managed_storage_supported:supported,storage_provider_override:override,env_keys:override?['STORAGE_DIR','STORAGE_PROVIDER']:['STORAGE_DIR']};}
  await route.fulfill({json});
 });
 const base=process.env.TEST_WEB_URL||'http://127.0.0.1:8880';
 await page.goto(base+'/console.html#page/settings/portal');await page.locator('#workspace-loading').waitFor({state:'hidden'});
 assert.match(await page.locator('#settings-storage-status').textContent(),/override controls the storage provider/);
 await page.locator('#settings-form [name=domain]').fill('unsaved.example.com');await page.locator('#settings-storage-unpin').click();
 assert.equal(patches.length,0);assert.match(await page.locator('#settings-error').textContent(),/Save or discard/);
 await page.reload();await page.locator('#workspace-loading').waitFor({state:'hidden'});await page.locator('#settings-storage-unpin').click();
 await page.waitForFunction(()=>document.querySelector('#settings-storage-unpin').hidden);
 assert.deepEqual(patches,[{branch:'master',domain:'portal.example.com',env_changes:{STORAGE_PROVIDER:null}}]);
 assert.match(await page.locator('#settings-storage-status').textContent(),/Deploy does not set a storage provider/);
 assert.match(await page.locator('#toast').textContent(),/Local files are preserved/);
 override=true;supported=false;await page.reload();await page.locator('#workspace-loading').waitFor({state:'hidden'});
 assert(await page.locator('#settings-storage-unpin').isDisabled());assert.match(await page.locator('#settings-storage-status').textContent(),/Update this hosting agent/);
 assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);assert.deepEqual(errors,[]);await page.close();
}console.log('PASS: mobile/desktop storage override removal preserves other settings, protects unsaved drafts, shows app-managed state and requires a compatible agent.');}finally{await browser.close()}})().catch(error=>{console.error(error);process.exit(1)});
