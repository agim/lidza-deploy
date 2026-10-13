const {chromium}=require('playwright'),assert=require('node:assert/strict');
(async()=>{const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});try{
 for(const width of [390,1440]){
  const page=await browser.newPage({viewport:{width,height:844}}),errors=[],saved=[];
  page.on('pageerror',e=>errors.push(e.message));
  let available=6553;
  const policy={memory_mode:'',host_memory_mb:1536,host_cpus:0.5,build_memory_mb:1024,build_cpus:1,operation_memory_mb:512,operation_cpus:0.5,services:{}};
  const report=()=>({capacity_mode:policy.memory_mode||'automatic',host_available_mb:available,supported:true,checked:new Date().toISOString(),policy,memory_mb:7941,cpus:4,reserved_memory_mb:11776,available_memory_mb:available-policy.host_memory_mb-policy.operation_memory_mb,building:false,errors:[],apps:[],services:[]});
  await page.route('**/api/control/**',async route=>{
   const req=route.request(),path=new URL(req.url()).pathname;let json=[];
   if(path.endsWith('/resources')){if(req.method()==='PUT'){saved.push(req.postDataJSON());Object.assign(policy,req.postDataJSON());json={status:'applied'};}else json=report();}
   else if(path.endsWith('/status'))json={roles:['admin']};
   else if(path.endsWith('/servers'))json=[{id:'local',name:'This server'}];
   else if(path.endsWith('/deployments'))json={deployments:[]};
   await route.fulfill({json});
  });
  await page.goto((process.env.TEST_WEB_URL||'http://127.0.0.1:8880')+'/console.html#page/server-resources/local');
  await page.locator('[data-resource-metrics]').waitFor();
  const metrics=await page.locator('[data-resource-metrics]').textContent();
  assert(metrics.includes('RAM available')&&metrics.includes('Available for new work'));
  assert(!metrics.includes('11.50')&&!metrics.includes('-3835')&&!metrics.includes('RAM reserved'));
  assert.equal(await page.locator('[data-resource-warnings]').isVisible(),false,'Unused ceilings displayed as an error');
  assert.equal(await page.locator('#resource-policy-form [name=host_memory_mb]').isVisible(),false,'Optional management still fills the page');
  if(width===390)await page.screenshot({path:'/tmp/lidza-auto-resources-overview.png',fullPage:true});
  await page.locator('.resource-advanced>summary').click();
  assert.equal(await page.locator('[name=memory_mode]').inputValue(),'automatic');
  await page.locator('[name=host_memory_mb]').fill('1600');
  await page.locator('[name=host_memory_mb]').focus();
  const y=await page.evaluate(()=>scrollY);
  available=6200;
  await page.waitForFunction(()=>document.querySelector('[data-resource-metrics]').textContent.includes('6.05 GiB'),{timeout:15000});
  assert.equal(await page.locator('[name=host_memory_mb]').inputValue(),'1600');
  assert.equal(await page.evaluate(()=>document.activeElement.name),'host_memory_mb');
  assert.equal(await page.evaluate(()=>scrollY),y);
  assert.equal(await page.locator('.resource-advanced').getAttribute('open'),'');
  await page.locator('#resource-policy-form button').click();
  await page.waitForFunction(()=>!dirtyForms.size);
  assert.equal(saved[0].memory_mode,'automatic','Memory mode was incorrectly converted to a number');
  assert.equal(saved[0].host_memory_mb,1600);
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
  await page.screenshot({path:'/tmp/lidza-auto-resources-'+width+'.png',fullPage:true});
  assert.deepEqual(errors,[]);await page.close();
 }
 console.log('PASS: automatic capacity defaults, actual RAM metrics, optional advanced controls, typed mode saves and stable mobile/desktop polling.');
}finally{await browser.close()}})().catch(e=>{console.error(e);process.exit(1)});
