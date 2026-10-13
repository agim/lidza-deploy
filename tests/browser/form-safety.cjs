const {chromium}=require('playwright');

const base=process.env.TEST_WEB_URL||'http://127.0.0.1:8880';
const ignored=['data-1p-ignore','data-lpignore','data-bwignore','data-form-type'];

(async()=>{
 const browser=await chromium.launch({executablePath:process.env.CHROMIUM_BIN||'/usr/bin/chromium',headless:true,args:['--no-sandbox']});
 try{
  const page=await browser.newPage();
  await page.goto(base+'/console.html?demo=1');
  await page.waitForSelector('.app-card');
  const unsafe=await page.locator('form').evaluateAll((forms,attributes)=>forms.flatMap(form=>{
   const missing=attributes.filter(attribute=>!form.hasAttribute(attribute));
   const controls=[...form.querySelectorAll('input,textarea')].filter(control=>attributes.some(attribute=>!control.hasAttribute(attribute)));
   return missing.length||controls.length?[{id:form.id,missing,controls:controls.map(control=>control.name||control.id||control.className)}]:[];
  }),ignored);
  if(unsafe.length)throw Error('non-login forms are not protected from password managers: '+JSON.stringify(unsafe));

  await page.locator('#new-app').click();
  const database=page.locator('#create-database-fields');
  if(!await database.locator('[name=database_url]').isDisabled()||!await database.locator('[name=backup_keep]').isDisabled())throw Error('hidden database controls are enabled');
  await database.locator('[name=database_mode]').selectOption('external');
  if(await database.locator('[name=database_url]').isDisabled()||await database.locator('[name=backup_keep]').isDisabled())throw Error('visible database controls are disabled');
  await database.locator('[name=database_mode]').selectOption('none');
  if(!await database.locator('[name=database_url]').isDisabled()||!await database.locator('[name=backup_keep]').isDisabled())throw Error('database controls remain enabled after being hidden');

  await page.locator('#create-env-editor').getByRole('button',{name:'Add variable'}).click();
  const key=page.locator('#create-env-editor .env-name'),value=page.locator('#create-env-editor .env-value');
  if(await key.getAttribute('name')!=='application-environment-key'||await value.getAttribute('name')!=='application-environment-content')throw Error('environment fields use password-manager-like names');
  if(await key.evaluate(control=>control.form!==null)||await value.evaluate(control=>control.form!==null))throw Error('environment helper fields are unexpectedly submitted by FormData');
  if(await value.getAttribute('autocomplete')!=='new-password')throw Error('environment secret input permits saved-password autofill');

  await page.goto(base+'/setup.html');
  if(!await page.locator('#setup-form [name=email]').isDisabled()||!await page.locator('#setup-form [name=password]').isDisabled())throw Error('hidden setup form is enabled');
  if(await page.locator('#claim-form').getAttribute('autocomplete')!=='off')throw Error('setup claim form permits autofill');

  await page.goto(base+'/login.html');
  if(await page.locator('form').getAttribute('autocomplete')==='off')throw Error('sign-in autofill was disabled');
  if(await page.locator('[name=email]').getAttribute('autocomplete')!=='username'||await page.locator('[name=password]').getAttribute('autocomplete')!=='current-password')throw Error('sign-in fields are no longer autofillable');
  if(await page.locator('[name=email]').getAttribute('data-1p-ignore')!==null)throw Error('sign-in was marked for password-manager ignore');

  console.log('PASS: non-login autofill protection, hidden controls and environment field safety');
 }finally{await browser.close()}
})().catch(error=>{console.error(error);process.exit(1)});
