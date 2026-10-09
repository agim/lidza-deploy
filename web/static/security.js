'use strict';
let securityApp='',securityBusy=false,securityVersion=0,securityFetched=0;
function renderSecurityActivity(){
 if(!apps.some(a=>a.id===securityApp&&!a.retiring))securityApp=apps.find(a=>!a.retiring)?.id||'';
 if(!document.querySelector('#security-dashboard')){
 $('#content').innerHTML='<section id="security-dashboard"><div class="toolbar"><label>Application<select id="security-app"></select></label><button id="security-refresh">Refresh activity</button></div><p class="hint">Suspected probes collected from structured app request logs, separate from application errors. A 2xx response needs review; it may be a fallback page. Redirects do not establish compromise. Client addresses are not inferred from untrusted headers.</p><p id="security-error" role="alert" hidden></p><div id="security-result" aria-live="polite"></div></section>';
 $('#security-app').onchange=e=>{securityApp=e.target.value;securityVersion++;$('#security-result').replaceChildren();loadSecurityActivity()};$('#security-refresh').onclick=loadSecurityActivity;loadSecurityActivity();
 }
 const options=apps.filter(a=>!a.retiring).map(a=>`<option value="${escape(a.id)}">${escape(a.id)}</option>`).join('');if($('#security-app').innerHTML!==options)$('#security-app').innerHTML=options;$('#security-app').value=securityApp;
 if(securityApp&&!securityBusy&&Date.now()-securityFetched>30000)loadSecurityActivity();
}
async function loadSecurityActivity(){
 if(!securityApp){$('#security-result').textContent='No applications available.';return}
 const id=securityApp,version=++securityVersion;securityBusy=true;$('#security-refresh').disabled=true;
 try{
 const data=demo?{status:'ready',errors:[]}:await api('control/apps/'+encodeURIComponent(id)+'/security');
 if(tab!=='security'||version!==securityVersion)return;
 const rows=data.errors||[];paintSecurityActivity($('#security-result'),rows,data);
 $('#security-error').hidden=true;
 }catch(e){if(tab==='security'&&version===securityVersion){$('#security-error').textContent=e.message;$('#security-error').hidden=false}}
 finally{if(version===securityVersion){securityBusy=false;securityFetched=Date.now()}if(tab==='security'&&version===securityVersion)$('#security-refresh').disabled=false}
}

function paintSecurityActivity(result,rows,data){
 if(!result.querySelector('.security-summary'))result.innerHTML='<p class="security-summary"></p><p class="hint">Up to 500 retained reports; collection is sampled under load. This is not a total traffic count or a completed vulnerability scan.</p><div class="security-records"></div><p class="security-empty">No captured probes. Older agents or unstructured request logs may not report this activity.</p>';
 result.querySelector('.security-summary').textContent=rows.length+' recent suspected probes · Collector: '+(data.status||'unknown')+(data.truncated?' · Results truncated':'');
 const list=result.querySelector('.security-records'),existing=new Map([...list.children].map(e=>[e.dataset.probeId,e]));
 const visible=[...list.children].find(e=>e.getBoundingClientRect().bottom>0);const anchor=visible&&{id:visible.dataset.probeId,top:visible.getBoundingClientRect().top};
 const retained=new Set();
 rows.forEach((e,i)=>{retained.add(e.id);let node=existing.get(e.id);if(!node){node=document.createElement('article');node.className='panel';node.dataset.probeId=e.id}
 const html=`<strong>${escape(e.security?.category||'Probe')} · HTTP ${escape(e.security?.status||'')} · ${escape(e.security?.outcome||'')}</strong><p><code>${escape(e.method||'')} ${escape(e.route||'')}</code></p><small>${escape(new Date(e.createdAt).toLocaleString())} · Request ${escape(e.requestId||'—')} · Release ${escape(e.release||'—')}</small>`;
 if(node.innerHTML!==html)node.innerHTML=html;if(list.children[i]!==node)list.insertBefore(node,list.children[i]||null);
 });
 for(const node of [...list.children])if(!retained.has(node.dataset.probeId))node.remove();
 result.querySelector('.security-empty').hidden=rows.length>0;
 if(anchor){const node=[...list.children].find(e=>e.dataset.probeId===anchor.id);if(node)window.scrollBy(0,node.getBoundingClientRect().top-anchor.top)}
}
