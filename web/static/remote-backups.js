'use strict';
function renderRemoteBackups(){
 if(!isAdmin()){$('#content').innerHTML='<p class="error">Remote database backups require administrator access.</p>';return}
 if($('#remote-backup-panel'))return;
 $('#content').innerHTML='<article id="remote-backup-panel" class="panel"><div class="toolbar"><h2>Objects in remote storage</h2><button type="button" id="remote-backup-refresh">Check bucket</button></div><p id="remote-backup-status" role="status">Checking the configured bucket…</p><p class="hint">This lists real S3 objects under the configured prefix, including backups no longer retained locally. These files stay private and do not need a public CDN URL. Changing the bucket or prefix changes the location being viewed.</p><p id="remote-backup-warning" class="notice" hidden></p><p id="remote-backup-failure" class="error" role="alert" hidden></p><div class="table-wrap"><table><thead><tr><th>Object</th><th>Size</th><th>Modified</th><th>Actions</th></tr></thead><tbody id="remote-backup-rows"></tbody></table></div><p id="remote-backup-limit" class="hint" hidden></p></article>';
 $('#remote-backup-refresh').onclick=loadRemoteBackups;loadRemoteBackups();
}
async function loadRemoteBackups(){
 const button=$('#remote-backup-refresh'),panel=$('#remote-backup-panel');if(!button||button.disabled)return;
 button.disabled=true;$('#remote-backup-status').textContent='Checking remote storage…';
 try{const data=demo?{objects:[],bucket:'demo-private-bucket',prefix:'lidza-deploy',checked:new Date().toISOString()}:await api('control/storage/backups');if(!panel.isConnected)return;
 $('#remote-backup-failure').hidden=true;$('#remote-backup-warning').hidden=!data.warning;$('#remote-backup-warning').textContent=data.warning||'';$('#remote-backup-status').textContent=`${data.objects.length} remote backup objects · ${data.bucket}/${data.prefix||''} · Checked ${new Date(data.checked).toLocaleString()}`;
 $('#remote-backup-rows').innerHTML=data.objects.map(o=>`<tr><td><code class="remote-backup-key">${escape([o.prefix??data.prefix,o.key].filter(Boolean).join('/'))}</code>${o.legacy?'<small>Previous endpoint layout</small>':''}</td><td>${formatBackupSize(o.size)}</td><td>${escape(new Date(o.modifiedAt).toLocaleString())}</td><td><a class="button" href="/api/control/storage/backups/download?key=${encodeURIComponent(o.key)}${o.legacy?'&legacy=1':''}">Download remote copy</a></td></tr>`).join('')||'<tr><td colspan="4">No remote backups found in this bucket/prefix. Enable off-site copies for the database and run a backup, or upload a retained local backup. If a backup failed, its local copy remains available in Databases & backups.</td></tr>';
 $('#remote-backup-limit').hidden=!data.limit_reached;$('#remote-backup-limit').textContent='Showing at most 1,000 objects. Additional objects may exist; use your storage provider’s private bucket browser for the full inventory.';
 }catch(error){if(panel.isConnected){$('#remote-backup-failure').textContent=error.message+' Previously loaded objects are retained.';$('#remote-backup-failure').hidden=false;$('#remote-backup-status').textContent='Remote inventory could not be verified.'}}finally{button.disabled=false}
}

function formatBackupSize(value){const size=Number(value)||0;if(size<1024)return size+' bytes';if(size<1048576)return (size/1024).toFixed(1)+' KB';if(size<1073741824)return (size/1048576).toFixed(2)+' MB';return (size/1073741824).toFixed(2)+' GB'}
