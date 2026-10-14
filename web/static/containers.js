'use strict';
let containerPageGeneration=0;

function demoContainerInventory(){return {checked_at:new Date().toISOString(),containers:[
 {id:'7cc38e2',name:'lidza-sample-web',app_id:'sample',role:'Web · current release',state:'running',status:'Up 3 hours (healthy)',health:'healthy',cpu:'0.12%',memory:'42.3MiB / 512MiB',memory_percent:'8.26%',network_io:'3.1MB / 8.8MB',block_io:'4.1MB / 0B',pids:'9',restartable:true},
 {id:'4af21d9',name:'lidza-db-sample',app_id:'sample',role:'PostgreSQL',state:'running',status:'Up 3 hours',cpu:'0.34%',memory:'118MiB / 1GiB',memory_percent:'11.52%',network_io:'9.4MB / 6.2MB',block_io:'22MB / 8MB',pids:'14',restartable:true}
]}}

function setContainerCell(row,name,value){const node=row.querySelector(`[data-field="${name}"]`);if(node&&node.textContent!==value)node.textContent=value}

async function openContainers(serverID){
 const server=servers.find(value=>value.id===serverID);if(!server)throw Error('Server not found.');
 const generation=++containerPageGeneration;
 const page=featurePage(server.name+' · containers',`<section aria-label="Managed container inventory"><div class="toolbar container-toolbar"><div><h3>Managed containers</h3><p class="hint">Live Docker usage for applications and services owned by this agent. Unrelated host containers are intentionally excluded.</p></div><button type="button" id="containers-refresh">Refresh now</button></div><div class="container-filters"><label>Search<input id="container-search" type="search" placeholder="App, role or container"></label><label>Application<select id="container-app"><option value="">All applications</option></select></label><label>Status<select id="container-state"><option value="">All states</option><option value="attention">Needs attention</option><option value="running">Running</option><option value="stopped">Stopped</option></select></label></div><p id="container-summary" role="status">Loading container usage…</p><p id="container-warning" class="notice" hidden></p><div class="table-wrap container-table-wrap"><table class="container-table"><thead><tr><th>Application / role</th><th>Container</th><th>State</th><th>CPU</th><th>Memory</th><th>I/O / processes</th><th>Action</th></tr></thead><tbody></tbody></table></div><div id="container-empty" class="empty" hidden>No managed containers match these filters.</div><p class="hint">Usage is sampled about every 10 seconds. Restart stops and starts the same Docker container; its configured volumes, network and restart policy are retained. Use this only to recover a stuck process—deploy or reload when replacing application code.</p></section>`,'containers',serverID);
 const body=page.querySelector('tbody'),search=page.querySelector('#container-search'),appFilter=page.querySelector('#container-app'),stateFilter=page.querySelector('#container-state'),summary=page.querySelector('#container-summary'),warning=page.querySelector('#container-warning');
 let items=[],timer=0,loading=false,closed=false;
 const applyFilters=()=>{
  const query=search.value.trim().toLowerCase(),app=appFilter.value,state=stateFilter.value;let visible=0;
  for(const row of body.children){const item=items.find(value=>value.name===row.dataset.containerName),matches=item&&(!query||[item.app_id,item.role,item.name,item.state,item.status].join(' ').toLowerCase().includes(query))&&(!app||item.app_id===app)&&(!state||(state==='attention'?item.needs_attention:state==='running'?item.state==='running':item.state!=='running'));row.hidden=!matches;if(matches)visible++}
  page.querySelector('#container-empty').hidden=visible!==0;
 };
 const paint=data=>{
  items=Array.isArray(data.containers)?data.containers:[];
  const appsInView=[...new Set(items.map(item=>item.app_id).filter(Boolean))].sort(),selected=appFilter.value,options='<option value="">All applications</option>'+appsInView.map(id=>`<option value="${escape(id)}">${escape(id)}</option>`).join('');if(appFilter.innerHTML!==options){appFilter.innerHTML=options;if(appsInView.includes(selected))appFilter.value=selected}
  const retained=new Set();
  for(const [index,item] of items.entries()){
   let row=[...body.children].find(value=>value.dataset.containerName===item.name);
   if(!row){row=document.createElement('tr');row.dataset.containerName=item.name;row.innerHTML='<td data-label="Application / role"><strong data-field="app"></strong><small data-field="role"></small></td><td data-label="Container"><code data-field="name"></code><small data-field="id"></small></td><td data-label="State"><span class="pill neutral" data-field="state"></span><small data-field="status"></small></td><td data-label="CPU"><strong data-field="cpu"></strong></td><td data-label="Memory"><strong data-field="memory"></strong><small data-field="memory-percent"></small></td><td data-label="I/O / processes"><span data-field="io"></span><small data-field="pids"></small></td><td data-label="Action"><button type="button" data-restart-container></button><small data-field="action-hint"></small></td>';body.insertBefore(row,body.children[index]||null)}
   if(body.children[index]!==row)body.insertBefore(row,body.children[index]||null);retained.add(row);
   setContainerCell(row,'app',item.app_id||'Retained resource');setContainerCell(row,'role',item.role||'Managed service');setContainerCell(row,'name',item.name);setContainerCell(row,'id',item.id?'ID '+item.id.slice(0,12):'Not found in Docker');setContainerCell(row,'state',item.needs_attention?'Needs attention':item.health==='healthy'?'Healthy':item.state||'Unknown');setContainerCell(row,'status',item.status||item.state||'Unavailable');setContainerCell(row,'cpu',item.cpu||'—');setContainerCell(row,'memory',item.memory||'—');setContainerCell(row,'memory-percent',item.memory_percent||'');setContainerCell(row,'io',[item.network_io&&'Network '+item.network_io,item.block_io&&'Disk '+item.block_io].filter(Boolean).join(' · ')||'—');setContainerCell(row,'pids',item.pids?'Processes '+item.pids:'');
   const pill=row.querySelector('[data-field=state]');pill.className='pill '+(item.needs_attention?'failed':item.state==='running'?'':'neutral');
   const button=row.querySelector('[data-restart-container]');button.dataset.restartContainer=item.name;button.textContent=item.state==='running'?'Restart':'Start / restart';button.hidden=!item.restartable||!isAdmin();button.disabled=false;setContainerCell(row,'action-hint',item.restartable?(isAdmin()?'Briefly interrupts this container.':'Administrator access required.'):'Use application controls.');
  }
  for(const row of [...body.children])if(!retained.has(row))row.remove();
  const attention=items.filter(item=>item.needs_attention).length,running=items.filter(item=>item.state==='running').length,checked=data.checked_at?new Date(data.checked_at).toLocaleTimeString():new Date().toLocaleTimeString();summary.textContent=`${items.length} managed container${items.length===1?'':'s'} · ${running} running${attention?' · '+attention+' need attention':''} · sampled ${checked}`;warning.hidden=!data.warning;warning.textContent=data.warning||'';applyFilters();
 };
 const schedule=()=>{clearTimeout(timer);if(!closed&&generation===containerPageGeneration)timer=setTimeout(load,10000)};
 const load=async()=>{if(closed||generation!==containerPageGeneration||loading||workspacePage!==page)return;loading=true;try{const data=demo?demoContainerInventory():await api('control/servers/'+serverID+'/containers');if(!closed&&workspacePage===page)paint(data)}catch(error){if(!closed&&workspacePage===page){warning.hidden=false;warning.textContent=error.message}}finally{loading=false;schedule()}};
 page.querySelector('#containers-refresh').onclick=()=>{clearTimeout(timer);load()};for(const control of [search,appFilter,stateFilter])control.oninput=applyFilters;
 page.onclick=async event=>{const button=event.target.closest('[data-restart-container]');if(!button)return;const item=items.find(value=>value.name===button.dataset.restartContainer);if(!item||button.disabled)return;if(!confirm(`Restart ${item.name}?\n\n${item.app_id} · ${item.role}\nThis briefly interrupts that container.`))return;button.disabled=true;try{if(!demo)await api('control/servers/'+serverID+'/containers/restart','POST',{name:item.name});notice(item.name+' restarted. Refreshing live state…');clearTimeout(timer);setTimeout(load,800)}catch(error){featureError(error);button.disabled=false}};
 page.addEventListener('close',()=>{closed=true;clearTimeout(timer);containerPageGeneration++},{once:true});
 await load();
}
