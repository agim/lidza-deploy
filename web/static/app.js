'use strict';
const $=s=>document.querySelector(s), params=new URLSearchParams(location.search), demo=params.get('demo')==='1';
const sectionTabs={help:'help',updates:'updates',security:'security',applications:'apps',deployments:'deployments',servers:'servers',databases:'backups',integrations:'settings',errors:'errors',team:'team',audit:'audit'};
const tabSections=Object.fromEntries(Object.entries(sectionTabs).map(([section,value])=>[value,section]));
function tabFromURL(){return sectionTabs[location.hash.slice(1).split('/')[0]]||'apps'}
let design=demo?(params.get('design')||'terminal'):'terminal', tab=params.has('github')?'settings':tabFromURL();
function sectionURL(next){const url=new URL(location.href);url.hash=tabSections[next];return url}
function navigate(next){if(!tabSections[next])return;if(next!==tab&&dirtyForms.size&&!confirm('Leave this section and discard unsaved changes?'))return Promise.resolve();if(next!==tab)dirtyForms.clear();closeMobileNavigation();const url=sectionURL(next);if(url.href!==location.href)history.pushState(null,'',url);tab=next;filter='';render();return refresh()}
function restoreSection(){const next=tabFromURL();if(next===tab){if(tab==='help')renderHelp();return}for(const dialog of document.querySelectorAll('dialog[open]'))dialog.close();if(dirtyForms.size){if(!confirm('Leave this section and discard unsaved changes?')){history.pushState(null,'',sectionURL(tab));return}dirtyForms.clear()}closeMobileNavigation();tab=next;filter='';render();refresh().catch(fail)}
window.addEventListener('popstate',restoreSection);
window.addEventListener('hashchange',restoreSection);
let repositoryInstallations=new Map(), repositoryBranches=new Map(), branchEdited=false;
function applyRepositoryBranch(){const form=$('#app-form');const branch=repositoryBranches.get(form.elements.repository.value.trim());if(branch&&!branchEdited)form.elements.branch.value=branch;}
$('#app-form').elements.branch.addEventListener('input',()=>branchEdited=true);
$('#app-form').elements.repository.addEventListener('input',applyRepositoryBranch);
let apps=[],servers=[],deployments=[],deliveries=[],status={},filter='',poll,unavailableServers=[],refreshing=false;
const dirtyForms=new Set();let lastRefresh=0,refreshFailures=0;
function clearFormDraft(id){dirtyForms.delete(id)}
function captureFormDrafts(){return [...dirtyForms].map(id=>{const node=document.getElementById(id);return {id,fields:node?[...node.querySelectorAll('input,select,textarea')].filter(e=>e.name).map(e=>({name:e.name,value:e.value,checked:e.checked})):[],value:node?.tagName==='INPUT'?node.value:undefined}})}
function restoreFormDrafts(drafts){for(const draft of drafts){const node=document.getElementById(draft.id);if(!node){dirtyForms.delete(draft.id);continue}if(draft.value!==undefined)node.value=draft.value;for(const field of draft.fields){const input=node.querySelector('[name="'+CSS.escape(field.name)+'"]');if(input){input.value=field.value;if(input.type==='checkbox'||input.type==='radio')input.checked=field.checked}}}}
function markFormDraft(event){if(!event.target.matches('input,select,textarea'))return;const form=event.target.closest('form');if(form?.id)dirtyForms.add(form.id);else if(event.target.id==='github-organization')dirtyForms.add(event.target.id);updateConnectionBadge()}
function updateConnectionBadge(){if(demo)return;const seconds=lastRefresh?Math.floor((Date.now()-lastRefresh)/1000):0;$('#mode').textContent=refreshFailures?'Some data unavailable':!lastRefresh?'Connecting…':dirtyForms.size?'Unsaved changes · refresh paused':'Updated '+(seconds<5?'just now':seconds+'s ago');$('#mode').title=lastRefresh?'Last successful workspace refresh: '+new Date(lastRefresh).toLocaleString():'Waiting for the control panel';}
function closeMobileNavigation(){const toggle=$('#menu-toggle');$('#workspace-sidebar').classList.remove('menu-open');toggle.setAttribute('aria-expanded','false')}
function applicationEmptyState(){if(apps.length)return '<div class="empty"><h3>No matching applications.</h3><p>Try another application name or hostname.</p><button data-clear-search>Clear search</button></div>';if(servers.length)return '<div class="empty"><h3>Add your first application.</h3><p>Your server is configured. Choose a GitHub repository to get started.</p>'+(isAdmin()?'<button class="primary" data-add-first-app>＋ New application</button>':'<p>Ask an administrator to add an application.</p>')+'</div>';return '<div class="empty"><h3>Connect your first server.</h3><p>Install an agent on your hosting server, then connect it here.</p>'+(isAdmin()?'<button class="primary" data-manage-server="">Connect server</button>':'<p>Ask an administrator to connect a server.</p>')+'</div>'}
function deliveryTable(rows){return '<div class="table-wrap"><table><thead><tr><th>Job</th><th>State</th><th>Attempts</th><th>Last error</th></tr></thead><tbody>'+rows.map(d=>`<tr><td><code>${escape(d.ID.slice(0,12))}</code></td><td>${pill(d.State==='pending'?(d.Attempts?'retrying':'queued'):d.State)}</td><td>${d.Attempts} / ${d.MaxAttempts}</td><td>${escape(d.LastError||'—')}</td></tr>`).join('')+'</tbody></table></div>'}

