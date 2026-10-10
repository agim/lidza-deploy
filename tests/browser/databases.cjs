const {chromium}=require('playwright');
const {spawn,execFileSync}=require('node:child_process');
const fs=require('node:fs'),path=require('node:path'),os=require('node:os'),crypto=require('node:crypto'),net=require('node:net');
(async()=>{
 const root=path.resolve(__dirname,'../..'),base=process.env.TEST_WEB_URL||'http://127.0.0.1:3000';
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),'lidza-db-browser-')),key=crypto.randomBytes(32).toString('hex'),id='browser-db-'+crypto.randomBytes(5).toString('hex'),host='dbhost-'+crypto.randomBytes(5).toString('hex');
 const listener=net.createServer();await new Promise(r=>listener.listen(0,'127.0.0.1',r));const port=listener.address().port;await new Promise(r=>listener.close(r));
 const config=path.join(dir,'agent.json');fs.writeFileSync(config,JSON.stringify({listen:'127.0.0.1:'+port,proxy_listen:'127.0.0.1:0',data_dir:dir,api_key:key}),{mode:0o600});
 const agent=spawn(path.join(root,'bin/lidza-agent'),['-config',config],{cwd:root,stdio:'ignore'});let browser,page;
 try{
  for(let i=0;i<100;i++){try{if((await fetch('http://127.0.0.1:'+port+'/health')).ok)break}catch{}await new Promise(r=>setTimeout(r,100));}
  browser=await chromium.launch({executablePath:'/usr/bin/chromium',headless:true,args:['--no-sandbox']});page=await browser.newPage({viewport:{width:1440,height:1000},acceptDownloads:true});
  const errors=[];page.on('pageerror',e=>errors.push(e.message));const cfg={};for(const line of fs.readFileSync(path.join(root,'.local/dev.env'),'utf8').split('\n')){const m=line.match(/^export ([A-Z_]+)=(.*)$/);if(m)cfg[m[1]]=m[2].replace(/^'|'$/g,'');}
  await page.goto(base+'/login.html');await page.fill('[name=email]',cfg.CONTROL_USER);await page.fill('[name=password]',cfg.CONTROL_PASSWORD);await page.click('button[type=submit]');await page.waitForURL('**/console.html');
  let response=await page.request.post(base+'/api/control/servers',{headers:{Origin:base},data:{id:host,name:'Database browser fixture',url:'http://127.0.0.1:'+port,token:key}});if(!response.ok())throw Error('Could not connect fixture host');
  await page.reload();await page.locator('#new-app').click();await page.fill('#app-form [name=id]',id);await page.fill('#app-form [name=repository]','acme/database-fixture');await page.fill('#app-form [name=domain]',id+'.example.com');await page.selectOption('#server-select',host);
  await page.selectOption('#app-form [name=database_mode]','external');if(!await page.locator('#app-form [name=database_url]').isVisible()||await page.locator('#app-form [name=database_url]').getAttribute('type')!=='password')throw Error('External URL not masked');
  await page.selectOption('#app-form [name=database_mode]','local');await page.selectOption('#app-form [name=backup_hours]','0');await page.fill('#app-form [name=backup_keep]','2');
  fs.mkdirSync(path.join(root,'.local/screenshots'),{recursive:true});await page.screenshot({path:path.join(root,'.local/screenshots/new-app-database.png'),fullPage:true});await page.click('#app-form button[type=submit]');await page.locator('#app-dialog').waitFor({state:'hidden'});
await page.locator('[data-app-menu="'+id+'"] summary').click();
  await page.locator('[data-action=database][data-id="'+id+'"]').click();await page.locator('#database-dialog').waitFor({state:'visible'});
  for(let i=0;i<100;i++){const text=await page.locator('#database-run').textContent();if(text==='Back up now')break;await page.click('#database-refresh');await page.waitForTimeout(500);}
  if(await page.locator('#database-run').textContent()!=='Back up now')throw Error('Database not ready');
  await page.click('#database-run');
  for(let i=0;i<100;i++){if(await page.locator('#database-dialog a[download]').count())break;await page.click('#database-refresh');await page.waitForTimeout(300);}
  const downloadEvent=page.waitForEvent('download');await page.locator('#database-dialog a[download]').first().click();const download=await downloadEvent;const backupPath=await download.path();const bytes=fs.readFileSync(backupPath);if(bytes.subarray(0,5).toString()!=='PGDMP')throw Error('Not a PostgreSQL custom dump');
  await page.locator('#database-dialog [data-restore]').first().click();await page.selectOption('#restore-form [name=app]',id);await page.fill('#restore-form [name=target]',id+'-restored');await page.fill('#restore-form [name=env_key]','ARCHIVE_DATABASE_URL');const restoration=page.waitForResponse(r=>r.url().endsWith('/apps/'+id+'/restore'));await page.click('#restore-form button[type=submit]');if(!(await restoration).ok())throw Error('GUI restore request failed');await page.locator('#feature-dialog').waitFor({state:'hidden'});
  for(let i=0;i<100;i++){const v=await (await page.request.get(base+'/api/control/apps/'+id+'/settings')).json();if(v.database_bindings?.ARCHIVE_DATABASE_URL===id+'-restored')break;await page.waitForTimeout(300)}const restoredSettings=await (await page.request.get(base+'/api/control/apps/'+id+'/settings')).json();if(restoredSettings.database_bindings?.ARCHIVE_DATABASE_URL!==id+'-restored')throw Error('Restored database was not attached');
  await page.selectOption('#backup-form [name=backup_hours]','24');await page.click('#backup-form button[type=submit]');await page.waitForTimeout(300);await page.screenshot({path:path.join(root,'.local/screenshots/database-backups.png'),fullPage:true});
  const saved=await (await page.request.get(base+'/api/control/databases')).json();if(saved.databases.find(d=>d.app_id===id)?.backup.hours!==24)throw Error('Backup frequency not saved');
  await page.locator('#database-dialog summary').filter({hasText:'Create or connect another database'}).click();
  await page.fill('#attach-database-form [name=id]',id+'-extra');await page.fill('#attach-database-form [name=env_key]','ANALYTICS_DATABASE_URL');await page.selectOption('#attach-database-form [name=database_mode]','local');await page.selectOption('#attach-database-form [name=backup_hours]','0');const creation=page.waitForResponse(r=>r.url().endsWith('/apps/'+id+'/database')&&r.request().method()==='POST');await page.click('#attach-database-form button[type=submit]');const created=await creation;if(!created.ok())throw Error('Additional database creation failed: '+(await created.json()).error);await page.waitForFunction(expected=>document.querySelector('#managed-database')?.value===expected,id+'-extra');
  for(let i=0;i<100;i++){if(await page.locator('#database-run').textContent()==='Back up now')break;await page.click('#database-refresh');await page.waitForTimeout(500);}
  await page.fill('#bind-database-form [name=env_key]','DATABASE_URL');await page.selectOption('#bind-database-form [name=id]',id+'-extra');const switching=page.waitForResponse(r=>r.url().endsWith('/apps/'+id+'/database')&&r.request().method()==='POST');await page.click('#bind-database-form button[type=submit]');if(!(await switching).ok())throw Error('Primary switch request failed');
  const settings=await (await page.request.get(base+'/api/control/apps/'+id+'/settings')).json();if(settings.database_bindings.DATABASE_URL!==id+'-extra'||settings.database_bindings.ANALYTICS_DATABASE_URL!==id+'-extra')throw Error('Additional attachment or main switch failed');if(!settings.backup_before_deploy)throw Error('Pre-deployment backups not enabled by default');
  await page.locator('#database-dialog [data-close]').click();response=await page.request.delete(base+'/api/control/apps/'+id,{headers:{Origin:base},data:{confirm:id}});if(!response.ok())throw Error('App removal failed');
  await page.locator('[data-tab=backups]').click();await page.getByText('Retained after app removal',{exact:false}).first().waitFor();
  await page.locator('[data-tab=settings]').click();await page.locator('#storage-form').waitFor();if(!await page.locator('#mail-form').count())throw Error('Missing infrastructure controls');
  if(errors.length)throw Error(errors.join('\n'));
  console.log('PASS: GUI local database creation, masked external URL, backup/download/restore, schedule editing, named attachments, primary switch, pre-deployment default, retained backups, integration controls');
 }finally{
  if(page){await page.request.delete(base+'/api/control/apps/'+id,{headers:{Origin:base},data:{confirm:id}}).catch(()=>{});await page.request.delete(base+'/api/control/servers/'+host,{headers:{Origin:base}}).catch(()=>{});}
  if(browser)await browser.close();if(agent.exitCode===null){agent.kill('SIGTERM');await new Promise(r=>agent.once('exit',r));}
  for(const dbID of [id,id+'-extra',id+'-restored']){const name='lidza-db-'+dbID;for(const args of [['rm','-f',name],['volume','rm',name+'-data'],['network','rm',name]])try{execFileSync('docker',args,{stdio:'ignore'})}catch{}}
  fs.rmSync(dir,{recursive:true,force:true});
 }
})().catch(e=>{console.error(e.message);process.exitCode=1});
