const { chromium } = require('playwright');
const assert = require('node:assert/strict');
const { mockConsole } = require('./fixtures/console.cjs');
const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8880';
(async () => {
 const browser = await chromium.launch({ executablePath: '/usr/bin/chromium', args: ['--no-sandbox'] });
 try { for (const width of [320,390,768,1440,2560]) {
  const page = await browser.newPage({ viewport: { width, height: 900 } }), errors=[];
  page.on('pageerror', e=>errors.push(e.message));const state=await mockConsole(page);
  await page.goto(`${base}/console.html#servers`);
  await page.getByRole('button',{name:'More actions'}).click();
  await page.getByRole('menuitem',{name:'Disk cleanup & retention'}).click();
  await page.getByRole('heading',{name:'Automatic cleanup',exact:true}).waitFor();
  const geometry=await page.locator('.maintenance-options .check-row').evaluateAll(rows=>rows.map(row=>{
   const box=row.querySelector('input').getBoundingClientRect(),icon=row.querySelector('svg').getBoundingClientRect(),text=row.querySelector('span').getBoundingClientRect(),outer=row.getBoundingClientRect();
   return {box:box.width,left:box.left-outer.left,iconGap:icon.left-box.right,textGap:text.left-icon.right,top:Math.abs(text.top-box.top)};
  }));
  for(const row of geometry){
   assert(row.box>=14&&row.box<=24,'Checkbox stretched like a full-width text field');
   assert(row.left<=20,'Checkbox drifted away from the left edge');
   assert(row.iconGap>=0&&row.iconGap<=16,'Icon is detached from checkbox');
   assert(row.textGap>=0&&row.textGap<=16,'Text is detached from icon');
   assert(row.top<=4,'Checkbox and text are not aligned at the top');
  }
  const option=page.locator('.maintenance-options .check-row').first();
  await option.getByText('Older local backups',{exact:true}).click();assert.equal(await option.locator('input').isChecked(),false);
  await option.getByText('Older local backups',{exact:true}).click();assert.equal(await option.locator('input').isChecked(),true);
  await page.screenshot({path:`/tmp/lidza-cleanup-alignment-${width}.png`,fullPage:true});
  await page.getByLabel('Run every',{exact:true}).selectOption('6');
  await page.getByLabel('Minimum age (days)',{exact:true}).fill('14');
  await page.getByRole('button',{name:'Save cleanup schedule'}).click();
  await page.getByText('Server cleanup schedule saved.').waitFor();
  const saved=state.calls.find(c=>c.method==='PATCH'&&c.path.endsWith('/maintenance'));
  assert.deepEqual(saved.body,{enabled:true,interval_hours:6,build_cache:true,logs:true,older_than_days:14});
  await page.getByRole('button',{name:'Clean selected now'}).click();
  const dialog=page.getByRole('alertdialog',{name:'Clean selected server data?'});
  assert.equal(state.calls.some(c=>c.path.endsWith('/maintenance/run')),false,'cleanup ran before confirmation');
  await dialog.getByRole('button',{name:'Start cleanup'}).click();
  await dialog.waitFor({state:'hidden'});
  await page.getByText('Total reclaimed space: 2 GB',{exact:false}).waitFor();
  const run=state.calls.find(c=>c.path.endsWith('/maintenance/run'));
  assert.deepEqual(run.body,{backups:true,build_cache:true,logs:true});
  await page.getByRole('button',{name:'Copy cleanup report'}).click();
  assert.match(await page.evaluate(()=>window.consoleCopied),/Removed 2 older local backups/);
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'mobile overflow');
  assert.deepEqual(errors,[]);await page.close();console.log(`PASS: cleanup navigation, policy, confirmation, async report, clipboard and layout at ${width}px`);
 }} finally { await browser.close(); }
})().catch(e=>{console.error(e);process.exit(1)});
