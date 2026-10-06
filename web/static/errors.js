'use strict';

const errorView = {mode:'console', app: '', query: '', source: '', data: null, loading: false, failure: '', fetched: 0, version: 0};

function openAppErrors(id) {
  if (errorView.app !== id) resetErrorApp(id);
  navigate('errors').catch(fail);
}

function resetErrorApp(id) {
  Object.assign(errorView, {app: id, data: null, loading: false, failure: '', fetched: 0});
  errorView.version++;
}

function renderErrors() {
  const available = apps.filter(a => !a.retiring);
  if (!available.some(a => a.id === errorView.app)) resetErrorApp(available[0]?.id || '');
  if (!$('#error-dashboard')) {
    $('#content').innerHTML = `<section id="error-dashboard"><div class="form-row error-filters">
      <label>Application<select id="errors-app"></select></label>
      <label>Search errors<input id="errors-search" type="search" placeholder="Message, route, stack or request ID"></label>
      <label>Capture<select id="errors-mode"><option value="console">Agent console errors</option><option value="analytics">Optional app analytics</option></select></label>
      <label>Source<select id="errors-source"><option value="">All sources</option><option value="server">Server</option><option value="client">Frontend</option></select></label>
      <button id="errors-refresh">Refresh errors</button></div>
      <p class="hint">Console errors are collected automatically by the hosting agent and retained in Deploy for 30 days. Counts and search cover up to 500 recent reports. Optional app analytics reads the app’s own database.</p>
      <div id="errors-result" aria-live="polite"></div></section>`;
    $('#errors-search').value = errorView.query;
    $('#errors-source').value = errorView.source;
    $('#errors-mode').value=errorView.mode;
    $('#errors-mode').onchange=e=>{errorView.mode=e.target.value;errorView.source='';$('#errors-source').value='';resetErrorApp(errorView.app);renderErrors();};
    $('#errors-app').onchange = e => { resetErrorApp(e.target.value); renderErrors(); };
    $('#errors-search').oninput = e => { errorView.query = e.target.value; paintErrors(); };
    $('#errors-source').onchange = e => { errorView.source = e.target.value; paintErrors(); };
    $('#errors-refresh').onclick = () => loadAppErrors();
    $('#errors-result').onclick = e => {
      const button = e.target.closest('[data-error-detail]');
      if (button) showErrorDetail(button.dataset.errorDetail);
    };
  }
  const options = available.map(a => `<option value="${escape(a.id)}">${escape(a.id)}</option>`).join('') || '<option value="">No applications</option>';
  if ($('#errors-app').innerHTML !== options) $('#errors-app').innerHTML = options;
  $('#errors-app').value = errorView.app;
  paintErrors();
  if (errorView.app && !errorView.loading && Date.now() - errorView.fetched > 30000) loadAppErrors();
}

async function loadAppErrors() {
  if (!errorView.app || errorView.loading) return;
  const id = errorView.app, version = errorView.version;
  errorView.loading = true;
  errorView.failure = '';
  paintErrors();
  try {
    const data = demo ? demoAppErrors() : await api(`control/apps/${encodeURIComponent(id)}/${errorView.mode==='analytics'?'analytics-errors':'errors'}`);
    if (version !== errorView.version) return;
    errorView.data = data;
  } catch (err) {
    if (version !== errorView.version) return;
    errorView.failure = err.message;
  } finally {
    if (version === errorView.version) {
      errorView.loading = false;
      errorView.fetched = Date.now();
      paintErrors();
    }
  }
}

function errorGroups() {
  const groups = new Map();
  for (const row of errorView.data?.errors || []) {
    const key = row.fingerprint || row.id;
    let group = groups.get(key);
    if (!group) { group = {key, rows: []}; groups.set(key, group); }
    group.rows.push(row);
  }
  const query = errorView.query.trim().toLowerCase();
  return [...groups.values()].filter(g => g.rows.some(e =>
    (!errorView.source || e.source === errorView.source) &&
    (!query || [e.message, e.stack, e.route, e.requestId].some(v => String(v || '').toLowerCase().includes(query)))
  )).sort((a, b) => new Date(b.rows[0].createdAt) - new Date(a.rows[0].createdAt));
}

