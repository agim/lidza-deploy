'use strict';
async function openSSHAccess(id){
 const server=servers.find(s=>s.id===id);if(!server)throw Error('Server not found.');
 const endpoint='control/servers/'+encodeURIComponent(id)+'/ssh-access';
 let profile=demo?{supported:true,user:'deploy',keys:[],sudo:false,state:'idle'}:await api(endpoint);
 let host=new URL(server.url).hostname;if(['localhost','127.0.0.1','[::1]'].includes(host))host=location.hostname;
 host=host.replace(/^\[|\]$/g,'');const command='ssh deploy@'+host;
 const d=featureDialog(server.name+' · SSH access',`<p>Connect with your own SSH public key as <strong>deploy</strong>. This account can read service and kernel logs.</p><label>Connection command<input id="ssh-command" readonly value="${escape(command)}"></label><button type="button" id="ssh-copy">Copy command</button><p id="ssh-status" role="status"></p><div id="ssh-keys"></div><form id="ssh-key-form"><h3>Add a public key</h3><p class="hint">On your Mac, run <code>cat ~/.ssh/id_ed25519.pub</code> and paste its output here. Your private key stays on your Mac.</p><label>Key label<input name="label" required maxlength="80" placeholder="My MacBook"></label><label>SSH public key<textarea name="public_key" required maxlength="8192" spellcheck="false" placeholder="ssh-ed25519 AAAA…"></textarea></label><button type="submit">Add public key</button></form><form id="ssh-sudo-form"><h3>Administrative access</h3><label class="check-label"><input type="checkbox" name="sudo"> Allow full sudo administration without a password</label><p class="hint">This grants root-level control to everyone who can sign in as deploy, including through existing keys. Leave it off for log access. Existing privileges granted outside Līdza Deploy still apply.</p><button type="submit">Save administrative access</button></form><p class="hint">Existing root logins and unmanaged SSH keys are preserved. Revoking a key prevents new logins; it does not close existing sessions. Your server must allow SSH on port 22.</p>`);
 let stopped=false,busy=false,timer;
 const keyForm=d.querySelector('#ssh-key-form'),sudoForm=d.querySelector('#ssh-sudo-form');
 function render(){
  d.querySelector('#ssh-status').textContent=profile.supported?(profile.message||'Ready to configure deploy access.'):'Update this server from Updates to enable managed SSH access. Older installations may also need openssh-server and sudo installed.';
  d.querySelector('#ssh-keys').innerHTML='<h3>Managed public keys</h3>'+((profile.keys||[]).map(k=>`<article class="panel"><strong>${escape(k.label)}</strong><p><code class="ssh-fingerprint">${escape(k.fingerprint)}</code></p><button type="button" data-revoke-ssh="${escape(k.id)}">Revoke key</button></article>`).join('')||'<p class="hint">No managed keys added yet.</p>');
  const pending=['queued','applying'].includes(profile.state);
  for(const b of d.querySelectorAll('button[type=submit],[data-revoke-ssh]'))b.disabled=!profile.supported||pending||busy;
  if(!sudoForm.dataset.dirty)sudoForm.elements.sudo.checked=profile.sudo;
 }
 async function load(){if(stopped||demo)return;try{const next=await api(endpoint);if(stopped)return;if(JSON.stringify(next)!==JSON.stringify(profile)){profile=next;render()}}catch(e){if(!stopped)featureError(e)}}
 async function change(method,suffix,body){if(busy)return;busy=true;render();try{if(!demo){await api(endpoint+suffix,method,body);await load()}else{profile.state='ready';profile.message='Demo: SSH access configured.';if(method==='POST')profile.keys.push({id:'demo-'+Date.now(),label:body.label,fingerprint:'SHA256:demo'});if(method==='PATCH')profile.sudo=body.sudo;if(method==='DELETE')profile.keys=profile.keys.filter(k=>!suffix.endsWith(k.id));}d.querySelector('#feature-error').hidden=true;return true}catch(e){featureError(e);return false}finally{busy=false;if(!stopped)render()}}
 keyForm.onsubmit=async e=>{e.preventDefault();const value=Object.fromEntries(new FormData(keyForm));if(value.public_key.includes('PRIVATE KEY')){keyForm.elements.public_key.value='';featureError(Error('Only paste your .pub public key. Private keys are never accepted.'));return}if(await change('POST','/keys',value))keyForm.reset()};
 sudoForm.onchange=()=>sudoForm.dataset.dirty='true';sudoForm.onsubmit=async e=>{e.preventDefault();if(await change('PATCH','',{sudo:sudoForm.elements.sudo.checked})){delete sudoForm.dataset.dirty;render()}};
 d.querySelector('#ssh-keys').onclick=async e=>{const b=e.target.closest('[data-revoke-ssh]');if(b)await change('DELETE','/keys/'+encodeURIComponent(b.dataset.revokeSsh))};
 d.querySelector('#ssh-copy').onclick=async()=>{try{await navigator.clipboard.writeText(command);d.querySelector('#ssh-copy').textContent='Copied'}catch(e){featureError(Error('Could not copy. Select the connection command above and copy it.'))}};
 const stop=()=>{stopped=true;clearInterval(timer)};d.addEventListener('close',stop,{once:true});window.addEventListener('pagehide',stop,{once:true});timer=setInterval(load,2000);render();
}
