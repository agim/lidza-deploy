const {chromium}=require('playwright');
const assert=require('node:assert/strict');

function assertSeparated(rectangles,label){
 for(let index=1;index<rectangles.length;index++){
  const previous=rectangles[index-1],current=rectangles[index];
  const rowsOverlap=current.y<previous.y+previous.height&&previous.y<current.y+current.height;
  const gap=rowsOverlap?current.x-(previous.x+previous.width):current.y-(previous.y+previous.height);
  assert(gap>=7,`${label} controls are touching (${gap.toFixed(1)}px gap)`);
 }
}

(async()=>{
 const browser=await chromium.launch({executablePath:process.env.TEST_CHROMIUM_PATH||'/usr/bin/chromium',args:['--no-sandbox']});
 try{
  for(const width of [390,1440]){
   const page=await browser.newPage({viewport:{width,height:900}}),errors=[];
   page.on('pageerror',error=>errors.push(error.message));
   await page.route('**/api/control/**',async route=>{
    const path=new URL(route.request().url()).pathname;
    if(path.endsWith('/status'))return route.fulfill({json:{roles:['admin']}});
    if(path.endsWith('/apps'))return route.fulfill({json:[]});
    if(path.endsWith('/servers'))return route.fulfill({json:[]});
    if(path.endsWith('/deployments'))return route.fulfill({json:{deployments:[],unavailable_servers:[]}});
    if(path.endsWith('/team'))return route.fulfill({json:{members:[{subject:'owner',email:'owner@example.com',name:'Owner',role:'admin',owner:true},{subject:'member',email:'member@example.com',name:'Member',role:'deployer',owner:false}],next:'second-page'}});
    if(path.endsWith('/audit'))return route.fulfill({json:{records:[],next:'second-page'}});
    return route.fulfill({json:[]});
   });
   const base=process.env.TEST_WEB_URL||'http://127.0.0.1:8880';
   await page.goto(base+'/console.html#team');await page.locator('#team-next').waitFor();
   assertSeparated(await page.locator('.table-action-row button').evaluateAll(nodes=>nodes.map(node=>node.getBoundingClientRect().toJSON())),'Team row');
   assertSeparated(await page.locator('.pagination-actions button').evaluateAll(nodes=>nodes.map(node=>node.getBoundingClientRect().toJSON())),'Team pagination');
   await page.goto(base+'/console.html#audit');await page.locator('#audit-next').waitFor();
   assertSeparated(await page.locator('.pagination-actions button').evaluateAll(nodes=>nodes.map(node=>node.getBoundingClientRect().toJSON())),'Audit pagination');
   assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false,'Action rows overflow the viewport');
   assert.deepEqual(errors,[]);await page.close();
  }
  console.log('PASS: reusable action rows keep table and pagination buttons separated on desktop/mobile');
 }finally{await browser.close()}
})().catch(error=>{console.error(error);process.exit(1)});
