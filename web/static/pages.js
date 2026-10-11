'use strict';
let workspacePage=null,detailAddress='',detailLoaded='',detailLoading=false,detailReturn='#applications';
const detailParents=new Map();
function hideWorkspacePage(){if(!workspacePage)return;const page=workspacePage;workspacePage=null;page.hidden=true;page.open=false;page.dispatchEvent(new Event('close'));detailLoaded='';}
function showWorkspacePage(page,kind,id){
 const url=new URL(location.href);url.hash='page/'+kind+'/'+encodeURIComponent(id);
 if(workspacePage&&workspacePage!==page)hideWorkspacePage();
 if(location.href!==url.href)detailParents.set(url.hash,location.hash||'#applications');detailReturn=detailParents.get(url.hash)||'#applications';
 detailAddress=url.href;detailLoaded=url.hash;
 if(location.href!==url.href)history.pushState(null,'',url);
 workspacePage=page;page.hidden=false;page.open=true;page.close=()=>{dirtyForms.clear();hideWorkspacePage();const back=new URL(location.href);back.hash=detailParents.get(url.hash)||'#applications';history.pushState(null,'',back);tab=tabFromURL();render()};
 tab='detail';document.body.dataset.section='detail';$('#content').hidden=true;$('#new-app-page').hidden=true;$('.stats').hidden=true;$('#new-app').hidden=true;
 $('#title').textContent=page.querySelector('h2')?.textContent||'Application workspace';$('#crumb').textContent=$('#title').textContent;$('#subtitle').textContent='';$('#mobile-section').textContent=$('#title').textContent;
 for(const link of document.querySelectorAll('[data-tab]')){link.classList.remove('active');link.removeAttribute('aria-current')}
 closeMobileNavigation();window.scrollTo(0,0);return page;
}
async function restoreWorkspacePage(){
 if(detailLoading||detailLoaded===location.hash)return;
 const [prefix,kind,encoded]=location.hash.slice(1).split('/');if(prefix!=='page')return;
 let id;try{id=decodeURIComponent(encoded||'')}catch{fail(Error('Invalid page address.'));return}
 const routes={upgrade:openUpgrade,restore:value=>{const config=JSON.parse(value),button=document.createElement('button');button.dataset.restore=config.backup;button.dataset.source=config.source;button.dataset.server=config.server;return featureAction({target:button})},'error-detail':async value=>{const {app,key}=JSON.parse(value);resetErrorApp(app);await loadAppErrors();showErrorDetail(key)},settings:openSettings,'build-logs':openDeploymentHistory,'runtime-logs':openRuntimeLogs,database:openDatabase,tasks:openTasks,previews:openPreviews,cache:openCacheSettings,ssh:openSSHAccess,server:id=>openServer(id==='new'?undefined:id),defaults:async id=>reviewDefaults(id,demo?apps.find(a=>a.id===id):await api('control/apps/'+id+'/settings'))};
 if(!routes[kind]){fail(Error('Unknown workspace page.'));return}
 detailLoading=true;detailAddress=location.href;
 try{if(workspacePage)hideWorkspacePage();await routes[kind](id)}catch(error){fail(error)}finally{detailLoading=false}
}
async function openRuntimeLogs(id){const data=demo?{logs:'[demo] Application started\n[demo] GET /readyz 200\n[demo] Release is serving requests'}:await api(`control/apps/${id}/logs`);$('#logs').textContent=data.logs;$('#log-title').textContent=id+' · Runtime logs';showWorkspacePage($('#log-dialog'),'runtime-logs',id)}
function featurePage(title,html,kind,id){let page=$('#feature-page');if(!page){page=document.createElement('section');page.id='feature-page';page.className='workspace-page';$('main').insertBefore(page,$('main footer'))}if(workspacePage===page)hideWorkspacePage();page.innerHTML=`<div class="dialog-title"><h2>${escape(title)}</h2><button type="button" id="feature-close">Back</button></div>${html}<p id="feature-error" class="error" hidden></p>`;showWorkspacePage(page,kind,id);page.querySelector('#feature-close').onclick=()=>closePageFromButton(page.querySelector('#feature-close'));page.oninput=event=>{if(event.target.closest('form'))dirtyForms.add('feature-page')};page.addEventListener('close',()=>dirtyForms.delete('feature-page'),{once:true});return page}

function closePageFromButton(button){const page=button.closest('.workspace-page');if(page&&dirtyForms.size&&!confirm('Discard unsaved changes?'))return;(page||button.closest('dialog')).close()}

function allowWorkspaceTransition(){if(!workspacePage||!dirtyForms.size)return true;if(!confirm('Leave this page and discard unsaved changes?'))return false;dirtyForms.clear();return true}
