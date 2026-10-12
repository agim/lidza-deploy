const {chromium}=require('playwright');
const assert=require('node:assert/strict');
const base=process.env.TEST_WEB_URL||'http://127.0.0.1:8880';
function gate(){let release;const promise=new Promise(resolve=>release=resolve);return {promise,release};}
(async()=>{const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});try{
 for(const width of [390,1440]){
  const page=await browser.newPage({viewport:{width,height:844}}),errors=[];
  page.setDefaultTimeout(10000);
  page.on('pageerror',error=>errors.push(error.message));
  let statusGate=gate(),healthGate=null,settingsGate=null,statusRequests=0,healthRequests=0,settingsRequests=0,unavailable=false;
  const apps=Array.from({length:12},(_,i)=>({id:'app-'+i,repository:'acme/app-'+i,branch:'master',domain:'app-'+i+'.example.com',server_id:'local',current:{commit:'abcdef'}}));
  await page.route('**/api/control/**',async route=>{
   const path=new URL(route.request().url()).pathname;let json=[];
   if(path.endsWith('/status')){statusRequests++;if(statusGate)await statusGate.promise;if(unavailable){await route.fulfill({status:503,json:{error:'Control panel unavailable'}});return;}json={roles:['admin'],github_app_connected:false};}
   else if(path.endsWith('/servers'))json=[{id:'local',name:'Production host',url:'http://127.0.0.1:9090'}];
   else if(path.endsWith('/apps'))json=apps;
   else if(path.endsWith('/deployments'))json={deployments:[],unavailable_servers:[]};
   else if(path.endsWith('/server-health')){healthRequests++;if(healthGate)await healthGate.promise;json={servers:[]};}
   else if(path.endsWith('/settings')){settingsRequests++;if(settingsGate)await settingsGate.promise;json={...apps[0],env_keys:[],env:{},database:{mode:'none'}};}
   await route.fulfill({json});
  });
  await page.goto(base+'/console.html#applications');
  await page.waitForFunction(()=>document.querySelector('main').getAttribute('aria-busy')==='true');
  assert(await page.locator('#workspace-loading').isVisible());
  assert.equal(await page.locator('.app-card').count(),0);
  statusGate.release();statusGate=null;
  await page.locator('#workspace-loading').waitFor({state:'hidden'});
  assert.equal(await page.locator('.app-card').count(),12);
  await page.evaluate(()=>{clearInterval(poll);window.originalCard=document.querySelector('.app-card');window.originalSearch=document.querySelector('#search');originalSearch.focus({preventScroll:true});window.scrollTo(0,250);window.originalY=scrollY;});
  statusGate=gate();
  const backgroundRequest=page.waitForRequest('**/control/status');
  await page.evaluate(()=>{window.backgroundRefresh=refresh({background:true});});
  await backgroundRequest;
  assert.equal(await page.locator('#workspace-loading').isVisible(),false,'Background refresh covered the current page');
  assert(await page.evaluate(()=>originalCard===document.querySelector('.app-card')&&document.activeElement===originalSearch&&scrollY===originalY));
  statusGate.release();statusGate=null;await page.evaluate(()=>backgroundRefresh);
  assert(await page.evaluate(()=>originalCard===document.querySelector('.app-card')&&document.activeElement===originalSearch&&scrollY===originalY));
  healthGate=gate();const previousHealth=healthRequests;
  if(width<761)await page.locator('#menu-toggle').click();
  await page.locator('[data-tab=servers]').click();
  await page.locator('#workspace-loading').waitFor({state:'visible'});
  await page.waitForFunction(()=>!refreshing);
  assert(healthRequests>previousHealth);
  assert(await page.locator('#workspace-loading').isVisible(),'Navigation overlay disappeared before server data was ready');
  healthGate.release();healthGate=null;
  await page.locator('#workspace-loading').waitFor({state:'hidden'});
  assert.equal(await page.locator('#title').textContent(),'Servers.');
  assert.equal(await page.locator('[data-server-card]').count(),1);
  settingsGate=gate();
  await page.goto(base+'/console.html#page/settings/app-0');
  await page.locator('#workspace-loading').waitFor({state:'visible'});
  await page.waitForFunction(()=>!refreshing);
  assert(settingsRequests>0);
  assert(await page.locator('#workspace-loading').isVisible(),'Deep-linked form appeared before its values loaded');
  settingsGate.release();settingsGate=null;
  await page.locator('#workspace-loading').waitFor({state:'hidden'});
  assert.equal(await page.locator('#settings-form [name=domain]').inputValue(),'app-0.example.com');
  await page.goto(base+'/console.html#new-application');
  await page.locator('#workspace-loading').waitFor({state:'hidden'});
  assert(await page.locator('#app-form [name=auto_deploy]').isDisabled());
  assert.match(await page.locator('#create-auto-hint').textContent(),/Connect GitHub/);
  unavailable=true;
  await page.goto(base+'/console.html#applications');
  await page.locator('#workspace-loading').waitFor({state:'hidden'});
  assert.match(await page.locator('#error').textContent(),/Control panel unavailable/);
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
  assert.deepEqual(errors,[]);await page.close();
 }
 console.log('PASS: mobile/desktop initial, navigation and deep-link loading wait for dynamic values; background refresh retains visible data, focus and scroll; failures release the overlay; disconnected GitHub disables auto-deploy.');
}finally{await browser.close()}})().catch(error=>{console.error(error);process.exit(1)});
