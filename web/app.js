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

// ── Theme (light/system/dark, matches Nucleus) ──────────────────────────────
// Uses the shared `nucleus-theme` cookie so Anchor's theme tracks the hub on the
// same host (cookies are not port-scoped).
const THEME_KEY = 'nucleus-theme';
function getCookie(n) { const m = document.cookie.match(new RegExp('(?:^|; )' + n + '=([^;]*)')); return m ? decodeURIComponent(m[1]) : null; }
function setCookie(n, v) { document.cookie = `${n}=${encodeURIComponent(v)}; path=/; max-age=31536000; SameSite=Lax`; }
let themePref = getCookie(THEME_KEY) || 'system';
function applyTheme() {
  const dark = themePref === 'dark' || (themePref === 'system' && matchMedia('(prefers-color-scheme: dark)').matches);
  document.documentElement.classList.toggle('dark', dark);
  document.querySelectorAll('#theme button').forEach((b) => b.classList.toggle('active', b.dataset.theme === themePref));
}
function setTheme(v) { themePref = v; setCookie(THEME_KEY, v); applyTheme(); }
document.querySelectorAll('#theme button').forEach((b) => b.addEventListener('click', () => setTheme(b.dataset.theme)));
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => { if (themePref === 'system') applyTheme(); });
applyTheme();

// Back to the Nucleus hub (same host, default port — Anchor runs on :8888).
$('hub').addEventListener('click', () => { location.href = `${location.protocol}//${location.hostname}/`; });

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
let allServices = [];   // last fetched list
let expandedId = null;  // id of the currently expanded row
let expansionEl = null; // cached expansion DOM (built once per open, not per render)
let refreshTimer = null; // coalesces bursts of SSE refresh events

function showApp() {
  $('gate').classList.add('hidden');
  $('app').classList.remove('hidden');
  $('pw').value = ''; if ($('pw2')) $('pw2').value = '';
  loadServices();
  connectSSE();
  resumeJob();
}

$('logout').addEventListener('click', async () => {
  await api('/api/anchor/logout', { method: 'POST' });
  if (sse) sse.close();
  location.reload();
});

$('tab-services').addEventListener('click', () => switchTab('services'));
$('tab-audit').addEventListener('click', () => switchTab('audit'));
$('tab-env').addEventListener('click', () => switchTab('env'));
$('tab-backups').addEventListener('click', () => switchTab('backups'));
$('refresh').addEventListener('click', loadServices);
$('svc-search').addEventListener('input', renderServices);
$('refresh-audit').addEventListener('click', loadAudit);
$('env-reload').addEventListener('click', loadEnv);
$('env-reveal').addEventListener('click', revealEnv);
$('env-save').addEventListener('click', saveEnv);
$('refresh-backups').addEventListener('click', loadBackups);

function switchTab(name) {
  for (const t of ['services', 'audit', 'env', 'backups']) {
    $('tab-' + t).classList.toggle('active', t === name);
    $(t + '-view').classList.toggle('hidden', t !== name);
  }
  if (name === 'services') loadServices();
  if (name === 'audit') loadAudit();
  if (name === 'env') loadEnv();
  if (name === 'backups') loadBackups();
}

const HEALTH_LABEL = { healthy: 'healthy', unhealthy: 'unhealthy', starting: 'starting', none: '—' };

async function loadServices() {
  const r = await api('/api/anchor/services');
  if (!r.ok) {
    allServices = [];
    $('svc-body').innerHTML = `<tr><td colspan="6" class="err">${esc((r.body && r.body.error) || 'failed to load')}</td></tr>`;
    $('svc-count').textContent = '0 services';
    return;
  }
  allServices = r.body.services || [];
  renderServices();
}

