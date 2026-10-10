const {chromium}=require('playwright');const assert=require('node:assert/strict');
(async()=>{const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});try{
for(const width of [390,1440]){
 const page=await browser.newPage({viewport:{width,height:844}}),errors=[],calls=[];
 page.on('pageerror',e=>errors.push(e.message));
 let profile={values:{DB_MIGRATE:'true',LOG_LEVEL:'info',ADMIN_USERS:'shared@example.com'},revision:'reviewed'};
 const app={id:'portal',repository:'acme/portal',branch:'master',domain:'portal.example.com',server_id:'local',env_keys:['ADMIN_USERS','AUTH_SECRET'],env_sources:{ADMIN_USERS:'workspace'}};
 await page.route('**/api/control/**',async route=>{
  const r=route.request(),path=new URL(r.url()).pathname;let json=[];
  if(path.endsWith('/status'))json={roles:['admin']};else if(path.endsWith('/servers'))json=[{id:'local',name:'Hosting server'}];else if(path.endsWith('/apps'))json=[app];else if(path.endsWith('/deployments'))json={deployments:[]};else if(path.endsWith('/application-defaults')){if(r.method()==='PUT'){calls.push({kind:'profile',...r.postDataJSON()});profile={values:r.postDataJSON().values,revision:'updated'}}json=profile;}else if(path.endsWith('/apply-defaults')){calls.push({kind:'apply',...r.postDataJSON()});json={saved:true};}else if(path.endsWith('/settings')){if(r.method()==='PATCH')calls.push({kind:'settings',...r.postDataJSON()});json=app;}
  await route.fulfill({json});
 });
 await page.goto((process.env.TEST_WEB_URL||'http://127.0.0.1:8880')+'/console.html#defaults');
 const admin=page.locator('#defaults-form [name=ADMIN_USERS]');await page.waitForFunction(()=>document.querySelector('#defaults-form [name=ADMIN_USERS]')?.value==='shared@example.com');
 await admin.fill('agim@albaspot.com');await page.evaluate(()=>{window.defaultInput=document.querySelector('#defaults-form [name=ADMIN_USERS]');defaultInput.focus();window.savedY=scrollY;render();render()});
 assert(await page.evaluate(()=>defaultInput===document.querySelector('#defaults-form [name=ADMIN_USERS]')&&document.activeElement===defaultInput&&defaultInput.value==='agim@albaspot.com'&&scrollY===savedY));
 await page.locator('#defaults-form button[type=submit]').click();await page.waitForFunction(()=>!dirtyForms.size);
 assert.equal(calls[0].values.ADMIN_USERS,'agim@albaspot.com');assert(!Object.hasOwn(calls[0].values,'MAIL_FROM'),'empty defaults submitted');
 await page.evaluate(()=>navigate('apps'));await page.locator('[data-action=settings]').click();await page.locator('#settings-dialog[open]').waitFor();
 assert.match(await page.locator('#settings-env-editor').textContent(),/Workspace default/);
 // Browser autofill or programmatic fill can change a value without an input event.
 await page.evaluate(()=>{const row=[...document.querySelectorAll('#settings-env-editor .env-row')].find(r=>r.querySelector('.env-name').value==='ADMIN_USERS');row.querySelector('.env-value').value='direct@example.com'});
 await page.locator('#settings-form button[type=submit]').click();await page.waitForFunction(()=>!document.querySelector('#settings-dialog').open);
 assert.equal(calls.find(c=>c.kind==='settings').env_changes.ADMIN_USERS,'direct@example.com');
 await page.locator('[data-action=settings]').click();await page.locator('#settings-defaults').click();
 assert.equal(await page.locator('#apply-defaults-form [value=ADMIN_USERS]').isChecked(),false);
 await page.locator('#apply-defaults-form [value=ADMIN_USERS]').check();await page.locator('#apply-defaults-form button[type=submit]').click();await page.waitForFunction(()=>!document.querySelector('#feature-dialog').open);
 const applied=calls.find(c=>c.kind==='apply');assert.equal(applied.replace,false);assert.equal(applied.revision,'updated');assert(applied.keys.includes('ADMIN_USERS'));
 assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'mobile overflow');assert.deepEqual(errors,[]);await page.close();
}
console.log('PASS: mobile/desktop defaults save, empty omission, refresh stability, origins, direct environment editor, reviewed apply');
}finally{await browser.close()}})().catch(e=>{console.error(e);process.exit(1)});
