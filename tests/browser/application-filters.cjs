const {chromium}=require('playwright');
const assert=require('node:assert/strict');

(async()=>{
 const browser=await chromium.launch({executablePath:process.env.TEST_CHROMIUM_PATH||'/usr/bin/chromium',args:['--no-sandbox']});
 try{
  for(const width of [390,1440]){
   const page=await browser.newPage({viewport:{width,height:900}}),errors=[];
   page.on('pageerror',error=>errors.push(error.message));
   const apps=Array.from({length:30},(_,index)=>{const id='app-'+String(index).padStart(2,'0');return {id,domain:id+'.example.com',server_id:'one',repository:'acme/'+id,branch:'main',current:{commit:'abcdef0',database_ids:[id]}}});
   const deployments=apps.map((app,index)=>({id:'release-'+index,app_id:app.id,status:'live',commit:'abcdef'+index,created:new Date(Date.UTC(2026,9,13,12,index)).toISOString()}));
   const databases=apps.map(app=>({app_id:app.id,server_id:'one',ready:true,mode:'local',attachments:[{app_id:app.id,env_key:'DATABASE_URL'}],backup:{hours:24,keep:7},backups:[]}));
   await page.route('**/api/control/**',async route=>{const path=new URL(route.request().url()).pathname;let json={};
    if(path.endsWith('/status'))json={roles:['admin']};
    else if(path.endsWith('/apps'))json=apps;
    else if(path.endsWith('/servers'))json=[{id:'one',name:'Production',url:'https://agent.example.com'}];
    else if(path.endsWith('/deployments'))json={deployments,unavailable_servers:[]};
    else if(path.endsWith('/deliveries'))json=[];
    else if(path.endsWith('/databases'))json={databases};
    else if(path.endsWith('/infrastructure'))json={storage:{},mail:{},messages:[],incidents:[]};
    else if(path.endsWith('/storage/backups'))json={bucket:'private',prefix:'lidza-deploy',checked:new Date().toISOString(),objects:[0,17,29].map(index=>({key:'databases/app-'+String(index).padStart(2,'0')+'/snapshot.dump',size:4096,modifiedAt:new Date().toISOString()}))};
    else if(path.endsWith('/errors'))json={status:'ready',errors:[]};
    else if(path.endsWith('/security'))json={status:'ready',errors:[]};
    await route.fulfill({json});
   });
   const base=process.env.TEST_WEB_URL||'http://127.0.0.1:8880';
   await page.goto(base+'/console.html#deployments');await page.locator('#deployments-app-filter').waitFor();
   assert.equal(await page.locator('#release-table tbody tr[data-release]').count(),1,'Deployments rendered every app');
   const releaseFilter=page.locator('#deployments-app-filter');await releaseFilter.fill('app-19');await page.waitForFunction(()=>document.querySelector('#release-table tbody tr[data-release] strong')?.textContent==='app-19');
   assert.equal(await page.locator('#deployments-app-filter-options option').count(),30);
   await page.evaluate(()=>{window.releaseFilterNode=document.querySelector('#deployments-app-filter');deployments.find(d=>d.app_id==='app-19').duration_seconds=12;render()});
   assert(await page.evaluate(()=>releaseFilterNode===document.querySelector('#deployments-app-filter')),'Deployment refresh replaced the application picker');
   await page.goto(base+'/console.html#databases');await page.locator('#databases-app-filter').waitFor();
   await page.waitForFunction(()=>document.querySelectorAll('[data-database-card]').length===1);
   const databaseState=await page.evaluate(()=>({apps:apps.length,databases:databases.length,selected:applicationSelections.backups,input:document.querySelector('#databases-app-filter')?.value,html:document.querySelector('#database-inventory')?.innerText}));
   assert.equal(await page.locator('[data-database-card]').count(),1,'Databases rendered every app: '+JSON.stringify(databaseState));
   await page.locator('#databases-app-filter').fill('app-23');await page.waitForFunction(()=>document.querySelector('[data-database-card]')?.dataset.databaseCard==='one/app-23');
   await page.evaluate(()=>{window.databaseFilterNode=document.querySelector('#databases-app-filter');window.databaseCardNode=document.querySelector('[data-database-card]');databases.find(d=>d.app_id==='app-23').operation='backing up';render()});
   assert(await page.evaluate(()=>databaseFilterNode===document.querySelector('#databases-app-filter')&&databaseCardNode===document.querySelector('[data-database-card]')),'Database refresh replaced the picker or selected card');
   await page.goto(base+'/console.html#errors');await page.locator('#errors-app').waitFor();assert.equal(await page.locator('#errors-app').getAttribute('list'),'errors-app-options');
   await page.locator('#errors-app').fill('app-11');await page.waitForFunction(()=>errorView.app==='app-11');
   await page.goto(base+'/console.html#security');await page.locator('#security-app').waitFor();assert.equal(await page.locator('#security-app').getAttribute('list'),'security-app-options');
   await page.locator('#security-app').fill('app-17');await page.waitForFunction(()=>securityApp==='app-17');
   await page.goto(base+'/console.html#remote-backups');await page.locator('#remote-backup-filter').waitFor();await page.waitForFunction(()=>remoteBackupObjects.length===3);await page.locator('#remote-backup-filter').fill('app-17');assert.equal(await page.locator('#remote-backup-rows tr').count(),1,'Remote backups ignored the object filter');
   await page.goto(base+'/console.html#servers');await page.getByText('＋ 25 more applications',{exact:true}).waitFor();
   assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false,'Application filters overflow the viewport');
   assert.deepEqual(errors,[]);await page.close();
  }
  console.log('PASS: searchable application pickers limit fleet pages to one app and survive live refreshes on mobile and desktop');
 }finally{await browser.close()}
})().catch(error=>{console.error(error);process.exit(1)});
