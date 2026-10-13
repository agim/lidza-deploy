'use strict';
const $=s=>document.querySelector(s), params=new URLSearchParams(location.search), demo=params.get('demo')==='1';
let connectedControlVersion,initialWorkspaceReady=false;
const workspaceLoads=new Set();
function syncWorkspaceLoading(){const busy=!initialWorkspaceReady||workspaceLoads.size>0;$('#workspace-loading').hidden=!busy;$('.main-wrap main').setAttribute('aria-busy',String(busy))}
function beginWorkspaceLoad(label='Loading page…'){const token=Symbol();workspaceLoads.add(token);$('#workspace-loading-label').textContent=label;syncWorkspaceLoading();return()=>{workspaceLoads.delete(token);syncWorkspaceLoading()}}
const sectionTabs={'remote-backups':'remoteBackups',page:'detail','new-application':'newapp',defaults:'defaults',help:'help',updates:'updates',security:'security',applications:'apps',deployments:'deployments',servers:'servers',databases:'backups',integrations:'settings',errors:'errors',team:'team',audit:'audit'};
const tabSections=Object.fromEntries(Object.entries(sectionTabs).map(([section,value])=>[value,section]));
function tabFromURL(){if(location.hash==='#help/'){const url=new URL(location.href);url.hash='help';history.replaceState(null,'',url)}return sectionTabs[location.hash.slice(1).split('/')[0]]||'apps'}
let design=demo?(params.get('design')||'terminal'):'terminal', tab=location.hash?tabFromURL():params.has('github')?'settings':'apps';
function sectionURL(next){if(next==='detail')return new URL(detailAddress||location.href);const url=new URL(location.href);url.hash=tabSections[next];return url}
async function navigate(next){
 if(!tabSections[next])return;if(next!==tab&&dirtyForms.size&&!confirm('Leave this section and discard unsaved changes?'))return;
 const finish=next!==tab?beginWorkspaceLoad():()=>{};
 try{if(next!==tab){dirtyForms.clear();if(tab==='newapp')newAppPrepared=false;hideWorkspacePage()}closeMobileNavigation();const url=sectionURL(next);if(url.href!==location.href)history.pushState(null,'',url);tab=next;filter='';render();await refresh()}finally{finish()}
}
function restoreSection(){const next=tabFromURL();if(next===tab){if(tab==='detail'){if(detailLoaded!==location.hash&&dirtyForms.size){if(!confirm('Leave this page and discard unsaved changes?')){history.pushState(null,'',detailAddress);return;}dirtyForms.clear();}restoreWorkspacePage();if(workspacePage){$('#title').textContent=workspacePage.querySelector('h2')?.textContent||'Application workspace';$('#crumb').textContent=$('#title').textContent;$('#subtitle').textContent='';}}if(tab==='defaults')renderDefaults();
 if(tab==='help')renderHelp();return}for(const dialog of document.querySelectorAll('dialog[open]'))dialog.close();if(dirtyForms.size){if(!confirm('Leave this section and discard unsaved changes?')){history.pushState(null,'',sectionURL(tab));return}dirtyForms.clear()}if(tab==='newapp')newAppPrepared=false;hideWorkspacePage();closeMobileNavigation();const finish=beginWorkspaceLoad();try{tab=next;filter='';render();refresh().catch(fail).finally(finish)}catch(error){finish();fail(error)}}
window.addEventListener('popstate',restoreSection);
window.addEventListener('hashchange',restoreSection);
let repositoryInstallations=new Map(), repositoryBranches=new Map(), branchEdited=false;
function applyRepositoryBranch(){const form=$('#app-form');const branch=repositoryBranches.get(form.elements.repository.value.trim());if(branch&&!branchEdited)form.elements.branch.value=branch;}
$('#app-form').elements.branch.addEventListener('input',()=>branchEdited=true);
$('#app-form').elements.repository.addEventListener('input',applyRepositoryBranch);
let apps=[],servers=[],deployments=[],deliveries=[],status={},filter='',poll,unavailableServers=[],refreshing=false,refreshingDone;
const applicationSelections={deployments:'',backups:''};
const dirtyForms=new Set();let lastRefresh=0,refreshFailures=0;
function clearFormDraft(id){dirtyForms.delete(id);const form=document.getElementById(id);if(form?.tagName==='FORM')showFormState(form,'Saved','saved')}
function captureFormDrafts(){return [...dirtyForms].map(id=>{const node=document.getElementById(id);return {id,fields:node?[...node.querySelectorAll('input,select,textarea')].filter(e=>e.name).map(e=>({name:e.name,value:e.value,checked:e.checked})):[],value:node?.tagName==='INPUT'?node.value:undefined}})}
function restoreFormDrafts(drafts){for(const draft of drafts){const node=document.getElementById(draft.id);if(!node){dirtyForms.delete(draft.id);continue}if(draft.value!==undefined)node.value=draft.value;for(const field of draft.fields){const input=node.querySelector('[name="'+CSS.escape(field.name)+'"]');if(input){input.value=field.value;if(input.type==='checkbox'||input.type==='radio')input.checked=field.checked}}}}
function markFormDraft(event){if(!event.target.matches('input,select,textarea'))return;const form=event.target.closest('form');if(form?.getAttribute('id')){dirtyForms.add(form.getAttribute('id'));showFormState(form,'Unsaved changes','dirty')}else if(event.target.id==='github-organization')dirtyForms.add(event.target.id);updateConnectionBadge()}
function checkUIVersion(){
 const current=status.control_version;if(!current||current==='development')return;if(!connectedControlVersion)connectedControlVersion=current;
 let panel=$('#ui-update-notice');if(current===connectedControlVersion){if(panel)panel.hidden=true;return}
 if(!panel){panel=document.createElement('aside');panel.id='ui-update-notice';panel.className='notice';panel.setAttribute('role','status');const text=document.createElement('span'),button=document.createElement('button');button.type='button';button.textContent='Reload interface';button.onclick=()=>{if(dirtyForms.size&&!confirm('Reload the updated interface and discard unsaved changes?'))return;location.reload()};panel.append(text,button);document.body.append(panel)}
 panel.firstElementChild.textContent=`Control panel updated to ${current}. Reload to load the updated interface.`;panel.hidden=false;
}
function updateConnectionBadge(){if(demo)return;checkUIVersion();const seconds=lastRefresh?Math.floor((Date.now()-lastRefresh)/1000):0;$('#mode').textContent=refreshFailures?'Some data unavailable':!lastRefresh?'Connecting…':dirtyForms.size?'Unsaved changes · refresh paused':'Updated '+(seconds<5?'just now':seconds+'s ago');$('#mode').title=lastRefresh?'Last successful workspace refresh: '+new Date(lastRefresh).toLocaleString():'Waiting for the control panel';}
function closeMobileNavigation(){const toggle=$('#menu-toggle');$('#workspace-sidebar').classList.remove('menu-open');toggle.setAttribute('aria-expanded','false')}
function applicationEmptyState(){if(apps.length)return '<div class="empty"><h3>No matching applications.</h3><p>Try another application name or hostname.</p><button data-clear-search>Clear search</button></div>';if(servers.length)return '<div class="empty"><h3>Add your first application.</h3><p>Your server is configured. Choose a GitHub repository to get started.</p>'+(isAdmin()?'<button class="primary" data-add-first-app>＋ New application</button>':'<p>Ask an administrator to add an application.</p>')+'</div>';return '<div class="empty"><h3>Connect your first server.</h3><p>Install an agent on your hosting server, then connect it here.</p>'+(isAdmin()?'<button class="primary" data-manage-server="">Connect server</button>':'<p>Ask an administrator to connect a server.</p>')+'</div>'}
function deliveryTable(rows){return '<div class="table-wrap"><table><thead><tr><th>Job</th><th>State</th><th>Attempts</th><th>Last error</th></tr></thead><tbody>'+rows.map(d=>`<tr><td><code>${escape(d.ID.slice(0,12))}</code></td><td>${pill(d.State==='pending'?(d.Attempts?'retrying':'queued'):d.State)}</td><td>${d.Attempts} / ${d.MaxAttempts}</td><td>${escape(d.LastError||'—')}</td></tr>`).join('')+'</tbody></table></div>'}

