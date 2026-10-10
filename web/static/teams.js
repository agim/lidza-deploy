'use strict';
let teamCursor='',teamPrevious=[],auditCursor='',auditPrevious=[],teamLoading=false;
function isAdmin(){return demo||status.roles?.includes('admin')}
function canDeploy(){return isAdmin()||status.roles?.includes('deployer')}
function renderTeamPermissions(){
 for(const node of document.querySelectorAll('[data-tab=team],[data-tab=audit]'))node.hidden=!isAdmin();
 $('#new-app').hidden=!isAdmin()||tab==='newapp';
 for(const node of document.querySelectorAll('#defaults-form input,#defaults-form button[type=submit]'))node.disabled=!isAdmin()||!$('#defaults-form').dataset.ready||!!$('#defaults-form').dataset.saving;
 for(const node of document.querySelectorAll('[data-ssh-access],[data-manage-server],[data-remove-server],[data-upgrade],[data-previews],[data-action=remove],[data-action=github-app],#github-config,#connect,#disconnect'))node.hidden=!isAdmin();
 if($('#connect'))$('#connect').hidden=!isAdmin()||(!demo&&!status.github_owner);
 if($('#disconnect'))$('#disconnect').hidden=!isAdmin()||(!demo&&!status.github_owner);
 for(const node of document.querySelectorAll('[data-action=deploy],[data-action=reload],[data-action=rollback],[data-action=webhook],[data-action=disable-webhook],[data-app-state],[data-maintenance],[data-tasks],#settings-form button[type=submit]'))node.disabled=!canDeploy()||node.dataset.stopped==='1';
 document.body.dataset.role=isAdmin()?'admin':canDeploy()?'deployer':'viewer';
 if((tab==='team'||tab==='audit')&&!isAdmin()){$('#content').textContent='Administrator access is required.';return}
 if(tab==='team'||tab==='audit')renderTeamPage().catch(fail);
}
async function renderTeamPage(){
 if(teamLoading||$('#content').contains(document.activeElement)&&['INPUT','SELECT'].includes(document.activeElement.tagName))return;
 const selected=tab;teamLoading=true;
 try{
 if(selected==='team'){
 const data=demo?{members:[{subject:'demo-owner',email:'owner@example.com',name:'Owner',role:'admin',owner:true}],next:''}:await api('control/team'+(teamCursor?'?cursor='+encodeURIComponent(teamCursor):''));if(tab!==selected)return;
 $('#content').innerHTML=`<article class="panel"><h2>Fleet team</h2><p>One team shares this control panel’s applications and servers. Admins manage everything; deployers release and configure existing apps; viewers inspect operational status. Only admins create/remove apps, provision databases, configure previews, download backups, or manage infrastructure.</p><div class="table-wrap"><table><thead><tr><th>Member</th><th>Role</th><th>Actions</th></tr></thead><tbody>${data.members.map(m=>`<tr><td>${escape(m.name||m.email)}<br><small>${escape(m.email)}</small></td><td>${escape(m.role)} ${m.owner?'· installation owner':''}</td><td>${m.owner?'Protected administrator':`<button data-member-edit="${escape(m.subject)}">Change role</button><button data-member-remove="${escape(m.subject)}">Remove access</button>`}</td></tr>`).join('')}</tbody></table></div><button id="team-prev" ${teamPrevious.length?'':'disabled'}>Previous</button><button id="team-next" ${data.next?'':'disabled'}>Next</button></article><article class="panel"><h2>Add member or change role</h2><form id="team-form"><div class="form-row"><label>Email<input name="email" type="email" required autocomplete="off"></label><label>Name<input name="name" maxlength="100" autocomplete="off"></label></div><div class="form-row"><label>Role<select name="role"><option value="viewer">Viewer</option><option value="deployer">Deployer</option><option value="admin">Administrator</option></select></label><label>New account password<input name="password" type="password" minlength="16" maxlength="1024" autocomplete="new-password"></label></div><p class="hint">A new account needs a password of at least 16 characters. Share it privately. Leave blank for an existing account; changing a role never resets its password. Removing access takes effect on the next request, including existing sessions.</p><button type="submit">Save member</button></form></article>`;
 $('#team-form').onsubmit=async e=>{e.preventDefault();try{if(demo){notice('Preview: member saved.');return}await api('control/team','POST',Object.fromEntries(new FormData(e.target)));clearFormDraft(e.target.id);e.target.reset();teamCursor='';teamPrevious=[];await renderTeamPage();notice('Member saved.')}catch(err){fail(err)}};
 for(const b of document.querySelectorAll('[data-member-edit]'))b.onclick=()=>{const m=data.members.find(m=>m.subject===b.dataset.memberEdit);for(const key of ['email','name','role'])$('#team-form').elements[key].value=m[key]||'';$('#team-form').elements.password.value='';$('#team-form').scrollIntoView({block:'center'});};
 for(const b of document.querySelectorAll('[data-member-remove]'))b.onclick=async()=>{if(!confirm('Remove this member’s access to the fleet?'))return;try{if(!demo)await api('control/team/'+encodeURIComponent(b.dataset.memberRemove),'DELETE');await renderTeamPage();notice('Member access removed.')}catch(err){fail(err)}};
 $('#team-prev').onclick=()=>{teamCursor=teamPrevious.pop()||'';renderTeamPage().catch(fail)};$('#team-next').onclick=()=>{teamPrevious.push(teamCursor);teamCursor=data.next;renderTeamPage().catch(fail)};
 }else if(selected==='audit'){
 const data=demo?{records:[],next:''}:await api('control/audit'+(auditCursor?'?cursor='+encodeURIComponent(auditCursor):''));if(tab!==selected)return;
 $('#content').innerHTML=`<article class="panel"><h2>Audit log</h2><p>Persisted requests and their immediate results, attributed to authenticated members. An accepted deployment is queued; see Deployments for its eventual result. Request bodies, passwords and environment values are never recorded.</p><div class="table-wrap"><table><thead><tr><th>When</th><th>Actor</th><th>Action</th><th>Resource</th><th>Result</th></tr></thead><tbody>${data.records.map(r=>`<tr><td>${escape(new Date(r.at).toLocaleString())}</td><td><code>${escape(r.actor)}</code></td><td>${escape(r.action)}</td><td>${escape(r.resource)}<br><small>${escape(Object.entries(r.meta||{}).filter(([k])=>k!=='route').map(([k,v])=>k+': '+v).join(' · '))}</small></td><td>${escape(r.outcome)}</td></tr>`).join('')||'<tr><td colspan="5">No audit events yet.</td></tr>'}</tbody></table></div><button id="audit-prev" ${auditPrevious.length?'':'disabled'}>Previous</button><button id="audit-next" ${data.next?'':'disabled'}>Next</button></article>`;
 $('#audit-prev').onclick=()=>{auditCursor=auditPrevious.pop()||'';renderTeamPage().catch(fail)};$('#audit-next').onclick=()=>{auditPrevious.push(auditCursor);auditCursor=data.next;renderTeamPage().catch(fail)};
 }
 }finally{teamLoading=false}
}
