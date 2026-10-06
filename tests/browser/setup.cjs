const {chromium}=require('playwright');
const {spawn,execFileSync}=require('node:child_process');
const fs=require('node:fs');const path=require('node:path');const os=require('node:os');const crypto=require('node:crypto');const net=require('node:net');
(async()=>{
 const root=path.resolve(__dirname,'../..'),dir=fs.mkdtempSync(path.join(os.tmpdir(),'lidza-browser-setup-'));
 const listener=net.createServer();await new Promise(r=>listener.listen(0,'127.0.0.1',r));const port=listener.address().port;await new Promise(r=>listener.close(r));const origin=`http://127.0.0.1:${port}`;
 let processHandle,browser;
 const screenshotDir=process.env.SETUP_SCREENSHOT_DIR;
 const capture=async(page,name,fullPage=true)=>{if(screenshotDir){fs.mkdirSync(screenshotDir,{recursive:true});await page.screenshot({path:path.join(screenshotDir,name+'.png'),fullPage})}};
 const start=(dataDir=dir,setupOrigin=origin)=>spawn(path.join(root,'bin/lidza-control'),[],{cwd:root,env:{PATH:process.env.PATH,HOME:process.env.HOME,CONTROL_DATA_DIR:dataDir,CONTROL_SETUP_ORIGIN:setupOrigin,LIDZA_ADDR:`127.0.0.1:${port}`,JOBS_WORKERS:'0'},stdio:'ignore'});
 const wait=async()=>{for(let n=0;n<100;n++){try{if((await fetch(origin+'/healthz')).ok)return}catch{}await new Promise(r=>setTimeout(r,100))}throw Error('Control panel did not start')};
 const stop=async()=>{if(processHandle&&processHandle.exitCode===null){processHandle.kill('SIGTERM');await new Promise(r=>processHandle.once('exit',r))}};
 try{
 processHandle=start();await wait();browser=await chromium.launch({executablePath:'/usr/bin/chromium',headless:true,args:['--no-sandbox']});const page=await browser.newPage({viewport:{width:1100,height:900}});
 await page.goto(origin);await capture(page,'01-claim');await page.fill('#setup-key',fs.readFileSync(path.join(dir,'setup-token'),'utf8'));await page.click('#claim-form button');await page.waitForSelector('#setup-form:visible');
 await capture(page,'02-interview');
 await page.selectOption('#database-mode','external');await capture(page,'03-external-database');await page.selectOption('#database-mode','managed');
 await page.setViewportSize({width:390,height:844});await capture(page,'04-mobile');if(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth))throw Error('Setup overflows on mobile');await page.setViewportSize({width:1100,height:900});
 await page.fill('[name=email]','browser-owner@example.com');await page.fill('[name=password]','browser-first-run-password');if(await page.locator('#public-url').inputValue()!==origin||!await page.locator('#public-url').evaluate(el=>el.readOnly))throw Error('Installer URL was not locked in the wizard');if(await page.locator('#public-network').isVisible())throw Error('Wizard asks for already configured network');
 const completed=page.waitForResponse(r=>r.url().endsWith('/api/setup/complete'),{timeout:150000});await page.click('#finish');const response=await completed;if(!response.ok())throw Error('First-run setup: '+(await response.json()).error);await page.waitForURL('**/login.html',{timeout:150000});
 if(fs.existsSync(path.join(dir,'setup-token')))throw Error('Setup token survived completion');
 const result=await page.request.post(origin+'/api/v1/auth/login',{data:{email:'browser-owner@example.com',password:'browser-first-run-password'}});if(result.status()!==200)throw Error('Operator login failed');
 await stop();processHandle=start();await wait();if(!(await fetch(origin+'/readyz')).ok)throw Error('Restart not ready');const closed=await fetch(origin+'/api/setup/check',{method:'POST'});if(closed.status!==404)throw Error('Setup reopened after restart');
 // Exercise the real public-origin wizard without claiming public ACME: only
 // the browser's network route is mapped to this isolated local process.
 await stop();const publicOrigin='https://deploy.example.com',publicDir=path.join(dir,'public-origin');fs.mkdirSync(publicDir,{recursive:true});fs.writeFileSync(path.join(publicDir,'servers.json'),JSON.stringify([{id:'local',name:'This server',url:'http://127.0.0.1:9090',token:'paired-agent-browser-fixture-token'}]),{mode:0o600});processHandle=start(publicDir,publicOrigin);await wait();
 const publicPage=await browser.newPage({viewport:{width:1100,height:1000}});
 await publicPage.route(publicOrigin+'/**',async route=>{const request=route.request(),target=origin+new URL(request.url()).pathname;const headers={...request.headers()};if(request.method()!=='GET')headers.origin=publicOrigin;const response=await publicPage.request.fetch(target,{method:request.method(),headers,data:request.postDataBuffer(),maxRedirects:0});await route.fulfill({response})});
 await publicPage.goto(publicOrigin+'/setup.html');await capture(publicPage,'05-public-claim');await publicPage.fill('#setup-key',fs.readFileSync(path.join(publicDir,'setup-token'),'utf8'));await publicPage.click('#claim-form button');await publicPage.waitForSelector('#setup-form:visible');
 if(await publicPage.locator('#public-url').inputValue()!==publicOrigin||await publicPage.locator('#public-network').isVisible())throw Error('Public installer address was not retained');
 if(!await publicPage.locator('#paired-agent').isVisible()||await publicPage.locator('#extra-agent').evaluate(e=>e.open))throw Error('Paired agent still asks for connection details');if((await publicPage.locator('body').textContent()).includes('paired-agent-browser-fixture-token'))throw Error('Paired agent token exposed');
 if(await publicPage.locator('[name=github_id],[name=github_secret]').count())throw Error('First-run wizard still requires OAuth credentials');
 await publicPage.evaluate(()=>scrollTo(0,0));await capture(publicPage,'06-public-interview');await capture(publicPage,'07-public-account-database',false);await publicPage.evaluate(()=>scrollTo(0,document.documentElement.scrollHeight));await capture(publicPage,'08-public-github-agent',false);
 await publicPage.setViewportSize({width:390,height:844});await capture(publicPage,'09-public-mobile');if(await publicPage.evaluate(()=>document.documentElement.scrollWidth>innerWidth))throw Error('Public setup overflows on mobile');
 console.log('PASS: browser first-run wizard, managed PostgreSQL, operator login, durable restart, closed setup');
 }finally{if(browser)await browser.close();await stop();const name='lidza-control-db-'+crypto.createHash('sha256').update(dir).digest('hex').slice(0,12);try{execFileSync('docker',['rm','-f',name],{stdio:'ignore'});execFileSync('docker',['volume','rm',name],{stdio:'ignore'})}catch{}fs.rmSync(dir,{recursive:true,force:true})}
})().catch(e=>{console.error(e.message);process.exitCode=1});
