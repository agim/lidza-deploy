'use strict';
const defaultFields=[
 ['DB_MIGRATE','Apply database migrations','true or false; a predeployment backup protects an attached managed database.'],
 ['ADMIN_USERS','Administrators','Comma-separated account emails or IDs, e.g. agim@albaspot.com.'],
 ['AUTH_OWNER_CLAIM','Require operator claim','true or false; use the one-time setup token for the first account.'],
 ['MAIL_FROM','Sender address','Verified sender address accepted by your mail provider.'],
 ['MAIL_PROVIDER','Mail provider','Provider supported by the app’s mail pack. Configure credentials separately in each app.'],
 ['LOG_LEVEL','Log level','For apps that support this setting, e.g. info.']
];
let demoDefaults={values:{DB_MIGRATE:'true',LOG_LEVEL:'info'},revision:'demo'};
function defaultProfile(){return demo?Promise.resolve(structuredClone(demoDefaults)):api('control/application-defaults')}
function renderDefaults(){
 if($('#defaults-form'))return;
 $('#content').innerHTML=`<article class="panel"><h2>First deployment settings</h2><p>These values are copied once, when an app is first queued for deployment. Failed retries keep that snapshot. Explicit app settings take precedence. Editing these defaults does not change existing apps.</p><form id="defaults-form"><div class="form-row">${defaultFields.map(([key,label,hint])=>`<label>${escape(label)} <code>${key}</code><input disabled name="${key}" maxlength="2048" autocomplete="off" placeholder="Optional · omitted when empty"><small>${escape(hint)}</small></label>`).join('')}</div><p id="defaults-error" class="error" role="alert" hidden></p><button class="primary" type="submit" disabled>Save defaults</button></form><p class="hint">AUTH_SECRET is generated uniquely for each app. APP_URL follows its domain. Database, cache and storage settings come from attachments. LIDZA_MASTER_KEY and provider credentials stay manual per app; secrets are never shared through this profile.</p><p class="hint">For an existing app, open Settings → Review workspace defaults. Only the values you approve are applied, and a deployed app reloads automatically.</p></article>`;
 const form=$('#defaults-form'),button=form.querySelector('button[type=submit]');
 defaultProfile().then(profile=>{
  if(!form.isConnected)return;
  form.dataset.ready="1";
  for(const [k,v] of Object.entries(profile.values))if(form.elements[k])form.elements[k].value=v;
  button.disabled=!isAdmin();
  for(const input of form.querySelectorAll('input'))input.disabled=!isAdmin();
 }).catch(error=>{if(form.isConnected){$('#defaults-error').textContent=error.message;$('#defaults-error').hidden=false}});
 form.onsubmit=async event=>{
  event.preventDefault();button.disabled=true;form.dataset.saving="1";
  try{
   const values=Object.fromEntries([...new FormData(form)].filter(([,v])=>v.trim()).map(([k,v])=>[k,v.trim()]));
   if(demo)demoDefaults={values,revision:String(Date.now())};else await api('control/application-defaults','PUT',{values});
   dirtyForms.delete(form.id);notice('Defaults saved. They apply to apps that have not attempted deployment yet.');
  }catch(error){$('#defaults-error').textContent=error.message;$('#defaults-error').hidden=false}
  finally{delete form.dataset.saving;button.disabled=!isAdmin()}
 };
}
async function reviewDefaults(id,settings){
 const profile=await defaultProfile(),present=new Set(settings.env_keys||[]);
 const entries=Object.entries(profile.values).filter(([,v])=>v);
 const d=featureDialog(id+' · Review workspace defaults',`<p>Choose the values to copy. Saved values are write-only; only their presence is shown. Changes reload a deployed app automatically.</p><form id="apply-defaults-form">${entries.map(([key,value])=>`<label class="check-label"><input type="checkbox" name="keys" value="${escape(key)}" ${present.has(key)?'':'checked'}><span><code>${escape(key)}</code> = ${escape(value)}<small>${present.has(key)?'Already configured · preserved unless replacement is approved':'Not configured'}</small></span></label>`).join('')}${entries.length?'':'<p>No defaults configured.</p>'}<label class="check-label"><input type="checkbox" name="replace"> Replace selected variables that are already configured</label><p class="hint">Leaving replacement unchecked preserves all existing values, including explicit empty values. Unsaved edits in the Settings form must be saved first.</p><button class="primary" type="submit" ${entries.length?'':'disabled'}>Apply selected defaults</button></form>`);
 d.querySelector('form').onsubmit=async event=>{
  event.preventDefault();const button=event.submitter;button.disabled=true;
  try{
   if(dirtyForms.has('settings-form'))throw Error('Save your Settings changes before applying workspace defaults.');
   const values=new FormData(event.target),keys=values.getAll('keys'),replace=values.has('replace');
   if(!keys.length)throw Error('Select at least one default.');
   if(!demo)await api('control/apps/'+id+'/apply-defaults','POST',{keys,replace,revision:profile.revision});
   else{const app=apps.find(a=>a.id===id);app.env_keys=[...new Set([...(app.env_keys||[]),...keys])].sort()}
   d.close();$('#settings-dialog').close();await refresh();notice('Selected defaults saved. A deployed app reloads automatically.');
  }catch(error){featureError(error)}finally{button.disabled=false}
 };
}
