const {chromium}=require('playwright');const assert=require('node:assert/strict');
(async()=>{const browser=await chromium.launch({executablePath:'/usr/bin/chromium',args:['--no-sandbox']});try{
 for(const width of [390,900,1440]){
  const page=await browser.newPage({viewport:{width,height:844}}),errors=[];page.on('pageerror',e=>errors.push(e.message));let polls=0,change=false;
  await page.route('**/api/control/**',async route=>{
   const path=new URL(route.request().url()).pathname;let json=[];
   if(path.endsWith('/status'))json={roles:['admin']};
   else if(path.endsWith('/servers'))json=[{id:'local',name:'This server'}];
   else if(path.endsWith('/apps')){
    json=['portal-albaspot-com','agim-dev'].map(id=>({id,repository:'acme/'+id,domain:id+'.example.com',branch:'main',server_id:'local',current:{commit:change?'bbbbbbb':'aaaaaaa'}}));
    if(++polls%2===0)json.reverse();
   }else if(path.endsWith('/deployments'))json={deployments:[]};
   await route.fulfill({json});
  });
  await page.goto((process.env.TEST_WEB_URL||'http://127.0.0.1:8880')+'/console.html#applications');await page.locator('.app-card').first().waitFor();
  await page.evaluate(()=>{
   clearInterval(poll);
   window.cards=[...document.querySelectorAll('[data-app-card]')];
   window.action=cards[1].querySelector('[data-action=settings]');
   cards[1].querySelector('details').open=true;
   window.scrollTo(0,500);window.readingY=scrollY;window.positions=cards.map(c=>c.getBoundingClientRect().top);
   action.focus({preventScroll:true});window.cardMoves=0;
   window.observer=new MutationObserver(records=>{for(const r of records)cardMoves+=[...r.addedNodes,...r.removedNodes].filter(n=>cards.includes(n)).length});
   observer.observe(document.querySelector('.cards'),{childList:true});
  });
  for(let i=0;i<12;i++){
   change=!!(i%2);
   await page.evaluate(()=>refresh({background:true}));
   assert(await page.evaluate(()=>cards.every((c,i)=>c===document.querySelectorAll('[data-app-card]')[i]&&Math.abs(c.getBoundingClientRect().top-positions[i])<1)&&document.activeElement===action&&cards[1].querySelector('details').open&&scrollY===readingY&&cardMoves===0),'Alternating API order moved apps, focus, menu or reading position at '+width);
  }
  assert.deepEqual(await page.locator('[data-app-card]').evaluateAll(nodes=>nodes.map(n=>n.dataset.appCard)),['agim-dev','portal-albaspot-com']);
  assert.deepEqual(errors,[]);await page.close();
 }
 console.log('PASS: alternating API list order and live app changes retain card order, DOM, focus, open menu and scroll on mobile/tablet/desktop');
}finally{await browser.close()}})().catch(e=>{console.error(e);process.exit(1)});
