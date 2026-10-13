const {chromium}=require('playwright');
const assert=require('node:assert/strict');
const base=process.env.TEST_WEB_URL||'http://127.0.0.1:8880';
const branches=['main','codex/kulla-rails-port','dependabot/github_actions/actions/checkout-7','dependabot/github_actions/actions/setup-go-7','feature/'+ 'a'.repeat(160)];

(async()=>{
 const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});
 try{
  for(const width of [320,390,600,960,1440]){
   const page=await browser.newPage({viewport:{width,height:844}}),errors=[],saved=[];
   page.on('pageerror',error=>errors.push(error.message));
   const app={id:'kulla-lidza',server_id:'local',repository:'agim/kulla-lidza',branch:'main',domain:'kulla.albaspot.com'};
   await page.route('**/api/control/**',async route=>{
    const request=route.request(),path=new URL(request.url()).pathname;let json=[];
    if(path.endsWith('/status'))json={roles:['admin']};
    else if(path.endsWith('/apps'))json=[app];
    else if(path.endsWith('/servers'))json=[{id:'local',name:'Production'}];
    else if(path.endsWith('/deployments'))json={deployments:[]};
    else if(path.endsWith('/github/branches'))json=branches.map(name=>({name}));
    else if(path.endsWith('/settings')){
     if(request.method()==='PATCH'){saved.push(request.postDataJSON());app.branch=request.postDataJSON().branch;json={status:'accepted'};}
     else json={...app,env_keys:[],database_bindings:{}};
    }
    await route.fulfill({json});
   });
   async function checkLayout(prefix){
    await page.locator('#'+prefix+'-branch-options button').last().waitFor();
    const result=await page.evaluate(prefix=>{
     const list=document.querySelector('#'+prefix+'-branch-options'),bounds=list.getBoundingClientRect();
     return {pageFits:document.documentElement.scrollWidth<=innerWidth,choicesFit:[...list.querySelectorAll('button')].every(button=>button.getBoundingClientRect().left>=bounds.left&&button.getBoundingClientRect().right<=bounds.right),internalWidthFits:list.scrollWidth<=list.clientWidth};
    },prefix);
    assert.deepEqual(result,{pageFits:true,choicesFit:true,internalWidthFits:true},prefix+' branch picker overflows at '+width+'px');
   }
   await page.goto(base+'/console.html#page/settings/kulla-lidza');
   await checkLayout('settings');
   if(width===390)await page.locator('#settings-form .setup-card').first().screenshot({path:'/tmp/lidza-settings-source-fixed.png'});
   await page.locator('#settings-branch-options').getByRole('button',{name:branches[4],exact:true}).click();
   assert.equal(await page.locator('#settings-form [name=branch]').inputValue(),branches[4]);
   await page.evaluate(()=>refresh({background:true}));
   assert.equal(await page.locator('#settings-form [name=branch]').inputValue(),branches[4]);
   if(width===390)await page.screenshot({path:'/tmp/lidza-settings-mobile-fixed.png',fullPage:true});
   await page.locator('#settings-form button[type=submit]').click();
   await page.waitForURL(/#applications$/);
   assert.equal(saved[0].branch,branches[4],'Saving must retain the full branch name');
   await page.goto(base+'/console.html#new-application');
   await page.locator('#app-form [name=repository]').fill(app.repository);
   await page.locator('#app-form [name=branch]').fill('');
   await page.locator('#load-branches').click();
   await checkLayout('create');
   await page.locator('#create-branch-options').getByRole('button',{name:branches[3],exact:true}).click();
   assert.equal(await page.locator('#app-form [name=branch]').inputValue(),branches[3]);
   assert.deepEqual(errors,[]);
   await page.close();
  }
  console.log('PASS: long branch names fit settings and creation at 320–1440px; selection, refresh and saving retain exact names.');
 }finally{await browser.close();}
})().catch(error=>{console.error(error);process.exit(1)});