const escape=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
function theme(){document.body.dataset.design=design;$('#design').value=design;$('#design').hidden=!demo;$('#design-link').hidden=!demo}
function showNotice(target,message){target.replaceChildren();const text=document.createElement('span');text.textContent=message;const close=document.createElement('button');close.type='button';close.className='notice-dismiss';close.textContent='Dismiss';close.setAttribute('aria-label','Dismiss notification');close.onclick=()=>{target.hidden=true};target.append(text,close);target.hidden=false}
function notice(s){showNotice($('#toast'),s)}
function fail(e,source='action'){const target=$('#error');if(source==='refresh'&&!target.hidden&&target.dataset.source==='action')return;target.dataset.source=source;showNotice(target,e.message||String(e))}
async function api(path,method='GET',body){const res=await fetch('/api/'+path,{method,headers:{'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body)});if(res.status===401){location.href='/login.html?next='+encodeURIComponent(location.pathname+location.search+location.hash);throw Error('Please sign in.')}let data;try{data=await res.json()}catch{data={}}if(!res.ok)throw Error(data.error?.message||data.error||'Request failed ('+res.status+')');return data}
function initDemo(){servers=[{id:'eu-1',name:'Frankfurt · production',url:'https://agent-eu.example.com'},{id:'us-1',name:'Virginia · staging',url:'https://agent-us.example.com'}];apps=[{id:'customer-portal',repository:'acme/customer-portal',domain:'portal.example.com',branch:'main',server_id:'eu-1',auto_deploy:true},{id:'content-studio',repository:'acme/content-studio',domain:'studio.example.com',branch:'main',server_id:'eu-1',auto_deploy:true},{id:'documentation',repository:'acme/docs',domain:'docs.example.com',branch:'main',server_id:'us-1',auto_deploy:false}];deployments=apps.map((a,i)=>({id:'demo-'+i,app_id:a.id,status:'live',commit:['a6b92fe','8f129bc','319fa27'][i],created:new Date(Date.now()-(i+1)*3600000).toISOString()}));apps.forEach(a=>a.current={commit:latest(a.id).commit});status={github_connected:true,github_configured:true};$('#demo-banner').hidden=false;$('#mode').textContent='Interactive preview'}
async function refresh({background=false}={}) {
 if(refreshing||background&&dirtyForms.size)return;
 refreshing=true;
 try {
 if (!demo) {
  if(tab==='settings'||tab==='backups')await loadInfrastructure();
  const results = await Promise.allSettled(['apps','servers','deployments','status','deliveries'].map(name=>api('control/'+name)));
  const current=[apps,servers,deployments,status,deliveries];
  [apps,servers,deployments,status,deliveries]=results.map((result,i)=>result.status==='fulfilled'?result.value:current[i]);
  if(results[2].status==='fulfilled'){unavailableServers=deployments.unavailable_servers||[];deployments=deployments.deployments||[];}
  const failures=results.filter(result=>result.status==='rejected');refreshFailures=failures.length+unavailableServers.length;
  if(failures.length)fail(Error('Some data could not be refreshed: '+failures.map(result=>result.reason.message).join('; ')),'refresh');
  else if(unavailableServers.length)fail(Error('Deployment history unavailable for: '+unavailableServers.join(', ')+'. Other servers remain available.'),'refresh');
  else if($('#error').dataset.source==='refresh')$('#error').hidden=true;
 }
 if(tab==='updates'&&!document.querySelector('dialog[open]'))await loadUpdates();
 if(!background||!dirtyForms.size){const snapshot=workspaceSnapshot();if((!background||snapshot!==renderedSnapshot)&&!(background&&document.querySelector('dialog[open]'))){render();renderedSnapshot=snapshot;}if(!refreshFailures)lastRefresh=Date.now()}if(!document.querySelector('dialog[open]')){if(tab==='servers')renderServerFeatures();if(tab==='errors'&&errorView.app&&!errorView.loading&&Date.now()-errorView.fetched>30000)loadAppErrors();if(tab==='security'&&securityApp&&!securityBusy&&Date.now()-securityFetched>30000)loadSecurityActivity()}updateConnectionBadge();
 }finally{refreshing=false}
}
let renderedSnapshot='';
function latest(id){return deployments.filter(d=>d.app_id===id).sort((a,b)=>new Date(b.created)-new Date(a.created))[0]}
function pill(s){const symbol=({failed:'ⓧ',live:'✓',ready:'✓',queued:'◷',pending:'◷',retrying:'↻',building:'↻',running:'↻',done:'✓'})[s]||'○';return `<span class="pill ${s==='failed'?'failed':s==='live'?'':'neutral'}"><span aria-hidden="true">${symbol}</span> ${escape(s||'not deployed')}</span>`}
function workspaceSnapshot(){
 const permissions=[status.roles,status.github_app_connected];
 if(tab==='apps')return JSON.stringify([tab,apps,servers,apps.map(a=>{const d=latest(a.id);return [a.id,d?.id,d?.status]}),permissions]);
 if(tab==='deployments')return JSON.stringify([tab,deployments.map(({log,...d})=>d),deliveries,permissions]);
 if(tab==='servers')return JSON.stringify([tab,servers,apps.map(a=>[a.id,a.domain,a.server_id]),permissions]);
 if(tab==='backups')return JSON.stringify([tab,databases,servers,databaseServerErrors,permissions]);
 if(tab==='settings')return JSON.stringify([tab,status,infrastructure,permissions]);
 return JSON.stringify([tab,['errors','security'].includes(tab)?apps.map(a=>[a.id,a.retiring]):[],permissions]);
}
function card(a){let d=latest(a.id),server=servers.find(s=>s.id===a.server_id);return `<article class="app-card" data-app-card="${escape(a.id)}"><div class="card-body"><div class="card-head"><span class="app-icon">${escape(a.id.slice(0,2).toUpperCase())}</span><div><h3>${escape(a.id)}</h3><span class="domain">${escape(a.domain)}</span>${a.domain_status?.message?`<small>${escape(a.domain_status.message)}</small>`:''}</div>${pill(a.retiring?'removal pending':a.agent_error||(['building','queued'].includes(d?.status)?d.status:d?.status==='failed'?'failed':a.current?'live':d?.status))}</div><div class="repo"><span>⑂</span>${escape(a.repository)} <b>· ${escape(a.branch)}</b> <code>${escape(a.current?.commit?.slice(0,7)||'')}</code></div><div class="meta-row"><span>${escape(server?.name||a.server_id)}</span><span>${a.auto_deploy?'↻ Auto-deploy enabled':'Manual deploys'} · ${demo?'TLS demo':a.domain_status?({waiting_dns:'Waiting for DNS',requesting_ssl:'Requesting SSL',ready:'HTTPS ready'}[a.domain_status.state]||'Checking HTTPS'):'Auto TLS configured'}</span></div></div><div class="card-actions">${a.retiring?`<button data-action="remove" data-id="${escape(a.id)}">Retry removal</button>`:`<button class="deploy" data-action="deploy" data-id="${escape(a.id)}">Deploy ↗</button><button data-history="${escape(d?.id||'')}" ${d?'':'disabled'}>Build logs</button><button data-action="settings" data-id="${escape(a.id)}">Settings</button><details class="app-more" data-app-menu="${escape(a.id)}"><summary>More actions</summary><div class="app-more-actions"><button data-action="logs" data-id="${escape(a.id)}">Runtime logs</button>${(status.roles||[]).some(r=>r==='admin'||r==='deployer')?`<button data-owner-claim="${escape(a.id)}">Operator setup token</button>`:''}${status.github_app_connected&&!a.github_installation&&!a.preview_parent?`<button data-action="github-app" data-id="${escape(a.id)}">Connect repository</button>`:''}<button data-tasks="${escape(a.id)}">Workers & jobs</button>${!a.preview_parent?`<button data-previews="${escape(a.id)}">PR previews</button>`:''}<button data-maintenance="${escape(a.id)}">${a.maintenance?.enabled?'End maintenance':'Maintenance'}</button><button data-action="database" data-id="${escape(a.id)}">Database & backups</button><button data-security="${escape(a.id)}">Security activity</button><button data-errors="${escape(a.id)}">Errors</button><button data-action="reload" data-id="${escape(a.id)}">Reload</button><button data-action="rollback" data-id="${escape(a.id)}">Rollback</button><button data-action="${a.auto_deploy?'disable-webhook':'webhook'}" data-id="${escape(a.id)}">${a.auto_deploy?'Disable auto-deploy':'Auto-deploy'}</button>${a.auto_deploy&&a.previews?.enabled?`<button data-action="webhook" data-id="${escape(a.id)}">Refresh webhook</button>`:''}<button data-action="remove" data-id="${escape(a.id)}">Remove application</button></div></details>`}</div></article>`}
// These renderers retain application actions and release table scrollers across polls.
function renderApplications(){
 const content=$('#content');
 if(!content.querySelector('.cards')){content.innerHTML='<div class="toolbar"><h2>Your applications <span class="badge"></span></h2><label class="sr-only" for="search">Search applications</label><input id="search" class="search" placeholder="Search applications…"></div><div class="cards"></div>';$('#search').oninput=e=>{filter=e.target.value;render()}}
 content.querySelector('.toolbar .badge').textContent=apps.length;
 if($('#search').value!==filter)$('#search').value=filter;
 const list=content.querySelector('.cards'),visible=apps.filter(a=>(a.id+' '+a.domain).toLowerCase().includes(filter.toLowerCase())),retained=new Set();
 for(const [index,a] of visible.entries()){
  let node=[...list.children].find(e=>e.dataset.appCard===a.id);
  const template=document.createElement('template');template.innerHTML=card(a);const next=template.content.firstElementChild;
  if(!node){node=next;list.insertBefore(node,list.children[index]||null)}
  else{
   const body=node.querySelector('.card-body'),html=next.querySelector('.card-body').innerHTML;if(body.innerHTML!==html)body.innerHTML=html;
   // Deployment output/duration changes never recreate Settings, Deploy or the open menu.
   const signature=JSON.stringify([a.retiring,a.auto_deploy,a.maintenance?.enabled,a.github_installation,a.preview_parent,a.previews?.enabled,status.roles,status.github_app_connected]);
   if(node.dataset.actions!==signature){const opened=node.querySelector('details')?.open;node.querySelector('.card-actions').replaceWith(next.querySelector('.card-actions'));if(opened&&node.querySelector('details'))node.querySelector('details').open=true}
  }
  node.dataset.actions=JSON.stringify([a.retiring,a.auto_deploy,a.maintenance?.enabled,a.github_installation,a.preview_parent,a.previews?.enabled,status.roles,status.github_app_connected]);
  const history=node.querySelector('[data-history]'),d=latest(a.id);if(history){history.dataset.history=d?.id||'';history.disabled=!d}
  if(list.children[index]!==node)list.insertBefore(node,list.children[index]||null);retained.add(node);
 }
 for(const node of [...list.children])if(!retained.has(node))node.remove();
 if(!visible.length)list.innerHTML=applicationEmptyState();
}
function renderReleaseHistory(){
 const content=$('#content');
 if(!content.querySelector('#release-table'))content.innerHTML='<div class="toolbar"><h2>Release history</h2><span class="badge"></span></div><div class="table-wrap" id="release-table"><table><thead><tr><th>Application</th><th>Status</th><th>Commit</th><th>Started</th><th>Result</th></tr></thead><tbody></tbody></table></div>';
 content.querySelector('.toolbar .badge').textContent='Latest '+deployments.length+' releases';
 const body=content.querySelector('#release-table tbody'),retained=new Set();
 for(const [index,d] of [...deployments].sort((a,b)=>new Date(b.created)-new Date(a.created)).entries()){
  let row=[...body.children].find(e=>e.dataset.release===d.id);
  if(!row){row=document.createElement('tr');row.dataset.release=d.id;row.innerHTML='<td data-label="Application"><strong></strong></td><td data-label="Status"></td><td data-label="Commit"><code></code></td><td data-label="Started"></td><td data-label="Result"><span></span> <button>Details & build logs</button></td>';row.querySelector('button').dataset.history=d.id;body.insertBefore(row,body.children[index]||null)}
  const cells=row.cells,values=[d.app_id,pill(d.status),d.commit?.slice(0,8)||'pending',new Date(d.created).toLocaleString(),Math.round(d.duration_seconds||0)+'s · '+(d.error||d.commit_message||'—')];
  for(let i=0;i<5;i++){const target=i===0?cells[i].querySelector('strong'):i===2?cells[i].querySelector('code'):i===4?cells[i].querySelector('span'):cells[i];if(i===1){if(target.innerHTML!==values[i])target.innerHTML=values[i]}else if(target.textContent!==values[i])target.textContent=values[i]}
  if(body.children[index]!==row)body.insertBefore(row,body.children[index]||null);retained.add(row);
 }
 for(const row of [...body.children])if(!retained.has(row))row.remove();
 if(!deployments.length)body.innerHTML='<tr><td colspan="5">No deployments yet. Deploy an application to see its release history.</td></tr>';
 const active=deliveries.filter(d=>d.State!=='done'),completed=deliveries.filter(d=>d.State==='done');
 for(const [kind,rows] of [['pending',active],['completed',completed]]){
  let panel=content.querySelector('[data-deliveries="'+kind+'"]');
  // Keep the same wrappers and completed disclosure even as deliveries change state.
  if(!panel){panel=document.createElement(kind==='pending'?'article':'details');panel.dataset.deliveries=kind;panel.className='panel '+(kind==='pending'?'delivery-attention':'completed-deliveries');panel.innerHTML=kind==='pending'?'<h2>Push deliveries needing attention</h2><p>Queued and retrying deliveries run when their hosting agent is available.</p>':'<summary></summary>';panel.insertAdjacentHTML('beforeend',deliveryTable([]));content.append(panel)}
  panel.hidden=!rows.length;if(kind==='completed')panel.querySelector('summary').textContent='Completed push deliveries ('+rows.length+')';
  const tableBody=panel.querySelector('tbody'),html=rows.map(d=>`<tr><td><code>${escape(d.ID.slice(0,12))}</code></td><td>${pill(d.State==='pending'?(d.Attempts?'retrying':'queued'):d.State)}</td><td>${d.Attempts} / ${d.MaxAttempts}</td><td>${escape(d.LastError||'—')}</td></tr>`).join('');if(tableBody.innerHTML!==html)tableBody.innerHTML=html;
 }
}
function renderServers(){
 const content=$('#content'),template=document.createElement('template');template.innerHTML=`<div class="toolbar"><h2>Connected hosts</h2><button data-manage-server="">＋ Connect server</button><span class="badge">${servers.length} configured</span></div><div class="server-grid">${servers.map(s=>`<article class="panel" data-server-card="${escape(s.id)}"><span class="server-icon">▤</span><h2>${escape(s.name)}</h2><p>${escape(s.url)}</p><div class="server-apps">${apps.filter(a=>a.server_id===s.id).length} applications · dedicated agent</div><p>${apps.filter(a=>a.server_id===s.id).map(a=>escape(a.domain)).join('<br>')||'Ready for its first application.'}</p><button data-server="${escape(s.id)}">Add application →</button> <button data-manage-server="${escape(s.id)}">Edit</button> <button data-upgrade="${escape(s.id)}">Agent upgrade</button> <button data-ssh-access="${escape(s.id)}">SSH access</button> <button data-remove-server="${escape(s.id)}" ${apps.some(a=>a.server_id===s.id)?'disabled':''}>Remove</button></article>`).join('')||'<div class="empty">No servers connected. Install an agent, then choose Connect server.</div>'}</div><div class="panel"><h2>One server, many domains</h2><p>Each application has a separate container and FQDN. Point DNS at the server; Caddy issues and renews HTTPS certificates for registered domains. Certificate issuance requires reachable ports 80 and 443.</p></div>`;
 if(!content.querySelector('.server-grid')){content.replaceChildren(template.content);return}
 content.querySelector('.toolbar .badge').textContent=servers.length+' configured';
 const grid=content.querySelector('.server-grid'),fresh=[...template.content.querySelector('.server-grid').children],keep=new Set();
 for(const [index,next] of fresh.entries()){
  let node=[...grid.children].find(e=>e.dataset.serverCard===next.dataset.serverCard);
  if(!node){node=next;grid.insertBefore(node,grid.children[index]||null)}else if(next.dataset.serverCard){
   for(const selector of ['h2','p','.server-apps','p:nth-of-type(2)']){const target=node.querySelector(selector),value=next.querySelector(selector);if(target.innerHTML!==value.innerHTML)target.innerHTML=value.innerHTML}
   node.querySelector('[data-remove-server]').disabled=next.querySelector('[data-remove-server]').disabled;
  }
  if(grid.children[index]!==node)grid.insertBefore(node,grid.children[index]||null);keep.add(node);
 }
 for(const node of [...grid.children])if(!keep.has(node))node.remove();
}
function render(){
 const sameSection=document.body.dataset.section===tab;const drafts=captureFormDrafts();const scrolls=[...document.querySelectorAll('#content .table-wrap')].map(e=>({node:e,left:e.scrollLeft,top:e.scrollTop}));const anchor=sameSection?[...document.querySelectorAll('#content [data-app-card],#content [data-release],#content [data-database-card],#content [data-server-card]')].find(e=>{const r=e.getBoundingClientRect();return r.top>=0&&r.top<innerHeight}):null;const anchorTop=anchor?.getBoundingClientRect().top;const pageScroll=[window.scrollX,window.scrollY];const expanded=[...document.querySelectorAll('#content details[open][data-app-menu]')].map(e=>e.dataset.appMenu);document.body.dataset.section=tab;
 for(const b of document.querySelectorAll('[data-tab]')){b.classList.toggle('active',b.dataset.tab===tab);if(b.dataset.tab===tab)b.setAttribute('aria-current','page');else b.removeAttribute('aria-current')}
 const labels={help:['Help','Practical guides for deploying and operating your apps.'],updates:['Updates','Keep the control panel and hosting agents current.'],security:['Security activity','Review suspicious traffic across hosted applications.'],errors:['Errors','Understand failures in your applications.'],team:['Team','Give your team the access they need.'],audit:['Audit log','Who changed what, and when.'],apps:['Applications','A clear view of everything you’re building.'],deployments:['Deployments','Every release, from queued to live.'],servers:['Servers','A home for each app. A view across every host.'],backups:['Databases & backups','Protect application data across your servers.'],settings:['Integrations','Connect your repositories and automate your releases.']};
 $('#mobile-section').textContent=labels[tab][0];$('#new-app').disabled=!servers.length;$('#new-app').title=servers.length?'':'Connect a server before creating an application';$('#new-app').classList.toggle('primary',tab!=='settings');$('#title').textContent=labels[tab][0]+'.';$('#crumb').textContent=labels[tab][0];$('#subtitle').textContent=labels[tab][1];$('#app-count').textContent=apps.length;$('#stat-apps').textContent=apps.length;$('#stat-releases').textContent=deployments.filter(d=>d.status==='live').length;$('#stat-servers').textContent=servers.length;
 const content=$('#content'),minimum=content.style.minHeight;if(sameSection)content.style.minHeight=content.offsetHeight+'px';
 if(tab==='apps')renderApplications();
 if(tab==='help')renderHelp();
 if(tab==='updates'){renderUpdates();loadUpdates().catch(fail)}
 if(tab==='backups')renderBackups();
 if(tab==='errors')renderErrors();
 if(tab==='security')renderSecurityActivity();
 if(tab==='deployments')renderReleaseHistory();
 if(tab==='servers')renderServers();
 if(tab==='settings'){const github=JSON.stringify(status);if(!$('#github-title')||content.dataset.github!==github){renderGitHubSettings();content.dataset.github=github}renderInfrastructure()}
 renderServerFeatures();
 renderTeamPermissions();
 restoreFormDrafts(drafts);
 for(const s of scrolls)if(sameSection&&s.node.isConnected){s.node.scrollLeft=s.left;s.node.scrollTop=s.top}for(const e of document.querySelectorAll('#content details[data-app-menu]'))e.open=sameSection&&expanded.includes(e.dataset.appMenu);if(sameSection)window.scrollTo(pageScroll[0],pageScroll[1]+(anchor?.isConnected?anchor.getBoundingClientRect().top-anchorTop:0));content.style.minHeight=minimum;renderedSnapshot=workspaceSnapshot();
}
function openForm(serverID){$('#app-form').reset();branchEdited=false;$('#repo-options').hidden=true;$('#create-branch-options').hidden=true;wireDatabaseFields($('#app-form'));$('#app-form').querySelectorAll('details').forEach(d=>d.open=false);envEditor('#create-env-editor',[]);$('#server-select').innerHTML=servers.map(s=>`<option value="${escape(s.id)}">${escape(s.name)}</option>`).join('');if(serverID)$('#server-select').value=serverID;$('#form-error').hidden=true;$('#app-dialog').showModal()}
$('#app-log-copy').onclick=()=>copyLogText($('#log-title').textContent+'\n\n'+$('#logs').textContent);$('#app-log-download').onclick=()=>downloadLogText('application.log',$('#log-title').textContent+'\n\n'+$('#logs').textContent);
$('#new-app').onclick=()=>openForm();$('#design').onchange=e=>{design=e.target.value;theme();render()};for(const b of document.querySelectorAll('[data-tab]'))b.onclick=e=>{if(e.ctrlKey||e.metaKey||e.shiftKey||e.altKey||e.button!==0)return;e.preventDefault();navigate(b.dataset.tab).catch(fail)};for(const b of document.querySelectorAll('[data-close]'))b.onclick=()=>b.closest('dialog').close();
$('#load-repos').onclick=async()=>{try{const repos=demo?[{full_name:'acme/private-portal',default_branch:'master'},{full_name:'acme/public-site',default_branch:'main'}]:await api('control/github/repos');repositoryInstallations=new Map(repos.map(r=>[r.full_name,r.installation_id||0]));repositoryBranches=new Map(repos.map(r=>[r.full_name,r.default_branch]));applyRepositoryBranch();$('#repo-options').innerHTML=repos.map(r=>`<button type="button" data-repository-choice="${escape(r.full_name)}">${escape(r.full_name)}</button>`).join('');$('#repo-options').hidden=false;notice(`${repos.length} repositories loaded. Type in the repository field to choose.`)}catch(e){$('#form-error').textContent=e.message;$('#form-error').hidden=false}};
$('#app-form').onsubmit=async e=>{e.preventDefault();const b=e.submitter;b.disabled=true;try{let value=Object.fromEntries(new FormData(e.target));value.github_installation=repositoryInstallations.get(value.repository)||0;value.database=takeDatabase(value);value.env=value.env.trim()?JSON.parse(value.env):{};value.env=readEnv('#create-env-editor',value.env,false);if(demo){if(apps.some(a=>a.id===value.id||a.domain===value.domain))throw Error('Application ID or domain already exists.');apps.push({...value,env:undefined,env_keys:Object.keys(value.env).sort(),auto_deploy:false})}else {const created=await api('control/apps','POST',value);if(created.warning){$('#app-dialog').close();await refresh();fail(Error(created.warning));return;}}$('#app-dialog').close();await navigate('apps');notice('Application created. If a database was selected, wait for it to be ready before deploying.')}catch(err){$('#form-error').textContent=err.message;$('#form-error').hidden=false}finally{b.disabled=false}};
$('#content').onclick=async e=>{if(e.target.closest('[data-add-first-app]')){openForm();return}if(e.target.closest('[data-clear-search]')){filter='';render();return}const errorsButton=e.target.closest('[data-errors]');if(errorsButton){openAppErrors(errorsButton.dataset.errors);return;}try{if(await featureAction(e))return}catch(err){fail(err);return}if(await serverAction(e))return;const s=e.target.closest('[data-server]');if(s){openForm(s.dataset.server);return}const b=e.target.closest('[data-action]');if(!b)return;const {action,id}=b.dataset;b.disabled=true;try{if(action==='remove'){if(prompt('This removes '+id+' and its containers, environment, and domain routing. Databases and backups are preserved; shared databases keep their schedules. Type the application ID to confirm:')!==id)return;if(demo){apps=apps.filter(a=>a.id!==id)}else await api('control/apps/'+id,'DELETE',{confirm:id});await refresh();notice('Application removed. Remove its unused webhook in GitHub.');return}if(action==='database'){await openDatabase(id);return}if(action==='settings'){await openSettings(id);return}if(action==='logs'){const data=demo?{logs:'[demo] Application started\n[demo] GET /readyz 200\n[demo] Release is serving requests'}:await api(`control/apps/${id}/logs`);$('#logs').textContent=data.logs;$('#log-title').textContent=id+' · logs';$('#log-dialog').showModal();return}if(action==='rollback'&&!confirm('Switch '+id+' to its previous healthy release?'))return;if(demo){if(action==='deploy'){const d={id:'demo-'+Date.now(),app_id:id,status:'building',created:new Date().toISOString()};deployments.push(d);setTimeout(()=>{d.status='live';d.commit='d3e091a';const app=apps.find(a=>a.id===id);app.previous=app.current;app.current={commit:d.commit};render();notice('Demo deployment is live.')},1200)}if(action==='rollback'){const app=apps.find(a=>a.id===id);if(!app.previous)throw Error('No previous demo release. Deploy this app first.');[app.current,app.previous]=[app.previous,app.current]}if(action==='webhook'||action==='disable-webhook')apps.find(a=>a.id===id).auto_deploy=action==='webhook';notice('Demo: '+action+' accepted.')}else{await api(`control/apps/${id}/${action==='disable-webhook'?'webhook':action}`,action==='disable-webhook'?'DELETE':'POST',{});notice(action==='deploy'?'Deployment queued.':action==='reload'?'Reload queued. Check Deployments for its result.':action+' complete.')}await refresh()}catch(err){fail(err)}finally{b.disabled=false}};

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
 $('#env-keys').textContent=value.env_keys?.join(', ')||'No variables saved';envEditor('#settings-env-editor',value.env_keys||[],[...Object.keys(value.database_bindings||{}),...(value.cache?.managed?['CACHE_URL']:[]),...(value.persistent_storage?['STORAGE_DIR']:[])]);
 const primary=value.database_bindings?.DATABASE_URL;$('#settings-database-status').textContent=primary?'Primary database: '+primary:value.env_keys?.includes('DATABASE_URL')?'DATABASE_URL is set manually. Attach a managed database to enable backups.':'No primary database attached. Apps using the Līdza DB pack require DATABASE_URL.';
 $('#settings-database').onclick=()=>openDatabase(id).catch(err=>dialogError('#settings-error',err));
 $('#settings-cache-status').textContent=cacheStatusText(value.cache);$('#settings-cache').hidden=!isAdmin();$('#settings-cache').onclick=()=>openCacheSettings(id).catch(err=>dialogError('#settings-error',err));
 $('#settings-title').textContent=id+' · settings'; $('#settings-error').hidden=true;
 $('#settings-dialog').showModal();loadBranchOptions(app.repository,app.github_installation||0,'settings').catch(()=>{});
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
document.addEventListener('invalid',e=>{let parent=e.target.closest('details');while(parent){parent.open=true;parent=parent.parentElement.closest('details')}},true);
$('#content').addEventListener('input',markFormDraft);$('#content').addEventListener('change',markFormDraft);
$('#menu-toggle').onclick=()=>{const open=$('#workspace-sidebar').classList.toggle('menu-open');$('#menu-toggle').setAttribute('aria-expanded',String(open))};
document.addEventListener('keydown',e=>{if(e.key==='Escape'){const mobileOpen=$('#workspace-sidebar').classList.contains('menu-open'),accountOpen=$('#account-menu').open;closeMobileNavigation();$('#account-menu').open=false;if(mobileOpen)$('#menu-toggle').focus();else if(accountOpen)$('#account-menu summary').focus()}});
document.addEventListener('click',e=>{if(!e.target.closest('#account-menu'))$('#account-menu').open=false});
$('#logout').onclick=async()=>{try{if(dirtyForms.size&&!confirm('Sign out and discard unsaved changes?'))return;if(!demo)await api('v1/auth/logout','POST',{});dirtyForms.clear();location.href='/login.html'}catch(e){fail(e)}};
theme();if(demo)initDemo();refresh().catch(fail);if(!demo)poll=setInterval(()=>{updateConnectionBadge();if(!dirtyForms.size&&!document.querySelector('dialog[open]')&&document.activeElement?.id!=='search'&&!document.activeElement?.closest('form'))refresh({background:true}).catch(fail)},5000);window.addEventListener('pagehide',()=>clearInterval(poll));

function renderGitHubSettings(){
 const registered=!!status.github_app_configured,connected=!!status.github_app_connected,localOnly=!demo&&status.github_public_https===false;
 $('#content').innerHTML=`<article class="panel integration-panel" aria-labelledby="github-title">
  <header class="integration-header"><div><span class="eyebrow">SOURCE CONTROL</span><h2 id="github-title">GitHub</h2></div><span class="integration-status">${localOnly?'Local mode':connected?'Connected':registered?'Repository selection needed':'Not connected'}</span></header>
  <p class="integration-description">Connect your repositories for private access and automatic deployments.</p>
  <p class="integration-guidance">${connected?'Repository access is connected. Deployment credentials renew automatically.':registered?'Your GitHub App is registered. Choose the repositories it can access.':'GitHub will guide you through creating an app for this installation and choosing its repositories. No credentials to copy.'}</p>
  ${status.github_app_slug?`<p class="integration-app">App: <strong>${escape(status.github_app_slug)}</strong></p>`:''}
  ${!registered&&!localOnly?'<div class="integration-account"><label for="github-organization">GitHub organization <span class="optional">Optional</span></label><input id="github-organization" autocomplete="off" placeholder="e.g. your-company" aria-describedby="github-account-help"><p id="github-account-help">Leave blank to use your personal GitHub account. Organizations may require administrator approval.</p></div>':''}
  ${localOnly?'<div class="integration-requirement" id="github-https-help"><strong>Public HTTPS is required to connect GitHub</strong><p>This installation uses localhost. Use a public HTTPS control panel to connect private repositories and receive deployment webhooks.</p><details><summary>Installation and hostname guidance</summary><p>Install with <code>--fqdn deploy.your-domain.com</code> on a public server with DNS and ports 80/443 ready. An existing panel needs a hostname migration; reinstalling under a different hostname is blocked.</p></details></div>':''}
  <div class="integration-actions"><button class="primary" id="connect" ${localOnly?'disabled aria-describedby="github-https-help"':''}>${registered?'Choose repositories':'Connect GitHub'}</button>${registered?'<button id="disconnect">Disconnect GitHub App</button>':''}</div>
  <p class="integration-note">Public repositories can deploy without a GitHub connection.</p>
 </article>`;
 $('#connect').onclick=async()=>{try{if(demo){notice('Preview: GitHub app registration and repository selection would open.');return}if(registered){const result=await api('control/github/app/install','POST',{});location.href=result.url}else{const result=await api('control/github/app/register','POST',{organization:$('#github-organization').value.trim()});const form=document.createElement('form');form.method='POST';form.action=result.action;const input=document.createElement('input');input.type='hidden';input.name='manifest';input.value=JSON.stringify(result.manifest);form.append(input);document.body.append(form);clearFormDraft('github-organization');form.submit()}}catch(err){fail(err)}};
 if($('#disconnect'))$('#disconnect').onclick=async()=>{try{if(!demo)await api('control/github/app','DELETE');await refresh();notice('Connection removed here. Uninstall the app in GitHub to revoke its access.')}catch(err){fail(err)}};
 if(params.has('github')){notice(({registered:'GitHub App registered. Choose repositories next.',connected:'GitHub connected. Your selected repositories are available.',pending:'GitHub approval is pending. Return to Choose repositories once approved.'})[params.get('github')]||'GitHub connection updated.');params.delete('github');const url=sectionURL('settings');url.searchParams.delete('github');history.replaceState(null,'',url)}
}

async function loadBranchOptions(repository,installation,prefix){
 const hint=$('#'+prefix+'-branch-hint'),options=$('#'+prefix+'-branch-options');options.innerHTML='';
 try{const branches=demo?[{name:'main'},{name:'master'},{name:'release'}]:await api('control/github/branches?repository='+encodeURIComponent(repository)+'&installation='+installation);options.innerHTML=branches.map(b=>`<button type="button" data-branch-choice="${escape(b.name)}">${escape(b.name)}</button>`).join('');options.hidden=false;hint.textContent=branches.length+' branches available. Choose a branch or enter its exact name.';}catch(e){hint.textContent=e.message;}
}
$('#load-branches').onclick=()=>{const repository=$('#app-form').elements.repository.value.trim();loadBranchOptions(repository,repositoryInstallations.get(repository)||0,'create');};
$('#app-form').elements.repository.addEventListener('change',()=>$('#load-branches').click());
$('#settings-load-branches').onclick=()=>{const app=apps.find(a=>a.id===editingApp);loadBranchOptions(app.repository,app.github_installation||0,'settings');};

for(const prefix of ['create','settings']){
 const form=$('#'+(prefix==='create'?'app':'settings')+'-form'),options=$('#'+prefix+'-branch-options');
 form.elements.branch.addEventListener('input',()=>{const query=form.elements.branch.value.toLowerCase();for(const button of options.querySelectorAll('button'))button.hidden=!button.dataset.branchChoice.toLowerCase().includes(query);});
 options.addEventListener('click',event=>{const button=event.target.closest('[data-branch-choice]');if(!button)return;form.elements.branch.value=button.dataset.branchChoice;form.elements.branch.dispatchEvent(new Event('input',{bubbles:true}));if(prefix==='create')branchEdited=true;options.hidden=true;form.elements.branch.focus();});
}

$('#app-form').elements.repository.addEventListener('input',event=>{const query=event.target.value.toLowerCase();for(const button of $('#repo-options').querySelectorAll('button'))button.hidden=!button.dataset.repositoryChoice.toLowerCase().includes(query);});
$('#repo-options').addEventListener('click',event=>{const button=event.target.closest('[data-repository-choice]');if(!button)return;const input=$('#app-form').elements.repository;input.value=button.dataset.repositoryChoice;input.dispatchEvent(new Event('input',{bubbles:true}));input.dispatchEvent(new Event('change',{bubbles:true}));$('#repo-options').hidden=true;input.focus();});
