const {chromium}=require('playwright');
const {spawn}=require('node:child_process');
const http=require('node:http');
const fs=require('node:fs'),path=require('node:path'),os=require('node:os'),crypto=require('node:crypto'),net=require('node:net');
(async()=>{
 const root=path.resolve(__dirname,'../..'),base=process.env.TEST_WEB_URL||'http://127.0.0.1:3000';
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),'lidza-features-browser-')),key=crypto.randomBytes(32).toString('hex'),id='features-'+crypto.randomBytes(5).toString('hex'),host='host-'+crypto.randomBytes(5).toString('hex');
 async function port(){const listener=net.createServer();await new Promise(r=>listener.listen(0,'127.0.0.1',r));const p=listener.address().port;await new Promise(r=>listener.close(r));return p}
 const apiPort=await port(),proxyPort=await port(),config=path.join(dir,'agent.json');
 fs.writeFileSync(config,JSON.stringify({listen:'127.0.0.1:'+apiPort,proxy_listen:'127.0.0.1:'+proxyPort,tls_listen:'',data_dir:dir,api_key:key}),{mode:0o600});
 const agent=spawn(path.join(root,'bin/lidza-agent'),['-config',config],{cwd:root,stdio:'ignore'});let browser,page;
 try{
  for(let i=0;i<100;i++){try{if((await fetch('http://127.0.0.1:'+apiPort+'/health')).ok)break}catch{}await new Promise(r=>setTimeout(r,100))}
  browser=await chromium.launch({executablePath:'/usr/bin/chromium',headless:true,args:['--no-sandbox']});page=await browser.newPage({viewport:{width:1440,height:1000}});const errors=[];page.on('pageerror',e=>errors.push(e.message));
  const cfg={};for(const line of fs.readFileSync(path.join(root,'.local/dev.env'),'utf8').split('\n')){const m=line.match(/^export ([A-Z_]+)=(.*)$/);if(m)cfg[m[1]]=m[2].replace(/^'|'$/g,'')}
  await page.goto(base+'/login.html');await page.fill('[name=email]',cfg.CONTROL_USER);await page.fill('[name=password]',cfg.CONTROL_PASSWORD);await page.click('button[type=submit]');await page.waitForURL('**/console.html');
  const headers={Origin:base};let response=await page.request.post(base+'/api/control/servers',{headers,data:{id:host,name:'Feature fixture',url:'http://127.0.0.1:'+apiPort,token:key}});if(!response.ok())throw Error('Host registration failed');
  response=await page.request.post(base+'/api/control/apps',{headers,data:{id,server_id:host,repository:'acme/fixture',branch:'main',domain:id+'.example.com',env:{},database:{mode:'none'}}});if(!response.ok())throw Error('App creation failed');
  await page.reload();await page.locator('[data-app-menu="'+id+'"] summary').click();await page.locator('[data-maintenance="'+id+'"]').click();await page.check('#maintenance-form [name=enabled]');await page.fill('#maintenance-form [name=message]','Fixture maintenance <safe>');const saving=page.waitForResponse(r=>r.url().endsWith('/apps/'+id+'/maintenance'));await page.click('#maintenance-form button[type=submit]');const saved=await saving;if(!saved.ok())throw Error('Maintenance save failed: '+await saved.text());
  const appState=await (await page.request.get(base+'/api/control/apps')).json();if(!appState.find(a=>a.id===id)?.maintenance.enabled)throw Error('Maintenance flag not saved');
  const maintenance=await new Promise((resolve,reject)=>{http.get({hostname:'127.0.0.1',port:proxyPort,path:'/',headers:{Host:id+'.example.com'}},res=>{let body='';res.on('data',b=>body+=b);res.on('end',()=>resolve({status:res.statusCode,body}))}).on('error',reject)});if(maintenance.status!==503||!maintenance.body.includes('&lt;safe&gt;'))throw Error('Maintenance not served safely: '+maintenance.status+' '+maintenance.body);

  await page.locator('[data-tasks="'+id+'"]').click();await page.fill('#task-form [name=id]','cleanup');await page.selectOption('#task-form [name=mode]','schedule');await page.fill('#task-form [name=command]','["/app/app","cleanup"]');await page.fill('#task-form [name=every_minutes]','60');await page.click('#task-form button[type=submit]');await page.waitForSelector('[data-task-toggle=cleanup]');const toggling=page.waitForResponse(r=>r.url().endsWith('/tasks/cleanup')&&r.request().method()==='PUT');await page.click('[data-task-toggle=cleanup]');if(!(await toggling).ok())throw Error('Task toggle request failed');let tasks=await (await page.request.get(base+'/api/control/tasks')).json();if(tasks.find(t=>t.app_id===id)?.enabled!==false)throw Error('Task disable did not persist');await page.click('#feature-close');
  await page.locator('[data-previews="'+id+'"]').click();await page.check('#preview-form [name=enabled]');await page.check('#preview-form [name=database]');await page.fill('#preview-form [name=base_domain]','preview.example.com');await page.fill('#preview-form [name=env]','{"APP_MODE":"preview","SECRET":"fixture-preview-secret"}');await page.click('#preview-form button[type=submit]');await page.waitForTimeout(400);const appBody=await (await page.request.get(base+'/api/control/apps')).text();if(appBody.includes('fixture-preview-secret')||!appBody.includes('preview.example.com'))throw Error('Preview configuration leaked secret or was not saved');
  await page.route('**/api/control/servers/'+host+'/upgrade',route=>route.fulfill({json:{current:'development',latest:'v0.2.0',state:'idle',supported:false}}));await page.click('[data-tab=servers]');await page.locator('[data-upgrade="'+host+'"]').click();if(!await page.locator('#upgrade-form button').isDisabled())throw Error('Unsupported upgrade was enabled');await page.click('#feature-close');
  await page.waitForSelector('#host-health');await page.waitForFunction(()=>document.querySelector('#host-health')?.textContent.includes('CPU:'));await page.screenshot({path:path.join(root,'.local/screenshots/host-health.png'),fullPage:true});if(errors.length)throw Error(errors.join('\n'));
  console.log('PASS: live maintenance response, scheduled task configuration/disable, preview configuration with secret omission, host metrics and upgrade availability UI');
 }finally{
  if(page){await page.request.delete(base+'/api/control/apps/'+id,{headers:{Origin:base},data:{confirm:id}});await page.request.delete(base+'/api/control/servers/'+host,{headers:{Origin:base}})}
  if(browser)await browser.close();agent.kill('SIGTERM');await new Promise(resolve=>{if(agent.exitCode!==null)resolve();else agent.once('exit',resolve)});fs.rmSync(dir,{recursive:true,force:true});
 }
})().catch(e=>{console.error(e);process.exit(1)});
