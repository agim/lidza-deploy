const {chromium}=require('playwright');
const assert=require('node:assert/strict');
const base=process.env.TEST_WEB_URL||'http://127.0.0.1:8880';
(async()=>{const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});try{
 for(const width of [390,1440]){
  const page=await browser.newPage({viewport:{width,height:844}}),errors=[];page.on('pageerror',e=>errors.push(e.message));
  await page.goto(base+'/console.html?demo=1#help/first-admin');await page.locator('[data-help-guide="first-admin"][open]').waitFor();
  assert(await page.locator('[data-help-guide="first-admin"]').textContent().then(t=>t.includes('AUTH_OWNER_CLAIM=true')));
  await page.locator('#help-search').fill('42P01');assert.equal(await page.locator('[data-help-guide]:visible').count(),1);
  await page.evaluate(()=>{window.helpSearch=document.querySelector('#help-search');window.helpArticle=document.querySelector('[data-help-guide="migrations"]');helpArticle.open=true;helpSearch.focus({preventScroll:true});window.helpY=scrollY;render();render()});
  assert(await page.evaluate(()=>helpSearch===document.querySelector('#help-search')&&document.activeElement===helpSearch&&helpSearch.value==='42P01'&&helpArticle.open&&scrollY===helpY));
  await page.locator('#help-search').fill('nothing-will-match-this');await page.locator('#help-empty').waitFor({state:'visible'});
  await page.locator('#help-search').fill('');await page.locator('[data-help-guide="first-admin"] a[data-help-topic="migrations"]').click();assert(page.url().endsWith('#help/migrations'));await page.locator('[data-help-guide="migrations"][open]').waitFor();
  await page.goBack();await page.locator('[data-help-guide="first-admin"][open]').waitFor();
  await page.evaluate(()=>navigate('apps'));await page.locator('.app-card').first().waitFor();await page.evaluate(()=>openSettings(apps[0].id));await page.locator('#settings-dialog[open]').waitFor();
  await page.locator('#settings-dialog [data-help-topic="first-admin"]').click();await page.locator('[data-help-guide="first-admin"][open]').waitFor();assert.equal(await page.locator('dialog[open]').count(),0);
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'Help overflows mobile viewport');assert.deepEqual(errors,[]);await page.close();
 }
 console.log('PASS: searchable help, topic links, history, settings links, mobile layout and refresh stability');
 }finally{await browser.close()}})().catch(error=>{console.error(error);process.exit(1)});
