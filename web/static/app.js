'use strict';
const $=s=>document.querySelector(s), params=new URLSearchParams(location.search), demo=params.get('demo')==='1';
let design=demo?(params.get('design')||'terminal'):'terminal', tab=params.has('github')?'settings':'apps';
let repositoryInstallations=new Map();
let apps=[],servers=[],deployments=[],deliveries=[],status={},filter='',poll,unavailableServers=[],refreshing=false;
const escape=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
function theme(){document.body.dataset.design=design;$('#design').value=design;$('#design').hidden=!demo;$('#design-link').hidden=!demo}
function notice(s){$('#toast').textContent=s;$('#toast').hidden=false;clearTimeout(notice.timer);notice.timer=setTimeout(()=>$('#toast').hidden=true,5000)}
function fail(e){$('#error').textContent=e.message||String(e);$('#error').hidden=false}
async function api(path,method='GET',body){const res=await fetch('/api/'+path,{method,headers:{'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body)});if(res.status===401){location.href='/login.html';throw Error('Please sign in.')}let data;try{data=await res.json()}catch{data={}}if(!res.ok)throw Error(data.error?.message||data.error||'Request failed ('+res.status+')');return data}
function initDemo(){servers=[{id:'eu-1',name:'Frankfurt · production',url:'https://agent-eu.example.com'},{id:'us-1',name:'Virginia · staging',url:'https://agent-us.example.com'}];apps=[{id:'customer-portal',repository:'acme/customer-portal',domain:'portal.example.com',branch:'main',server_id:'eu-1',auto_deploy:true},{id:'content-studio',repository:'acme/content-studio',domain:'studio.example.com',branch:'main',server_id:'eu-1',auto_deploy:true},{id:'documentation',repository:'acme/docs',domain:'docs.example.com',branch:'main',server_id:'us-1',auto_deploy:false}];deployments=apps.map((a,i)=>({id:'demo-'+i,app_id:a.id,status:'live',commit:['a6b92fe','8f129bc','319fa27'][i],created:new Date(Date.now()-(i+1)*3600000).toISOString()}));apps.forEach(a=>a.current={commit:latest(a.id).commit});status={github_connected:true,github_configured:true};$('#demo-banner').hidden=false;$('#mode').textContent='Interactive preview'}
async function refresh() {
 if(refreshing)return;
 refreshing=true;
 try {
 if (!demo) {
  if(tab==='settings'||tab==='backups')await loadInfrastructure();
  const results = await Promise.allSettled(['apps','servers','deployments','status','deliveries'].map(name=>api('control/'+name)));
  const current=[apps,servers,deployments,status,deliveries];
  [apps,servers,deployments,status,deliveries]=results.map((result,i)=>result.status==='fulfilled'?result.value:current[i]);
  if(results[2].status==='fulfilled'){unavailableServers=deployments.unavailable_servers||[];deployments=deployments.deployments||[];}
  const failures=results.filter(result=>result.status==='rejected');
  if(failures.length)fail(Error('Some data could not be refreshed: '+failures.map(result=>result.reason.message).join('; ')));
  else if(unavailableServers.length)fail(Error('Deployment history unavailable for: '+unavailableServers.join(', ')+'. Other servers remain available.'));
  else $('#error').hidden=true;
 }
 render();
 }finally{refreshing=false}
}
function latest(id){return deployments.filter(d=>d.app_id===id).sort((a,b)=>new Date(b.created)-new Date(a.created))[0]}
function pill(s){return `<span class="pill ${s==='failed'?'failed':s==='live'?'':'neutral'}">${escape(s||'not deployed')}</span>`}
function card(a){let d=latest(a.id),server=servers.find(s=>s.id===a.server_id);return `<article class="app-card"><div class="card-body"><div class="card-head"><span class="app-icon">${escape(a.id.slice(0,2).toUpperCase())}</span><div><h3>${escape(a.id)}</h3><span class="domain">${escape(a.domain)}</span>${a.domain_status?.message?`<small>${escape(a.domain_status.message)}</small>`:''}</div>${pill(a.retiring?'removal pending':a.agent_error||(['building','queued'].includes(d?.status)?d.status:d?.status==='failed'?'failed':a.current?'live':d?.status))}</div><div class="repo"><span>⑂</span>${escape(a.repository)} <b>· ${escape(a.branch)}</b> <code>${escape(a.current?.commit?.slice(0,7)||'')}</code></div><div class="meta-row"><span>${escape(server?.name||a.server_id)}</span><span>${a.auto_deploy?'↻ Auto-deploy enabled':'Manual deploys'} · ${demo?'TLS demo':a.domain_status?({waiting_dns:'Waiting for DNS',requesting_ssl:'Requesting SSL',ready:'HTTPS ready'}[a.domain_status.state]||'Checking HTTPS'):'Auto TLS configured'}</span></div></div><div class="card-actions">${a.retiring?`<button data-action="remove" data-id="${escape(a.id)}">Retry removal</button>`:`<button data-action="remove" data-id="${escape(a.id)}">Remove</button><button data-action="settings" data-id="${escape(a.id)}">Settings</button>${status.github_app_connected&&!a.github_installation&&!a.preview_parent?`<button data-action="github-app" data-id="${escape(a.id)}">Use GitHub App</button>`:''}<button data-tasks="${escape(a.id)}">Workers & jobs</button>${!a.preview_parent?`<button data-previews="${escape(a.id)}">PR previews</button>`:''}<button data-maintenance="${escape(a.id)}">${a.maintenance?.enabled?'End maintenance':'Maintenance'}</button><button data-action="database" data-id="${escape(a.id)}">Database & backups</button><button data-errors="${escape(a.id)}">Errors</button><button data-action="logs" data-id="${escape(a.id)}">Logs</button><button data-action="reload" data-id="${escape(a.id)}">Reload</button><button data-action="rollback" data-id="${escape(a.id)}">Rollback</button><button data-action="${a.auto_deploy?'disable-webhook':'webhook'}" data-id="${escape(a.id)}">${a.auto_deploy?'Disable auto-deploy':'Auto-deploy'}</button>${a.auto_deploy&&a.previews?.enabled?`<button data-action="webhook" data-id="${escape(a.id)}">Refresh webhook</button>`:''}<button class="deploy" data-action="deploy" data-id="${escape(a.id)}">Deploy ↗</button>`}</div></article>`}
function render(){
 for(const b of document.querySelectorAll('[data-tab]'))b.classList.toggle('active',b.dataset.tab===tab);
 const labels={errors:['Errors','Understand failures in your applications.'],team:['Team','Give your team the access they need.'],audit:['Audit log','Who changed what, and when.'],apps:['Applications','A clear view of everything you’re building.'],deployments:['Deployments','Every release, from queued to live.'],servers:['Servers','A home for each app. A view across every host.'],backups:['Databases & backups','Protect application data across your servers.'],settings:['Integrations','Connect your repositories and automate your releases.']};
 $('#title').textContent=labels[tab][0]+'.';$('#crumb').textContent=labels[tab][0];$('#subtitle').textContent=labels[tab][1];$('#app-count').textContent=apps.length;$('#stat-apps').textContent=apps.length;$('#stat-releases').textContent=deployments.filter(d=>d.status==='live').length;$('#stat-servers').textContent=servers.length;
 const content=$('#content');
 if(tab==='apps'){content.innerHTML=`<div class="toolbar"><h2>Your applications <span class="badge">${apps.length}</span></h2><label class="sr-only" for="search">Search applications</label><input id="search" class="search" placeholder="Search applications…" value="${escape(filter)}"></div><div class="cards">${apps.filter(a=>(a.id+' '+a.domain).includes(filter.toLowerCase())).map(card).join('')||'<div class="empty"><h3>Your next application belongs here.</h3><p>Connect a server, then add a GitHub repository to begin.</p></div>'}</div>`;$('#search').addEventListener('input',e=>{filter=e.target.value;const start=e.target.selectionStart;render();$('#search').focus();$('#search').setSelectionRange(start,start)})}
 if(tab==='backups')renderBackups();
 if(tab==='errors')renderErrors();
 if(tab==='deployments'){content.innerHTML=`<div class="toolbar"><h2>Release history</h2><span class="badge">Latest ${deployments.length} releases</span></div><div class="table-wrap"><table><thead><tr><th>Application</th><th>Status</th><th>Commit</th><th>Started</th><th>Result</th></tr></thead><tbody>${[...deployments].sort((a,b)=>new Date(b.created)-new Date(a.created)).map(d=>`<tr><td><strong>${escape(d.app_id)}</strong></td><td>${pill(d.status)}</td><td><code>${escape(d.commit?.slice(0,8)||'pending')}</code></td><td>${escape(new Date(d.created).toLocaleString())}</td><td>${Math.round(d.duration_seconds||0)}s · ${escape(d.error||d.commit_message||'—')} <button data-history="${escape(d.id)}">Details & build logs</button></td></tr>`).join('')||'<tr><td colspan="5">No deployments yet. Deploy an application to see its release history.</td></tr>'}</tbody></table></div>`}
 if(tab==='deployments'&&deliveries.length){content.insertAdjacentHTML('beforeend',`<div class="panel"><h2>Push delivery queue</h2><p>Durable dispatch retries when a remote agent is unavailable.</p><div class="table-wrap"><table><thead><tr><th>Job</th><th>State</th><th>Attempts</th><th>Last error</th></tr></thead><tbody>${deliveries.map(d=>`<tr><td><code>${escape(d.ID.slice(0,12))}</code></td><td>${escape(d.State)}</td><td>${d.Attempts} / ${d.MaxAttempts}</td><td>${escape(d.LastError||'—')}</td></tr>`).join('')}</tbody></table></div></div>`)}
 if(tab==='servers'){content.innerHTML=`<div class="toolbar"><h2>Connected hosts</h2><button data-manage-server="">＋ Connect server</button><span class="badge">${servers.length} configured</span></div><div class="server-grid">${servers.map(s=>`<article class="panel"><span class="server-icon">▤</span><h2>${escape(s.name)}</h2><p>${escape(s.url)}</p><div class="server-apps">${apps.filter(a=>a.server_id===s.id).length} applications · dedicated agent</div><p>${apps.filter(a=>a.server_id===s.id).map(a=>escape(a.domain)).join('<br>')||'Ready for its first application.'}</p><button data-server="${escape(s.id)}">Add application →</button> <button data-manage-server="${escape(s.id)}">Edit</button> <button data-upgrade="${escape(s.id)}">Agent upgrade</button> <button data-remove-server="${escape(s.id)}" ${apps.some(a=>a.server_id===s.id)?'disabled':''}>Remove</button></article>`).join('')||'<div class="empty">No servers connected. Install an agent, then choose Connect server.</div>'}</div><div class="panel"><h2>One server, many domains</h2><p>Each application has a separate container and FQDN. Point DNS at the server; Caddy issues and renews HTTPS certificates for registered domains. Certificate issuance requires reachable ports 80 and 443.</p></div>`}
 if(tab==='settings'){renderGitHubSettings();renderInfrastructure()}
 renderServerFeatures();
 renderTeamPermissions();
}
function openForm(serverID){$('#app-form').reset();wireDatabaseFields($('#app-form'));$('#app-form').querySelectorAll('details').forEach(d=>d.open=false);envEditor('#create-env-editor',[]);$('#server-select').innerHTML=servers.map(s=>`<option value="${escape(s.id)}">${escape(s.name)}</option>`).join('');if(serverID)$('#server-select').value=serverID;$('#form-error').hidden=true;$('#app-dialog').showModal()}
$('#new-app').onclick=()=>openForm();$('#design').onchange=e=>{design=e.target.value;theme();render()};for(const b of document.querySelectorAll('[data-tab]'))b.onclick=()=>{tab=b.dataset.tab;filter='';render();refresh().catch(fail)};for(const b of document.querySelectorAll('[data-close]'))b.onclick=()=>b.closest('dialog').close();
$('#load-repos').onclick=async()=>{try{const repos=demo?[{full_name:'acme/private-portal'},{full_name:'acme/public-site'}]:await api('control/github/repos');repositoryInstallations=new Map(repos.map(r=>[r.full_name,r.installation_id||0]));$('#repo-options').innerHTML=repos.map(r=>`<option value="${escape(r.full_name)}"></option>`).join('');notice(`${repos.length} repositories loaded. Type in the repository field to choose.`)}catch(e){$('#form-error').textContent=e.message;$('#form-error').hidden=false}};
$('#app-form').onsubmit=async e=>{e.preventDefault();const b=e.submitter;b.disabled=true;try{let value=Object.fromEntries(new FormData(e.target));value.github_installation=repositoryInstallations.get(value.repository)||0;value.database=takeDatabase(value);value.env=value.env.trim()?JSON.parse(value.env):{};value.env=readEnv('#create-env-editor',value.env,false);if(demo){if(apps.some(a=>a.id===value.id||a.domain===value.domain))throw Error('Application ID or domain already exists.');apps.push({...value,env:undefined,env_keys:Object.keys(value.env).sort(),auto_deploy:false})}else {const created=await api('control/apps','POST',value);if(created.warning){$('#app-dialog').close();await refresh();fail(Error(created.warning));return;}}$('#app-dialog').close();tab='apps';await refresh();notice('Application created. If a database was selected, wait for it to be ready before deploying.')}catch(err){$('#form-error').textContent=err.message;$('#form-error').hidden=false}finally{b.disabled=false}};
$('#content').onclick=async e=>{const errorsButton=e.target.closest('[data-errors]');if(errorsButton){openAppErrors(errorsButton.dataset.errors);return;}try{if(await featureAction(e))return}catch(err){fail(err);return}if(await serverAction(e))return;const s=e.target.closest('[data-server]');if(s){openForm(s.dataset.server);return}const b=e.target.closest('[data-action]');if(!b)return;const {action,id}=b.dataset;b.disabled=true;try{if(action==='remove'){if(prompt('This removes '+id+' and its containers, environment, and domain routing. Databases and backups are preserved; shared databases keep their schedules. Type the application ID to confirm:')!==id)return;if(demo){apps=apps.filter(a=>a.id!==id)}else await api('control/apps/'+id,'DELETE',{confirm:id});await refresh();notice('Application removed. Remove its unused webhook in GitHub.');return}if(action==='database'){await openDatabase(id);return}if(action==='settings'){await openSettings(id);return}if(action==='logs'){const data=demo?{logs:'[demo] Application started\n[demo] GET /readyz 200\n[demo] Release is serving requests'}:await api(`control/apps/${id}/logs`);$('#logs').textContent=data.logs;$('#log-title').textContent=id+' · logs';$('#log-dialog').showModal();return}if(action==='rollback'&&!confirm('Switch '+id+' to its previous healthy release?'))return;if(demo){if(action==='deploy'){const d={id:'demo-'+Date.now(),app_id:id,status:'building',created:new Date().toISOString()};deployments.push(d);setTimeout(()=>{d.status='live';d.commit='d3e091a';const app=apps.find(a=>a.id===id);app.previous=app.current;app.current={commit:d.commit};render();notice('Demo deployment is live.')},1200)}if(action==='rollback'){const app=apps.find(a=>a.id===id);if(!app.previous)throw Error('No previous demo release. Deploy this app first.');[app.current,app.previous]=[app.previous,app.current]}if(action==='webhook'||action==='disable-webhook')apps.find(a=>a.id===id).auto_deploy=action==='webhook';notice('Demo: '+action+' accepted.')}else{await api(`control/apps/${id}/${action==='disable-webhook'?'webhook':action}`,action==='disable-webhook'?'DELETE':'POST',{});notice(action==='deploy'?'Deployment queued.':action==='reload'?'Reload queued. Check Deployments for its result.':action+' complete.')}await refresh()}catch(err){fail(err)}finally{b.disabled=false}};

let editingServer = null, editingApp = null;
function dialogError(id, error) { $(id).textContent = error.message; $(id).hidden = false; }
function openServer(id) {
 const server = servers.find(s => s.id === id);
 editingServer = server?.id || null;
 const form = $('#server-form'); form.reset();
 for (const name of ['id', 'name', 'url']) form.elements[name].value = server?.[name] || '';
 form.elements.id.readOnly = !!server;
 form.elements.token.required = !server;
 $('#server-title').textContent = server ? 'Edit server' : 'Connect server';
 $('#server-error').hidden = true; $('#server-dialog').showModal();
}
async function serverAction(event) {
 const edit = event.target.closest('[data-manage-server]');
 if (edit) { openServer(edit.dataset.manageServer); return true; }
 const remove = event.target.closest('[data-remove-server]');
 if (!remove) return false;
 const id = remove.dataset.removeServer;
 if (!confirm('Remove '+id+' from the control panel? This does not uninstall its agent.')) return true;
 try {
  if (demo) servers = servers.filter(s => s.id !== id);
  else await api('control/servers/'+id, 'DELETE');
  await refresh(); notice('Server removed.');
 } catch (error) { fail(error); }
 return true;
}
$('#server-form').onsubmit = async event => {
 event.preventDefault(); const button=event.submitter; button.disabled=true;
 try {
  const value=Object.fromEntries(new FormData(event.target));
  if (demo) {
   if (servers.some(s=>s.id!==editingServer && (s.id===value.id||s.url===value.url))) throw Error('Server ID or URL already exists.');
   delete value.token;
   if (editingServer) servers[servers.findIndex(s=>s.id===editingServer)]=value;
   else servers.push(value);
  } else await api('control/servers'+(editingServer?'/'+editingServer:''),editingServer?'PUT':'POST',value);
  event.target.reset(); $('#server-dialog').close(); await refresh(); notice('Server saved.');
 } catch(error) { dialogError('#server-error',error); }
 finally { button.disabled=false; }
};
async function openSettings(id) {
 const app=apps.find(a=>a.id===id);
 const value=demo?{...app,env_keys:app.env_keys||[]}:await api('control/apps/'+id+'/settings');
 editingApp=id; const form=$('#settings-form'); form.reset();form.querySelectorAll('details').forEach(d=>d.open=false);
 form.elements.backup_before_deploy.checked=value.backup_before_deploy!==false;form.elements.branch.value=value.branch; form.elements.domain.value=value.domain;
 $('#env-keys').textContent=value.env_keys?.join(', ')||'No variables saved';envEditor('#settings-env-editor',value.env_keys||[],Object.keys(value.database_bindings||{}));
 $('#settings-title').textContent=id+' · settings'; $('#settings-error').hidden=true;
 $('#settings-dialog').showModal();
}
$('#settings-form').onsubmit = async event => {
 event.preventDefault(); const button=event.submitter;button.disabled=true;
 try {
  const value=Object.fromEntries(new FormData(event.target));
  value.backup_before_deploy=event.target.elements.backup_before_deploy.checked;value.env_changes=value.env_changes.trim()?JSON.parse(value.env_changes):{};
  if (!value.env_changes || Array.isArray(value.env_changes) || typeof value.env_changes!=='object' || Object.values(value.env_changes).some(v=>v!==null&&typeof v!=='string')) throw Error('Environment changes must be a JSON object containing strings or null.');
  value.env_changes=readEnv('#settings-env-editor',value.env_changes,true);
  if (demo) {
   if(apps.some(a=>a.id!==editingApp&&a.domain===value.domain))throw Error('Domain already registered.');
   const app=apps.find(a=>a.id===editingApp),keys=new Set(app.env_keys||[]);
   for(const [key,v] of Object.entries(value.env_changes)) { if(v===null)keys.delete(key);else keys.add(key); }
   Object.assign(app,{backup_before_deploy:value.backup_before_deploy,branch:value.branch,domain:value.domain,env_keys:[...keys].sort()});
  } else await api('control/apps/'+editingApp+'/settings','PATCH',value);
  event.target.reset();$('#settings-dialog').close();await refresh();notice('Settings saved. A deployed app reloads automatically; check Deployments for the result. Branch changes apply to the next Git deployment.');
 } catch(error) { dialogError('#settings-error',error); }
 finally {button.disabled=false;}
};
for(const id of ['server-dialog','settings-dialog','app-dialog'])$("#"+id).addEventListener('close',()=>$("#"+id+' form').reset());
function envEditor(selector,keys,managed=[]) {
 const host=$(selector);host.replaceChildren();
 const rows=document.createElement('div');host.append(rows);
 function add(key='') {
  const row=document.createElement('div');row.className='env-row';row.dataset.saved=key?'1':'0';
  const nameLabel=document.createElement('label');nameLabel.textContent='Name';const name=document.createElement('input');name.className='env-name';name.value=key;name.readOnly=!!key;name.autocomplete='off';name.spellcheck=false;name.placeholder='DATABASE_URL';nameLabel.append(name);
  const valueLabel=document.createElement('label');valueLabel.textContent='Value';const value=document.createElement('input');value.className='env-value';value.type='password';value.autocomplete='new-password';value.spellcheck=false;value.placeholder=key?'Saved value stays unchanged':'Enter value';valueLabel.append(value);
  const actionLabel=document.createElement('label');actionLabel.textContent='Action';const action=document.createElement('select');action.className='env-action';
  for(const [v,text] of (key?[['keep','Keep saved value'],['set','Replace value'],['delete','Delete variable']]:[['set','Set value'],['discard','Discard row']])){const o=document.createElement('option');o.value=v;o.textContent=text;action.append(o)}
  value.oninput=()=>{action.value='set'};action.onchange=()=>{value.disabled=action.value!=='set';if(value.disabled)value.value=''};actionLabel.append(action);row.append(nameLabel,valueLabel,actionLabel);rows.append(row);
 }
 keys.forEach(add);
 for(const row of rows.children){if(managed.includes(row.querySelector(".env-name").value)){row.querySelector(".env-value").disabled=true;row.querySelector(".env-value").placeholder="Managed in Database & backups";row.querySelector(".env-action").disabled=true;}}const button=document.createElement('button');button.type='button';button.textContent='＋ Add variable';button.onclick=()=>add();host.append(button);
}
function readEnv(selector,base,allowDelete) {
 if(!base||Array.isArray(base)||typeof base!=='object'||Object.values(base).some(v=>typeof v!=='string'&&!(allowDelete&&v===null)))throw Error('Environment must contain string values'+(allowDelete?' or null to delete.':'.'));
 const changes=Object.assign(Object.create(null),base),seen=new Set();
 for(const row of $(selector).querySelectorAll('.env-row')){
  const action=row.querySelector('.env-action').value,key=row.querySelector('.env-name').value;
  if(action==='discard')continue;
  if(!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key))throw Error('Enter a valid variable name.');
  if(seen.has(key))throw Error('Each variable name must be unique.');seen.add(key);
  if(action==='keep')continue;
  if(Object.hasOwn(changes,key))throw Error('Variable '+key+' also appears in the JSON changes.');
  changes[key]=action==='delete'?null:row.querySelector('.env-value').value;
 }
 return changes;
}
for(const id of ['app-dialog','settings-dialog'])$('#'+id).addEventListener('close',()=>{const editor=$(id==='app-dialog'?'#create-env-editor':'#settings-env-editor');editor.replaceChildren()});
$('#create-database-fields').innerHTML=databaseFields();
$('#database-dialog').addEventListener('close',()=>$('#database-content').replaceChildren());
theme();if(demo)initDemo();refresh().catch(fail);if(!demo)poll=setInterval(()=>{if(!document.querySelector('dialog[open]')&&document.activeElement?.id!=='search'&&!document.activeElement?.closest('form'))refresh().catch(fail)},5000);window.addEventListener('pagehide',()=>clearInterval(poll));

function renderGitHubSettings(){
 const registered=!!status.github_app_configured,connected=!!status.github_app_connected,localOnly=!demo&&status.github_public_https===false;
 $('#content').innerHTML=`<article class="panel integration-panel" aria-labelledby="github-title">
  <header class="integration-header"><div><span class="eyebrow">SOURCE CONTROL</span><h2 id="github-title">GitHub</h2></div><span class="integration-status">${localOnly?'Local mode':connected?'Connected':registered?'Repository selection needed':'Not connected'}</span></header>
  <p class="integration-description">Connect your repositories for private access and automatic deployments.</p>
  <p class="integration-guidance">${connected?'Repository access is connected. Deployment credentials renew automatically.':registered?'Your GitHub App is registered. Choose the repositories it can access.':'GitHub will guide you through creating an app for this installation and choosing its repositories. No credentials to copy.'}</p>
  ${status.github_app_slug?`<p class="integration-app">App: <strong>${escape(status.github_app_slug)}</strong></p>`:''}
  ${!registered&&!localOnly?'<div class="integration-account"><label for="github-organization">GitHub organization <span class="optional">Optional</span></label><input id="github-organization" autocomplete="off" placeholder="e.g. your-company" aria-describedby="github-account-help"><p id="github-account-help">Leave blank to use your personal GitHub account. Organizations may require administrator approval.</p></div>':''}
  ${localOnly?'<div class="integration-requirement" id="github-https-help"><strong>Public HTTPS is required to connect GitHub</strong><p>This control panel was installed with localhost. GitHub needs a public HTTPS address for registration callbacks and automatic deployment webhooks. For GitHub integration, install the control panel with <code>--fqdn deploy.your-domain.com</code> on a public server with working DNS and ports 80/443 available. Reinstalling this existing panel under a different hostname requires a migration.</p></div>':''}
  <div class="integration-actions"><button class="primary" id="connect" ${localOnly?'disabled aria-describedby="github-https-help"':''}>${registered?'Choose repositories':'Connect GitHub'}</button>${registered?'<button id="disconnect">Disconnect GitHub App</button>':''}</div>
  <p class="integration-note">Public repositories can deploy without a GitHub connection.</p>
  ${!localOnly?'<details class="integration-legacy"><summary>Existing OAuth connection</summary><p>Existing deployments keep their OAuth connection until you switch each app using Use GitHub App. Remove the old repository webhook after switching.</p><a class="button" href="/api/v1/auth/connect/github/start?redirect=/console.html">Reconnect existing OAuth account</a></details>':''}
 </article><button id="logout">Sign out</button>`;
 $('#connect').onclick=async()=>{try{if(demo){notice('Preview: GitHub app registration and repository selection would open.');return}if(registered){const result=await api('control/github/app/install','POST',{});location.href=result.url}else{const result=await api('control/github/app/register','POST',{organization:$('#github-organization').value.trim()});const form=document.createElement('form');form.method='POST';form.action=result.action;const input=document.createElement('input');input.type='hidden';input.name='manifest';input.value=JSON.stringify(result.manifest);form.append(input);document.body.append(form);form.submit()}}catch(err){fail(err)}};
 if($('#disconnect'))$('#disconnect').onclick=async()=>{try{if(!demo)await api('control/github/app','DELETE');await refresh();notice('Connection removed here. Uninstall the app in GitHub to revoke its access.')}catch(err){fail(err)}};
 $('#logout').onclick=async()=>{if(!demo)await api('v1/auth/logout','POST',{});location.href='/login.html'};
 if(params.has('github')){notice(({registered:'GitHub App registered. Choose repositories next.',connected:'GitHub connected. Your selected repositories are available.',pending:'GitHub approval is pending. Return to Choose repositories once approved.'})[params.get('github')]||'GitHub connection updated.');params.delete('github');history.replaceState(null,'','/console.html')}
}