function renderServices() {
  const body = $('svc-body');
  body.innerHTML = '';
  const q = ($('svc-search').value || '').toLowerCase().trim();
  const list = q
    ? allServices.filter((s) => `${s.name} ${s.app} ${s.role} ${s.state}`.toLowerCase().includes(q))
    : allServices;
  $('svc-count').textContent = q
    ? `${list.length} of ${allServices.length} services`
    : `${allServices.length} service${allServices.length === 1 ? '' : 's'}`;

  let lastGroup = null;
  for (const s of list) {
    const group = s.app || '(infrastructure)';
    if (group !== lastGroup) {
      lastGroup = group;
      const g = document.createElement('tr');
      g.className = 'grouphdr';
      g.innerHTML = `<td colspan="6">${esc(group)}</td>`;
      body.appendChild(g);
    }
    body.appendChild(buildRow(s));
    if (s.id === expandedId) {
      // Reuse the existing expansion node so logs/inspect are NOT re-fetched on
      // every list refresh — only on open / explicit Reload.
      if (!expansionEl) expansionEl = buildExpansion(s);
      body.appendChild(expansionEl);
    }
  }
}

// A service row: inline Start/Restart/Stop actions; clicking elsewhere on the row
// toggles its in-place expansion.
function buildRow(s) {
  const running = s.state === 'running';
  const open = s.id === expandedId;
  const tr = document.createElement('tr');
  tr.className = 'svc' + (open ? ' open' : '');
  const stateColor = running ? 'var(--green)' : 'var(--muted)';
  tr.innerHTML = `
    <td><span class="caret">${open ? '▾' : '▸'}</span>${esc(s.name)}</td>
    <td>${s.app ? esc(s.app) : '<span class="muted">—</span>'}</td>
    <td><span class="tag">${esc(s.role || '—')}</span></td>
    <td style="color:${stateColor}">${esc(s.state)}</td>
    <td><span class="dot ${s.health}"></span>${HEALTH_LABEL[s.health] || esc(s.health)}</td>
    <td class="actcell">
      <button class="mini" data-a="start" ${running ? 'disabled' : ''}>Start</button>
      <button class="mini" data-a="restart" ${running ? '' : 'disabled'}>Restart</button>
      <button class="mini danger" data-a="stop" ${running ? '' : 'disabled'}>Stop</button>
    </td>`;
  tr.addEventListener('click', (e) => {
    if (e.target.closest('.actcell')) return; // row actions handle themselves
    if (open) { expandedId = null; expansionEl = null; }
    else { expandedId = s.id; expansionEl = buildExpansion(s); } // build once, on open
    renderServices();
  });
  tr.querySelectorAll('.actcell button').forEach((b) =>
    b.addEventListener('click', (e) => {
      e.stopPropagation();
      if (!b.disabled) doAction(s, b.dataset.a, $('svc-msg'));
    }));
  return tr;
}

// In-place expansion: details + the heavier actions (recreate/rebuild) + logs +
// inspect. Start/Stop/Restart live on the row above, so they are not repeated here.
function buildExpansion(s) {
  const tr = document.createElement('tr');
  tr.className = 'exp';
  const td = document.createElement('td');
  td.colSpan = 6;
  td.innerHTML = `
    <div class="muted" style="margin-bottom:10px">${esc(s.image)} — ${esc(s.status)}</div>
    <div class="actions">
      <button data-act="recreate" class="danger">Recreate</button>
      <button data-act="rebuild" class="danger">Rebuild</button>
    </div>
    <pre class="out hidden"></pre>
    <div class="row" style="margin:8px 0 0"><strong>Logs</strong><span class="muted">— last 300</span><span class="spacer"></span><button class="reload-logs">Reload</button></div>
    <pre class="logs">loading…</pre>
    <details><summary>Raw inspect</summary><pre class="inspect">loading…</pre></details>`;
  tr.appendChild(td);
  const out = td.querySelector('.out');
  td.querySelectorAll('button[data-act]').forEach((b) =>
    b.addEventListener('click', () => doAction(s, b.dataset.act, out)));
  td.querySelector('.reload-logs').addEventListener('click', () => loadLogsInto(s.id, td.querySelector('.logs')));
  loadLogsInto(s.id, td.querySelector('.logs'));
  loadInspectInto(s.id, td.querySelector('.inspect'));
  return tr;
}

