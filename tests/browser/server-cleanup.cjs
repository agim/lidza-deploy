const { chromium } = require('playwright');
const assert = require('node:assert/strict');
const { mockConsole } = require('./fixtures/console.cjs');
const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8880';
(async () => {
 const browser = await chromium.launch({ executablePath: '/usr/bin/chromium', args: ['--no-sandbox'] });
 try { for (const width of [390,1440]) {
  const page = await browser.newPage({ viewport: { width, height: 900 } }), errors=[];
  page.on('pageerror', e=>errors.push(e.message));const state=await mockConsole(page);
  await page.goto(`${base}/console.html#servers`);
  await page.getByRole('button',{name:'More actions'}).click();
  await page.getByRole('menuitem',{name:'Disk cleanup & retention'}).click();
  await page.getByRole('heading',{name:'Automatic cleanup',exact:true}).waitFor();
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