const escape=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
function theme(){document.body.dataset.design=design;$('#design').value=design;$('#design').hidden=!demo;$('#design-link').hidden=!demo}
function showNotice(target,message){target.replaceChildren();const text=document.createElement('span');text.textContent=message;const close=document.createElement('button');close.type='button';close.className='notice-dismiss';close.textContent='Dismiss';close.setAttribute('aria-label','Dismiss notification');close.onclick=()=>{target.hidden=true};target.append(text,close);target.hidden=false}
let noticeTimer;
function notificationTarget(error=false){const dialog=document.querySelector('dialog[open]');if(!dialog)return error?$('#error'):$('#toast');let target=dialog.querySelector('[data-dialog-notice]');if(!target){target=document.createElement('div');target.dataset.dialogNotice='';dialog.prepend(target)}target.className=error?'error dialog-notice':'notice dialog-notice';target.setAttribute('role',error?'alert':'status');return target}
function notice(s){clearTimeout(noticeTimer);const target=notificationTarget();showNotice(target,s);const expire=()=>{noticeTimer=setTimeout(()=>target.hidden=true,8000)};target.onpointerenter=()=>clearTimeout(noticeTimer);target.onpointerleave=expire;target.onfocusin=()=>clearTimeout(noticeTimer);target.onfocusout=expire;expire()}
function fail(e,source='action'){if(source==='action')clearTimeout(noticeTimer);const target=source==='action'?notificationTarget(true):$('#error');if(source==='refresh'&&!target.hidden&&target.dataset.source==='action')return;target.onpointerenter=target.onpointerleave=target.onfocusin=target.onfocusout=null;target.dataset.source=source;showNotice(target,e.message||String(e))}
function byID(a,b){return a.id<b.id?-1:a.id>b.id?1:0}
function applicationChoiceIDs(extra=[]){return [...new Set([...apps.filter(a=>!a.retiring).map(a=>a.id),...extra].filter(Boolean))].sort()}
function selectedApplication(section,ids){if(!ids.includes(applicationSelections[section]))applicationSelections[section]=ids[0]||'';return applicationSelections[section]}
function applicationPickerMarkup(id){return `<label class="application-picker">Application<input id="${id}" type="search" list="${id}-options" autocomplete="off" placeholder="Search or choose an application" aria-describedby="${id}-hint"><datalist id="${id}-options"></datalist><small id="${id}-hint">Type to search, then choose an application.</small></label>`}
function syncApplicationPicker(id,ids,value,onSelect){
 const input=document.getElementById(id),list=document.getElementById(id+'-options');if(!input||!list)return;
 const labels=new Map(ids.map(appID=>{const app=apps.find(a=>a.id===appID);return [appID,app?.domain?appID+' · '+app.domain:appID]}));
 const options=ids.map(appID=>`<option value="${escape(appID)}" label="${escape(labels.get(appID))}"></option>`).join('');if(list.innerHTML!==options)list.innerHTML=options;
 if(document.activeElement!==input&&input.value!==value)input.value=value;
 const choose=()=>{const query=input.value.trim().toLowerCase(),exact=ids.find(appID=>appID.toLowerCase()===query),matches=ids.filter(appID=>labels.get(appID).toLowerCase().includes(query));const next=exact||(matches.length===1?matches[0]:'');if(next&&next!==value)onSelect(next);else if(!next&&document.activeElement!==input)input.value=value};
 input.oninput=()=>{const exact=ids.find(appID=>appID.toLowerCase()===input.value.trim().toLowerCase());if(exact&&exact!==value)onSelect(exact)};input.onchange=choose;input.onblur=choose;
}
async function api(path,method='GET',body){const finish=method==='GET'&&(!initialWorkspaceReady||workspaceLoads.size)?beginWorkspaceLoad(initialWorkspaceReady?'Loading page…':'Loading workspace…'):()=>{};try{const res=await fetch('/api/'+path,{method,headers:{'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body)});if(res.status===401){location.href='/login.html?next='+encodeURIComponent(location.pathname+location.search+location.hash);throw Error('Please sign in.')}let data;try{data=await res.json()}catch{data={}}if(!res.ok)throw Error(data.error?.message||data.error||'Request failed ('+res.status+')');return data}finally{finish()}}
function initDemo(){servers=[{id:'eu-1',name:'Frankfurt · production',url:'https://agent-eu.example.com'},{id:'us-1',name:'Virginia · staging',url:'https://agent-us.example.com'}];apps=[{id:'customer-portal',repository:'acme/customer-portal',domain:'portal.example.com',branch:'main',server_id:'eu-1',auto_deploy:true},{id:'content-studio',repository:'acme/content-studio',domain:'studio.example.com',branch:'main',server_id:'eu-1',auto_deploy:true},{id:'documentation',repository:'acme/docs',domain:'docs.example.com',branch:'main',server_id:'us-1',auto_deploy:false}];deployments=apps.map((a,i)=>({id:'demo-'+i,app_id:a.id,status:'live',commit:['a6b92fe','8f129bc','319fa27'][i],created:new Date(Date.now()-(i+1)*3600000).toISOString()}));apps.forEach(a=>a.current={commit:latest(a.id).commit});status={github_connected:true,github_configured:true};$('#demo-banner').hidden=false;$('#mode').textContent='Interactive preview'}
async function refresh({background=false}={}) {
 const requestedTab=tab;
 if(refreshing){if(background)return;await refreshingDone;if(requestedTab!==tab)return;return refresh({background})}
 if(background&&dirtyForms.size)return;
 refreshing=true;let complete;refreshingDone=new Promise(resolve=>complete=resolve);
 try {
 if (!demo) {
  if(tab==='settings'||tab==='backups')await loadInfrastructure();
  const results = await Promise.allSettled(['apps','servers','deployments','status','deliveries'].map(name=>api('control/'+name)));
  const current=[apps,servers,deployments,status,deliveries];
  [apps,servers,deployments,status,deliveries]=results.map((result,i)=>result.status==='fulfilled'?result.value:current[i]);
  apps=[...apps].sort(byID);servers=[...servers].sort(byID);
  if(results[2].status==='fulfilled'){unavailableServers=deployments.unavailable_servers||[];deployments=deployments.deployments||[];}
  const failures=results.filter(result=>result.status==='rejected');refreshFailures=failures.length+unavailableServers.length;
  if(failures.length)fail(Error('Some data could not be refreshed: '+failures.map(result=>result.reason.message).join('; ')),'refresh');
  else if(unavailableServers.length)fail(Error('Deployment history unavailable for: '+unavailableServers.join(', ')+'. Other servers remain available.'),'refresh');
  else if($('#error').dataset.source==='refresh')$('#error').hidden=true;
 }
 if(tab==='updates'&&!document.querySelector('dialog[open]'))await loadUpdates();
 if(!background||!dirtyForms.size){const snapshot=workspaceSnapshot();if((!background||snapshot!==renderedSnapshot)&&!(background&&document.querySelector('dialog[open]'))){render();renderedSnapshot=snapshot;}if(!refreshFailures)lastRefresh=Date.now()}if(!document.querySelector('dialog[open]')){if(tab==='servers')renderServerFeatures();if(tab==='errors'&&errorView.app&&!errorView.loading&&Date.now()-errorView.fetched>30000)loadAppErrors();if(tab==='security'&&securityApp&&!securityBusy&&Date.now()-securityFetched>30000)loadSecurityActivity()}updateConnectionBadge();
 }finally{refreshing=false;initialWorkspaceReady=true;complete();syncWorkspaceLoading()}
}
let renderedSnapshot='';
function latest(id){return deployments.filter(d=>d.app_id===id).sort((a,b)=>new Date(b.created)-new Date(a.created))[0]}
function pill(s){const symbol=({failed:'ⓧ',live:'✓',ready:'✓',queued:'◷',pending:'◷',retrying:'↻',building:'↻',running:'↻',done:'✓'})[s]||'○';return `<span class="pill ${s==='failed'?'failed':s==='live'?'':'neutral'}"><span aria-hidden="true">${symbol}</span> ${escape(s||'not deployed')}</span>`}
function workspaceSnapshot(){
 const permissions=[status.roles,status.github_app_connected];
 if(tab==='apps')return JSON.stringify([tab,apps,servers,apps.map(a=>{const d=latest(a.id);return [a.id,d?.id,d?.status]}),permissions]);
 if(tab==='deployments')return JSON.stringify([tab,deployments.map(({log,...d})=>d),deliveries,permissions]);
 if(tab==='servers')return JSON.stringify([tab,servers,apps.map(a=>[a.id,a.domain,a.server_id]),permissions]);
 if(tab==='backups')return JSON.stringify([tab,retainedResources,retainedResourceErrors,databases,servers,databaseServerErrors,permissions]);
 if(tab==='settings')return JSON.stringify([tab,status,infrastructure,permissions]);
 return JSON.stringify([tab,['errors','security'].includes(tab)?apps.map(a=>[a.id,a.retiring]):[],permissions]);
}
function domainRetryText(status){const seconds=status.recheck_seconds||60;return seconds===60?'Automatically rechecked about every minute.':seconds<60?'Automatically rechecked about every '+seconds+' seconds.':'Automatically rechecked about every '+Math.round(seconds/60)+' minutes.'}
function card(a){
 const d=latest(a.id),server=servers.find(s=>s.id===a.server_id);
 const state=a.retiring?'removal pending':a.stopped?(d?.kind==='start'&&['queued','building'].includes(d?.status)?'starting':'stopped'):a.agent_error||(['building','queued'].includes(d?.status)?d.status:d?.status==='failed'?'failed':a.current?'live':d?.status);
 return `<article class="app-card" data-app-card="${escape(a.id)}">
  <div class="card-body"><div class="card-head"><span class="app-icon">${escape(a.id.slice(0,2).toUpperCase())}</span><div class="app-identity"><h3>${escape(a.id)}</h3><span class="domain">${escape(a.domain)}</span>${a.domain_status?.message?`<div class="domain-status-message"><p>${escape(a.domain_status.message)}</p><small>${escape(domainRetryText(a.domain_status))}</small></div>`:''}</div>${pill(state)}</div>
   <div class="repo"><span>⑂</span>${escape(a.repository)} <b>· ${escape(a.branch)}</b> <code>${escape(a.current?.commit?.slice(0,7)||'')}</code></div>
   <div class="meta-row"><span>${escape(server?.name||a.server_id)}</span><span>${a.auto_deploy?(a.stopped?'Auto-deploy paused · app stopped':'↻ Auto-deploy enabled'):'Manual deploys'} · ${demo?'TLS demo':a.domain_status?({waiting_dns:'Waiting for DNS',requesting_ssl:'Requesting SSL',ready:'HTTPS ready'}[a.domain_status.state]||'Checking HTTPS'):'Auto TLS configured'}</span></div>
  </div>
  <div class="card-actions">
   <div class="app-primary-actions">${a.retiring?`<button data-action="remove" data-id="${escape(a.id)}">Retry removal</button>`:`<button class="deploy" data-action="deploy" data-id="${escape(a.id)}">Deploy ↗</button><button data-history="${escape(d?.id||'')}" ${d?'':'disabled'}>Build logs</button><button data-action="settings" data-id="${escape(a.id)}">Settings</button>`}</div>
   ${a.retiring?'':`<details class="app-more" data-app-menu="${escape(a.id)}"><summary>More actions</summary><div class="app-more-actions">
    <section class="app-action-group"><h4>Inspect</h4><button data-action="logs" data-id="${escape(a.id)}">Runtime logs</button><button data-security="${escape(a.id)}">Security activity</button><button data-errors="${escape(a.id)}">Errors</button></section>
    <section class="app-action-group"><h4>Resources</h4><button data-action="database" data-id="${escape(a.id)}">Database & backups</button><button data-tasks="${escape(a.id)}">Workers & jobs</button>${!a.preview_parent?`<button data-previews="${escape(a.id)}">PR previews</button>`:''}</section>
    <section class="app-action-group"><h4>Operations</h4><button data-app-state="${escape(a.id)}">${a.stopped?'Start application':'Stop application'}</button><button data-maintenance="${escape(a.id)}">${a.maintenance?.enabled?'End maintenance':'Maintenance'}</button><button data-action="reload" data-id="${escape(a.id)}">Reload</button><button data-action="rollback" data-id="${escape(a.id)}">Rollback</button><button data-action="${a.auto_deploy?'disable-webhook':'webhook'}" data-id="${escape(a.id)}">${a.auto_deploy?'Disable auto-deploy':'Auto-deploy'}</button>${a.auto_deploy&&a.previews?.enabled?`<button data-action="webhook" data-id="${escape(a.id)}">Refresh webhook</button>`:''}</section>
    <section class="app-action-group"><h4>Access & ownership</h4>${(status.roles||[]).some(r=>r==='admin'||r==='deployer')?`<button data-owner-claim="${escape(a.id)}">Operator setup token</button>`:''}${status.github_app_connected&&!a.github_installation&&!a.preview_parent?`<button data-action="github-app" data-id="${escape(a.id)}">Connect repository</button>`:''}<button class="app-remove" data-action="remove" data-id="${escape(a.id)}">Remove application</button></section>
   </div></details>`}
  </div>
 </article>`;
}
// These renderers retain application actions and release table scrollers across polls.
function renderApplications(){
 const content=$('#content');
 if(!content.querySelector('.cards')){content.innerHTML='<div class="toolbar"><h2>Your applications <span class="badge"></span></h2><label class="sr-only" for="search">Search applications</label><input id="search" class="search" placeholder="Search applications…"></div><div class="cards"></div>';$('#search').oninput=e=>{filter=e.target.value;render()}}
 content.querySelector('.toolbar .badge').textContent=apps.length;
 if($('#search').value!==filter)$('#search').value=filter;
 const list=content.querySelector('.cards'),visible=[...apps].sort(byID).filter(a=>(a.id+' '+a.domain).toLowerCase().includes(filter.toLowerCase())),retained=new Set();
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
  const history=node.querySelector('[data-history]'),d=latest(a.id);const stateButton=node.querySelector('[data-app-state]');if(stateButton){stateButton.textContent=a.stopped?'Start application':'Stop application';stateButton.dataset.stopped=d?.kind==='start'&&['queued','building'].includes(d?.status)?'1':'0';}if(history){history.dataset.history=d?.id||'';history.disabled=!d}
 for(const button of node.querySelectorAll('[data-action=deploy],[data-action=reload],[data-action=rollback]'))button.dataset.stopped=a.stopped?'1':'0';
  if(list.children[index]!==node)list.insertBefore(node,list.children[index]||null);retained.add(node);
 }
 for(const node of [...list.children])if(!retained.has(node))node.remove();
 if(!visible.length)list.innerHTML=applicationEmptyState();
}
function renderReleaseHistory(){
 const content=$('#content');
 if(!content.querySelector('#release-table'))content.innerHTML='<div class="toolbar fleet-filter-toolbar"><div><h2>Release history</h2><span class="badge"></span></div>'+applicationPickerMarkup('deployments-app-filter')+'</div><div class="table-wrap" id="release-table"><table><thead><tr><th>Application</th><th>Status</th><th>Commit</th><th>Started</th><th>Result</th></tr></thead><tbody></tbody></table></div>';
 const ids=applicationChoiceIDs(deployments.map(d=>d.app_id)),selected=selectedApplication('deployments',ids),visible=deployments.filter(d=>d.app_id===selected);
 syncApplicationPicker('deployments-app-filter',ids,selected,next=>{applicationSelections.deployments=next;renderReleaseHistory()});
 content.querySelector('.toolbar .badge').textContent=selected?visible.length+' release'+(visible.length===1?'':'s')+' for '+selected:'No applications';
 const body=content.querySelector('#release-table tbody'),retained=new Set();
 for(const [index,d] of [...visible].sort((a,b)=>new Date(b.created)-new Date(a.created)).entries()){
  let row=[...body.children].find(e=>e.dataset.release===d.id);
  if(!row){row=document.createElement('tr');row.dataset.release=d.id;row.innerHTML='<td data-label="Application"><strong></strong></td><td data-label="Status"></td><td data-label="Commit"><code></code></td><td data-label="Started"></td><td data-label="Result"><span></span> <button>Details & build logs</button></td>';row.querySelector('button').dataset.history=d.id;body.insertBefore(row,body.children[index]||null)}
  const cells=row.cells,values=[d.app_id,pill(d.status),d.commit?.slice(0,8)||'pending',new Date(d.created).toLocaleString(),Math.round(d.duration_seconds||0)+'s · '+(d.error||d.commit_message||'—')];
  for(let i=0;i<5;i++){const target=i===0?cells[i].querySelector('strong'):i===2?cells[i].querySelector('code'):i===4?cells[i].querySelector('span'):cells[i];if(i===1){if(target.innerHTML!==values[i])target.innerHTML=values[i]}else if(target.textContent!==values[i])target.textContent=values[i]}
  if(body.children[index]!==row)body.insertBefore(row,body.children[index]||null);retained.add(row);
 }
 for(const row of [...body.children])if(!retained.has(row))row.remove();
 if(!visible.length)body.innerHTML=`<tr><td colspan="5">${selected?'No deployments recorded for '+escape(selected)+'.':'No applications available.'}</td></tr>`;
 const related=deliveries.filter(d=>!d.AppID||d.AppID===selected),active=related.filter(d=>d.State!=='done'),completed=related.filter(d=>d.State==='done');
 for(const [kind,rows] of [['pending',active],['completed',completed]]){
  let panel=content.querySelector('[data-deliveries="'+kind+'"]');
  // Keep the same wrappers and completed disclosure even as deliveries change state.
  if(!panel){panel=document.createElement(kind==='pending'?'article':'details');panel.dataset.deliveries=kind;panel.className='panel '+(kind==='pending'?'delivery-attention':'completed-deliveries');panel.innerHTML=kind==='pending'?'<h2>Push deliveries needing attention</h2><p>Queued and retrying deliveries run when their hosting agent is available.</p>':'<summary></summary>';panel.insertAdjacentHTML('beforeend',deliveryTable([]));content.append(panel)}
  panel.hidden=!rows.length;if(kind==='completed')panel.querySelector('summary').textContent='Completed push deliveries ('+rows.length+')';
  const tableBody=panel.querySelector('tbody'),html=rows.map(d=>`<tr><td><code>${escape(d.ID.slice(0,12))}</code></td><td>${pill(d.State==='pending'?(d.Attempts?'retrying':'queued'):d.State)}</td><td>${d.Attempts} / ${d.MaxAttempts}</td><td>${escape(d.LastError||'—')}</td></tr>`).join('');if(tableBody.innerHTML!==html)tableBody.innerHTML=html;
 }
}
function renderServers(){
 const content=$('#content'),template=document.createElement('template');template.innerHTML=`<div class="toolbar"><h2>Connected hosts</h2><button data-manage-server="">＋ Connect server</button><span class="badge">${servers.length} configured</span></div><div class="server-grid" data-server-inventory>${servers.map(s=>{const hosted=apps.filter(a=>a.server_id===s.id),shown=hosted.slice(0,5);return `<article class="panel" data-server-card="${escape(s.id)}"><span class="server-icon">▤</span><h2>${escape(s.name)}</h2><p>${escape(s.url)}</p><div class="server-apps">${hosted.length} application${hosted.length===1?'':'s'} · dedicated agent</div><p>${shown.map(a=>escape(a.domain)).join('<br>')||'Ready for its first application.'}${hosted.length>shown.length?`<br><span class="hint">＋ ${hosted.length-shown.length} more applications</span>`:''}</p><div class="server-card-actions"><button data-server="${escape(s.id)}">Add application</button><button data-manage-server="${escape(s.id)}">Manage server</button></div></article>`}).join('')||'<div class="empty">No servers connected. Install an agent, then choose Connect server.</div>'}</div><div class="panel"><h2>One server, many domains</h2><p>Each application has a separate container and FQDN. Point DNS at the server; Caddy issues and renews HTTPS certificates for registered domains. Certificate issuance requires reachable ports 80 and 443.</p></div>`;
 if(!content.querySelector('[data-server-inventory]')){content.replaceChildren(template.content);return}
 content.querySelector('.toolbar .badge').textContent=servers.length+' configured';
 const grid=content.querySelector('[data-server-inventory]'),fresh=[...template.content.querySelector('[data-server-inventory]').children],keep=new Set();
 for(const [index,next] of fresh.entries()){
  let node=[...grid.children].find(e=>e.dataset.serverCard===next.dataset.serverCard);
  if(!node){node=next;grid.insertBefore(node,grid.children[index]||null)}else if(next.dataset.serverCard){
   for(const selector of ['h2','p','.server-apps','p:nth-of-type(2)']){const target=node.querySelector(selector),value=next.querySelector(selector);if(target.innerHTML!==value.innerHTML)target.innerHTML=value.innerHTML}
  }
  if(grid.children[index]!==node)grid.insertBefore(node,grid.children[index]||null);keep.add(node);
 }
 for(const node of [...grid.children])if(!keep.has(node))node.remove();
}
function render(){
 const sameSection=document.body.dataset.section===tab;const drafts=captureFormDrafts();const scrolls=[...document.querySelectorAll('#content .table-wrap')].map(e=>({node:e,left:e.scrollLeft,top:e.scrollTop}));const anchor=sameSection?[...document.querySelectorAll('#content [data-app-card],#content [data-release],#content [data-database-card],#content [data-backup],#content [data-server-card]')].find(e=>{const r=e.getBoundingClientRect();return r.top>=0&&r.top<innerHeight}):null;const anchorTop=anchor?.getBoundingClientRect().top;const pageScroll=[window.scrollX,window.scrollY];const expanded=[...document.querySelectorAll('#content details[open][data-app-menu]')].map(e=>e.dataset.appMenu);document.body.dataset.section=tab;
 for(const b of document.querySelectorAll('[data-tab]')){b.classList.toggle('active',b.dataset.tab===tab);if(b.dataset.tab===tab)b.setAttribute('aria-current','page');else b.removeAttribute('aria-current')}
 const labels={remoteBackups:['Remote backups','Verify the backup objects actually stored in your private bucket.'],detail:['Application workspace','Manage configuration and inspect logs.'],newapp:['New application','Choose the source, hosting and runtime settings before your first deployment.'],defaults:['Application defaults','Shared settings for an app’s first deployment.'],help:['Help','Practical guides for deploying and operating your apps.'],updates:['Updates','Keep the control panel and hosting agents current.'],security:['Security activity','Review suspicious traffic across hosted applications.'],errors:['Errors','Understand failures in your applications.'],team:['Team','Give your team the access they need.'],audit:['Audit log','Who changed what, and when.'],apps:['Applications','A clear view of everything you’re building.'],deployments:['Deployments','Every release, from queued to live.'],servers:['Servers','A home for each app. A view across every host.'],backups:['Databases & backups','Protect application data across your servers.'],settings:['Integrations','Connect your repositories and automate your releases.']};
 $('#kicker').textContent=({apps:'APPLICATIONS',deployments:'RELEASE HISTORY',servers:'INFRASTRUCTURE',backups:'DATA PROTECTION',remoteBackups:'OFF-SITE BACKUPS',settings:'WORKSPACE CONFIGURATION',newapp:'CREATE APPLICATION'})[tab]||'WORKSPACE';$('#mobile-section').textContent=labels[tab][0];$('#new-app').disabled=!servers.length;$('#new-app').title=servers.length?'':'Connect a server before creating an application';$('#new-app').classList.toggle('primary',tab!=='settings');$('#title').textContent=labels[tab][0]+'.';$('#crumb').textContent=labels[tab][0];$('#subtitle').textContent=labels[tab][1];$('#app-count').textContent=apps.length;$('#stat-apps').textContent=apps.length;$('#stat-releases').textContent=deployments.filter(d=>d.status==='live').length;$('#stat-servers').textContent=servers.length;
 $('#new-app-page').hidden=tab!=='newapp';$('#content').hidden=tab==='newapp'||tab==='detail';$('.stats').hidden=!['apps','deployments','servers'].includes(tab);$('#new-app').hidden=!['apps','deployments'].includes(tab);if(tab==='detail'){restoreWorkspacePage();if(workspacePage){$('#title').textContent=workspacePage.querySelector('h2')?.textContent||'Application workspace';$('#crumb').textContent=$('#title').textContent;$('#subtitle').textContent='';}}if(tab==='detail')decorateWorkspacePage();if(tab==='newapp')prepareNewAppPage();
 const content=$('#content'),minimum=content.style.minHeight;if(sameSection)content.style.minHeight=content.offsetHeight+'px';
 if(tab==='apps')renderApplications();
 if(tab==='defaults')renderDefaults();
 if(tab==='help')renderHelp();
 if(tab==='updates'){renderUpdates();loadUpdates().catch(fail)}
 if(tab==='backups')renderBackups();if(tab==='remoteBackups')renderRemoteBackups();
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
let newAppPrepared=false;
function prepareNewAppPage(){if(!newAppPrepared){$('#app-form').reset();$('#app-form .form-state')?.remove();branchEdited=false;$('#repo-options').hidden=true;$('#create-branch-options').hidden=true;wireDatabaseFields($('#app-form'));envEditor('#create-env-editor',[]);$('#form-error').hidden=true;newAppPrepared=true;}const select=$('#server-select'),value=select.value;const options=servers.map(s=>`<option value="${escape(s.id)}">${escape(s.name)}</option>`).join('');if(select.innerHTML!==options){select.innerHTML=options;if(servers.some(s=>s.id===value))select.value=value;}const auto=$('#app-form [name=auto_deploy]'),available=demo||status.github_app_connected&&status.github_public_https!==false;auto.disabled=!available;$('#create-auto-hint').textContent=available?'Pushes to the selected branch deploy this app. Creating the app does not start its first deployment.':'Connect GitHub in Integrations and authorize this repository to enable automatic deployments. A public HTTPS control panel is required.';$('#app-form button[type=submit]').disabled=!servers.length||!isAdmin();}
function openForm(serverID){if(tab==='newapp')return;newAppPrepared=false;navigate('newapp').then(()=>{if(serverID)$('#server-select').value=serverID;window.scrollTo(0,0)}).catch(fail)}
$('#cancel-new-app').onclick=()=>navigate('apps').catch(fail);
$('#app-form').addEventListener('input',markFormDraft);$('#app-form').addEventListener('change',markFormDraft);

$('#app-log-copy').onclick=()=>copyLogText($('#log-title').textContent+'\n\n'+$('#logs').textContent);$('#app-log-download').onclick=()=>downloadLogText('application.log',$('#log-title').textContent+'\n\n'+$('#logs').textContent);
$('#new-app').onclick=()=>openForm();$('#design').onchange=e=>{design=e.target.value;theme();render()};for(const b of document.querySelectorAll('[data-tab]'))b.onclick=e=>{if(e.ctrlKey||e.metaKey||e.shiftKey||e.altKey||e.button!==0)return;e.preventDefault();navigate(b.dataset.tab).catch(fail)};for(const b of document.querySelectorAll('[data-close]'))b.onclick=()=>closePageFromButton(b);
$('#load-repos').onclick=async()=>{const button=$('#load-repos');button.disabled=true;try{const repos=demo?[{full_name:'acme/private-portal',default_branch:'master',private:true},{full_name:'acme/public-site',default_branch:'main',private:false}]:await api('control/github/repos');repos.sort((a,b)=>a.full_name.localeCompare(b.full_name));repositoryInstallations=new Map(repos.map(r=>[r.full_name,r.installation_id||0]));repositoryBranches=new Map(repos.map(r=>[r.full_name,r.default_branch]));applyRepositoryBranch();$('#repository-list').innerHTML=repos.map(r=>`<button type="button" class="repository-choice" data-repository-choice="${escape(r.full_name)}"><span class="repository-name">${escape(r.full_name)}</span><span class="repository-visibility">${r.private?'Private':'Public'}</span><span class="repository-branch">${escape(r.default_branch||'')}</span></button>`).join('');$('#repository-search').value='';$('#repo-options').hidden=false;filterRepositories();$('#repository-search').focus({preventScroll:true})}catch(e){$('#form-error').textContent=e.message;$('#form-error').hidden=false}finally{button.disabled=false}};
$('#app-form').onsubmit=async e=>{e.preventDefault();const b=e.submitter;b.disabled=true;try{let value=Object.fromEntries(new FormData(e.target));value.customer_hosts=e.target.elements.customer_hosts.checked;value.auto_deploy=!e.target.elements.auto_deploy.disabled&&e.target.elements.auto_deploy.checked;value.github_installation=repositoryInstallations.get(value.repository)||0;value.database=takeDatabase(value);value.env=value.env.trim()?JSON.parse(value.env):{};value.env=readEnv('#create-env-editor',value.env,false);if(demo){if(apps.some(a=>a.id===value.id||a.domain===value.domain))throw Error('Application ID or domain already exists.');apps.push({...value,env:undefined,env_keys:Object.keys(value.env).sort(),auto_deploy:value.auto_deploy})}else {const created=await api('control/apps','POST',value);if(created.warning){clearFormDraft('app-form');newAppPrepared=false;await navigate('apps');fail(Error(created.warning));return;}}clearFormDraft('app-form');newAppPrepared=false;await navigate('apps');notice('Application created. If a database was selected, wait for it to be ready before deploying.')}catch(err){$('#form-error').textContent=err.message;$('#form-error').hidden=false;$('#form-error').scrollIntoView({block:'nearest'})}finally{b.disabled=false}};
$('#content').onclick=async e=>{if(e.target.closest('[data-add-first-app]')){openForm();return}if(e.target.closest('[data-clear-search]')){filter='';render();return}const errorsButton=e.target.closest('[data-errors]');if(errorsButton){openAppErrors(errorsButton.dataset.errors);return;}try{if(await featureAction(e))return}catch(err){fail(err);return}if(await serverAction(e))return;const s=e.target.closest('[data-server]');if(s){openForm(s.dataset.server);return}const b=e.target.closest('[data-action]');if(!b)return;const {action,id}=b.dataset;b.disabled=true;try{if(action==='remove'){if(prompt('This removes '+id+' and its containers, environment, and domain routing. Files, cache, databases and backups are preserved separately. Recreating this ID starts fresh; shared databases keep their schedules. Type the application ID to confirm:')!==id)return;if(demo){apps=apps.filter(a=>a.id!==id)}else await api('control/apps/'+id,'DELETE',{confirm:id});await refresh();notice('Application removed. Its ID can be reused; retained data is listed in Databases & backups.');return}if(action==='database'){await openDatabase(id);return}if(action==='settings'){await openSettings(id);return}if(action==='logs'){await openRuntimeLogs(id);return}if(action==='rollback'&&!confirm('Switch '+id+' to its previous healthy release?'))return;if(demo){if(action==='deploy'){const d={id:'demo-'+Date.now(),app_id:id,status:'building',created:new Date().toISOString()};deployments.push(d);setTimeout(()=>{d.status='live';d.commit='d3e091a';const app=apps.find(a=>a.id===id);app.previous=app.current;app.current={commit:d.commit};render();notice('Demo deployment is live.')},1200)}if(action==='rollback'){const app=apps.find(a=>a.id===id);if(!app.previous)throw Error('No previous demo release. Deploy this app first.');[app.current,app.previous]=[app.previous,app.current]}if(action==='webhook'||action==='disable-webhook')apps.find(a=>a.id===id).auto_deploy=action==='webhook';notice('Demo: '+action+' accepted.')}else{await api(`control/apps/${id}/${action==='disable-webhook'?'webhook':action}`,action==='disable-webhook'?'DELETE':'POST',{});notice(action==='deploy'?'Deployment queued.':action==='reload'?'Reload queued. Check Deployments for its result.':action+' complete.')}await refresh()}catch(err){fail(err)}finally{b.disabled=false}};

let editingServer = null, editingApp = null;
function dialogError(id, error) { $(id).textContent = error.message; $(id).hidden = false; }
function openServer(id) {
 const server = servers.find(s => s.id === id);
 editingServer = server?.id || null;
 const form = $('#server-form'); form.reset(); form.querySelector('.form-state')?.remove();
 for (const name of ['id', 'name', 'url']) form.elements[name].value = server?.[name] || '';
 form.elements.id.readOnly = !!server;
 form.elements.token.required = !server;
 $('#server-title').textContent = server ? server.name+' · Connection' : 'Connect server';
 updateServerRemoval(server);
 $('#server-error').hidden = true; showWorkspacePage($('#server-dialog'),'server',id||'new'); renderTeamPermissions();
}
function updateServerRemoval(server){
 const section=$('#server-removal');section.hidden=!server||!isAdmin();
 if(!server)return;
 const count=apps.filter(a=>a.server_id===server.id).length,button=section.querySelector('[data-remove-server]');
 button.dataset.removeServer=server.id;button.disabled=!!count;
 $('#server-removal-hint').textContent=count?count+' registered application'+(count===1?'':'s')+' must be removed before disconnecting this server.':'Removes this connection from the control panel. Its agent, containers and retained data stay on the host. Reconnect the same agent to manage them again.';
}
$('#server-removal').onclick=event=>serverAction(event);
async function serverAction(event) {
 const edit = event.target.closest('[data-manage-server]');
 if (edit) { openServer(edit.dataset.manageServer); return true; }
 const remove = event.target.closest('[data-remove-server]');
 if (!remove) return false;
 if(remove.disabled||!isAdmin())return true;
 const id = remove.dataset.removeServer;
 if (!confirm('Remove '+id+' from the control panel? This does not uninstall its agent.')) return true;
 remove.disabled=true;
 try {
  if (demo) servers = servers.filter(s => s.id !== id);
  else await api('control/servers/'+id, 'DELETE');
  dirtyForms.delete('server-form');hideWorkspacePage();await navigate('servers');notice('Server disconnected.');
 } catch (error) { fail(error);updateServerRemoval(servers.find(s=>s.id===id)); }
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
 editingApp=id; const form=$('#settings-form'); form.reset();form.querySelector('.form-state')?.remove();form.querySelectorAll('details').forEach(d=>d.open=false);
 form.elements.customer_hosts.checked=!!value.customer_hosts;form.elements.backup_before_deploy.checked=value.backup_before_deploy!==false;form.elements.branch.value=value.branch; form.elements.domain.value=value.domain;
 $('#env-keys').textContent=value.env_keys?.join(', ')||'No variables saved';envEditor('#settings-env-editor',value.env_keys||[],[...Object.keys(value.database_bindings||{}),...(value.cache?.managed?['CACHE_URL']:[]),...(value.persistent_storage?['STORAGE_DIR']:[])],value.env_sources||{});
 $('#settings-defaults').hidden=!isAdmin();$('#settings-defaults').onclick=()=>reviewDefaults(id,value).catch(err=>dialogError('#settings-error',err));
 const primary=value.database_bindings?.DATABASE_URL;$('#settings-database-status').textContent=primary?'Primary database: '+primary:value.env_keys?.includes('DATABASE_URL')?'DATABASE_URL is set manually. Attach a managed database to enable backups.':'No primary database attached. Apps using the Līdza DB pack require DATABASE_URL.';
 $('#settings-database').onclick=()=>openDatabase(id).catch(err=>dialogError('#settings-error',err));
 $('#settings-cache-status').textContent=cacheStatusText(value.cache);$('#settings-cache').hidden=!isAdmin();$('#settings-cache').onclick=()=>openCacheSettings(id).catch(err=>dialogError('#settings-error',err));
 const storageOverride=value.storage_provider_override??value.env_keys?.includes('STORAGE_PROVIDER');$('#settings-storage').hidden=!value.persistent_storage;$('#settings-storage-status').textContent=storageOverride?'A deployment environment override controls the storage provider and locks the app’s Admin → Storage choice. Older agents added local automatically; explicit operator choices are preserved.':'Deploy does not set a storage provider. A persistent volume is available for local files.';
 const unpin=$('#settings-storage-unpin');unpin.hidden=!storageOverride||!canDeploy();unpin.disabled=!value.app_managed_storage_supported;if(storageOverride&&!value.app_managed_storage_supported)$('#settings-storage-status').textContent+=' Update this hosting agent before removing the override.';
 unpin.onclick=async()=>{if(dirtyForms.has('settings-form')){dialogError('#settings-error',Error('Save or discard other settings changes before changing storage management.'));return}unpin.disabled=true;try{if(!demo)await api('control/apps/'+id+'/settings','PATCH',{branch:value.branch,domain:value.domain,env_changes:{STORAGE_PROVIDER:null}});if(editingApp===id&&workspacePage===$('#settings-dialog')){await openSettings(id);notice('Storage override removed. Local files are preserved. '+(app.current&&!value.stopped?'Follow Deployments for the reload result.':'It applies when the app is next started or deployed.'));}}catch(error){dialogError('#settings-error',error)}finally{if(unpin.isConnected)unpin.disabled=!value.app_managed_storage_supported}};
 $('#settings-title').textContent=id+' · settings'; $('#settings-error').hidden=true;
 showWorkspacePage($('#settings-dialog'),'settings',id);loadBranchOptions(app.repository,app.github_installation||0,'settings').catch(()=>{});
}
$('#settings-form').onsubmit = async event => {
 event.preventDefault(); const button=event.submitter;button.disabled=true;
 try {
  const value=Object.fromEntries(new FormData(event.target));
  value.customer_hosts=event.target.elements.customer_hosts.checked;value.backup_before_deploy=event.target.elements.backup_before_deploy.checked;value.env_changes=value.env_changes.trim()?JSON.parse(value.env_changes):{};
  if (!value.env_changes || Array.isArray(value.env_changes) || typeof value.env_changes!=='object' || Object.values(value.env_changes).some(v=>v!==null&&typeof v!=='string')) throw Error('Environment changes must be a JSON object containing strings or null.');
  value.env_changes=readEnv('#settings-env-editor',value.env_changes,true);
  if (demo) {
   if(apps.some(a=>a.id!==editingApp&&a.domain===value.domain))throw Error('Domain already registered.');
   const app=apps.find(a=>a.id===editingApp),keys=new Set(app.env_keys||[]);
   for(const [key,v] of Object.entries(value.env_changes)) { if(v===null)keys.delete(key);else keys.add(key); }
   Object.assign(app,{customer_hosts:value.customer_hosts,backup_before_deploy:value.backup_before_deploy,branch:value.branch,domain:value.domain,env_keys:[...keys].sort()});
  } else await api('control/apps/'+editingApp+'/settings','PATCH',value);
  event.target.reset();$('#settings-dialog').close();await refresh();notice('Settings saved. Running apps reload automatically; stopped apps apply changes when started. check Deployments for the result. Branch changes apply to the next Git deployment.');
 } catch(error) { dialogError('#settings-error',error); }
 finally {button.disabled=false;}
};
for(const id of ['server-dialog','settings-dialog'])$("#"+id).addEventListener('close',()=>$("#"+id+' form').reset());
const settingsDefaultsButton=document.querySelector('#settings-defaults');
function envEditor(selector,keys,managed=[],origins={}) {
 const host=$(selector),defaultsButton=selector==='#settings-env-editor'?($('#settings-defaults')||settingsDefaultsButton):null;host.replaceChildren();
 const rows=document.createElement('div');host.append(rows);
 function add(key='') {
  const row=document.createElement('div');row.className='env-row';row.dataset.saved=key?'1':'0';
  const nameLabel=document.createElement('label');nameLabel.textContent='Name';const name=document.createElement('input');name.className='env-name';name.value=key;name.readOnly=!!key;name.autocomplete='off';name.spellcheck=false;name.placeholder='DATABASE_URL';nameLabel.append(name);if(key){const source=document.createElement('small');source.textContent=origins[key]==='workspace'?'Workspace default':origins[key]==='built-in'?'Built-in default':'App setting';nameLabel.append(source)}
  const valueLabel=document.createElement('label');valueLabel.textContent='Value';const value=document.createElement('input');value.className='env-value';value.type='password';value.autocomplete='new-password';value.spellcheck=false;value.placeholder=key?'Saved value stays unchanged':'Enter value';valueLabel.append(value);
  const actionLabel=document.createElement('label');actionLabel.textContent='Action';const action=document.createElement('select');action.className='env-action';
  for(const [v,text] of (key?[['keep','Keep saved value'],['set','Replace value'],['delete','Delete variable']]:[['set','Set value'],['discard','Discard row']])){const o=document.createElement('option');o.value=v;o.textContent=text;action.append(o)}
  value.oninput=()=>{action.value='set'};action.onchange=()=>{value.disabled=action.value!=='set';if(value.disabled)value.value=''};actionLabel.append(action);row.append(nameLabel,valueLabel,actionLabel);rows.append(row);
 }
 keys.forEach(add);if(!keys.length){const empty=document.createElement('p');empty.className='hint env-empty';empty.textContent='No environment variables saved yet. Add an app-specific value or copy workspace defaults. Saved values remain private.';rows.append(empty)}
 for(const row of rows.querySelectorAll('.env-row')){if(managed.includes(row.querySelector(".env-name").value)){row.querySelector(".env-value").disabled=true;row.querySelector(".env-value").placeholder="Managed in Database & backups";row.querySelector(".env-action").disabled=true;}}const button=document.createElement('button');button.type='button';button.textContent='＋ Add variable';button.onclick=()=>{rows.querySelector('.env-empty')?.remove();add()};const actions=document.createElement('div');actions.className='env-editor-actions';if(defaultsButton)actions.append(defaultsButton);actions.append(button);host.prepend(actions);
}
function readEnv(selector,base,allowDelete) {
 if(!base||Array.isArray(base)||typeof base!=='object'||Object.values(base).some(v=>typeof v!=='string'&&!(allowDelete&&v===null)))throw Error('Environment must contain string values'+(allowDelete?' or null to delete.':'.'));
 const changes=Object.assign(Object.create(null),base),seen=new Set();
 for(const row of $(selector).querySelectorAll('.env-row')){
  const action=row.querySelector('.env-action').value,key=row.querySelector('.env-name').value;
  if(action==='discard')continue;
  if(!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key))throw Error('Enter a valid variable name.');
  if(seen.has(key))throw Error('Each variable name must be unique.');seen.add(key);
  if(action==='keep'&&!row.querySelector('.env-value').value)continue;
  if(Object.hasOwn(changes,key))throw Error('Variable '+key+' also appears in the JSON changes.');
  changes[key]=action==='delete'?null:row.querySelector('.env-value').value;
 }
 return changes;
}
$('#settings-dialog').addEventListener('close',()=>$('#settings-env-editor').replaceChildren());
$('#create-database-fields').innerHTML=databaseFields();
$('#database-dialog').addEventListener('close',()=>$('#database-content').replaceChildren());
document.addEventListener('invalid',e=>{let parent=e.target.closest('details');while(parent){parent.open=true;parent=parent.parentElement.closest('details')}},true);
$('#content').addEventListener('input',markFormDraft);$('#content').addEventListener('change',markFormDraft);
$('#menu-toggle').onclick=()=>{const open=$('#workspace-sidebar').classList.toggle('menu-open');$('#menu-toggle').setAttribute('aria-expanded',String(open))};
document.addEventListener('keydown',e=>{if(e.key==='Escape'){const mobileOpen=$('#workspace-sidebar').classList.contains('menu-open'),accountOpen=$('#account-menu').open;closeMobileNavigation();$('#account-menu').open=false;if(mobileOpen)$('#menu-toggle').focus();else if(accountOpen)$('#account-menu summary').focus()}});
document.addEventListener('click',e=>{if(!e.target.closest('#account-menu'))$('#account-menu').open=false});
$('#logout').onclick=async()=>{try{if(dirtyForms.size&&!confirm('Sign out and discard unsaved changes?'))return;if(!demo)await api('v1/auth/logout','POST',{});dirtyForms.clear();location.href='/login.html'}catch(e){fail(e)}};
groupWorkspaceNavigation();theme();if(demo)initDemo();refresh().catch(fail);if(!demo)poll=setInterval(()=>{updateConnectionBadge();if(tab!=='detail'&&!dirtyForms.size&&!document.querySelector('dialog[open]')&&document.activeElement?.id!=='search'&&!document.activeElement?.closest('form'))refresh({background:true}).catch(fail)},5000);window.addEventListener('pagehide',()=>clearInterval(poll));

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

function filterRepositories(){const query=$('#repository-search').value.trim().toLowerCase(),buttons=[...$('#repository-list').querySelectorAll('[data-repository-choice]')];let count=0;for(const button of buttons){button.hidden=!button.dataset.repositoryChoice.toLowerCase().includes(query);if(!button.hidden)count++;}$('#repository-count').textContent=`${count} of ${buttons.length} repositories`;$('#repository-empty').hidden=count>0;}
$('#repository-search').addEventListener('input',filterRepositories);
$('#repo-options').addEventListener('click',event=>{const button=event.target.closest('[data-repository-choice]');if(!button)return;const input=$('#app-form').elements.repository;input.value=button.dataset.repositoryChoice;input.dispatchEvent(new Event('input',{bubbles:true}));input.dispatchEvent(new Event('change',{bubbles:true}));$('#repo-options').hidden=true;input.focus();});

$('#settings-form').addEventListener('input',markFormDraft);
$('#settings-form').addEventListener('change',markFormDraft);
$('#settings-dialog').addEventListener('close',()=>clearFormDraft('settings-form'));

function showFormState(form,text,state){let node=form.querySelector(':scope > .form-state');if(!node){node=document.createElement('p');node.className='form-state';node.setAttribute('role','status');form.prepend(node)}node.textContent=text;node.dataset.state=state}
document.addEventListener('input',markFormDraft);document.addEventListener('change',markFormDraft);

function groupWorkspaceNavigation(){const nav=document.querySelector('#workspace-navigation');for(const [name,tabs] of [['APPLICATIONS',['apps','deployments','errors','security']],['INFRASTRUCTURE',['servers','backups','remoteBackups','updates']],['WORKSPACE',['settings','defaults','team','audit','help']]]){const title=document.createElement('span');title.className='nav-group-title';title.textContent=name;nav.append(title);for(const tabName of tabs){const link=nav.querySelector(`[data-tab="${tabName}"]`);if(link)nav.append(link)}}}
