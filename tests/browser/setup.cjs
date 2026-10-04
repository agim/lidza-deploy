const {chromium}=require('playwright');
const {spawn,execFileSync}=require('node:child_process');
const fs=require('node:fs');const path=require('node:path');const os=require('node:os');const crypto=require('node:crypto');const net=require('node:net');
(async()=>{
 const root=path.resolve(__dirname,'../..'),dir=fs.mkdtempSync(path.join(os.tmpdir(),'lidza-browser-setup-'));
 const listener=net.createServer();await new Promise(r=>listener.listen(0,'127.0.0.1',r));const port=listener.address().port;await new Promise(r=>listener.close(r));const origin=`http://127.0.0.1:${port}`;
 let processHandle,browser;
 const start=()=>spawn(path.join(root,'bin/lidza-control'),[],{cwd:root,env:{PATH:process.env.PATH,HOME:process.env.HOME,CONTROL_DATA_DIR:dir,LIDZA_ADDR:`127.0.0.1:${port}`,JOBS_WORKERS:'0'},stdio:'ignore'});
 const wait=async()=>{for(let n=0;n<100;n++){try{if((await fetch(origin+'/healthz')).ok)return}catch{}await new Promise(r=>setTimeout(r,100))}throw Error('Control panel did not start')};
 const stop=async()=>{if(processHandle&&processHandle.exitCode===null){processHandle.kill('SIGTERM');await new Promise(r=>processHandle.once('exit',r))}};
 try{
 processHandle=start();await wait();browser=await chromium.launch({executablePath:'/usr/bin/chromium',headless:true,args:['--no-sandbox']});const page=await browser.newPage();
 await page.goto(origin);await page.fill('#setup-key',fs.readFileSync(path.join(dir,'setup-token'),'utf8'));await page.click('#claim-form button');await page.waitForSelector('#setup-form:visible');
 await page.fill('[name=email]','browser-owner@example.com');await page.fill('[name=password]','browser-first-run-password');await page.fill('#public-url',origin);
 const completed=page.waitForResponse(r=>r.url().endsWith('/api/setup/complete'),{timeout:150000});await page.click('#finish');const response=await completed;if(!response.ok())throw Error('First-run setup: '+(await response.json()).error);await page.waitForURL('**/login.html',{timeout:150000});
 if(fs.existsSync(path.join(dir,'setup-token')))throw Error('Setup token survived completion');
 const result=await page.request.post(origin+'/api/v1/auth/login',{data:{email:'browser-owner@example.com',password:'browser-first-run-password'}});if(result.status()!==200)throw Error('Operator login failed');
 await stop();processHandle=start();await wait();if(!(await fetch(origin+'/readyz')).ok)throw Error('Restart not ready');const closed=await fetch(origin+'/api/setup/check',{method:'POST'});if(closed.status!==404)throw Error('Setup reopened after restart');
 console.log('PASS: browser first-run wizard, managed PostgreSQL, operator login, durable restart, closed setup');
 }finally{if(browser)await browser.close();await stop();const name='lidza-control-db-'+crypto.createHash('sha256').update(dir).digest('hex').slice(0,12);try{execFileSync('docker',['rm','-f',name],{stdio:'ignore'});execFileSync('docker',['volume','rm',name],{stdio:'ignore'})}catch{}fs.rmSync(dir,{recursive:true,force:true})}
})().catch(e=>{console.error(e.message);process.exitCode=1});
