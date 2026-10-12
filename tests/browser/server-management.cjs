const {chromium}=require('playwright');
const assert=require('node:assert/strict');
(async()=>{
 const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});
 try{for(const width of [390,1440]){
  const page=await browser.newPage({viewport:{width,height:844}}),errors=[],calls=[];
  page.on('pageerror',error=>errors.push(error.message));
  let hosts=[{id:'local',name:'This server',url:'http://127.0.0.1:9090'},{id:'empty',name:'Spare server',url:'https://agent.example.com'}];
  const applications=[{id:'site',server_id:'local',domain:'site.example.com',repository:'owner/site',branch:'main'}];
  await page.route('**/api/control/**',async route=>{
   const request=route.request(),path=new URL(request.url()).pathname,method=request.method();
   calls.push({path,method,body:request.postDataJSON()});
   if(path.endsWith('/servers/empty')&&method==='DELETE'){hosts=hosts.filter(h=>h.id!=='empty');await route.fulfill({json:{}});return}
   if(path.endsWith('/servers/local')&&method==='PUT'){Object.assign(hosts[0],request.postDataJSON());await route.fulfill({json:{}});return}
   const name=path.split('/').pop();
   await route.fulfill({json:name==='status'?{roles:['admin']}:name==='apps'?applications:name==='servers'?hosts:name==='deployments'?{deployments:[],unavailable_servers:[]}:name==='server-health'?{servers:[]}:name==='upgrade'?{current:'v0.2.46',latest:'v0.2.47',state:'idle',supported:false}:[]});
  });
  const base=process.env.TEST_WEB_URL||'http://127.0.0.1:8880';
  await page.goto(base+'/console.html#servers');
  assert.deepEqual(await page.locator('[data-server-card=local] button:visible').allTextContents(),['Add application','Manage server']);
  if(width===390){const boxes=await page.locator('[data-server-card=local] button').evaluateAll(nodes=>nodes.map(n=>({y:n.getBoundingClientRect().y,bottom:n.getBoundingClientRect().bottom})));assert(boxes[1].y-boxes[0].bottom>=12,'Mobile actions have no spacing')}
  await page.locator('[data-server-card=local]').screenshot({path:'/tmp/lidza-server-card-'+width+'.png'});
  await page.locator('[data-server-card=local] [data-server]').click();
  await page.waitForURL('**/console.html#new-application');assert.equal(await page.locator('#server-select').inputValue(),'local');
  await page.locator('#cancel-new-app').click();
  await page.goto(base+'/console.html#servers');await page.locator('[data-manage-server=local]').click();
  assert.equal(await page.locator('#server-form [name=token]').inputValue(),'');
  assert(await page.locator('#server-form [name=id]').evaluate(node=>node.readOnly));
  assert(await page.locator('#server-removal button').isDisabled());
  assert.match(await page.locator('#server-removal-hint').textContent(),/1 registered application/);
  await page.locator('#server-form [name=name]').fill('Changed server');
  page.once('dialog',dialog=>dialog.dismiss());await page.locator('.resource-navigation a[href="#page/upgrade/local"]').click();
  await page.waitForURL('**/console.html#page/server/local');assert.equal(await page.locator('#server-form [name=name]').inputValue(),'Changed server');
  await page.locator('#server-form button[type=submit]').click();await page.waitForURL('**/console.html#servers');
  assert(calls.some(c=>c.method==='PUT'&&c.body.name==='Changed server'&&c.body.token===''),'Save did not preserve blank token semantics');
  await page.locator('[data-manage-server=local]').click();await page.locator('.resource-navigation a[href="#page/upgrade/local"]').click();
  await page.locator('#upgrade-form').waitFor();assert(await page.locator('#upgrade-form button').isDisabled());
  await page.reload();await page.locator('#upgrade-form').waitFor();assert.equal(await page.locator('.resource-navigation a[aria-current=page]').textContent(),'Agent upgrade');
  await page.locator('.resource-navigation a[href="#page/server/local"]').click();await page.locator('#server-form').waitFor();
  assert(await page.locator('#server-removal button').isDisabled());
  await page.goto(base+'/console.html#page/server/empty');await page.locator('#server-form').waitFor();
  assert.equal(await page.locator('#server-removal button').isDisabled(),false);
  page.once('dialog',dialog=>dialog.accept());await page.locator('#server-removal button').click();
  await page.waitForURL('**/console.html#servers');await page.waitForFunction(()=>!document.querySelector('[data-server-card=empty]'));
  assert(calls.some(c=>c.method==='DELETE'&&c.path.endsWith('/servers/empty')));
  await page.locator('.toolbar [data-manage-server]').click();assert.equal(await page.locator('#server-removal').isVisible(),false);assert.equal(await page.locator('#server-dialog .resource-navigation').count(),0);
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth-innerWidth),0);assert.deepEqual(errors,[]);
  await page.close();
 }console.log('PASS: desktop/mobile two-action cards, host selection, connection save without credential disclosure, dirty navigation guard, upgrade deep links, protected disconnect and removal return');
 }finally{await browser.close()}
})().catch(error=>{console.error(error);process.exit(1)});
