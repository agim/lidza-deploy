const {chromium}=require('playwright');
const assert=require('node:assert/strict');

(async()=>{
 const browser=await chromium.launch({executablePath:process.env.TEST_CHROMIUM_PATH||'/usr/bin/chromium',args:['--no-sandbox']});
 try{
  for(const width of [390,1440]){
   const page=await browser.newPage({viewport:{width,height:900}}),errors=[];
   page.on('pageerror',error=>errors.push(error.message));
   await page.goto((process.env.TEST_WEB_URL||'http://127.0.0.1:8880')+'/console.html?demo=1#databases');
   await page.evaluate(()=>{
    servers=[{id:'one',name:'Production'}];
    apps=['alpha','beta'].map(id=>({id,domain:id+'.example.com',server_id:'one'}));
    applicationSelections.backups='alpha';
    databases=['alpha','beta'].map(app_id=>({app_id,server_id:'one',ready:true,mode:'local',backup:{hours:24,keep:20},backups:Array.from({length:8},(_,i)=>({id:'snapshot-'+i,created:new Date(Date.UTC(2026,9,11,12,i)).toISOString(),size:4096,offsite:true,object_key:'databases/'+app_id+'/snapshot-'+i+'.dump',sha256:'a'.repeat(64)}))}));
    render();
   });
   await page.evaluate(()=>{
    window.filterNode=document.querySelector('#databases-app-filter');window.card=document.querySelector('[data-database-card="one/alpha"]');window.scroller=card.querySelector('.table-wrap');window.row=card.querySelector('[data-backup="snapshot-5"]');window.detail=row.querySelector('details');detail.open=true;window.button=row.querySelector('[data-restore]');button.focus({preventScroll:true});row.scrollIntoView({block:'center'});scroller.scrollLeft=200;window.rowTop=row.getBoundingClientRect().top;window.oldY=scrollY;window.oldLeft=scroller.scrollLeft;
   });
   await page.evaluate(()=>{
    databases.reverse();databases.forEach(database=>database.backups.reverse());render();databases[0].backups.push({id:'snapshot-new',created:'2026-10-11T13:00:00Z',size:8192,offsite:true,object_key:'databases/beta/snapshot-new.dump'});databases.find(database=>database.app_id==='alpha').backups.push({id:'alpha-new',created:'2026-10-11T13:00:00Z',size:4096,offsite:true});databases.find(database=>database.app_id==='alpha').backups.find(backup=>backup.id==='snapshot-5').size=8192;render();
   });
   assert(await page.evaluate(()=>filterNode===document.querySelector('#databases-app-filter')&&card===document.querySelector('[data-database-card="one/alpha"]')&&scroller===card.querySelector('.table-wrap')&&row===card.querySelector('[data-backup="snapshot-5"]')&&detail===row.querySelector('details')&&detail.open&&button===row.querySelector('[data-restore]')&&document.activeElement===button&&scroller.scrollLeft===oldLeft&&Math.abs(row.getBoundingClientRect().top-rowTop)<=2),'Refresh replaced nodes, moved reading position or lost focus');
   assert.deepEqual(await page.locator('[data-database-card]').evaluateAll(nodes=>nodes.map(node=>node.dataset.databaseCard)),['one/alpha']);
   await page.evaluate(()=>mergeDatabaseResponse({databases:[],unavailable_servers:['Production'],unavailable_server_ids:['one']}));await page.evaluate(()=>render());assert.equal(await page.locator('[data-database-card]').count(),1,'Offline agent erased the selected database card');assert(await page.evaluate(()=>scroller===card.querySelector('.table-wrap')&&detail.open&&button===row.querySelector('[data-restore]')));
   await page.evaluate(()=>{retainedResources=[{id:'retained-one',app_id:'removed-app',server_id:'one',created:'2026-10-11T12:00:00Z',storage_volume:'lidza-storage-removed-application-with-234567890123456789012345',cache_volume:'lidza-cache-removed-application-with-234567890123456789012345-data',clickhouse_volume:'lidza-clickhouse-removed-application-with-234567890123456789012345-data'}];applicationSelections.backups='removed-app';render()});
   assert.match(await page.locator('[data-retained-resources]').textContent(),/Recreating the same app ID starts fresh/);assert.match(await page.locator('[data-retained-resources]').textContent(),/lidza-storage-removed-application-with/);assert.match(await page.locator('[data-retained-resources]').textContent(),/lidza-cache-removed-application-with/);assert.match(await page.locator('[data-retained-resources]').textContent(),/ClickHouse volume: lidza-clickhouse-removed-application-with/);
   await page.evaluate(()=>{mergeDatabaseResponse({databases:[],unavailable_servers:['Production'],unavailable_server_ids:['one'],retained_resources:[],retained_resources_unavailable_server_ids:['one']});render()});assert.match(await page.locator('[data-retained-resources]').textContent(),/Agent offline; last known data/);assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);assert.deepEqual(errors,[]);await page.close();
  }
  console.log('PASS: selected database refresh keeps picker/card/row order, open details, focus, scroll and offline retained data');
 }finally{await browser.close()}
})().catch(error=>{console.error(error);process.exit(1)});
