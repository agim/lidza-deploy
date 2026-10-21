const {chromium}=require('playwright');
const assert=require('node:assert/strict');
const {mockConsole,app}=require('./fixtures/console.cjs');
const base=process.env.TEST_WEB_URL||'http://127.0.0.1:34226';
(async()=>{const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});try {
for(const width of [320,390,768,1440]) {
 const page=await browser.newPage({viewport:{width,height:1000}});const errors=[];page.on('pageerror',error=>errors.push(error.message));
 const state=await mockConsole(page);state.apps.push({...app,id:'fresh',current:undefined});
 let deployment;let requests=0;let fail=false;
 await page.route('**/api/control/deployments/fresh-build',route=>route.fulfill({json:deployment}));
 await page.route('**/api/control/deployments',route=>route.fulfill({json:{deployments:deployment?[deployment]:[],unavailable_servers:[]}}));
 await page.route('**/api/control/apps/fresh/deploy',route=>{requests++;if(fail)return route.fulfill({status:409,json:{error:'GitHub installation unavailable'}});deployment={id:'fresh-build',app_id:'fresh',status:'queued',created:new Date().toISOString()};return route.fulfill({status:202,json:deployment});});
 await page.goto(`${base}/console.html#applications`);const card=page.locator('[data-app-card="fresh"]');await card.getByText('No release deployed').waitFor();
 await card.getByRole('button',{name:'Deploy',exact:true}).click();await page.getByText('Deployment queued.',{exact:true}).waitFor();
 assert.equal(requests,1);await card.getByRole('button',{name:'Build logs',exact:true}).waitFor();await page.waitForFunction(()=>!document.querySelector('[data-app-card="fresh"] [data-history]').disabled);
 await card.getByRole('button',{name:'Build logs',exact:true}).click();await page.waitForURL(url=>url.hash==='#page/build-logs/fresh-build');
 await page.goto(`${base}/console.html#applications`);fail=true;await card.getByRole('button',{name:'Deploy',exact:true}).click();await card.getByRole('alert').filter({hasText:'GitHub installation unavailable'}).waitFor();assert.equal(requests,2);
 for(const route of ['errors','security']) {
  await page.goto(`${base}/console.html#${route}`);await page.locator('.activity-filters select').waitFor();
  const geometry=await page.locator('.activity-filters').evaluate(element=>{const a=element.querySelector('.search-field').getBoundingClientRect();const b=element.querySelector('select').getBoundingClientRect();return{a:{top:a.top,bottom:a.bottom,height:a.height},b:{top:b.top,bottom:b.bottom,height:b.height},columns:getComputedStyle(element).gridTemplateColumns.split(' ').length};});
  assert(Math.abs(geometry.a.height-geometry.b.height)<1,`${route} control heights: ${JSON.stringify(geometry)}`);
  if(geometry.columns===2) assert(Math.abs((geometry.a.top+geometry.a.bottom)/2-(geometry.b.top+geometry.b.bottom)/2)<1,`${route} controls not centered: ${JSON.stringify(geometry)}`);
 }
 assert.deepEqual(errors,[]);await page.close();console.log(`PASS: Deploy request/queue/log navigation, inline rejection and centered diagnostics filters at ${width}px`);
}
}finally{await browser.close()}})().catch(error=>{console.error(error);process.exit(1)});