function paintErrors() {
  if (tab !== 'errors' || !$('#errors-result')) return;
  $('#errors-refresh').disabled = errorView.loading || !errorView.app;
  const result = $('#errors-result');
  if (!errorView.app) { result.innerHTML = '<p>Add an application to view its captured errors.</p>'; return; }
  if (errorView.failure) { result.innerHTML = `<div class="error" role="alert">${escape(errorView.failure)} Refresh to retry. Previously loaded data is unavailable until a successful refresh.</div>`; return; }
  if (!errorView.data) { result.textContent = 'Loading captured errors…'; return; }
  if (errorView.mode === 'analytics' && (errorView.data.status === 'needs_database' || errorView.data.status === 'needs_analytics')) {
    result.innerHTML = `<article class="app-card"><div class="card-body"><h2>Enable error reporting in this app</h2>
      <p>${errorView.data.status === 'needs_database' ? 'Attach PostgreSQL as DATABASE_URL in the app’s database settings, then enable the analytics pack.' : 'The configured database has no analytics error table yet.'}</p>
      <p>In the app repository, run <code>lidza pack add analytics</code>, follow the generated migration instructions, commit and redeploy. For browser errors, enable the framework frontend reporter with <code>VITE_ANALYTICS=1</code> when building the frontend.</p>
      <p class="hint">This dashboard reads the existing Līdza error store. Enabling a pack changes your app’s source; runtime environment changes alone cannot add it. Runtime Logs remain available.</p></div></article>`;
    return;
  }
  const groups = errorGroups(), total = errorView.data.errors?.length || 0;
  const warning=errorView.data.mode==='console' && errorView.data.status!=='ready' ? `<div class="notice" role="status">${escape(errorView.data.collection_error || (errorView.data.status==='waiting_agent'?'Waiting for the hosting agent to connect its error collector.':'The hosting agent has not reported recently. Stored errors remain available.'))}</div>` : '';
  result.innerHTML = `${warning}${errorView.data.shared ? '<div class="notice">This primary database is shared with another configured app. Its error store may include reports from every app using it. Separate primary databases provide separate error stores.</div>' : ''}<div class="toolbar"><h2>Captured failures</h2><span class="badge">${groups.length} matching groups · ${total} recent occurrences${errorView.data.truncated ? ' · response size capped' : ''}${errorView.loading ? ' · refreshing' : ''}</span></div>
    <div class="table-wrap"><table><thead><tr><th>Error</th><th>Source / route</th><th>Occurrences</th><th>Last seen</th><th>Details</th></tr></thead><tbody>${groups.map(g => {
      const e = g.rows[0];
      return `<tr><td class="error-message">${escape(e.message)}</td><td>${escape(e.source)}<br><code>${escape(e.route || '—')}</code></td><td>${g.rows.length}</td><td>${escape(new Date(e.createdAt).toLocaleString())}</td><td><button data-error-detail="${escape(g.key)}">Inspect error</button></td></tr>`;
    }).join('') || `<tr><td colspan="5">${total ? 'No errors match these filters.' : 'No captured errors in this sample. This does not establish that the app is healthy or reporting correctly.'}</td></tr>`}</tbody></table></div>`;
}

function showErrorDetail(key) {
  const group = errorGroups().find(g => g.key === key);
  if (!group) return;
  let dialog = $('#error-detail-dialog');
  if (!dialog) {
    dialog = document.createElement('dialog');
    dialog.id = 'error-detail-dialog';
    document.body.append(dialog);
  }
  const last = group.rows[0], first = group.rows[group.rows.length - 1];
  dialog.innerHTML = `<div class="dialog-title"><h2>Error details · ${escape(errorView.app)}</h2><button id="error-detail-close" aria-label="Close error details">×</button></div>
    <p>${group.rows.length} occurrences in this sample · first ${escape(new Date(first.createdAt).toLocaleString())} · last ${escape(new Date(last.createdAt).toLocaleString())}</p>
    <p>Source: ${escape(last.source)} · Route: ${escape(last.method || '')} ${escape(last.route || '—')}</p>
    <h3>Message</h3><pre class="error-stack">${escape(last.message)}</pre><h3>Stack trace</h3><pre class="error-stack">${escape(last.stack || 'No stack trace was captured.')}</pre>
    <p>Container: ${escape(last.container || '—')} · Release: ${escape(last.release || '—')} · Commit: ${escape(last.commit || '—')}</p><h3>Recent occurrences</h3><div class="table-wrap"><table><thead><tr><th>Time</th><th>Request ID</th></tr></thead><tbody>${group.rows.map(e => `<tr><td>${escape(new Date(e.createdAt).toLocaleString())}</td><td><code>${escape(e.requestId || '—')}</code></td></tr>`).join('')}</tbody></table></div>`;
  $('#error-detail-close').onclick = () => dialog.close();
  dialog.showModal();
}

function demoAppErrors() {
  return {status: 'ready', limit: 500, errors: [0, 1, 2].map(i => ({id: 'demo-error-' + i, fingerprint: 'demo-checkout', source: 'server', message: 'Payment service temporarily unavailable', route: 'POST /checkout', method: 'POST', stack: 'handlers.Checkout\n    handlers/checkout.go:42', requestId: 'demo-request-' + i, createdAt: new Date(Date.now() - i * 60000).toISOString()}))};
}
