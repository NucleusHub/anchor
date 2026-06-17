'use strict';

const $ = (id) => document.getElementById(id);

async function api(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    headers: { 'Content-Type': 'application/json', ...(opts.headers || {}) },
  });
  let body = null;
  try { body = await res.json(); } catch { /* non-JSON (e.g. raw inspect) */ }
  return { ok: res.ok, status: res.status, body };
}

// ── Theme (dark/light, matches Nucleus) ─────────────────────────────────────
function currentDark() { return document.documentElement.classList.contains('dark'); }
function setTheme(dark) {
  document.documentElement.classList.toggle('dark', dark);
  try { localStorage.setItem('anchor-theme', dark ? 'dark' : 'light'); } catch (e) {}
  $('theme').textContent = dark ? '☀' : '🌙';
}
$('theme').textContent = currentDark() ? '☀' : '🌙';
$('theme').addEventListener('click', () => setTheme(!currentDark()));

let needsSetup = false;

// ── Gate (setup / login) ─────────────────────────────────────────────────────

async function boot() {
  const { body } = await api('/api/anchor/status');
  needsSetup = !!(body && body.needsSetup);
  if (body && body.authed) return showApp();
  showGate();
}

function showGate() {
  $('app').classList.add('hidden');
  $('gate').classList.remove('hidden');
  if (needsSetup) {
    $('gate-title').textContent = '⚓ Anchor — first run';
    $('gate-sub').textContent = 'Set the root password. There is no software reset: recovery means deleting config.json on the server.';
    $('pw').placeholder = 'New password (min 8 chars)';
    $('pw2').classList.remove('hidden');
    $('gate-btn').textContent = 'Set password & enter';
  } else {
    $('gate-title').textContent = '⚓ Anchor';
    $('gate-sub').textContent = 'Enter the root password.';
    $('pw2').classList.add('hidden');
    $('gate-btn').textContent = 'Log in';
  }
  $('pw').focus();
}

async function submitGate() {
  const pw = $('pw').value;
  const err = $('gate-err');
  err.textContent = '';
  if (needsSetup) {
    if (pw.length < 8) { err.textContent = 'Password must be at least 8 characters.'; return; }
    if (pw !== $('pw2').value) { err.textContent = 'Passwords do not match.'; return; }
    const r = await api('/api/anchor/setup', { method: 'POST', body: JSON.stringify({ password: pw }) });
    if (!r.ok) { err.textContent = (r.body && r.body.error) || 'Setup failed.'; return; }
    needsSetup = false;
    return showApp();
  }
  const r = await api('/api/anchor/login', { method: 'POST', body: JSON.stringify({ password: pw }) });
  if (!r.ok) { err.textContent = (r.body && r.body.error) || 'Login failed.'; return; }
  showApp();
}

$('gate-btn').addEventListener('click', submitGate);
document.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !$('gate').classList.contains('hidden')) submitGate();
});

// ── App ──────────────────────────────────────────────────────────────────────

let sse = null;
let selected = null; // currently open service

function showApp() {
  $('gate').classList.add('hidden');
  $('app').classList.remove('hidden');
  $('pw').value = ''; if ($('pw2')) $('pw2').value = '';
  loadServices();
  connectSSE();
}

$('logout').addEventListener('click', async () => {
  await api('/api/anchor/logout', { method: 'POST' });
  if (sse) sse.close();
  location.reload();
});

$('tab-services').addEventListener('click', () => switchTab('services'));
$('tab-audit').addEventListener('click', () => switchTab('audit'));
$('tab-env').addEventListener('click', () => switchTab('env'));
$('refresh').addEventListener('click', loadServices);
$('refresh-audit').addEventListener('click', loadAudit);
$('env-reload').addEventListener('click', loadEnv);
$('env-reveal').addEventListener('click', revealEnv);
$('env-save').addEventListener('click', saveEnv);

function switchTab(name) {
  for (const t of ['services', 'audit', 'env']) {
    $('tab-' + t).classList.toggle('active', t === name);
    $(t + '-view').classList.toggle('hidden', t !== name);
  }
  if (name === 'services') loadServices();
  if (name === 'audit') loadAudit();
  if (name === 'env') loadEnv();
}

const HEALTH_LABEL = { healthy: 'healthy', unhealthy: 'unhealthy', starting: 'starting', none: '—' };

