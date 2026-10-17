const{chromium}=require('playwright');const assert=require('node:assert/strict');const{mockConsole}=require('./fixtures/console.cjs');
const base=process.env.TEST_WEB_URL||'http://127.0.0.1:8880';
(async()=>{const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});try{
 for(const width of[390,1440]){
  const page=await browser.newPage({viewport:{width,height:1000}}),errors=[];page.on('pageerror',e=>errors.push(e.message));
  const fixture=await mockConsole(page);fixture.aliases=['saved.example.com'];fixture.domainStatuses=[{domain:'saved.example.com',state:'waiting_dns',message:'DNS check pending: '+('long-dns-response'.repeat(15))}];let saves=[];
  await page.route('**/api/control/apps/portal/settings',async route=>{
   const req=route.request();if(req.method()!=='PATCH')return route.fallback();
   const body=req.postDataJSON();saves.push(body);fixture.aliases=body.additional_domains;return route.fulfill({json:{status:'saved'}});
  });
  await page.goto(`${base}/console.html#page/settings/portal`);
  await page.getByLabel('Additional hostname 1',{exact:true}).waitFor();
  await page.getByRole('button',{name:'Add hostname',exact:true}).click();
  await page.getByLabel('Additional hostname 2',{exact:true}).fill('WWW.EXAMPLE.ORG');
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth+1),false,'Additional hostname fields or DNS status overflow');
  await page.getByRole('button',{name:'Save changes',exact:true}).click();
  await page.waitForURL(url=>url.hash==='#applications');
  assert.deepEqual(saves[0].additional_domains,['saved.example.com','www.example.org']);assert.equal(saves[0].domain,'portal.example.com');
  await page.goto(`${base}/console.html#page/settings/portal`);
  await page.getByLabel('Additional hostname 2',{exact:true}).waitFor();
  await page.getByRole('button',{name:'Remove additional hostname 1',exact:true}).click();
  assert.equal(await page.getByLabel('Additional hostname 1',{exact:true}).inputValue(),'www.example.org');
  await page.getByRole('tab',{name:'Environment',exact:true}).click();
  await page.getByRole('button',{name:'Save changes',exact:true}).click();await page.waitForURL(url=>url.hash==='#applications');
  assert.deepEqual(saves[1].additional_domains,['www.example.org']);
  await page.goto(`${base}/console.html#page/settings/portal`);await page.getByLabel('Additional hostname 1',{exact:true}).waitFor();
  await page.getByRole('button',{name:'Remove additional hostname 1',exact:true}).click();
  await page.getByRole('button',{name:'Save changes',exact:true}).click();await page.waitForURL(url=>url.hash==='#applications');assert.deepEqual(saves[2].additional_domains,[]);
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth+1),false);assert.deepEqual(errors,[]);await page.close();console.log(`PASS: additional domains add, normalize, persist, remove, clear and survive environment saves at ${width}px`);
 }
}finally{await browser.close()}})().catch(e=>{console.error(e);process.exit(1)});