// Runs any action; destructive ones go through the two-step confirm. `outEl` is
// where the result is written (the row message line, or the expansion output pre).
async function doAction(s, action, outEl) {
  const post = (extra) => api(`/api/anchor/services/${encodeURIComponent(s.id)}/${action}`,
    { method: 'POST', body: JSON.stringify(extra || {}) });
  let r = await post({});
  if (r.status === 428 && r.body && r.body.needsConfirm) {
    const ok = await showConfirm(`Confirm: ${action} ${s.name}`, r.body.impact);
    if (!ok) return;
    r = await post({ confirmToken: r.body.confirmToken });
  }
  if (outEl) {
    outEl.classList.remove('hidden');
    outEl.textContent = r.ok
      ? `✓ ${action} ok` + (r.body && r.body.output ? `\n\n${r.body.output}` : '')
      : `✗ ${action} failed: ${(r.body && r.body.error) || r.status}` + (r.body && r.body.output ? `\n\n${r.body.output}` : '');
  }
  setTimeout(loadServices, 600);
}

async function loadLogsInto(id, el) {
  if (!el) return;
  el.textContent = 'loading…';
  const r = await api(`/api/anchor/services/${encodeURIComponent(id)}/logs`);
  el.textContent = r.ok ? (r.body.logs || '(no output)') : `error: ${(r.body && r.body.error) || r.status}`;
}