async function loadServices() {
  const r = await api('/api/anchor/services');
  const body = $('svc-body');
  body.innerHTML = '';
  if (!r.ok) {
    body.innerHTML = `<tr><td colspan="6" class="err">${esc((r.body && r.body.error) || 'failed to load')}</td></tr>`;
    $('svc-count').textContent = '0 services';
    return;
  }
  const services = r.body.services || [];
  $('svc-count').textContent = `${services.length} service${services.length === 1 ? '' : 's'}`;

  let lastGroup = null;
  for (const s of services) {
    const group = s.app || '(infrastructure)';
    if (group !== lastGroup) {
      lastGroup = group;
      const g = document.createElement('tr');
      g.className = 'grouphdr';
      g.innerHTML = `<td colspan="6">${esc(group)}</td>`;
      body.appendChild(g);
    }
    const tr = document.createElement('tr');
    tr.className = 'svc';
    const stateColor = s.state === 'running' ? 'var(--green)' : 'var(--muted)';
    tr.innerHTML = `
      <td>${esc(s.name)}</td>
      <td>${s.app ? esc(s.app) : '<span class="muted">—</span>'}</td>
      <td><span class="tag">${esc(s.role || '—')}</span></td>
      <td style="color:${stateColor}">${esc(s.state)}</td>
      <td><span class="dot ${s.health}"></span>${HEALTH_LABEL[s.health] || esc(s.health)}</td>
      <td class="muted">${esc(s.status)}</td>`;
    tr.addEventListener('click', () => openDetail(s));
    body.appendChild(tr);
  }
  // keep an open detail panel in sync
  if (selected) {
    const fresh = services.find((x) => x.id === selected.id);
    if (fresh) selected = fresh;
  }
}

function openDetail(s) {
  selected = s;
  const running = s.state === 'running';
  const d = $('detail');
  d.classList.remove('hidden');
  d.innerHTML = `
    <div class="row">
      <h3 style="margin:0">${esc(s.name)}</h3>
      <span class="tag">${esc(s.role || '—')}</span>
      ${s.app ? `<span class="tag">${esc(s.app)}</span>` : ''}
      <span class="spacer"></span>
      <button id="close-detail">Close</button>
    </div>
    <div class="muted">${esc(s.image)} — ${esc(s.status)}</div>
    <div class="actions">
      <button data-act="start" ${running ? 'disabled' : ''}>Start</button>
      <button data-act="restart">Restart</button>
      <button data-act="stop" class="danger" ${running ? '' : 'disabled'}>Stop</button>
      <button data-act="recreate" class="danger">Recreate</button>
      <button data-act="rebuild" class="danger">Rebuild</button>
    </div>
    <pre id="action-out" class="hidden"></pre>
    <div class="row" style="margin:0"><h4 style="margin:0">Logs (last 300)</h4><span class="spacer"></span><button id="reload-logs">Reload</button></div>
    <pre id="logs">loading…</pre>
    <details><summary>Raw inspect</summary><pre id="inspect">loading…</pre></details>`;

  $('close-detail').addEventListener('click', () => { d.classList.add('hidden'); selected = null; });
  $('reload-logs').addEventListener('click', () => loadLogs(s.id));
  d.querySelectorAll('button[data-act]').forEach((b) =>
    b.addEventListener('click', () => runAction(s, b.dataset.act)));

  loadLogs(s.id);
  loadInspect(s.id);
  d.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
}

async function runAction(s, action) {
  const out = $('action-out');
  const postIt = (extra) => api(`/api/anchor/services/${encodeURIComponent(s.id)}/${action}`,
    { method: 'POST', body: JSON.stringify(extra || {}) });

  let r = await postIt({});
  if (r.status === 428 && r.body && r.body.needsConfirm) {
    const ok = await showConfirm(`Confirm: ${action} ${s.name}`, r.body.impact);
    if (!ok) return;
    r = await postIt({ confirmToken: r.body.confirmToken });
  }
  if (out) {
    out.classList.remove('hidden');
    if (r.ok) {
      out.textContent = `✓ ${action} ok` + (r.body && r.body.output ? `\n\n${r.body.output}` : '');
    } else {
      out.textContent = `✗ ${action} failed: ${(r.body && r.body.error) || r.status}` +
        (r.body && r.body.output ? `\n\n${r.body.output}` : '');
    }
  }
  setTimeout(loadServices, 500);
}

async function loadLogs(id) {
  const el = $('logs'); if (!el) return;
  el.textContent = 'loading…';
  const r = await api(`/api/anchor/services/${encodeURIComponent(id)}/logs`);
  el.textContent = r.ok ? (r.body.logs || '(no output)') : `error: ${(r.body && r.body.error) || r.status}`;
}

async function loadInspect(id) {
  const el = $('inspect'); if (!el) return;
  const res = await fetch(`/api/anchor/services/${encodeURIComponent(id)}`);
  if (!res.ok) { el.textContent = `error: ${res.status}`; return; }
  try { el.textContent = JSON.stringify(await res.json(), null, 2); }
  catch { el.textContent = await res.text(); }
}

async function loadAudit() {
  const r = await api('/api/anchor/audit');
  const body = $('audit-body');
  body.innerHTML = '';
  const entries = (r.ok && r.body.entries) || [];
  if (!entries.length) { body.innerHTML = '<tr><td colspan="5" class="muted">no entries yet</td></tr>'; return; }
  for (const e of entries) {
    const tr = document.createElement('tr');
    tr.innerHTML = `<td class="muted">${esc(e.ts)}</td><td>${esc(e.event)}</td><td>${esc((e.target || '').slice(0, 24))}</td><td class="muted">${esc(e.detail || '')}</td><td class="muted">${esc(e.ip || '')}</td>`;
    body.appendChild(tr);
  }
}

async function loadEnv() {
  const r = await api('/api/anchor/env');
  $('env-msg').textContent = '';
  if (!r.ok) { $('env-text').value = ''; $('env-path').textContent = (r.body && r.body.error) || 'error'; return; }
  const avail = !!r.body.available;
  $('env-path').textContent = r.body.path + (avail ? ' — masked' : ' (not mounted — read-only)');
  $('env-text').value = r.body.content || '';
  $('env-text').disabled = true;        // read-only until revealed
  $('env-save').disabled = true;
  $('env-reveal').disabled = !avail;
}

async function revealEnv() {
  const r = await api('/api/anchor/env?reveal=1');
  if (!r.ok) { $('env-msg').textContent = (r.body && r.body.error) || 'reveal failed'; return; }
  $('env-text').value = r.body.content || '';
  $('env-text').disabled = false;
  $('env-save').disabled = false;
  $('env-reveal').disabled = true;
  $('env-path').textContent = r.body.path + ' — revealed (editing)';
  $('env-msg').textContent = 'Secrets revealed — edit and Save.';
}

async function saveEnv() {
  const content = $('env-text').value;
  const put = (extra) => api('/api/anchor/env', { method: 'PUT', body: JSON.stringify({ content, ...extra }) });
  let r = await put({});
  if (r.status === 428 && r.body && r.body.needsConfirm) {
    const ok = await showConfirm('Confirm .env edit', r.body.impact);
    if (!ok) return;
    r = await put({ confirmToken: r.body.confirmToken });
  }
  const msg = $('env-msg');
  if (r.ok) {
    msg.textContent = `✓ Saved. ${r.body.note || ''}` + (r.body.backup ? ` (backup: ${r.body.backup})` : '');
  } else {
    msg.textContent = `✗ ${(r.body && r.body.error) || r.status}`;
  }
}

// ── Confirm modal ────────────────────────────────────────────────────────────

function showConfirm(title, impact) {
  return new Promise((resolve) => {
    $('modal-title').textContent = title;
    $('modal-impact').textContent = impact || 'Are you sure?';
    $('modal').classList.add('show');
    const done = (val) => {
      $('modal').classList.remove('show');
      $('modal-ok').onclick = null;
      $('modal-cancel').onclick = null;
      resolve(val);
    };
    $('modal-ok').onclick = () => done(true);
    $('modal-cancel').onclick = () => done(false);
  });
}

// ── Live updates ─────────────────────────────────────────────────────────────

function connectSSE() {
  if (sse) sse.close();
  sse = new EventSource('/api/anchor/events');
  sse.onopen = () => setConn(true);
  sse.onerror = () => setConn(false);
  sse.onmessage = (ev) => {
    if (ev.data === 'refresh' && !$('services-view').classList.contains('hidden')) loadServices();
  };
}

function setConn(live) {
  const c = $('conn');
  c.textContent = live ? 'live' : 'disconnected';
  c.className = 'pill ' + (live ? 'live' : 'dead');
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

boot();