async function loadInspectInto(id, el) {
  if (!el) return;
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

// ── Backups ───────────────────────────────────────────────────────────────────

function fmtBytes(b) {
  if (b == null) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let n = b, i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${i === 0 ? n : n.toFixed(n >= 100 ? 0 : 1)} ${units[i]}`;
}

function fmtDateTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  return isNaN(d.getTime()) ? esc(iso) : d.toLocaleString();
}

async function loadBackups() {
  const r = await api('/api/anchor/backups');
  const body = $('backups-body');
  $('backups-msg').textContent = '';
  body.innerHTML = '';
  if (!r.ok) {
    body.innerHTML = `<tr><td colspan="4" class="err">${esc((r.body && r.body.error) || 'failed to load')}</td></tr>`;
    return;
  }
  $('backups-dir').textContent = r.body.dir ? `Source: ${r.body.dir}` : '';
  const list = (r.body && r.body.backups) || [];
  if (!list.length) {
    body.innerHTML = '<tr><td colspan="4" class="muted">no backups yet</td></tr>';
    return;
  }
  for (const b of list) {
    const tr = document.createElement('tr');
    tr.innerHTML =
      `<td>${esc(b.name)}${b.hasMongo ? ' <span class="tag">MinIO + DB</span>' : ' <span class="tag">MinIO only</span>'}</td>` +
      `<td>${fmtBytes(b.size)}</td>` +
      `<td class="muted">${fmtDateTime(b.modified)}</td>` +
      `<td class="actcell" style="text-align:right"><button class="mini">Restore</button></td>`;
    tr.querySelector('button').addEventListener('click', () => restoreFlow(b.name));
    body.appendChild(tr);
  }
}

// ── Backup create + restore (single-flight job; corner bar / loader UI) ───────

let jobPoll = null;

// One warning screen; resolves true to advance, false to cancel. `final` styles
// it as the irreversible last step.
function showRestoreWarn({ name, impact, final }) {
  return new Promise((resolve) => {
    $('restore-pw').classList.add('hidden');
    $('restore-pw-err').classList.add('hidden');
    $('restore-step').textContent = final
      ? 'Restore backup — step 2 of 2: final confirmation'
      : 'Restore backup — step 1 of 2';
    $('restore-impact').innerHTML = final
      ? `You are about to replace all MinIO data with <strong>${esc(name)}</strong>. ` +
        `This is <strong>irreversible</strong>. Proceed only if you are sure.`
      : esc(impact || 'This will overwrite current data.');
    $('restore-next').textContent = 'Continue';
    $('restore-back').textContent = final ? 'Go back' : 'Cancel';
    $('restore-modal').classList.add('show');
    const done = (val) => {
      $('restore-modal').classList.remove('show');
      $('restore-next').onclick = null;
      $('restore-back').onclick = null;
      resolve(val);
    };
    $('restore-next').onclick = () => done(true);
    $('restore-back').onclick = () => done(false);
  });
}

// Final auth step: re-enter the Anchor password. Resolves to the typed password,
// or null if cancelled. `errMsg` re-prompts after a wrong password.
function askRestorePassword(name, errMsg) {
  return new Promise((resolve) => {
    $('restore-step').textContent = 'Authorize restore';
    $('restore-impact').innerHTML = `Re-enter your Anchor password to restore <strong>${esc(name)}</strong>.`;
    const pw = $('restore-pw');
    pw.classList.remove('hidden'); pw.value = '';
    const err = $('restore-pw-err');
    if (errMsg) { err.textContent = errMsg; err.classList.remove('hidden'); } else { err.classList.add('hidden'); }
    $('restore-next').textContent = 'Restore now';
    $('restore-back').textContent = 'Cancel';
    $('restore-modal').classList.add('show');
    pw.focus();
    const done = (val) => {
      $('restore-modal').classList.remove('show');
      pw.classList.add('hidden'); err.classList.add('hidden');
      $('restore-next').onclick = null; $('restore-back').onclick = null; pw.onkeydown = null;
      resolve(val);
    };
    $('restore-next').onclick = () => done(pw.value);
    $('restore-back').onclick = () => done(null);
    pw.onkeydown = (e) => { if (e.key === 'Enter') done(pw.value); };
  });
}

async function restoreFlow(name) {
  const reqRestore = (extra) =>
    api(`/api/anchor/backups/${encodeURIComponent(name)}/restore`,
      { method: 'POST', body: JSON.stringify(extra || {}) });

  // Step 0 — ask the server for a confirm token + impact summary.
  let r = await reqRestore({});
  if (r.status === 409) { $('backups-msg').textContent = 'A restore is already running.'; return resumeJob(); }
  if (!(r.status === 428 && r.body && r.body.needsConfirm)) {
    $('backups-msg').textContent = (r.body && r.body.error) || 'Could not start restore.';
    return;
  }
  const token = r.body.confirmToken, impact = r.body.impact;

  // Two-step warning. Both screens are client-side; the token is spent on execute.
  if (!await showRestoreWarn({ name, impact, final: false })) return;
  if (!await showRestoreWarn({ name, impact, final: true })) return;

  // Final gate: re-enter the Anchor password. The server verifies it before
  // consuming the token, so a wrong password lets us retry with the same token.
  let exec, err = '';
  for (;;) {
    const pw = await askRestorePassword(name, err);
    if (pw === null) return;                       // cancelled
    exec = await reqRestore({ confirmToken: token, password: pw });
    if (exec.status === 401) { err = 'Incorrect password — try again.'; continue; }
    if (exec.status === 429) { err = (exec.body && exec.body.error) || 'Too many attempts.'; continue; }
    break;
  }
  if (!(exec.ok || exec.status === 202)) {
    $('backups-msg').textContent = (exec.body && (exec.body.error || exec.body.impact)) || 'Restore failed to start.';
    return;
  }
  openRestoreLoader(name);
  pollJob();
}

// Loader overlay (full screen, restore only). Dismiss collapses it to the corner
// bar; both stay in sync via pollJob().
function openRestoreLoader(name) {
  $('restore-mini').classList.remove('show');
  $('restore-loader-title').textContent = 'Restoring backup…';
  $('restore-loader-name').textContent = name;
  $('restore-loader-phase').textContent = 'starting…';
  $('restore-loader-bar').className = 'bar indeterminate';
  $('restore-loader').classList.add('show');
}

function minimizeRestore() {
  $('restore-loader').classList.remove('show');
  $('restore-mini').classList.add('show');
}

$('restore-dismiss').addEventListener('click', minimizeRestore);
$('restore-mini-open').addEventListener('click', () => {
  $('restore-mini').classList.remove('show');
  $('restore-loader').classList.add('show');
});

// Render job state (backup OR restore) into the overlay + corner bar.
function applyJobState(st) {
  const isBackup = st.kind === 'backup';
  const verb = isBackup ? 'Backup' : 'Restore';
  const gerund = isBackup ? 'Backing up' : 'Restoring';
  // Full overlay (restore only — backups use just the corner bar)
  $('restore-loader-name').textContent = st.target || '';
  $('restore-loader-phase').textContent = st.error || st.phase || '';
  // Corner bar
  $('restore-mini-text').textContent = st.error ? `${verb} failed`
    : st.done ? `${verb} complete` : `${gerund}… ${st.phase || ''}`;
  // Reopening the full overlay only makes sense for a restore.
  $('restore-mini-open').style.display = isBackup ? 'none' : '';

  if (st.done) {
    const cls = st.error ? 'bar err' : 'bar done';
    $('restore-loader-bar').className = cls;
    $('restore-mini-bar').className = cls + ' sm';
    $('restore-mini-spin').style.display = 'none';
    $('restore-loader-title').textContent = st.error ? `${verb} failed` : `${verb} complete`;
  } else {
    $('restore-loader-bar').className = 'bar indeterminate';
    $('restore-mini-bar').className = 'bar indeterminate sm';
    $('restore-mini-spin').style.display = '';
  }
}

function pollJob() {
  if (jobPoll) clearInterval(jobPoll);
  const tick = async () => {
    const r = await api('/api/anchor/backups/status');
    if (!r.ok) return;
    applyJobState(r.body);
    if (r.body.done) {
      clearInterval(jobPoll); jobPoll = null;
      loadBackups();
      // Let the result linger, then clear the indicators.
      setTimeout(() => {
        $('restore-mini').classList.remove('show');
        $('restore-loader').classList.remove('show');
      }, 6000);
    }
  };
  tick();
  jobPoll = setInterval(tick, 1000);
}

// On (re)load, if a backup/restore is mid-flight, resurface the corner bar.
async function resumeJob() {
  const r = await api('/api/anchor/backups/status');
  if (r.ok && r.body && r.body.running) {
    $('restore-mini').classList.add('show');
    applyJobState(r.body);
    pollJob();
  }
}

// Create a backup on demand (MinIO / Mongo / both). Non-destructive, so no
// confirm — progress shows in the corner bar.
async function createBackup() {
  const sel = $('backup-target').value;                  // both | minio | mongo
  const r = await api('/api/anchor/backups/create', {
    method: 'POST',
    body: JSON.stringify({ minio: sel !== 'mongo', mongo: sel !== 'minio' }),
  });
  if (r.status === 409) { $('backups-msg').textContent = 'A backup or restore is already running.'; return resumeJob(); }
  if (!(r.ok || r.status === 202)) {
    $('backups-msg').textContent = (r.body && r.body.error) || 'Could not start backup.';
    return;
  }
  $('backups-msg').textContent = '';
  $('restore-mini').classList.add('show');
  pollJob();
}
$('create-backup').addEventListener('click', createBackup);

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
    if (ev.data !== 'refresh' || refreshTimer) return;
    // Coalesce bursts of events into at most one refresh per ~1.2s.
    refreshTimer = setTimeout(() => {
      refreshTimer = null;
      if (!$('services-view').classList.contains('hidden')) loadServices();
    }, 1200);
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
