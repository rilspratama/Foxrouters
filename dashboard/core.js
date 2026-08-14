/* ══════════════════════════════════════════════════════════
   AUTH — cookie-based (HttpOnly, set by /login)
   No more ?key= URL param or localStorage — key never touches JS.
   ══════════════════════════════════════════════════════════ */
// authKey kept empty for backward compat with updateAuthKey() UI.
// Real auth is via foxrouters_session cookie (HttpOnly, server-set).
let authKey = '';

function updateAuthKey() {
  // Legacy: redirect to login page for new session
  window.location.href = '/login';
}

function clearAuthKey() {
  doLogout();
}

// Logout: POST to /logout (not GET — prevents CSRF via <img> tag)
async function doLogout() {
  try {
    await fetch('/logout', { method: 'POST', credentials: 'same-origin' });
  } catch(e) {}
  window.location.href = '/login';
}

// Sidebar collapse toggle (icon rail) — persisted in localStorage
function toggleSidebar() {
  var sb = document.querySelector('.sidebar');
  if (!sb) return;
  var collapsed = sb.classList.toggle('collapsed');
  try { localStorage.setItem('fx_sidebar', collapsed ? '1' : '0'); } catch(e) {}
}
(function initSidebarState() {
  try { if (localStorage.getItem('fx_sidebar') === '1') document.querySelector('.sidebar').classList.add('collapsed'); } catch(e) {}
})();

function showCurrentKey() {
  var el = document.getElementById('currentKeyDisplay');
  if (el) {
    el.textContent = 'Session via login cookie';
  }
}

function hasUsableDefaultKey() {
  return false; // cookie-based now
}

async function fetchJSON(url) {
  var r = await fetch(url, { credentials: 'same-origin' });
  if (r.status === 401) {
    // D8: stop polling before navigating.
    window._stopped = true;
    window.location.href = '/login';
    // D7: throw a distinguishable sentinel so callers can swallow it.
    throw new Error('__redirect__');
  }
  if (!r.ok) {
    throw new Error('HTTP ' + r.status + ' on ' + url);
  }
  return r.json();
}

// D7: suppress the `__redirect__` sentinel so 15+ .catch(alert(e.message)) sites
// don't briefly flash "Error: __redirect__" while the page unloads.
window.addEventListener('unhandledrejection', function(ev) {
  if (ev.reason && ev.reason.message === '__redirect__') ev.preventDefault();
});
window.addEventListener('error', function(ev) {
  if (ev.error && ev.error.message === '__redirect__') ev.preventDefault();
});
// Wrap window.alert once so any lingering `alert('... ' + e.message)` calls
// during page teardown silently drop the sentinel string.
(function(){
  var _origAlert = window.alert.bind(window);
  window.alert = function(msg) {
    if (typeof msg === 'string' && msg.indexOf('__redirect__') !== -1) return;
    return _origAlert(msg);
  };
})();

// fetchJSON with method/body support for CRUD operations
async function apiFetch(url, opts) {
  opts = opts || {};
  var headers = {};
  if (opts.headers) { for (var k in opts.headers) { headers[k] = opts.headers[k]; } }
  var fetchOpts = { method: opts.method || 'GET', headers: headers, credentials: 'same-origin' };
  if (opts.body) { fetchOpts.body = opts.body; }
  var r = await fetch(url, fetchOpts);
  if (r.status === 401) {
    // D8: stop background polling BEFORE navigating so any in-flight interval
    // ticks after this point short-circuit instead of piling up 401s.
    window._stopped = true;
    window.location.href = '/login';
    // D7: throw a distinguishable sentinel — returning undefined caused
    // TypeError when callers destructured the (missing) result.
    throw new Error('__redirect__');
  }
  if (!r.ok && r.status !== 200) {
    var errBody = await r.text().catch(function() { return ''; });
    throw new Error('HTTP ' + r.status + ': ' + errBody);
  }
  return r.json();
}

/* ══════════════════════════════════════════════════════════
   ROUTING (hash-based SPA)
   ══════════════════════════════════════════════════════════ */
var routes = {
  '#/':         { page: 'page-dashboard', title: 'Dashboard',   meta: 'Live · auto-refresh 5s' },
  '#/accounts': { page: 'page-accounts',  title: 'Accounts & Keys', meta: 'Live · auto-refresh 5s' },
  '#/keys':     { page: 'page-keys',      title: 'Gateway API Keys', meta: 'Redis-backed · CRUD' },
  '#/models':   { page: 'page-models',    title: 'Models', meta: 'Usage stats · aliases · custom · combos' },
  '#/proxies':  { page: 'page-proxies',   title: 'Proxies', meta: 'HTTP/SOCKS5 upstream proxy pool · round-robin' },
  '#/tunnel':   { page: 'page-tunnel',    title: 'Tunnel', meta: 'Cloudflare Tunnel · embedded cloudflared · Redis config' },
  '#/media':    { page: 'page-media',     title: 'Media Studio', meta: 'Grok free tier · image 5 · video 2 per akun' },
  '#/settings': { page: 'page-settings',  title: 'Settings', meta: 'Turnstile Solver · lazy SSO refresh' }
};

function navigateTo(hash) {
  window.location.hash = hash;
}

function router() {
  var hash = window.location.hash || '#/';
  var route = routes[hash] || routes['#/'];

  // Pages
  document.querySelectorAll('.page').forEach(function(p) { p.classList.remove('active'); });
  var pageEl = document.getElementById(route.page);
  if (pageEl) pageEl.classList.add('active');

  // Scroll to top on page change
  window.scrollTo(0, 0);
  var content = document.getElementById('content');
  if (content) content.scrollTop = 0;

  // Nav items
  document.querySelectorAll('.nav-item').forEach(function(n) {
    n.classList.toggle('active', n.getAttribute('data-route') === hash);
  });

  // Title
  document.getElementById('pageTitle').textContent = route.title;
  document.getElementById('topbarMeta').textContent = route.meta;

  // Page-specific actions
  if (hash === '#/keys') {
    showCurrentKey();
    loadGatewayKeys();
  }
  if (hash === '#/accounts') {
    if (window._selProvider) {
      selectProvider(window._selProvider);
    } else {
      var ap = document.getElementById('accountsPanel');
      if (ap) ap.style.display = 'none';
    }
  }
  if (hash === '#/models') {
    loadModels();
  }
  if (hash === '#/proxies') {
    if (typeof loadProxies === 'function') loadProxies();
  }
  if (hash === '#/tunnel') {
    if (typeof loadTunnelStatus === 'function') {
      loadTunnelStatus();
      startTunnelPolling();
    }
  } else {
    if (typeof stopTunnelPolling === 'function') stopTunnelPolling();
  }
  if (hash === '#/settings') {
    if (typeof loadTurnstileSettings === 'function') loadTurnstileSettings();
    if (typeof loadFilterSettings === 'function') loadFilterSettings();
  }
  if (hash === '#/media') {
    if (typeof mediaInit === 'function') mediaInit();
  } else {
    if (typeof mediaPersistChat === 'function') mediaPersistChat();
  }
}

window.addEventListener('hashchange', router);

/* ══════════════════════════════════════════════════════════
   HELPERS
   ══════════════════════════════════════════════════════════ */
function circuitBadge(state) {
  var cls = state === 'closed' ? 'circuit-closed' : state === 'open' ? 'circuit-open' : 'circuit-half-open';
  return '<span class="circuit-badge ' + cls + '">' + state + '</span>';
}

function sdot(ok) {
  return '<span class="sdot ' + (ok ? 'sdot-ok' : 'sdot-err') + '"></span>';
}

function escHtml(s) {
  if (!s) return '';
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
}

// ══════════════════════════════════════════════════════════
// GLOBAL DELETE/EDIT HANDLERS (XSS-safe via data-* attributes)
// Replaces vulnerable inline onclick handlers. Event delegation
// on document so dynamically-rendered buttons auto-bind.
// ══════════════════════════════════════════════════════════
document.addEventListener('click', function(e) {
  var btn = e.target.closest('button[data-key], button[data-email], button[data-id], button[data-alias], button[data-name], button[data-model], button[data-token], button[data-proxy-id]');
  if (!btn) return;
  var v;
  if (btn.classList.contains('delete-cb-btn')) {
    v = btn.getAttribute('data-key');
    if (v) deleteCBKey(v);
  } else if (btn.classList.contains('test-cb-btn')) {
    v = btn.getAttribute('data-key');
    if (v) testCBKey(v, btn);
  } else if (btn.classList.contains('delete-grok-btn')) {
    v = btn.getAttribute('data-email');
    if (v) deleteGrokAccount(v);
  } else if (btn.classList.contains('test-grok-btn')) {
    v = btn.getAttribute('data-email');
    if (v) testGrokAccount(v, btn);
  } else if (btn.classList.contains('test-fb-btn')) {
    v = btn.getAttribute('data-token');
    if (v) testFBAccount(v, btn);
  } else if (btn.classList.contains('delete-fb-btn')) {
    v = btn.getAttribute('data-token');
    if (v) deleteFBAccount(v);
  } else if (btn.classList.contains('delete-key-btn')) {
    v = btn.getAttribute('data-key');
    if (v) deleteKey(v);
  } else if (btn.classList.contains('edit-key-btn')) {
    v = btn.getAttribute('data-key');
    if (v) showEditKeyModal(v);
  } else if (btn.classList.contains('delete-custom-btn')) {
    v = btn.getAttribute('data-id');
    if (v) deleteCustomModel(v);
  } else if (btn.classList.contains('delete-alias-btn')) {
    v = btn.getAttribute('data-alias');
    if (v) deleteAlias(v);
  } else if (btn.classList.contains('delete-combo-btn')) {
    v = btn.getAttribute('data-name');
    if (v) deleteCombo(v);
  } else if (btn.classList.contains('quick-test-btn')) {
    // D1/S3: XSS-safe replacement for inline onclick="quickTestModel(...)".
    // Custom-model ids can contain user input — passing them through an
    // attribute + textContent lookup avoids the JS-string escape hole.
    v = btn.getAttribute('data-model');
    if (v) quickTestModel(v, btn);
  } else if (btn.classList.contains('delete-proxy-btn')) {
    v = btn.getAttribute('data-proxy-id');
    if (v) deleteProxy(v);
  } else if (btn.classList.contains('toggle-proxy-btn')) {
    v = btn.getAttribute('data-proxy-id');
    if (v) toggleProxy(v);
  } else if (btn.classList.contains('test-proxy-btn')) {
    v = btn.getAttribute('data-proxy-id');
    if (v) testProxy(v, btn);
  } else if (btn.classList.contains('edit-proxy-btn')) {
    v = btn.getAttribute('data-proxy-id');
    if (v) showEditProxyModal(v);
  }
});

// ══════════════════════════════════════════════════════════
// PAGINATION HANDLERS (P4-2: replace legacy inline onclick)
// ══════════════════════════════════════════════════════════
document.addEventListener('click', function(e) {
  var btn = e.target.closest('button.pg-btn');
  if (!btn || btn.disabled) return;
  var action = btn.getAttribute('data-action');
  var page = btn.getAttribute('data-page');
  if (btn.classList.contains('pg-grok-prev') || btn.classList.contains('pg-grok-next') || btn.classList.contains('pg-grok-num')) {
    if (action === 'prev') window._grokPage = Math.max(1, (window._grokPage || 1) - 1);
    else if (action === 'next') window._grokPage = (window._grokPage || 1) + 1;
    else if (page) window._grokPage = parseInt(page, 10);
    loadGrokPage(window._grokPage, window._grokPageSize);
  } else if (btn.classList.contains('pg-cb-prev') || btn.classList.contains('pg-cb-next') || btn.classList.contains('pg-cb-num')) {
    if (action === 'prev') window._cbPage = Math.max(1, (window._cbPage || 1) - 1);
    else if (action === 'next') window._cbPage = (window._cbPage || 1) + 1;
    else if (page) window._cbPage = parseInt(page, 10);
    loadCBPage(window._cbPage, window._cbPageSize);
  }
});

/* ══════════════════════════════════════════════════════════
   REFRESH (health + accounts + cb-stats + models)
   ══════════════════════════════════════════════════════════ */
async function refresh() {
  if (window._stopped) return; // D8: skip after session expired
  try {
    var results = await Promise.all([
      fetchJSON('/health'),
      fetchJSON('/v1/models')
    ]);
    var health = results[0], models = results[1];

    // Version
    var verEl = document.getElementById('version');
    if (verEl && health.version) verEl.textContent = health.version;

    // Stats grid
    document.getElementById('grokCount').textContent = health.grok_accounts;
    var gDeltaEl = document.getElementById('grokDelta');
    var gBarUsedEl = document.getElementById('grokBarUsed');
    var gBarRestEl = document.getElementById('grokBarRest');
    if (health.grok_tokens_quota > 0) {
      var gPct = Math.round(health.grok_tokens_used / health.grok_tokens_quota * 100);
      var gClass = gPct >= 100 ? 'err' : (gPct >= 80 ? 'warn' : 'ok');
      if (gDeltaEl) { gDeltaEl.textContent = gPct + '%'; gDeltaEl.className = 'stat-delta ' + gClass; }
      if (gBarUsedEl && gBarRestEl) {
        var gw = Math.min(100, gPct);
        gBarUsedEl.style.width = gw + '%';
        gBarRestEl.style.width = (100 - gw) + '%';
        gBarUsedEl.className = 'stat-bar-seg used' + (gPct >= 100 ? ' err' : (gPct >= 80 ? ' warn' : ''));
      }
    } else if (gDeltaEl) { gDeltaEl.textContent = ''; gDeltaEl.className = 'stat-delta'; }
    document.getElementById('cbCount').textContent = health.cb_keys;
    document.getElementById('cbActive').innerHTML = '<span class="ok">' + health.cb_keys_active + ' active</span>';
    // CB aggregate credits → delta + quota bar
    var cbDeltaEl = document.getElementById('cbDelta');
    var cbBarUsedEl = document.getElementById('cbBarUsed');
    var cbBarRestEl = document.getElementById('cbBarRest');
    // CB aggregate credits → delta + quota bar (from /health; refresh no longer fetches /accounts)
    var cbUsed = health.cb_credits_used || 0, cbLimit = health.cb_credits_limit || 0;
    if (cbLimit > 0 && cbBarUsedEl && cbBarRestEl) {
      var cbPct = Math.round(cbUsed / cbLimit * 100);
      var cbClass = cbPct >= 100 ? 'err' : (cbPct >= 80 ? 'warn' : 'ok');
      if (cbDeltaEl) { cbDeltaEl.textContent = cbPct + '%'; cbDeltaEl.className = 'stat-delta ' + cbClass; }
      var cbw = Math.min(100, cbPct);
      cbBarUsedEl.style.width = cbw + '%';
      cbBarRestEl.style.width = (100 - cbw) + '%';
      cbBarUsedEl.className = 'stat-bar-seg used' + (cbPct >= 100 ? ' err' : (cbPct >= 80 ? ' warn' : ''));
    } else if (cbDeltaEl) { cbDeltaEl.textContent = ''; cbDeltaEl.className = 'stat-delta'; }
    document.getElementById('models').textContent = models.data ? models.data.length : '—';
    _availableModels = (models.data || []).map(function(m) { return m.id; }).sort();

    var g = health.upstreams.grok, cb = health.upstreams.codebuddy;
    var fb = health.upstreams.freebuff || {};

    // FB stats card
    document.getElementById('fbCount').textContent = health.fb_accounts || 0;
    var fbUsed = health.fb_quota_used || 0, fbLimit = health.fb_quota_limit || 0;
    var fbExhausted = health.fb_quota_exhausted || 0;
    var fbDeltaEl = document.getElementById('fbDelta');
    var fbBarUsedEl = document.getElementById('fbBarUsed');
    var fbBarRestEl = document.getElementById('fbBarRest');
    if (fbLimit > 0) {
      var fbPct = Math.round(fbUsed / fbLimit * 100);
      var fbClass = fbPct >= 100 ? 'err' : (fbPct >= 80 ? 'warn' : 'ok');
      document.getElementById('fbQuota').innerHTML = '<span class="' + fbClass + '">' + fbUsed.toFixed(1) + '/' + fbLimit.toFixed(0) + ' (' + fbPct + '%)</span>' + (fbExhausted > 0 ? ' <span class="err">⛔ ' + fbExhausted + ' exhausted</span>' : '');
      if (fbDeltaEl) { fbDeltaEl.textContent = fbUsed > 0 ? '+' + fbPct + '%' : '0%'; fbDeltaEl.className = 'stat-delta ' + fbClass; }
      if (fbBarUsedEl && fbBarRestEl) {
        var usedW = Math.min(100, fbPct);
        fbBarUsedEl.style.width = usedW + '%';
        fbBarRestEl.style.width = (100 - usedW) + '%';
        fbBarUsedEl.className = 'stat-bar-seg used' + (fbPct >= 100 ? ' err' : (fbPct >= 80 ? ' warn' : ''));
      }
    } else {
      document.getElementById('fbQuota').innerHTML = '<span class="ok">no quota data</span>';
      if (fbDeltaEl) { fbDeltaEl.textContent = ''; fbDeltaEl.className = 'stat-delta'; }
      if (fbBarUsedEl) fbBarUsedEl.style.width = '0%';
      if (fbBarRestEl) fbBarRestEl.style.width = '0%';
    }

    renderProviders(health);

    // Circuit cards
    document.getElementById('grokCircuit').innerHTML = circuitBadge(g.circuit_state);
    document.getElementById('cbCircuit').innerHTML = circuitBadge(cb.circuit_state);
    document.getElementById('fbCircuit').innerHTML = fb.circuit_state ? circuitBadge(fb.circuit_state) : '—';
    document.getElementById('grokLatencyVal').innerHTML = g.last_check_ok ? '<span class="ok">' + g.last_check_ms + 'ms</span>' : '<span class="err">down</span>';
    document.getElementById('cbLatencyVal').innerHTML = cb.last_check_ok ? '<span class="ok">' + cb.last_check_ms + 'ms</span>' : '<span class="err">down</span>';
    document.getElementById('fbLatencyVal').innerHTML = fb.last_check_ok ? '<span class="ok">' + fb.last_check_ms + 'ms</span>' : (fb.circuit_state ? '<span class="err">down</span>' : '—');
    document.getElementById('grokLatency').textContent = 'health check';
    document.getElementById('cbLatency').textContent = 'health check';
    document.getElementById('fbLatency').textContent = fb.circuit_state ? 'health check' : 'no probe';
    document.getElementById('grokCircuitSub').textContent = g.total_errors + ' errors';
    document.getElementById('cbCircuitSub').textContent = cb.total_errors + ' errors';
    document.getElementById('fbCircuitSub').textContent = (fb.total_errors || 0) + ' errors';
    document.getElementById('grokErrors').textContent = g.total_errors;
    document.getElementById('cbErrors').textContent = cb.total_errors;
    document.getElementById('fbErrors').textContent = fb.total_errors || 0;
    document.getElementById('grokErrRate').textContent = g.error_rate_pct.toFixed(1) + '% rate';
    document.getElementById('cbErrRate').textContent = cb.error_rate_pct.toFixed(1) + '% rate';
    document.getElementById('fbErrRate').textContent = fb.error_rate_pct ? fb.error_rate_pct.toFixed(1) + '% rate' : '—';

    // Sidebar status
    var dotEl = document.getElementById('sidebarDot');
    var statusEl = document.getElementById('sidebarStatus');
    var allOK = g.circuit_state !== 'open' && cb.circuit_state !== 'open' && (!fb.circuit_state || fb.circuit_state !== 'open');
    dotEl.className = 'status-dot' + (allOK ? '' : ' err');
    statusEl.textContent = allOK ? 'All systems operational' : 'Circuit open';

    // Upstream health table
    var hb = '';
    for (var name in health.upstreams) {
      if (!health.upstreams.hasOwnProperty(name)) continue;
      var u = health.upstreams[name];
      hb += '<tr>' +
        '<td class="mono">' + name + '</td>' +
        '<td>' + circuitBadge(u.circuit_state) + '</td>' +
        '<td>' + sdot(u.last_check_ok) + (u.last_check_ok ? 'OK' : 'FAIL') + '</td>' +
        '<td class="mono">' + u.last_check_ms + 'ms</td>' +
        '<td class="mono">' + u.total_requests + '</td>' +
        '<td class="mono">' + u.total_errors + '</td>' +
        '<td class="mono">' + u.error_rate_pct.toFixed(1) + '%</td>' +
        '<td class="muted" style="font-size:11px;max-width:200px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + escHtml(u.last_error_msg || '—') + '</td>' +
      '</tr>';
    }
    document.getElementById('healthBody').innerHTML = hb;

    // Grok stat sub (active/banned) from /health — no full account dump in 5s cycle
    var gActiveEl = document.getElementById('grokActive');
    if (gActiveEl) {
      gActiveEl.innerHTML = '<span class="ok">' + (health.grok_active || 0) + ' active</span>';
    }

    // Timestamp
    var ts_str = 'Updated ' + new Date().toLocaleTimeString();
    document.getElementById('lastUpdate').textContent = ts_str;
    document.getElementById('footerUpdate').textContent = ts_str;
  } catch (e) {
    document.getElementById('footerUpdate').innerHTML = '<span class="err-msg">Error: ' + escHtml(e.message) + '</span>';
    document.getElementById('sidebarStatus').textContent = 'Connection error';
    document.getElementById('sidebarDot').className = 'status-dot err';
  }
}

/* ══════════════════════════════════════════════════════════
   PROVIDERS GRID (Accounts & Keys entry point)
   ── compact cards, click = reveal that provider's table
   ══════════════════════════════════════════════════════════ */
function renderProviders(health) {
  var grid = document.getElementById('providersGrid');
  if (!grid) return;
  var defs = [
    { key: 'grok', name: 'Grok', sub: 'xAI · free tier', count: health.grok_accounts || 0, icon: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAYAAACqaXHeAAAOe0lEQVR42u1ba2xU1dp+1pq9Z5iZlimUoUq5t4UBKteW1iKIgSCo9VQIFLmoJJgQE0FNMP7AiCFGY7S/jCRKEH6gJUTwWFA4AgXSym2SsaW0FdqGFjpMW5x2epnbnr3f84PZ65tpuYMnH61vQrI77HV5n/W8l/WuvRiiQkQcABhjGhGZAoFAlizLLzHG5jHG0gEkcc5lAAz/v4U0TVMAdBBRHRGdUhTloNlsdjLGQrF6QleGiAyMMRUAAoFAmizLzxsMhuc0TZsIIAVAIgAT55zjMRBN0zQAIQBdAFo453+qqlqqKMoRs9lcH6szIyLGGCMikhRFmWEwGNYD+BfnPAX9SDRNawHwb1VVd8iy7GKMRYiIMSJiAKAoShZj7D1JkpYBkDVNUzjnhihL2GOqN0VNQo2arxKJRH4koiJZlp0AwBlj5PP50hhj6yVJWgpABoBoA/4YK6+bOI/qAgCyJElLGWProzoTJyKzyWRaxDlfBsAYRU1F/xM1qpuRc77MZDItIiIzDwQCM4xG43Oc82QAShQ1Qz8EQDdnhXOebDQanwsEAjO4LMv5nPOJALQo5fu7cAAa53yiLMv5HMA8TdOeiP7HQAGAR3WexyKRSIvBYEgEYMbAkoCqql2MiIIApH5q9yAiENH/hQXGwBjTnWKEEZH2mIe6h8JH6k/K66utrzhjDHfJ3pn0uCusaZpQ2GAw9FHY6/WiubkZlZWV6OrqQl5eHhwOB4xGI4gI0uNs05xzGAzxrquzsxNtbW3weDxoaGjAxYsXcenSJTidThARtmzZgjFjxjy+AMQ4sZvpnaoiGAyio6MDV65cQUVFBVwuFyoqKlBXV4f29nYAgNlsRnp6OkwmU5xTlB4nu+5t0263G1VVVTh//jz++OMPXLp0CU1NTfD7/QiHwwCAkSNHIjs7G3l5ecjKykJmZiYGDx4sGMQoFo7bUE0f/G6r8SiVVtWb2xFJil+ja9euoaqqCpWVlYLe165dQ3Nzs5jvkCFDMGvWLMyePRtZWVmYMGECUlNTkZSU1FeH2wFwH/vsRwJEb9uOldbWVjQ1NaGmpkbQu7q6Gh6PR7yTkJCAtLQ0TJ06FbNmzcLMmTMxefJkJCcn9xkjdr63BUDTNLEKnPM4BW9Fx0fNgHA4jI6ODtTU1OD3339HWVkZXC5XnNKyLMNqtWLcuHGYPXs2FixYgNzcXIwaNSouSsSytfdCSbcanDGGzs5O1NfXw+fzQZIkSJIEVVXBGIOqqpBlGU888QTGjx8vKGswGO6LCfqKaJom2mqahvr6epSXl+P48eNwOp1obm5GMBiEoiiircViQXZ2Np5//nk8++yzyMjIQFJSEmRZjjPR3lGit9zWCRoMBly/fh0//PADLl68CFmWYbFYoKqqiL3jxo3D8uXLsWjRIlitVmiaBk3T+jDmTkpzzsE5R3d3N1wuF8rKynD27FnU1tbC7Xajq6srrn1qaipyc3Mxd+5cZGVlISMjA8OHD+/D3nv1UbcFwGKxYPz48Zg0aRIqKytRUVHR550zZ86gsbERzc3NePHFFzFu3Li7rrZuOrr5eDwe1NTU4Pz58zh16hScTidaWlri2sqyjNTUVGRmZmLu3LmYN28epk+fjkGDBsU5zd593ysN+4imaXHPR48epdWrV5PdbieDwUCMMZIkiTjnBIBSUlLo7bffJqfTScFgkDRNi+ujt0QiEfJ6vVReXk5bt26l3NxcGjRoEOk1PMYYASBJkig5OZnmz59Pn3/+OV24cEH0q6oqRSIRUlX1jmPdTXAvL0UiEaqrq6NPPvmE0tPTxSQNBoOYdEJCAi1cuJD27NlDbW1tAjxVVUlRFDHJnp4eOnHiBL333ns0Y8YMSkhIEEDGKi/LMj3zzDNUVFREVVVV1NPT81CKPhAAugK6eDwe2rNnDy1ZsoRMJpNYJf0ZAOXl5dGhQ4coHA5TOBwWbf1+P5WWltIHH3xAc+bMoeTk5DilYxXPycmhDz/8kI4fPy7A1EVV1Yde9ViR7iXt1MNhSkoKVq5ciREjRmDkyJE4fPgwrl69ikgkArPZjPHjx+Opp56CzWYT3rinpwfV1dUoKyvDkSNHcPr0aXR2dgrb1p0q5xzp6emYN28elixZgoULF8JmswEAIpGICMePPPTeD1q63RERXb16lT777DPKzMykYcOG0UsvvUR79+6l7u5uIiJSFIWuXr1Ku3fvpvz8fBo8eLBYcUmSxDPnnJKSkmjhwoW0Y8cOam1tFewLBAJ/C+1j5aEywY6ODpSXl8Pn88HhcGDKlCkwmUzw+/04fvw4iouLUVpaitbWVrGK+srrMX3q1KkoLCzEyy+/jPT0dOHZXS4XmpqakJ2djREjRojQe7e4ft+bqwcBQE96ACAYDELTNFgsFhEaf/rpJxw7dgzV1dXw+/2CvpIkiU3K8OHDsXjxYixbtgw5OTlISbl5EldXV4eDBw/i0KFDaGtrw9NPP401a9Zgzpw5Ymw9T/mfm0DvyKCbAxFRY2Mj7dq1iwoKCshms8XR3Wg0ir9NJhNlZ2fTxx9/TBUVFaL9jRs36MCBA7RmzRoaO3aseN9sNlNBQQHt3buXWlpa4szxUZgHHraDQCBATqeTNm/eHDdxWZbjwhsAstls9Morr1BJSQmFQqG4furr62nTpk1kNpsFcAaDQfThcDjo008/pYaGhjjg/+cAxDrC9vZ22rlzJ82fP5+sVmucsowx4pwLBdLS0uijjz6impoaobyiKKQoCqmqSqFQiE6fPk1vvvmmCJF60qX3OXToUFq9ejWdOHGCFEURTIwN1X8bALGKExG5XC7avHkzORyOuElyzsXq6UxYtGgR7d69mzwej2hfXl5OW7ZsoW3btpHL5RK/X7p0iYqKiignJ0f0aTQaxRhWq5Xmz59PX3/9NbndbtEuHA4/kEnccyaoS0dHBx08eJBWr14dZ+s65WOzQ7vdTq+99hr99ttvYsUaGxtp3759tGLFChoyZAgNHz6c3njjDTpy5IgIoT09PVRcXEwFBQU0dOjQOCD05wkTJtCWLVvI5XLFze9+2XBfJtDa2krffPMNZWVlxaXDsQzQfx89ejS9//77VFdXJybm8XioqKiIHA5H3Pucc8rLy6OdO3eS2+0WK3nhwgXauHEjjRw5Ms6f6HmExWKhFStWxIH3yFNhHd3a2lravHkzjRo1qo9z05XQgZg5cyZt376dWlpaRLRQVZUuX75M69atI1mWb9l+1KhR9M4771BlZaUY3+1207fffkt5eXni3VjQZVmmadOm0RdffEHNzc0CbJ1xDwRAb3svLS2ltWvXkt1u7zMJxphQiDFGixcvpn379pHX673lgJWVlbRt2zbKyckhk8kknKXeb3JyMhUUFFBxcTH5/X5hdr/88gutW7dOmATnPA7IMWPG0IYNG6isrEyMpTvYO4lh69atW29VEeKcw+/34/Dhw/jqq6/w888/o6urSxQx9P23npwkJibihRdewMaNG5Gfnw+z2QyPx4MrV67A4/Hgxo0b8Hq9SEhIgMViwV9//YVr167B7/eDMQaj0SgKI7W1tWhsbEQoFMLQoUORmpqKjIwMpKWlwWQyob29HW1tbVBVFZIkgXOO9vZ2VFVVoaWlBZxz2O12JCYmgjEWV4u450ywq6sLv/76K7788kucO3dOgKJnYrGSmJiIgoICbNq0CbNmzUIwGERtbS1KSkpw/vx5BINBGI1GUTEym83w+Xyora3F9evX+2zA9HHsdjsKCwuxdu1aTJ06FYMGDUJPTw/279+PnTt34uzZswgEAiIz1NNlh8OB119/HYWFhRg9evSds8ZYe9edT0dHB23fvp0mT54svLoe03vHd5vNRu+++y5VV1cLup08eZJWrVoVV+S413+9TcJisdCCBQtoz549wiSCwSCdPXuW3nrrLRo2bJgwSb0d55zsdjutWrWKTp48GVdEueVmSN+O6nX3Xbt24bvvvkNDQ8Md02i73Y7169dj3bp1yMjIEEdTR48exdGjR9Ha2ipMRi+fx5bSdfpyzkFEaGpqwpkzZxAKhfqMNWnSJOTn5+PVV1/F9OnTxb6hpKQE33//PZxOZ582JpMJWVlZKCwsRH5+PsaOHXvnmmBLSwv27duH3bt3o6GhAVarNa60rFNUVVWMGDECy5cvx4YNGzB69Oi43drkyZMxZswYJCQk4HZ7rdjiqA5MY2MjDhw4gGPHjuH69esCnEgkgpqaGni9XnR2dmLFihXIyspCeno61q9fjyeffBLFxcU4c+YMfD6fqAaHQiGUl5fD6/XC6/Vi6dKlcDgckGVZ+ASJiMQKtLa2wu12Y9q0acjOzhYOpLdEIhHk5uaisLAQqampAiCDwQCr1QqHw/FAG7MpU6YgMzMTmZmZOHfunADJYDCAiKAoCnw+Hy5fvoy0tDRYrVYkJiZi5cqVmDhxIvbv34/6+npEIhFx+MkYg9/vh9vtRm1tLcaOHRsHgHCCRIRAIACfzyeUudNO2Wq1IiEh4W85GPH7/ejp6enDPP2wxmg0IjExUdQO9Pl3d3cjEAjEsSp2C20ymWCz2eKO2x76aOxW4eVOYedO7XU2PsiR2oOWyuIA0G0y9mChNz76b7qdPerDUb3G3/u7nt5fffRWWJ93b0B7nxL1nu9DM6A/fDOHfwD4B4B/ABjQAAxkJ0gcQBj9837AXY83AIS5qqq+KAgDTcKqqvo4EdVpmtatU2Ig0D6aOHUTUR0HcIpz7sHNCxPaAABAw80LEx4Ap7iiKCWapv0ZdYgDBQCuadqfiqKUcLPZ7AqHw6Wapv2FmzfG+vulKVnTtL/C4XCp2Wx2ccZYIBQK/UfTtB+jzrC/X5oKa5r2YygU+g9jLMCJiNlstnoi2hGJRPbj5s0xRO/fao+5Y6Sbqmj6B4ZKJBLZT0Q7ojqzf67ORvfNA/fydEwhYkBen/8viAU1e9CynzsAAAAASUVORK5CYII=" },
    { key: 'cb', name: 'CodeBuddy', sub: 'Tencent · trial', count: health.cb_keys || 0, icon: "data:image/svg+xml;base64,PHN2ZyB3aWR0aD0iNDAiIGhlaWdodD0iNDAiIHZpZXdCb3g9IjAgMCA0MCA0MCIgZmlsbD0ibm9uZSIgeG1sbnM9Imh0dHA6Ly93d3cudzMub3JnLzIwMDAvc3ZnIj4KPGcgY2xpcC1wYXRoPSJ1cmwoI2NsaXAwXzUxMTBfMTQ3NzEpIj4KPHJlY3Qgd2lkdGg9IjQwIiBoZWlnaHQ9IjQwIiByeD0iMjAiIGZpbGw9InVybCgjcGFpbnQwX2xpbmVhcl81MTEwXzE0NzcxKSIvPgo8ZyBmaWx0ZXI9InVybCgjZmlsdGVyMF9mXzUxMTBfMTQ3NzEpIj4KPGNpcmNsZSBjeD0iMTIuNjA3MSIgY3k9IjM0Ljk3OTQiIHI9IjE0LjA4MSIgZmlsbD0iIzMyRTZCOSIgZmlsbC1vcGFjaXR5PSIwLjQiLz4KPC9nPgo8ZyBmaWx0ZXI9InVybCgjZmlsdGVyMV9mXzUxMTBfMTQ3NzEpIj4KPGNpcmNsZSBjeD0iMzMuMzQwNCIgY3k9IjQzLjYxODUiIHI9IjEyLjg3MDQiIGZpbGw9IiMzMkU2QjkiLz4KPC9nPgo8cGF0aCBkPSJNMjcuNjg5NyAzLjM0MjZDMjguMDU3IDMuMDEzMjIgMjguMDc5NSAyLjk5OTgzIDI4LjM0ODUgMi45ODM2NkMyOC43ODUxIDIuOTUxNzcgMjkuMTg1OCAzLjE2MTA3IDI5Ljg2NjQgMy43ODA3MkMzMS40NTY2IDUuMjI1NzQgMzMuNjcwMyA4LjE5NzI0IDM1LjA0NyAxMC43MzQzTDM1LjU3OTYgMTEuNzE5MUwzNi4zMzA5IDEyLjA5MjdDMzcuMDU2MiAxMi40NTkzIDM4LjI0NTkgMTMuMjExMSAzOC43NDI4IDEzLjYxNEMzOC45NjczIDEzLjc5OTcgMzguOTk5MiAxMy44MDMyIDM5LjIzMjYgMTMuNzEyNEM0MC4yODcyIDEzLjMwMTcgNDEuNzk4MiAxMy44NDYgNDMuMTMwNyAxNS4xMjQ0QzQ0LjMzMDIgMTYuMjc0IDQ1LjQ3OSAxOC4yMzg0IDQ1LjkxOTEgMTkuODc3OEM0NS45ODMzIDIwLjE0MTcgNDYuMDY4MyAyMC43MDkgNDYuMDk5NCAyMS4xMzE0QzQ2LjE5OTcgMjIuNjE0NCA0NS43MjQzIDIzLjc5ODYgNDQuODA4NSAyNC4zMzQ5QzQ0LjYyMTMgMjQuNDQzIDQ0LjYwODUgMjQuNDcyNSA0NC42MTM3IDI0LjkzOUM0NC42NTU5IDI3LjE2MDQgNDQuMDU3MSAyOS4zNzc3IDQyLjg1NDMgMzEuNTRDNDEuNDk2NSAzMy45Njc4IDM5LjA3OTQgMzYuNDc5OCAzNS44MDczIDM4Ljg0NkMzNC4wNTAyIDQwLjEyNDcgMjkuODkxOCA0Mi41NDY5IDI4LjAxMjIgNDMuMzk3M0MyMy41MDk2IDQ1LjQyNDQgMTkuODk5OSA0Ni4yMDIyIDE2Ljc2NDUgNDUuODE4MUMxNC44OTQzIDQ1LjU5MTUgMTIuNzc3MSA0NC44NjExIDExLjUyNDYgNDQuMDEzN0MxMS4xOTUxIDQzLjc4NiAxMS4xNDI4IDQzLjc3MTkgMTAuODkwOSA0My44NDM5QzkuNTUwMTcgNDQuMjI4OSA3Ljc5NDQyIDQzLjQzNzQgNi4zMDI3MiA0MS43ODE3QzUuNzA3NzEgNDEuMTE5NyA0Ljc0NzA5IDM5LjQ5MzkgNC40MzU3NSAzOC42MjQxQzMuNzE1NjkgMzYuNTg4NyAzLjg1ODU5IDM0Ljc1MiA0LjgxNzgzIDMzLjY1NTFDNS4wNjU3IDMzLjM3MjUgNS4wNzM4NyAzMy4zNjA3IDUuMDE5NzIgMzIuODg1NkM0LjkzMDMzIDMyLjEwNzkgNC44ODk4MiAzMC45NTY2IDQuOTMwNjYgMzAuMjEzOEw0Ljk2MzQzIDI5LjUyMDVMMy45MjA5MSAyNy42Nzc3QzIuMzA3OCAyNC44MDc0IDEuMjgzMjYgMjIuMzk2OCAwLjg4Nzk4MiAyMC41NTU0QzAuNjc5NDAyIDE5LjU0NiAwLjY5Mjc4MSAxOS4wOTgxIDAuOTQ5MDYxIDE4Ljc2NjdDMS4xMDUyOSAxOC41NjY2IDEuNjE2MjIgMTguMzU5NCAyLjIzMjUyIDE4LjI0NTVDMy43ODUxOCAxNy45NzI5IDcuMTcyMDMgMTguMjE5OCAxMC45Mzk0IDE4Ljg4NUwxMS4zMzAzIDE4Ljk1MjVMMTIuMTkwMyAxOC4xOTIxQzEzLjYxNzkgMTYuOTI3NSAxNC41NjY1IDE2LjIxNzYgMTYuMzE1IDE1LjEyNzRDMTguMTM3MyAxMy45ODcyIDIwLjE5NDMgMTMuMDQ5OCAyMi41MTAzIDEyLjMwNzFMMjMuMjUzMyAxMi4wNjg3TDIzLjY2MTUgMTAuOTk2NEMyNS4xMjM5IDcuMTM1ODcgMjYuNjIxOCA0LjI4OTY5IDI3LjY4OTcgMy4zNDI2Wk0xNS40MzkzIDIzLjEyNjFDMTMuNzg2NCAyNC4wODA0IDEyLjk2MDIgMjQuNTU4MiAxMi4zNTI5IDI1LjA5M0M5Ljg5MzY0IDI3LjI1ODUgOC45NzM4IDMwLjY4ODggMTAuMDIwOCAzMy43OTM4QzEwLjI3OTQgMzQuNTYwNiAxMC43NTY4IDM1LjM4NyAxMS43MTExIDM3LjAzOThDMTIuNjY1MyAzOC42OTI2IDEzLjE0MjQgMzkuNTE5NCAxMy42NzcxIDQwLjEyNjdDMTUuODQyNiA0Mi41ODU5IDE5LjI3MzIgNDMuNTA0NSAyMi4zNzgzIDQyLjQ1NzRDMjMuMTQ1IDQyLjE5ODkgMjMuOTcyIDQxLjcyMjMgMjUuNjI0OCA0MC43NjhMMzUuMTMzMyAzNS4yNzgzQzM2Ljc4NjIgMzQuMzI0IDM3LjYxMjQgMzMuODQ2MiAzOC4yMTk2IDMzLjMxMTRDNDAuNjc4OSAzMS4xNDU5IDQxLjU5ODggMjcuNzE1NiA0MC41NTE4IDI0LjYxMDVDNDAuMjkzMiAyMy44NDM4IDM5LjgxNTcgMjMuMDE3NCAzOC44NjE1IDIxLjM2NDZDMzcuOTA3MiAxOS43MTE3IDM3LjQzMDIgMTguODg1IDM2Ljg5NTUgMTguMjc3N0MzNC43Mjk5IDE1LjgxODUgMzEuMjk5MyAxNC44OTk5IDI4LjE5NDMgMTUuOTQ3QzI3LjQyNzUgMTYuMjA1NSAyNi42MDA2IDE2LjY4MjEgMjQuOTQ3OCAxNy42MzY0TDE1LjQzOTMgMjMuMTI2MVoiIGZpbGw9InVybCgjcGFpbnQxX2xpbmVhcl81MTEwXzE0NzcxKSIvPgo8cmVjdCB4PSIxNS44ODE2IiB5PSIzMC4wMjczIiB3aWR0aD0iNC4wMDkwNCIgaGVpZ2h0PSI4LjMyNjQ2IiByeD0iMi4wMDQ1MiIgdHJhbnNmb3JtPSJyb3RhdGUoLTMwIDE1Ljg4MTYgMzAuMDI3MykiIGZpbGw9IndoaXRlIi8+CjxyZWN0IHg9IjI2LjY5NzgiIHk9IjIzLjc4MTIiIHdpZHRoPSI0LjAwOTA0IiBoZWlnaHQ9IjguMzI2NDYiIHJ4PSIyLjAwNDUyIiB0cmFuc2Zvcm09InJvdGF0ZSgtMzAgMjYuNjk3OCAyMy43ODEyKSIgZmlsbD0id2hpdGUiLz4KPC9nPgo8ZGVmcz4KPGZpbHRlciBpZD0iZmlsdGVyMF9mXzUxMTBfMTQ3NzEiIHg9Ii0xNC45MTYzIiB5PSI3LjQ1NTk5IiB3aWR0aD0iNTUuMDQ2OCIgaGVpZ2h0PSI1NS4wNDciIGZpbHRlclVuaXRzPSJ1c2VyU3BhY2VPblVzZSIgY29sb3ItaW50ZXJwb2xhdGlvbi1maWx0ZXJzPSJzUkdCIj4KPGZlRmxvb2QgZmxvb2Qtb3BhY2l0eT0iMCIgcmVzdWx0PSJCYWNrZ3JvdW5kSW1hZ2VGaXgiLz4KPGZlQmxlbmQgbW9kZT0ibm9ybWFsIiBpbj0iU291cmNlR3JhcGhpYyIgaW4yPSJCYWNrZ3JvdW5kSW1hZ2VGaXgiIHJlc3VsdD0ic2hhcGUiLz4KPGZlR2F1c3NpYW5CbHVyIHN0ZERldmlhdGlvbj0iNi43MjEyMyIgcmVzdWx0PSJlZmZlY3QxX2ZvcmVncm91bmRCbHVyXzUxMTBfMTQ3NzEiLz4KPC9maWx0ZXI+CjxmaWx0ZXIgaWQ9ImZpbHRlcjFfZl81MTEwXzE0NzcxIiB4PSI5LjQ2OTU5IiB5PSIxOS43NDc3IiB3aWR0aD0iNDcuNzQxNyIgaGVpZ2h0PSI0Ny43NDEiIGZpbHRlclVuaXRzPSJ1c2VyU3BhY2VPblVzZSIgY29sb3ItaW50ZXJwb2xhdGlvbi1maWx0ZXJzPSJzUkdCIj4KPGZlRmxvb2QgZmxvb2Qtb3BhY2l0eT0iMCIgcmVzdWx0PSJCYWNrZ3JvdW5kSW1hZ2VGaXgiLz4KPGZlQmxlbmQgbW9kZT0ibm9ybWFsIiBpbj0iU291cmNlR3JhcGhpYyIgaW4yPSJCYWNrZ3JvdW5kSW1hZ2VGaXgiIHJlc3VsdD0ic2hhcGUiLz4KPGZlR2F1c3NpYW5CbHVyIHN0ZERldmlhdGlvbj0iNS41MDAxOSIgcmVzdWx0PSJlZmZlY3QxX2ZvcmVncm91bmRCbHVyXzUxMTBfMTQ3NzEiLz4KPC9maWx0ZXI+CjxsaW5lYXJHcmFkaWVudCBpZD0icGFpbnQwX2xpbmVhcl81MTEwXzE0NzcxIiB4MT0iMjAiIHkxPSIwIiB4Mj0iMjAiIHkyPSI0MCIgZ3JhZGllbnRVbml0cz0idXNlclNwYWNlT25Vc2UiPgo8c3RvcCBzdG9wLWNvbG9yPSIjNkM0REZGIi8+CjxzdG9wIG9mZnNldD0iMSIgc3RvcC1jb2xvcj0iIzU4M0VEMyIvPgo8L2xpbmVhckdyYWRpZW50Pgo8bGluZWFyR3JhZGllbnQgaWQ9InBhaW50MV9saW5lYXJfNTExMF8xNDc3MSIgeDE9IjE0LjYzOSIgeTE9IjEwLjc5MTciIHgyPSIzMi4xNzEyIiB5Mj0iNDEuMTU4NSIgZ3JhZGllbnRVbml0cz0idXNlclNwYWNlT25Vc2UiPgo8c3RvcCBzdG9wLWNvbG9yPSJ3aGl0ZSIgc3RvcC1vcGFjaXR5PSIwLjgiLz4KPHN0b3Agb2Zmc2V0PSIwLjQzNzY4OSIgc3RvcC1jb2xvcj0id2hpdGUiLz4KPC9saW5lYXJHcmFkaWVudD4KPGNsaXBQYXRoIGlkPSJjbGlwMF81MTEwXzE0NzcxIj4KPHJlY3Qgd2lkdGg9IjQwIiBoZWlnaHQ9IjQwIiByeD0iMjAiIGZpbGw9IndoaXRlIi8+CjwvY2xpcFBhdGg+CjwvZGVmcz4KPC9zdmc+Cg==" },
    { key: 'fb', name: 'Freebuff', sub: 'Codebuff · free', count: health.fb_accounts || 0, icon: "data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCA1MTIgNTEyIj4KICA8cmVjdCB4PSIwIiB5PSIwIiB3aWR0aD0iNTEyIiBoZWlnaHQ9IjUxMiIgcng9IjQ1IiBmaWxsPSIjZmZmZmZmIi8+CiAgPHJlY3QgeD0iMzYiIHk9IjM2IiB3aWR0aD0iNDQwIiBoZWlnaHQ9IjQ0MCIgcng9IjE2IiBmaWxsPSIjMDAwMDAwIi8+CiAgPHBhdGggZD0iTSA3MiwxNjcgTCA3NSwxNjcgTCA3NiwxNjYgTCA4MSwxNjYgTCA4MiwxNjUgTCA4NSwxNjUgTCA4NiwxNjQgTCA4OCwxNjQgTCA4OSwxNjMgTCA5MSwxNjMgTCA5MiwxNjIgTCA5NCwxNjIgTCA5NSwxNjEgTCA5NywxNjEgTCA5OCwxNjAgTCA5OSwxNjAgTCAxMDAsMTU5IEwgMTAxLDE1OSBMIDEwMiwxNTggTCAxMDMsMTU4IEwgMTA1LDE1NiBMIDEwNiwxNTYgTCAxMDcsMTU1IEwgMTA4LDE1NSBMIDExMSwxNTIgTCAxMTIsMTUyIEwgMTIxLDE0MyBMIDEyMiwxNDEgTCAxMjQsMTM5IEwgMTI1LDEzNyBMIDEyNiwxMzYgTCAxMzAsMTI4IEwgMTMxLDEyNSBMIDEzMiwxMjMgTCAxMzMsMTIwIEwgMTM1LDExMiBMIDEzNywxMDIgTCAxMzgsMTAxIEwgMTQxLDEwMSBMIDE0MiwxMDIgTCAxNDMsMTA4IEwgMTQ0LDExMyBMIDE0NSwxMTcgTCAxNDgsMTI2IEwgMTUzLDEzNiBMIDE1NCwxMzcgTCAxNTUsMTM5IEwgMTU2LDE0MCBMIDE1NywxNDIgTCAxNjgsMTUzIEwgMTY5LDE1MyBMIDE3MSwxNTUgTCAxNzIsMTU1IEwgMTc0LDE1NyBMIDE3NSwxNTcgTCAxNzYsMTU4IEwgMTc3LDE1OCBMIDE3OCwxNTkgTCAxNzksMTU5IEwgMTgwLDE2MCBMIDE4MSwxNjAgTCAxODIsMTYxIEwgMTg0LDE2MSBMIDE4NSwxNjIgTCAxODcsMTYyIEwgMTg4LDE2MyBMIDE5MCwxNjMgTCAxOTEsMTY0IEwgMTkzLDE2NCBMIDE5NCwxNjUgTCAxOTcsMTY1IEwgMTk4LDE2NiBMIDIwMiwxNjYgTCAyMDMsMTY3IEwgMjA3LDE2NyBMIDIwNiwxNzEgTCAyMDUsMTcyIEwgMjAwLDE3MiBMIDE5OSwxNzMgTCAxOTUsMTczIEwgMTk0LDE3NCBMIDE5MiwxNzQgTCAxOTEsMTc1IEwgMTg4LDE3NSBMIDE4NywxNzYgTCAxODYsMTc2IEwgMTg1LDE3NyBMIDE4MywxNzcgTCAxODIsMTc4IEwgMTgxLDE3OCBMIDE4MCwxNzkgTCAxNzksMTc5IEwgMTc4LDE4MCBMIDE3NywxODAgTCAxNzYsMTgxIEwgMTc1LDE4MSBMIDE3NCwxODIgTCAxNzMsMTgyIEwgMTcxLDE4NCBMIDE3MCwxODQgTCAxNjcsMTg3IEwgMTY2LDE4NyBMIDE1OCwxOTUgTCAxNTcsMTk3IEwgMTU1LDE5OSBMIDE1NCwyMDEgTCAxNTMsMjAyIEwgMTQ5LDIxMCBMIDE0OCwyMTMgTCAxNDcsMjE1IEwgMTQ1LDIyMSBMIDE0NCwyMjUgTCAxNDMsMjMwIEwgMTQyLDIzNiBMIDE0MSwyMzcgTCAxMzcsMjM3IEwgMTM2LDIzMSBMIDEzNSwyMjYgTCAxMzQsMjIyIEwgMTMwLDIxMCBMIDEyNywyMDQgTCAxMjYsMjAzIEwgMTI1LDIwMSBMIDEyNCwyMDAgTCAxMjMsMTk4IEwgMTIxLDE5NiBMIDEyMCwxOTQgTCAxMTMsMTg3IEwgMTEyLDE4NyBMIDEwOSwxODQgTCAxMDgsMTg0IEwgMTA2LDE4MiBMIDEwNSwxODIgTCAxMDQsMTgxIEwgMTAzLDE4MSBMIDEwMiwxODAgTCAxMDEsMTgwIEwgMTAwLDE3OSBMIDk5LDE3OSBMIDk4LDE3OCBMIDk3LDE3OCBMIDk2LDE3NyBMIDk0LDE3NyBMIDkzLDE3NiBMIDkxLDE3NiBMIDkwLDE3NSBMIDg4LDE3NSBMIDg3LDE3NCBMIDg0LDE3NCBMIDgzLDE3MyBMIDgwLDE3MyBMIDc5LDE3MiBMIDc0LDE3MiBMIDczLDE3MSBMIDcyLDE3MSBaIiBmaWxsPSIjZmZmZmZmIi8+CiAgPHJlY3QgeD0iMjA4IiB5PSIyMzIiIHdpZHRoPSIxMzAiIGhlaWdodD0iMjkiIHJ4PSIxNC41IiBmaWxsPSIjZmZmZmZmIi8+Cjwvc3ZnPg==" }
  ];
  var sel = window._selProvider || '';
  grid.innerHTML = defs.map(function(p) {
    var isSel = sel === p.key;
    var badge = p.count > 0
      ? '<span style="display:inline-flex;align-items:center;gap:5px;padding:2px 8px;border-radius:20px;font-size:10px;color:var(--success);background:rgba(76,217,100,0.12);font-weight:600"><span style="width:5px;height:5px;border-radius:50%;background:var(--success);display:inline-block"></span>' + p.count + ' Connected</span>'
      : '<span style="color:var(--text-tertiary);font-size:10px">No connections</span>';
    return '<div onclick="selectProvider(\'' + p.key + '\')" data-provider="' + p.key + '" style="background:' + (isSel ? 'var(--bg-input)' : 'var(--bg-elevated)') + ';border:1px solid ' + (isSel ? 'var(--brand)' : 'var(--border)') + ';border-radius:10px;padding:10px 12px;display:flex;align-items:center;gap:10px;cursor:pointer;transition:border-color 150ms">' +
      '<div style="width:28px;height:28px;border-radius:7px;overflow:hidden;flex-shrink:0;border:1px solid var(--border)">' +
        (p.icon ? '<img src="' + p.icon + '" style="width:100%;height:100%;object-fit:cover" alt="" />' : '<div style="width:100%;height:100%;display:flex;align-items:center;justify-content:center;background:var(--bg-input);font-size:14px">🦊</div>') +
      '</div>' +
      '<div style="min-width:0">' +
        '<div style="font-size:13px;font-weight:600;color:var(--text-primary)">' + p.name + '</div>' +
        '<div style="font-size:10px;color:var(--text-tertiary);margin-top:2px">' + badge + '</div>' +
      '</div>' +
    '</div>';
  }).join('');
}

/* Click a provider card → reveal that provider's Accounts & Keys table */
function selectProvider(key) {
  window._selProvider = key;
  var panel = document.getElementById('accountsPanel');
  if (panel) panel.style.display = '';
  var cards = document.querySelectorAll('#providersGrid > div');
  cards.forEach(function(c) {
    var on = c.getAttribute('data-provider') === key;
    c.style.background = on ? 'var(--bg-input)' : 'var(--bg-elevated)';
    c.style.borderColor = on ? 'var(--brand)' : 'var(--border)';
  });
  showTab(null, key);
}

/* ══════════════════════════════════════════════════════════
   SERVER-SIDE PAGE LOADERS (Accounts & Keys)
   ── fetch ONE page per provider from the API, render from
      window._all* + window._*Total. No full dumps on 5s cycle.
   ══════════════════════════════════════════════════════════ */
async function loadGrokPage(page, size) {
  page = page || window._grokPage || 1;
  size = size || window._grokPageSize || 50;
  var body = document.getElementById('grokBody');
  if (body) body.innerHTML = '<tr><td colspan="7" style="text-align:center;padding:24px;color:var(--text-tertiary)">Loading…</td></tr>';
  try {
    var d = await fetchJSON('/accounts?upstream=grok&page=' + page + '&page_size=' + size);
    window._allGrok = d.grok || [];
    window._grokTotal = d.grok_total || 0;
    window._grokPage = page;
    window._grokPageSize = size;
  } catch (e) {
    window._allGrok = []; window._grokTotal = 0;
    if (body) body.innerHTML = '<tr><td colspan="7" style="text-align:center;padding:24px;color:var(--red)">Load failed: ' + escHtml(e.message) + '</td></tr>';
    return;
  }
  renderGrokPage();
}

async function loadCBPage(page, size) {
  page = page || window._cbPage || 1;
  size = size || window._cbPageSize || 50;
  var body = document.getElementById('cbBody');
  if (body) body.innerHTML = '<tr><td colspan="8" style="text-align:center;padding:24px;color:var(--text-tertiary)">Loading…</td></tr>';
  try {
    var d = await fetchJSON('/cb-stats?page=' + page + '&page_size=' + size);
    window._allCB = d.codebuddy_keys || [];
    window._cbTotal = d.cb_total || 0;
    window._cbPage = page;
    window._cbPageSize = size;
  } catch (e) {
    window._allCB = []; window._cbTotal = 0;
    if (body) body.innerHTML = '<tr><td colspan="8" style="text-align:center;padding:24px;color:var(--red)">Load failed: ' + escHtml(e.message) + '</td></tr>';
    return;
  }
  renderCBPage();
}

/* ══════════════════════════════════════════════════════════
   GROK PAGINATION
   ══════════════════════════════════════════════════════════ */
function renderGrokPage() {
  var all = window._allGrok || [];
  var page = window._grokPage || 1;
  var size = window._grokPageSize || 50;
  var total = window._grokTotal || all.length;
  var totalPages = Math.max(1, Math.ceil(total / size));
  if (page > totalPages) page = totalPages;
  if (page < 1) page = 1;
  window._grokPage = page;

  var start = 0;
  var end = all.length;
  var dispStart = (page - 1) * size + 1;
  var dispEnd = Math.min(dispStart + end - 1, total);
  var gRows = '';
  for (var j = start; j < end; j++) {
    var a = all[j];
    var ts = a.token_status || (a.disabled ? 'disabled' : 'active');
    var tsColor = ts === 'active' ? 'ok' : (ts === 'cooldown' ? 'warn' : 'err');
    var tsLabel = ts === 'active' ? '✅ active' : (ts === 'cooldown' ? '⏳ cooldown' : (ts === 'banned' ? '⛔ banned' : (ts === 'revoked' ? '⛔ revoked' : (ts === 'expired' ? '🔴 expired' : '❓ ' + ts))));
    var expAt = a.expires_at && a.expires_at !== '0001-01-01T00:00:00Z' ? new Date(a.expires_at).toLocaleString('en-GB', {month:'short',day:'2-digit',hour:'2-digit',minute:'2-digit',timeZone:'UTC'}) + ' UTC' : '—';
    var lastRef = a.last_refresh && a.last_refresh !== '0001-01-01T00:00:00Z' ? new Date(a.last_refresh).toLocaleString('en-GB', {month:'short',day:'2-digit',hour:'2-digit',minute:'2-digit',timeZone:'UTC'}) + ' UTC' : '—';
    var disAt = a.disabled_at && a.disabled_at !== '0001-01-01T00:00:00Z' ? new Date(a.disabled_at).toLocaleString('en-GB', {month:'short',day:'2-digit',hour:'2-digit',minute:'2-digit',timeZone:'UTC'}) + ' UTC' : '—';
    var periodEnd = a.period_end ? new Date(a.period_end).toLocaleString('en-GB', {month:'short',day:'2-digit',hour:'2-digit',minute:'2-digit',timeZone:'UTC'}) + ' UTC' : '—';
    var usageStr;
    var tokensUsed = a.tokens_used || 0;
    var quota = a.quota || 1000000;
    var usagePct = quota > 0 ? Math.min(100, Math.round(tokensUsed / quota * 100)) : 0;
    var tokensStr = tokensUsed >= 1000 ? (tokensUsed / 1000).toFixed(1) + 'K' : tokensUsed;
    var quotaStr = quota >= 1000000 ? (quota / 1000000) + 'M' : quota;
    if (a.on_demand_cap > 0) {
      usageStr = '$' + (a.on_demand_used / 100).toFixed(2) + ' / $' + (a.on_demand_cap / 100).toFixed(2) + ' PAYG';
    } else if (tokensUsed > 0) {
      var pctColor = usagePct >= 80 ? 'err' : (usagePct >= 50 ? 'warn' : 'ok');
      usageStr = '<span class="' + pctColor + '">' + tokensStr + ' / ' + quotaStr + ' (' + usagePct + '%)</span>';
    } else {
      usageStr = '<span class="ok">0 / ' + quotaStr + '</span>';
    }
    gRows += '<tr>' +
      '<td class="mono">' + (a.img_quota_limit > 0 ? '<span style="cursor:pointer;color:var(--accent);font-size:10px" onclick="toggleGrokQuota(' + j + ')">▶</span> ' : '') + escHtml(a.email) + '</td>' +
      '<td><span class="' + tsColor + '">' + tsLabel + '</span></td>' +
      '<td class="muted" style="font-size:11px">' + periodEnd + '</td>' +
      '<td class="muted" style="font-size:11px">' + usageStr + '</td>' +
      '<td class="muted" style="font-size:11px">' + expAt + '</td>' +
      '<td class="muted" style="font-size:11px">' + lastRef + '</td>' +
      '<td style="white-space:nowrap">' +
        '<button class="btn-ghost test-grok-btn" data-email="' + escHtml(a.email) + '" style="padding:3px 8px;font-size:12px;color:var(--brand);margin-right:4px">Test</button>' +
        '<button class="btn-ghost delete-grok-btn" data-email="' + escHtml(a.email) + '" style="padding:3px 8px;font-size:12px;color:var(--red)">Delete</button>' +
      '</td>' +
    '</tr>' + grokQuotaDetailRow(a, j);
  }
  document.getElementById('grokBody').innerHTML = gRows || '<tr><td colspan="7" class="history-empty">No Grok accounts loaded</td></tr>';

  // Pager
  var pager = document.getElementById('grokPager');
  var html = '';
  // Page size selector
  html += '<span>Show</span>';
  html += '<select onchange="loadGrokPage(1, parseInt(this.value))" style="background:var(--bg-elevated);border:1px solid var(--border);border-radius:var(--radius-sm);color:var(--text-primary);padding:3px 8px;font-size:12px">';
  [25, 50, 100, 200].forEach(function(n) {
    html += '<option value="' + n + '"' + (size === n ? ' selected' : '') + '>' + n + '</option>';
  });
  html += '</select>';
  // Prev
  html += '<button class="pg-btn pg-grok-prev" data-action="prev" style="padding:4px 10px;border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--bg-elevated);color:var(--text-secondary);cursor:pointer;font-size:12px"' + (page <= 1 ? ' disabled' : '') + '>‹ Prev</button>';
  // Page numbers
  var maxButtons = 7;
  var fromPg = Math.max(1, page - Math.floor(maxButtons / 2));
  var toPg = Math.min(totalPages, fromPg + maxButtons - 1);
  if (toPg - fromPg < maxButtons - 1) fromPg = Math.max(1, toPg - maxButtons + 1);
  if (fromPg > 1) {
    html += '<button class="pg-btn pg-grok-num" data-page="1" style="padding:4px 10px;border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--bg-elevated);color:var(--text-secondary);cursor:pointer;font-size:12px">1</button>';
    if (fromPg > 2) html += '<span style="padding:0 2px">…</span>';
  }
  for (var p = fromPg; p <= toPg; p++) {
    if (p === page) {
      html += '<button style="padding:4px 10px;border:1px solid var(--brand);border-radius:var(--radius-sm);background:var(--brand);color:#000;font-size:12px;cursor:default">' + p + '</button>';
    } else {
      html += '<button class="pg-btn pg-grok-num' + (p === page ? ' pg-active' : '') + '" data-page="' + p + '" style="padding:4px 10px;border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--bg-elevated);color:var(--text-secondary);cursor:pointer;font-size:12px">' + p + '</button>';
    }
  }
  if (toPg < totalPages) {
    if (toPg < totalPages - 1) html += '<span style="padding:0 2px">…</span>';
    html += '<button class="pg-btn pg-grok-num" data-page="' + totalPages + '" style="padding:4px 10px;border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--bg-elevated);color:var(--text-secondary);cursor:pointer;font-size:12px">' + totalPages + '</button>';
  }
  // Next
  html += '<button class="pg-btn pg-grok-next" data-action="next" style="padding:4px 10px;border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--bg-elevated);color:var(--text-secondary);cursor:pointer;font-size:12px"' + (page >= totalPages ? ' disabled' : '') + '>Next ›</button>';
  // Info
  html += '<span style="margin-left:auto">' + (total > 0 ? dispStart + '–' + dispEnd + ' of ' + total : '0 of 0') + '</span>';
  pager.innerHTML = html;
}

/* ══════════════════════════════════════════════════════════
   CODEBUDDY PAGINATION (mirrors Grok pattern)
   ══════════════════════════════════════════════════════════ */
function renderCBPage() {
  var all = window._allCB || [];
  var page = window._cbPage || 1;
  var size = window._cbPageSize || 50;
  var total = window._cbTotal || all.length;
  var totalPages = Math.max(1, Math.ceil(total / size));
  if (page > totalPages) page = totalPages;
  if (page < 1) page = 1;
  window._cbPage = page;

  var start = 0;
  var end = all.length;
  var dispStart = (page - 1) * size + 1;
  var dispEnd = Math.min(dispStart + end - 1, total);
  var cbRows = '';
  for (var j = start; j < end; j++) {
    var k = all[j];
    var isOAuth = k.cred_type === 'oauth';
    var limit = Number(k.credit_limit || 240);
    var used = Number(k.credits_used || 0);
    var remain = (k.credits_remain != null && k.credits_remain !== '')
      ? Number(k.credits_remain)
      : (limit - used);
    var pct = limit > 0 ? (used / limit) * 100 : 0;
    var color = pct > 90 ? 'pbar-red' : pct > 70 ? 'pbar-orange' : 'pbar-green';
    var status = k.disabled ? '<span class="err">disabled</span>' : '<span class="ok">active</span>';
    var typeBadge = isOAuth
      ? '<span style="padding:2px 8px;border-radius:10px;font-size:10px;background:rgba(255,255,255,0.16);color:#ffffff">OAuth</span>'
      : '<span style="padding:2px 8px;border-radius:10px;font-size:10px;background:rgba(255,255,255,0.08);color:#b3b3b3">API Key</span>';
    var identity = isOAuth
      ? escHtml(k.email || k.key)
      : escHtml(k.key);
    var exp = '—';
    if (isOAuth && k.expires_at && k.expires_at !== '0001-01-01T00:00:00Z') {
      exp = new Date(k.expires_at).toLocaleString('en-GB', {year:'numeric',month:'short',day:'2-digit',hour:'2-digit',minute:'2-digit',timeZone:'UTC'}) + ' UTC';
    }
    cbRows += '<tr>' +
      '<td>' + typeBadge + '</td>' +
      '<td class="mono">' + identity + '</td>' +
      '<td class="mono">' + used.toFixed(1) + '/' + limit + '</td>' +
      '<td><div class="pbar"><div class="pbar-fill ' + color + '" style="width:' + pct + '%"></div></div>' + remain.toFixed(1) + ' left</td>' +
      '<td class="mono">' + (k.total_requests || 0) + '</td>' +
      '<td class="muted" style="font-size:11px">' + exp + '</td>' +
      '<td>' + status + '</td>' +
      '<td style="white-space:nowrap">' +
        '<button class="btn-ghost test-cb-btn" data-key="' + escHtml(k.key) + '" style="padding:3px 8px;font-size:12px;color:var(--brand);margin-right:4px">Test</button>' +
        '<button class="btn-ghost delete-cb-btn" data-key="' + escHtml(k.key) + '" style="padding:3px 8px;font-size:12px;color:var(--red)">Delete</button>' +
      '</td>' +
    '</tr>';
  }
  document.getElementById('cbBody').innerHTML = cbRows || '<tr><td colspan="8" class="history-empty">No CodeBuddy keys loaded</td></tr>';

  // Pager
  var pager = document.getElementById('cbPager');
  if (!pager) return;
  var html = '';
  html += '<span>Show</span>';
  html += '<select onchange="loadCBPage(1, parseInt(this.value))" style="background:var(--bg-elevated);border:1px solid var(--border);border-radius:var(--radius-sm);color:var(--text-primary);padding:3px 8px;font-size:12px">';
  [25, 50, 100, 200].forEach(function(n) {
    html += '<option value="' + n + '"' + (size === n ? ' selected' : '') + '>' + n + '</option>';
  });
  html += '</select>';
  html += '<button class="pg-btn pg-cb-prev" data-action="prev" style="padding:4px 10px;border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--bg-elevated);color:var(--text-secondary);cursor:pointer;font-size:12px"' + (page <= 1 ? ' disabled' : '') + '>‹ Prev</button>';
  var maxButtons = 7;
  var fromPg = Math.max(1, page - Math.floor(maxButtons / 2));
  var toPg = Math.min(totalPages, fromPg + maxButtons - 1);
  if (toPg - fromPg < maxButtons - 1) fromPg = Math.max(1, toPg - maxButtons + 1);
  if (fromPg > 1) {
    html += '<button class="pg-btn pg-cb-num" data-page="1" style="padding:4px 10px;border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--bg-elevated);color:var(--text-secondary);cursor:pointer;font-size:12px">1</button>';
    if (fromPg > 2) html += '<span style="padding:0 2px">…</span>';
  }
  for (var p = fromPg; p <= toPg; p++) {
    if (p === page) {
      html += '<button style="padding:4px 10px;border:1px solid var(--brand);border-radius:var(--radius-sm);background:var(--brand);color:#000;font-size:12px;cursor:default">' + p + '</button>';
    } else {
      html += '<button class="pg-btn pg-cb-num" data-page="' + p + '" style="padding:4px 10px;border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--bg-elevated);color:var(--text-secondary);cursor:pointer;font-size:12px">' + p + '</button>';
    }
  }
  if (toPg < totalPages) {
    if (toPg < totalPages - 1) html += '<span style="padding:0 2px">…</span>';
    html += '<button class="pg-btn pg-cb-num" data-page="' + totalPages + '" style="padding:4px 10px;border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--bg-elevated);color:var(--text-secondary);cursor:pointer;font-size:12px">' + totalPages + '</button>';
  }
  html += '<button class="pg-btn pg-cb-next" data-action="next" style="padding:4px 10px;border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--bg-elevated);color:var(--text-secondary);cursor:pointer;font-size:12px"' + (page >= totalPages ? ' disabled' : '') + '>Next ›</button>';
  html += '<span style="margin-left:auto">' + (total > 0 ? dispStart + '–' + dispEnd + ' of ' + total : '0 of 0') + '</span>';
  pager.innerHTML = html;
}

/* ══════════════════════════════════════════════════════════
   TABS (Accounts page)
   ══════════════════════════════════════════════════════════ */
function showTab(e, tab) {
  document.getElementById('cbPanel').style.display = tab === 'cb' ? '' : 'none';
  document.getElementById('grokPanel').style.display = tab === 'grok' ? '' : 'none';
  // Hide pager of inactive tab (cbPager sits outside grokPanel)
  var cbPagerEl = document.getElementById('cbPager');
  if (cbPagerEl) cbPagerEl.style.display = tab === 'cb' ? 'flex' : 'none';
  // Toggle add/bulk/cleanup buttons to match active tab
  document.getElementById('cbAddBtn').style.display = tab === 'cb' ? '' : 'none';
  var oauthBtn = document.getElementById('cbOAuthBtn');
  if (oauthBtn) oauthBtn.style.display = tab === 'cb' ? '' : 'none';
  var oauthBulkBtn = document.getElementById('cbOAuthBulkBtn');
  if (oauthBulkBtn) oauthBulkBtn.style.display = tab === 'cb' ? '' : 'none';
  document.getElementById('cbBulkBtn').style.display = tab === 'cb' ? '' : 'none';
  var syncBtn = document.getElementById('cbSyncCreditsBtn');
  if (syncBtn) syncBtn.style.display = tab === 'cb' ? '' : 'none';
  document.getElementById('cbCleanupBtn').style.display = tab === 'cb' ? '' : 'none';
  document.getElementById('grokAddBtn').style.display = tab === 'grok' ? '' : 'none';
  document.getElementById('grokBulkBtn').style.display = tab === 'grok' ? '' : 'none';
  document.getElementById('grokCleanupBtn').style.display = tab === 'grok' ? '' : 'none';
  document.getElementById('grokCleanupBannedBtn').style.display = tab === 'grok' ? '' : 'none';
  document.getElementById('grokSyncBillingBtn').style.display = tab === 'grok' ? '' : 'none';
  // Selector mode bar only relevant for CodeBuddy tab
  var selBar = document.getElementById('cbSelectorBar');
  if (selBar) selBar.style.display = tab === 'cb' ? '' : 'none';
  if (tab === 'cb') { loadCBSelectorMode(); loadCBPage(); }
  // Grok selector mode bar
  var grokSelBar = document.getElementById('grokSelectorBar');
  if (grokSelBar) grokSelBar.style.display = tab === 'grok' ? 'flex' : 'none';
  if (tab === 'grok') { loadGrokSelectorMode(); loadGrokPage(); }
  // Freebuff panel + buttons
  var fbPanelEl = document.getElementById('fbPanel');
  if (fbPanelEl) fbPanelEl.style.display = tab === 'fb' ? 'block' : 'none';
  document.getElementById('fbAddBtn').style.display = tab === 'fb' ? '' : 'none';
  document.getElementById('fbBulkBtn').style.display = tab === 'fb' ? '' : 'none';
  document.getElementById('fbOAuthBtn').style.display = tab === 'fb' ? '' : 'none';
  document.getElementById('fbRefreshBtn').style.display = tab === 'fb' ? '' : 'none';
  var fbStreakBtn = document.getElementById('fbStreakBtn');
  if (fbStreakBtn) fbStreakBtn.style.display = tab === 'fb' ? '' : 'none';
  if (tab === 'fb') loadFBAccounts();
}

/* ══════════════════════════════════════════════════════════
   FREEBUFF ACCOUNTS
   ══════════════════════════════════════════════════════════ */
async function loadFBAccounts() {
  try {
    var data = await fetchJSON('/fb/accounts');
    var body = document.getElementById('fbBody');
    if (!data || !data.accounts || data.accounts.length === 0) {
      body.innerHTML = '<tr><td colspan="7" style="text-align:center;padding:24px;color:var(--text-tertiary);font-size:13px">No Freebuff tokens. Click "+ Add Token" to import.</td></tr>';
      return;
    }
    body.innerHTML = data.accounts.map(function(a, idx) {
      var statusBadge;
      if (a.status === 'active') {
        statusBadge = '<span style="color:var(--success);font-weight:600">● active</span>';
      } else if (a.status === 'banned') {
        statusBadge = '<span style="color:var(--danger);font-weight:700">⛔ banned</span>' + (a.banned_at ? ' <span class="mono" style="font-size:10px;color:var(--text-tertiary)">' + escHtml(String(a.banned_at).replace('T', ' ').substring(0, 16)) + '</span>' : '');
      } else if (a.status === 'cooldown') {
        statusBadge = '<span style="color:var(--warning);font-weight:600">● cooldown</span>';
      } else {
        statusBadge = '<span style="color:var(--danger);font-weight:600">● disabled</span>';
      }
      var quota = a.quota_limit > 0 ? (a.quota_recent + '/' + a.quota_limit) : '—';
      var tierBadge = '—';
      if (a.tier === 'full') tierBadge = '<span style="color:var(--success);font-weight:600">full</span>';
      else if (a.tier === 'limited') tierBadge = '<span style="color:var(--warning);font-weight:600">limited</span>';
      else if (a.tier === 'blocked') tierBadge = '<span style="color:var(--danger);font-weight:600">blocked</span>';
      var hasQuota = a.quota_by_model && Object.keys(a.quota_by_model).length > 0;
      return '<tr>' +
        '<td class="mono" style="font-size:12px">' + (hasQuota ? '<span style="cursor:pointer;color:var(--accent);font-size:10px" onclick="toggleFbQuota(' + idx + ')">▶</span> ' : '') + escHtml(a.token) + '</td>' +
        '<td>' + escHtml(a.email || '—') + '</td>' +
        '<td class="mono" style="font-size:11px;color:var(--text-tertiary)">' + escHtml(a.user_id ? a.user_id.substring(0,8) + '…' : '—') + '</td>' +
        '<td>' + quota + '</td>' +
        '<td>' + tierBadge + (a.country_code ? ' <span class="mono" style="font-size:10px;color:var(--text-tertiary)">' + escHtml(a.country_code) + '</span>' : '') + '</td>' +
        '<td>' + statusBadge + '</td>' +
        '<td><button class="btn-ghost test-fb-btn" data-token="' + escHtml(a.token_full) + '" style="padding:3px 10px;font-size:11px;color:var(--brand);margin-right:4px">Test</button><button class="btn-ghost delete-fb-btn" data-token="' + escHtml(a.token_full) + '" style="padding:3px 10px;font-size:11px;color:var(--danger)">Delete</button></td>' +
        '</tr>' +
        fbQuotaDetailRow(a, idx);
    }).join('');
  } catch (e) {
    var body = document.getElementById('fbBody');
    if (body) body.innerHTML = '<tr><td colspan="7" style="text-align:center;padding:24px;color:var(--danger);font-size:13px">Failed to load: ' + escHtml(e.message) + '</td></tr>';
  }
}

// fbQuotaDetailRow builds the hidden per-model quota row for one Freebuff account.
function fbQuotaDetailRow(a, idx) {
  if (!a.quota_by_model) return '';
  var names = Object.keys(a.quota_by_model).sort();
  if (names.length === 0) return '';
  var rows = names.map(function(m) {
    var q = a.quota_by_model[m] || {};
    var short = escHtml(m.replace(/^(deepseek|openai|minimax|z-ai|meta)\//, '').replace('-2.1-contributor', ''));
    var color = 'var(--text-primary)';
    if (q.limit > 0 && q.recent_count >= q.limit) color = 'var(--danger)';
    else if (q.limit > 0 && q.recent_count / q.limit >= 0.8) color = 'var(--warning)';
    var pct = q.limit > 0 ? Math.round(q.recent_count / q.limit * 100) + '%' : '—';
    var ent = 'base ' + (q.entitlement_base || 0);
    if (q.entitlement_referral) ent += ' +ref ' + q.entitlement_referral;
    if (q.entitlement_streak) ent += ' +str ' + q.entitlement_streak;
    var reset = q.reset_at ? q.reset_at.replace('T', ' ').replace('.000Z', '').replace('Z', '') : '—';
    return '<tr>' +
      '<td class="mono" style="font-size:11px">' + short + '</td>' +
      '<td style="font-size:12px;color:' + color + ';font-weight:600">' + (q.recent_count || 0) + '/' + (q.limit || 0) + '</td>' +
      '<td style="font-size:11px;color:var(--text-tertiary)">' + pct + '</td>' +
      '<td style="font-size:11px;color:var(--text-tertiary)">' + ent + '</td>' +
      '<td class="mono" style="font-size:10px;color:var(--text-tertiary)">' + escHtml(reset) + '</td>' +
      '</tr>';
  }).join('');
  return '<tr id="fbq-' + idx + '" style="display:none;background:rgba(255,255,255,0.02)"><td colspan="7" style="padding:0">' +
    '<div style="padding:8px 12px 12px 32px">' +
    '<table class="tbl" style="font-size:12px"><thead><tr><th>Model</th><th>Used/Limit</th><th>Used %</th><th>Entitlement</th><th>Reset</th></tr></thead><tbody>' + rows + '</tbody></table>' +
    '</div></td></tr>';
}

function toggleFbQuota(idx) {
  var el = document.getElementById('fbq-' + idx);
  if (!el) return;
  var hidden = el.style.display === 'none';
  el.style.display = hidden ? '' : 'none';
}

// grokQuotaDetailRow builds the hidden per-account free-tier quota row
// (Image / Video / Chat from console.x.ai GET /v1/usage) for one Grok account.
function grokQuotaDetailRow(a, idx) {
  if (!a.img_quota_limit && !a.vid_quota_limit && !a.chat_quota_limit) return '';
  function bar(kind, used, limit) {
    if (!limit) return '';
    var pct = Math.min(100, Math.round(used / limit * 100));
    var color = used >= limit ? 'var(--danger)' : (pct >= 80 ? 'var(--warning)' : 'var(--success)');
    return '<tr>' +
      '<td style="font-size:11px;color:var(--text-tertiary)">' + kind + '</td>' +
      '<td style="font-size:12px;color:' + color + ';font-weight:600">' + used + ' / ' + limit + '</td>' +
      '<td style="min-width:120px"><div style="height:6px;background:rgba(255,255,255,0.08);border-radius:3px;overflow:hidden"><div style="height:100%;width:' + pct + '%;background:' + color + '"></div></div></td>' +
      '<td style="font-size:11px;color:var(--text-tertiary)">' + pct + '%</td>' +
      '</tr>';
  }
  var synced = a.quota_synced_at && a.quota_synced_at !== '0001-01-01T00:00:00Z' ? new Date(a.quota_synced_at).toLocaleString('en-GB', {month:'short',day:'2-digit',hour:'2-digit',minute:'2-digit',timeZone:'UTC'}) + ' UTC' : '—';
  var rows = bar('🖼️ Image', a.img_quota_used || 0, a.img_quota_limit || 0) +
             bar('🎬 Video', a.vid_quota_used || 0, a.vid_quota_limit || 0) +
             bar('💬 Chat', a.chat_quota_used || 0, a.chat_quota_limit || 0);
  return '<tr id="grokq-' + idx + '" style="display:none;background:rgba(255,255,255,0.02)"><td colspan="7" style="padding:0">' +
    '<div style="padding:8px 12px 12px 32px">' +
    '<table class="tbl" style="font-size:12px"><thead><tr><th>Quota</th><th>Used/Limit</th><th></th><th>%</th></tr></thead><tbody>' + rows + '</tbody></table>' +
    '<div style="font-size:10px;color:var(--text-tertiary);margin-top:4px">synced ' + synced + '</div>' +
    '</div></td></tr>';
}

function toggleGrokQuota(idx) {
  var el = document.getElementById('grokq-' + idx);
  if (!el) return;
  var hidden = el.style.display === 'none';
  el.style.display = hidden ? '' : 'none';
}

// syncGrokQuota fires POST /grok/quota/sync (admin) then reloads the Grok page.
async function syncGrokQuota() {
  try {
    await apiFetch('/grok/quota/sync', { method: 'POST' });
    await loadGrokPage(window._grokPage, window._grokPageSize);
  } catch (e) {
    alert('Quota sync failed: ' + e.message);
  }
}

function showAddFBModal() {
  var m = document.getElementById('fbModal');
  if (m) { m.style.display = 'flex'; return; }
  // Create modal dynamically
  m = document.createElement('div');
  m.id = 'fbModal';
  m.className = 'modal-overlay';
  m.style.cssText = 'display:flex;position:fixed;inset:0;background:rgba(0,0,0,0.5);z-index:1000;align-items:center;justify-content:center';
  m.onclick = function(e) { if (e.target === m) m.style.display = 'none'; };
  m.innerHTML = '<div style="background:var(--bg-secondary);border:1px solid var(--border);border-radius:var(--radius);width:90%;max-width:480px;padding:24px">' +
    '<h3 style="margin:0 0 16px;font-size:16px">Add Freebuff Token</h3>' +
    '<div style="margin-bottom:12px">' +
    '<label style="display:block;margin-bottom:6px;font-size:13px;color:var(--text-secondary)">Auth Token (UUID)</label>' +
    '<input id="fbTokenInput" type="text" placeholder="UUID token (e.g. a1b2c3d4-...)" style="width:100%;padding:8px 12px;background:var(--bg-input);border:1px solid var(--border);border-radius:var(--radius-sm);color:var(--text-primary);font-family:var(--mono);font-size:13px" />' +
    '<p style="font-size:12px;color:var(--text-tertiary);margin-top:6px;line-height:1.5">Get token from <code style="font-family:var(--mono);color:var(--text-secondary)">extract_freebuff.py login</code></p>' +
    '</div>' +
    '<div style="display:flex;gap:8px;justify-content:flex-end">' +
    '<button class="btn-ghost" onclick="document.getElementById(\'fbModal\').style.display=\'none\'" style="padding:8px 16px;font-size:13px">Cancel</button>' +
    '<button class="btn-primary" onclick="submitAddFB()" style="padding:8px 16px;border-radius:var(--radius-sm);background:var(--brand);color:#000;border:none;font-size:13px;font-weight:500;cursor:pointer">Import</button>' +
    '</div></div>';
  document.body.appendChild(m);
}

async function submitAddFB() {
  var token = document.getElementById('fbTokenInput').value.trim();
  if (!token) { alert('Token is required'); return; }
  try {
    var r = await apiFetch('/fb/import', { method: 'POST', body: JSON.stringify({ token: token }) });
    if (r.error) { alert('Import failed: ' + r.error); return; }
    document.getElementById('fbModal').style.display = 'none';
    loadFBAccounts();
  } catch (e) {
    alert('Error: ' + e.message);
  }
}

function showBulkFBModal() {
  var existing = document.getElementById('fbBulkModal');
  if (existing) { existing.remove(); }
  var m = document.createElement('div');
  m.id = 'fbBulkModal';
  m.className = 'modal-overlay';
  m.style.cssText = 'display:flex;position:fixed;inset:0;background:rgba(0,0,0,0.5);z-index:1000;align-items:center;justify-content:center';
  m.onclick = function(e) { if (e.target === m) m.remove(); };
  m.innerHTML = '<div style="background:var(--bg-secondary);border:1px solid var(--border);border-radius:var(--radius);width:90%;max-width:560px;padding:24px">' +
    '<h3 style="margin:0 0 16px;font-size:16px">Bulk Import Freebuff Tokens</h3>' +
    '<div style="margin-bottom:8px;font-size:12px;color:var(--text-tertiary)">Format: <code style="font-family:var(--mono)">token|email|userid</code> (one per line). Email + userid optional. Also accepts bare UUID.</div>' +
    '<div style="margin-bottom:16px">' +
    '<textarea id="fbBulkInput" placeholder="token1|email1@example.com|userid1\ntoken2|email2@example.com|userid2\ntoken3" rows="10" style="width:100%;padding:8px 12px;border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--bg-primary);color:var(--text-primary);font-family:var(--mono);font-size:12px;resize:vertical"></textarea>' +
    '</div>' +
    '<div id="fbBulkResult" style="margin-bottom:12px;display:none;padding:10px;border-radius:var(--radius-sm);font-size:13px"></div>' +
    '<div style="display:flex;gap:8px;justify-content:flex-end">' +
    '<button class="btn-ghost" onclick="document.getElementById(\'fbBulkModal\').remove()" style="padding:8px 16px;font-size:13px">Cancel</button>' +
    '<button class="btn-primary" onclick="submitBulkFB()" style="padding:8px 16px;border-radius:var(--radius-sm);background:var(--brand);color:#000;border:none;font-size:13px;font-weight:500;cursor:pointer">Import</button>' +
    '</div></div>';
  document.body.appendChild(m);
}

async function submitBulkFB() {
  var input = document.getElementById('fbBulkInput').value.trim();
  if (!input) { alert('Paste at least one token'); return; }
  var result = document.getElementById('fbBulkResult');
  var btn = event.target;
  btn.disabled = true;
  btn.textContent = 'Importing...';
  result.style.display = 'block';
  result.style.color = 'var(--text-tertiary)';
  result.textContent = 'Importing...';

  try {
    var r = await apiFetch('/fb/import/bulk', { method: 'POST', body: JSON.stringify({ raw: input }) });
    if (r.error) {
      result.style.color = 'var(--danger)';
      result.textContent = 'Error: ' + r.error;
    } else {
      result.style.color = 'var(--success)';
      result.textContent = '✓ Added: ' + r.added + ' · Failed: ' + r.failed + ' · Total: ' + r.total;
      if (r.errors && r.errors.length > 0) {
        result.innerHTML += '<br><span style="color:var(--danger);font-size:12px">' + r.errors.join('<br>') + '</span>';
      }
      loadFBAccounts();
    }
  } catch (e) {
    result.style.color = 'var(--danger)';
    result.textContent = 'Error: ' + e.message;
  }
  btn.disabled = false;
  btn.textContent = 'Import';
}

/* ── Freebuff OAuth device flow (Login URL) ── */
var fbOAuthState = null;

function showFBOAuthModal() {
  var existing = document.getElementById('fbOAuthModal');
  if (existing) { existing.remove(); }
  var m = document.createElement('div');
  m.id = 'fbOAuthModal';
  m.className = 'modal-overlay';
  m.style.cssText = 'display:flex;position:fixed;inset:0;background:rgba(0,0,0,0.5);z-index:1000;align-items:center;justify-content:center';
  m.onclick = function(e) { if (e.target === m) m.remove(); };
  m.innerHTML = '<div style="background:var(--bg-secondary);border:1px solid var(--border);border-radius:var(--radius);width:90%;max-width:520px;padding:24px">' +
    '<h3 style="margin:0 0 16px;font-size:16px">Add Freebuff via Login URL</h3>' +
    '<div id="fbOAuthContent" style="margin-bottom:16px">' +
    '<p style="font-size:13px;color:var(--text-tertiary);line-height:1.6;margin-bottom:12px">Click "Generate Login URL" to start the OAuth device flow. Open the URL in your browser, log in with Google/GitHub, and the token will be auto-imported.</p>' +
    '<button class="btn-primary" onclick="startFBOAuth()" style="padding:8px 16px;border-radius:var(--radius-sm);background:var(--brand);color:#000;border:none;font-size:13px;font-weight:500;cursor:pointer">Generate Login URL</button>' +
    '</div>' +
    '<div style="display:flex;gap:8px;justify-content:flex-end">' +
    '<button class="btn-ghost" onclick="document.getElementById(\'fbOAuthModal\').remove()" style="padding:8px 16px;font-size:13px">Close</button>' +
    '</div></div>';
  document.body.appendChild(m);
}

async function startFBOAuth() {
  var content = document.getElementById('fbOAuthContent');
  content.innerHTML = '<p style="font-size:13px;color:var(--text-tertiary)">Generating login URL...</p>';
  try {
    var r = await apiFetch('/fb/oauth/device/start', { method: 'POST', body: '{}' });
    console.log('fb oauth start response:', r);
    if (r.error) {
      content.innerHTML = '<p style="color:var(--danger);font-size:13px">Error: ' + escHtml(r.error) + '</p>';
      return;
    }
    fbOAuthState = r;
    content.innerHTML =
      '<div style="margin-bottom:12px">' +
      '<label style="display:block;margin-bottom:6px;font-size:13px;color:var(--text-secondary)">Login URL (open in browser)</label>' +
      '<div style="display:flex;gap:8px">' +
      '<input id="fbOAuthUrl" readonly value="' + escHtml(r.auth_url) + '" style="flex:1;padding:8px 12px;background:var(--bg-input);border:1px solid var(--border);border-radius:var(--radius-sm);color:var(--text-primary);font-family:var(--mono);font-size:12px" />' +
      '<button class="btn-ghost" onclick="navigator.clipboard.writeText(document.getElementById(\'fbOAuthUrl\').value);this.textContent=\'Copied!\'" style="padding:8px 12px;font-size:12px;white-space:nowrap">Copy</button>' +
      '<button class="btn-ghost" onclick="window.open(document.getElementById(\'fbOAuthUrl\').value,\'_blank\')" style="padding:8px 12px;font-size:12px;white-space:nowrap">Open</button>' +
      '</div></div>' +
      '<div id="fbOAuthPollStatus" style="padding:10px;border-radius:var(--radius-sm);background:var(--bg-elevated);font-size:13px;color:var(--text-tertiary)">Waiting for login... <span class="spinner"></span></div>' +
      '<button class="btn-primary" onclick="pollFBOAuth()" style="margin-top:12px;padding:8px 16px;border-radius:var(--radius-sm);background:var(--brand);color:#000;border:none;font-size:13px;font-weight:500;cursor:pointer">Check Status</button>';
    // Auto-poll every 3s
    fbOAuthPollInterval = setInterval(pollFBOAuth, 3000);
    pollFBOAuth(); // immediate first check
  } catch (e) {
    content.innerHTML = '<p style="color:var(--danger);font-size:13px">Error: ' + escHtml(e.message) + '</p>';
  }
}

var fbOAuthPollInterval = null;

async function pollFBOAuth() {
  if (!fbOAuthState) return;
  var statusEl = document.getElementById('fbOAuthPollStatus');
  if (!statusEl) return;
  try {
    var url = '/fb/oauth/device/poll?fingerprint_id=' + encodeURIComponent(fbOAuthState.fingerprint_id) +
      '&fingerprint_hash=' + encodeURIComponent(fbOAuthState.fingerprint_hash) +
      '&expires_at=' + encodeURIComponent(fbOAuthState.expires_at);
    var r = await fetchJSON(url);
    console.log('fb oauth poll response:', r);
    if (r.status === 'ready') {
      clearInterval(fbOAuthPollInterval);
      statusEl.innerHTML = '<span style="color:var(--success)">✓ Login successful! Token imported.</span><br>' +
        '<span style="font-size:12px;color:var(--text-tertiary)">Email: ' + escHtml(r.email || '—') + ' · User: ' + escHtml(r.user_id || '—') + '</span>';
      loadFBAccounts();
      setTimeout(function() {
        var modal = document.getElementById('fbOAuthModal');
        if (modal) modal.remove();
      }, 3000);
    } else if (r.status === 'error') {
      clearInterval(fbOAuthPollInterval);
      statusEl.innerHTML = '<span style="color:var(--danger)">✗ ' + escHtml(r.error || 'Unknown error') + '</span>';
    } else {
      statusEl.innerHTML = 'Waiting for login... <span class="spinner"></span>';
    }
  } catch (e) {
    console.error('fb poll error:', e.message);
    statusEl.innerHTML = '<span style="color:var(--danger)">Poll error: ' + escHtml(e.message) + '</span>';
  }
}

async function deleteFBAccount(token) {
  if (!confirm('Delete this Freebuff token?')) return;
  try {
    await apiFetch('/fb/accounts/' + encodeURIComponent(token), { method: 'DELETE' });
    loadFBAccounts();
  } catch (e) {
    alert('Delete failed: ' + e.message);
  }
}

/* Test a Freebuff token directly against the chat upstream (fb/deepseek-v4-flash) */
async function testFBAccount(token, btn) {
  var orig = btn ? btn.innerHTML : 'Test';
  if (btn) { btn.disabled = true; btn.innerHTML = 'Testing…'; }
  try {
    var d = await apiFetch('/fb/accounts/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token: token })
    });
    if (d.ok) {
      var msg = '✅ OK (' + d.latency_ms + 'ms) · ' + d.model + ' → "' + (d.content || '').slice(0, 60) + '"';
      alert(msg);
    } else {
      alert('❌ Failed (' + (d.status || '?') + ') · ' + (d.error || 'unknown error'));
    }
  } catch (e) {
    alert('Test error: ' + e.message);
  } finally {
    if (btn) { btn.disabled = false; btn.innerHTML = orig; }
  }
}

/* Fire ads + streak check-in for all Freebuff accounts (daily streak keeper) */
async function runFBStreakCheckin() {
  var btn = document.getElementById('fbStreakBtn');
  var orig = btn ? btn.innerHTML : '';
  if (btn) { btn.disabled = true; btn.innerHTML = 'Checking…'; }
  try {
    var d = await apiFetch('/fb/streak/checkin', { method: 'POST' });
    alert('✅ Streak check-in done · ' + d.checked + ' ok / ' + d.failed + ' failed');
  } catch (e) {
    alert('Streak check-in failed: ' + e.message);
  } finally {
    if (btn) { btn.disabled = false; btn.innerHTML = orig; }
  }
}

/* ══════════════════════════════════════════════════════════
   QUICK TEST
   ══════════════════════════════════════════════════════════ */
async function runTest() {
  var model = document.getElementById('testModel').value;
  var prompt = document.getElementById('testPrompt').value;
  var btn = document.getElementById('testBtn');
  var result = document.getElementById('testResult');
  btn.innerHTML = '<span class="spinner"></span> Sending';
  btn.disabled = true;
  result.classList.add('show');
  result.textContent = 'Waiting for response...';
  try {
    var r = await fetch('/v1/chat/completions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...(authKey ? { 'Authorization': 'Bearer ' + authKey } : {}) },
      body: JSON.stringify({ model: model, messages: [{ role: 'user', content: prompt }], stream: false, max_completion_tokens: 100 })
    });
    var d = await r.json();
    if (d.error) {
      result.innerHTML = '<span class="err">Error ' + r.status + '</span>: ' + escHtml(d.error);
    } else {
      var tokens = d.usage ? d.usage.total_tokens : '?';
      result.innerHTML = '<span class="ok">OK ' + r.status + ' · ' + tokens + ' tokens</span>\n\n' + escHtml(d.choices && d.choices[0] ? d.choices[0].message.content : '—');
    }
  } catch (e) {
    result.innerHTML = '<span class="err">Error</span>: ' + escHtml(e.message);
  }
  btn.textContent = 'Send';
  btn.disabled = false;
}

/* ══════════════════════════════════════════════════════════
   HISTORY
   ══════════════════════════════════════════════════════════ */
var historyLogs = [];

async function loadHistory() {
  if (window._stopped) return; // D8: skip after session expired
  try {
    var results = await Promise.all([
      fetchJSON('/history?hours=24'),
      fetchJSON('/history/recent?limit=50')
    ]);
    var statsRes = results[0], recentRes = results[1];

    // Update Total Tokens card
    var s = statsRes;
    var ttEl = document.getElementById('totalTokens');
    if (ttEl) {
      ttEl.textContent = (s.total_tokens || 0).toLocaleString();
      var subEl = document.getElementById('totalTokensSub');
      if (subEl) subEl.textContent = 'in:' + (s.total_tokens_in||0) + ' out:' + (s.total_tokens_out||0);
    }

    // Update Cache Hit avg card (from recent requests that have cache_hit_pct)
    var recentRows = (recentRes.recent_requests || []);
    var cacheHits = recentRows.filter(function(r) { return r.cache_hit_pct !== undefined && r.cache_hit_pct >= 0; });
    var cacheEl = document.getElementById('cacheHitAvg');
    var cacheSubEl = document.getElementById('cacheHitSub');
    if (cacheEl) {
      if (cacheHits.length > 0) {
        var avg = cacheHits.reduce(function(a, r) { return a + r.cache_hit_pct; }, 0) / cacheHits.length;
        cacheEl.textContent = avg.toFixed(1) + '%';
        if (cacheSubEl) cacheSubEl.textContent = 'avg over ' + cacheHits.length + ' reqs';
        var cacheBarSeg = document.getElementById('cacheBarSeg');
        if (cacheBarSeg) cacheBarSeg.style.width = Math.min(100, avg) + '%';
      } else {
        cacheEl.textContent = '—';
        if (cacheSubEl) cacheSubEl.textContent = 'prompt cache';
        var cacheBarSeg0 = document.getElementById('cacheBarSeg');
        if (cacheBarSeg0) cacheBarSeg0.style.width = '0%';
      }
    }

    // History stats cards
    var statsEl = document.getElementById('historyStats');
    var errClass = s.total_errors > 0 ? 'err' : '';
    statsEl.innerHTML =
      '<div class="history-stat"><div class="label">Requests (24h)</div><div class="value">' + s.total_requests + '</div></div>' +
      '<div class="history-stat"><div class="label">Errors</div><div class="value ' + errClass + '">' + s.total_errors + '</div></div>' +
      '<div class="history-stat"><div class="label">Error Rate</div><div class="value ' + errClass + '">' + s.error_rate_pct.toFixed(1) + '%</div></div>' +
      '<div class="history-stat"><div class="label">Avg Latency</div><div class="value">' + Math.round(s.avg_latency_ms) + 'ms</div></div>';
    var avgLatCard = document.getElementById('avgLatencyVal');
    if (avgLatCard) {
      var avgMs = Math.round(s.avg_latency_ms);
      var latClass = avgMs > 10000 ? 'err' : avgMs > 5000 ? 'warn' : 'ok';
      avgLatCard.innerHTML = '<span class="' + latClass + '">' + avgMs + 'ms</span>';
    }

    // Recent table
    var body = document.getElementById('historyBody');
    historyLogs = recentRes.recent_requests || [];
    if (historyLogs.length === 0) {
      body.innerHTML = '<tr><td colspan="7" class="history-empty">No requests logged yet</td></tr>';
      return;
    }
    body.innerHTML = historyLogs.map(function(l, i) {
      var sc = l.status_code;
      var cls = sc >= 500 ? 'stcode-5xx' : sc >= 400 ? 'stcode-4xx' : 'stcode-2xx';
      var tok = (l.tokens_in + l.tokens_out) > 0 ? (l.tokens_in + '/' + l.tokens_out) : '—';
      var err = l.error_msg ? ' <span style="opacity:0.5" title="' + escHtml(l.error_msg) + '">!</span>' : '';
      var cachePct = l.cache_hit_pct;
      var cacheCol;
      if (cachePct === undefined || cachePct < 0) {
        cacheCol = '<span style="opacity:0.3">—</span>';
      } else if (cachePct === 0) {
        cacheCol = '<span style="opacity:0.4">0%</span>';
      } else {
        var cColor = cachePct >= 70 ? 'var(--green)' : cachePct >= 30 ? 'var(--yellow)' : 'var(--text-tertiary)';
        cacheCol = '<span style="color:' + cColor + ';font-weight:600">' + cachePct + '%</span>';
      }
      return '<tr style="cursor:pointer" onclick="showDetail(' + i + ')">' +
        '<td class="mono">' + escHtml(l.timestamp) + '</td>' +
        '<td>' + escHtml(l.model) + '</td>' +
        '<td style="opacity:0.6">' + escHtml(l.upstream) + '</td>' +
        '<td class="' + cls + '">' + sc + err + '</td>' +
        '<td class="mono">' + l.latency_ms + 'ms</td>' +
        '<td class="mono">' + tok + '</td>' +
        '<td class="mono">' + cacheCol + '</td>' +
      '</tr>';
    }).join('');
  } catch (e) {
    console.error('history error', e);
  }
}

/* ══════════════════════════════════════════════════════════
   DETAIL MODAL
   ══════════════════════════════════════════════════════════ */
function showDetail(idx) {
  var l = historyLogs[idx];
  if (!l) return;
  var sc = l.status_code;
  var cls = sc >= 500 ? 'stcode-5xx' : sc >= 400 ? 'stcode-4xx' : 'stcode-2xx';

  var html =
    '<div class="modal-field"><div class="field-label">Time</div><div class="field-value">' + escHtml(l.timestamp) + '</div></div>' +
    '<div class="modal-grid">' +
      '<div class="modal-field"><div class="field-label">Model</div><div class="field-value">' + escHtml(l.model) + '</div></div>' +
      '<div class="modal-field"><div class="field-label">Upstream</div><div class="field-value">' + escHtml(l.upstream) + '</div></div>' +
      '<div class="modal-field"><div class="field-label">Status</div><div class="field-value ' + cls + '">' + sc + '</div></div>' +
      '<div class="modal-field"><div class="field-label">Latency</div><div class="field-value">' + l.latency_ms + 'ms</div></div>' +
      '<div class="modal-field"><div class="field-label">Tokens</div><div class="field-value">' + l.tokens_in + ' in / ' + l.tokens_out + ' out</div></div>' +
      (l.cache_hit_pct !== undefined && l.cache_hit_pct >= 0 ? '<div class="modal-field"><div class="field-label">Cache Hit</div><div class="field-value">' + l.cache_hit_pct + '%</div></div>' : '') +
    '</div>';

  if (l.error_msg) {
    html += '<div class="modal-field"><div class="field-label">Error</div><div class="modal-text-block" style="color:var(--red)">' + escHtml(l.error_msg) + '</div></div>';
  }

  // Tabs: Preview | Request JSON | Response JSON
  html += '<div class="modal-tabs">' +
    '<button class="modal-tab active" onclick="switchDetailTab(\'preview\')">Preview</button>' +
    '<button class="modal-tab" onclick="switchDetailTab(\'reqjson\')">Request JSON</button>' +
    '<button class="modal-tab" onclick="switchDetailTab(\'respjson\')">Response JSON</button>' +
  '</div>';

  // Tab: Preview (existing input/output text)
  html += '<div class="modal-tab-content active" id="tab-preview">';
  if (l.input_text) {
    html += '<div class="modal-field"><div class="field-label">Input</div><div class="modal-text-block">' + escHtml(l.input_text) + '</div></div>';
  } else {
    html += '<div class="modal-json-empty">No input text captured</div>';
  }
  if (l.output_text) {
    html += '<div class="modal-field"><div class="field-label">Output</div><div class="modal-text-block output">' + escHtml(l.output_text) + '</div></div>';
  } else {
    html += '<div class="modal-json-empty">No output text captured</div>';
  }
  html += '</div>';

  // Tab: Request JSON (lazy-loaded)
  html += '<div class="modal-tab-content" id="tab-reqjson"><div class="modal-loading" id="reqjson-loading">Loading full request JSON...</div></div>';

  // Tab: Response JSON (lazy-loaded)
  html += '<div class="modal-tab-content" id="tab-respjson"><div class="modal-loading" id="respjson-loading">Loading full response JSON...</div></div>';

  document.getElementById('detailContent').innerHTML = html;
  document.getElementById('detailModal').classList.add('show');

  // Store current log ID for lazy loading
  currentDetailId = l.id;
  detailJsonLoaded = { req: false, resp: false };
}

var currentDetailId = null;
var detailJsonLoaded = { req: false, resp: false };

function switchDetailTab(tab) {
  document.querySelectorAll('.modal-tab').forEach(function(t) { t.classList.remove('active'); });
  document.querySelectorAll('.modal-tab-content').forEach(function(c) { c.classList.remove('active'); });
  event.target.classList.add('active');
  document.getElementById('tab-' + tab).classList.add('active');

  // Lazy-load JSON when switching to those tabs
  if (tab === 'reqjson' && !detailJsonLoaded.req && currentDetailId) {
    detailJsonLoaded.req = true;
    loadDetailJson(currentDetailId, 'request');
  }
  if (tab === 'respjson' && !detailJsonLoaded.resp && currentDetailId) {
    detailJsonLoaded.resp = true;
    loadDetailJson(currentDetailId, 'response');
  }
}

async function loadDetailJson(id, type) {
  var containerId = type === 'request' ? 'tab-reqjson' : 'tab-respjson';
  try {
    // id must stay a string — UInt64 log ids exceed JS Number.MAX_SAFE_INTEGER
    var data = await fetchJSON('/history/detail/' + encodeURIComponent(String(id)));
    var jsonField = type === 'request' ? data.request_body : data.response_body;
    var container = document.getElementById(containerId);
    if (jsonField === null || jsonField === undefined || jsonField === '') {
      container.innerHTML = '<div class="modal-json-empty">No ' + type + ' body captured for this request.</div>';
      return;
    }
    // Object from API, or string body — always pretty-print full content
    var formatted;
    if (typeof jsonField === 'string') {
      try {
        formatted = JSON.stringify(JSON.parse(jsonField), null, 2);
      } catch (e) {
        formatted = jsonField;
      }
    } else {
      formatted = JSON.stringify(jsonField, null, 2);
    }
    container.innerHTML = '<div class="modal-json-block">' + escHtml(formatted) + '</div>';
  } catch (e) {
    var el = document.getElementById(containerId);
    if (el) el.innerHTML = '<div class="modal-json-empty">Error loading: ' + escHtml(e.message) + '</div>';
  }
}

function closeModal() {
  document.getElementById('detailModal').classList.remove('show');
}

document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') closeModal();
});

/* ══════════════════════════════════════════════════════════
   ACCOUNTS REFRESH (POST /accounts/refresh)
   ══════════════════════════════════════════════════════════ */
async function refreshAccounts() {
  try {
    var r = await fetch('/accounts/refresh', {
      method: 'POST',
      headers: authKey ? { 'Authorization': 'Bearer ' + authKey } : {}
    });
    var d = await r.json();
    if (d.error) {
      alert('Refresh failed: ' + d.error);
    } else {
      alert('Token refresh triggered successfully.');
      refresh();
    }
  } catch (e) {
    alert('Refresh error: ' + e.message);
  }
}

/* ── CB selector mode (rr|sticky|content-hash|hybrid) ── */
async function loadCBSelectorMode() {
  try {
    var r = await fetchJSON('/cb/selector-mode');
    var sel = document.getElementById('cbSelectorMode');
    if (sel && r.mode) sel.value = r.mode;
    var stickyEl = document.getElementById('cbSelectorSticky');
    if (stickyEl) stickyEl.textContent = r.sticky_active > 0 ? ('· ' + r.sticky_active + ' active session' + (r.sticky_active !== 1 ? 's' : '')) : '';
  } catch (e) { /* silent — only visible to admin */ }
}
async function changeCBSelectorMode(mode) {
  try {
    var r = await apiFetch('/cb/selector-mode', { method: 'PUT', body: JSON.stringify({ mode: mode }) });
    if (r.error) { alert('Failed: ' + r.error); loadCBSelectorMode(); return; }
    loadCBSelectorMode();
  } catch (e) { alert('Error: ' + e.message); loadCBSelectorMode(); }
}

/* ── Grok selector mode (rr|sticky|content-hash|hybrid) ── */
async function loadGrokSelectorMode() {
  try {
    var r = await fetchJSON('/grok/selector-mode');
    var sel = document.getElementById('grokSelectorMode');
    if (sel && r.mode) sel.value = r.mode;
    var stickyEl = document.getElementById('grokSelectorSticky');
    if (stickyEl) stickyEl.textContent = r.sticky_active > 0 ? ('· ' + r.sticky_active + ' active session' + (r.sticky_active !== 1 ? 's' : '')) : '';
  } catch (e) { /* silent — only visible to admin */ }
}
async function changeGrokSelectorMode(mode) {
  try {
    var r = await apiFetch('/grok/selector-mode', { method: 'PUT', body: JSON.stringify({ mode: mode }) });
    if (r.error) { alert('Failed: ' + r.error); loadGrokSelectorMode(); return; }
    loadGrokSelectorMode();
  } catch (e) { alert('Error: ' + e.message); loadGrokSelectorMode(); }
}

/* ══════════════════════════════════════════════════════════
   GATEWAY KEYS PAGE (Redis-backed CRUD)
   ══════════════════════════════════════════════════════════ */
var editingKeyMasked = null;

// loadCBStats: refresh CodeBuddy key stats table.
// Called after CB key import/delete operations.
function loadCBStats() {
  apiFetch('/cb-stats').then(function(data) {
    if (!data || !data.codebuddy_keys) return;
    var el = document.getElementById('cbStatsBody');
    if (!el) return;
    el.innerHTML = '';
    data.codebuddy_keys.forEach(function(k) {
      var tr = document.createElement('tr');
      var limit = k.credit_limit != null ? k.credit_limit : 240;
      var remain = (k.credits_remain != null) ? k.credits_remain : (limit - k.credits_used);
      tr.innerHTML = '<td class="mono">' + escHtml(k.key) + '</td>' +
        '<td>' + escHtml(String(k.credits_used)) + '/' + escHtml(String(limit)) +
        ' <span class="muted">(' + Number(remain).toFixed(1) + ' left)</span></td>' +
        '<td>' + escHtml(String(k.total_requests)) + '</td>' +
        '<td>' + (k.disabled ? '<span style="color:var(--danger)">disabled</span>' : '<span style="color:var(--success)">active</span>') + '</td>';
      el.appendChild(tr);
    });
  }).catch(function(e) { console.warn('[loadCBStats]', e); });
}

// syncCBCredits: POST /cb/credits/sync then refresh the accounts table.
async function syncCBCredits() {
  var btn = document.getElementById('cbSyncCreditsBtn');
  if (btn) { btn.disabled = true; btn.textContent = 'Syncing…'; }
  try {
    var res = await apiFetch('/cb/credits/sync', { method: 'POST', body: '{}' });
    var synced = (res && res.synced) || 0;
    var failed = (res && res.failed) || 0;
    if (failed > 0) {
      alert('Credit sync: ' + synced + ' ok, ' + failed + ' failed');
    }
    refresh();
  } catch (e) {
    alert('Credit sync error: ' + e.message);
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = 'Sync credits'; }
  }
}

async function syncGrokBilling() {
  var btn = document.getElementById('grokSyncBillingBtn');
  if (btn) { btn.disabled = true; btn.textContent = 'Syncing…'; }
  try {
    var res = await apiFetch('/accounts/billing/sync', { method: 'POST', body: '{}' });
    var synced = (res && res.synced) || 0;
    var failed = (res && res.failed) || 0;
    if (failed > 0) {
      alert('Billing sync: ' + synced + ' ok, ' + failed + ' failed');
    }
    refresh();
  } catch (e) {
    alert('Billing sync error: ' + e.message);
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = 'Sync Billing'; }
  }
}

function loadGatewayKeys() {
  var container = document.getElementById('keysContainer');
  // Auth is handled via HttpOnly cookie (foxrouters_session) — no JS key needed.
  // Previously checked `authKey` which was always empty after cookie migration,
  // making the entire Keys page non-functional.

  apiFetch('/api/keys').then(function(data) {
    if (!data.keys || data.keys.length === 0) {
      container.innerHTML = '<div style="text-align:center;padding:32px;color:var(--text-tertiary);font-size:13px">No gateway keys found. Click "Create Key" to add one.</div>';
      return;
    }

    var html = '<table style="width:100%;border-collapse:collapse;font-size:13px">' +
      '<thead><tr style="border-bottom:1px solid var(--border-strong)">' +
        '<th style="text-align:left;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Name</th>' +
        '<th style="text-align:left;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Key</th>' +
        '<th style="text-align:center;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Role</th>' +
        '<th style="text-align:left;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Models</th>' +
        '<th style="text-align:right;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">RPM</th>' +
        '<th style="text-align:right;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Token Quota</th>' +
        '<th style="text-align:right;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Requests</th>' +
        '<th style="text-align:center;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Status</th>' +
        '<th style="text-align:right;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Actions</th>' +
      '</tr></thead><tbody>';

    data.keys.forEach(function(k) {
      var rpmDisplay = k.rpm > 0 ? k.rpm : '∞';
      var roleBadge = k.role === 'admin'
        ? '<span style="padding:2px 8px;border-radius:10px;font-size:10px;background:rgba(234,179,8,0.12);color:var(--orange)">admin</span>'
        : '<span style="padding:2px 8px;border-radius:10px;font-size:10px;background:rgba(255,255,255,0.10);color:#ffffff">inference</span>';
      var modelsDisplay = (k.allowed_models && k.allowed_models.length > 0)
        ? '<span style="font-family:var(--mono);font-size:11px;color:var(--text-secondary)">' + escHtml(k.allowed_models.join(', ')) + '</span>'
        : '<span style="color:var(--text-tertiary);font-size:11px">all</span>';
      var quotaDisplay = k.token_quota > 0
        ? '<div style="font-family:var(--mono);font-size:12px">' + formatNum(k.tokens_used) + ' / ' + formatNum(k.token_quota) + '</div>' +
          '<div style="width:80px;height:4px;background:var(--bg-elevated);border-radius:2px;margin-top:3px;overflow:hidden">' +
            '<div style="width:' + Math.min(100, k.token_quota > 0 ? (k.tokens_used / k.token_quota * 100) : 0) + '%;height:100%;background:' + (k.tokens_used / k.token_quota > 0.8 ? 'var(--red)' : 'var(--brand)') + '"></div>' +
          '</div>'
        : '<span style="color:var(--text-tertiary)">' + formatNum(k.tokens_used) + ' (∞)</span>';
      var statusBadge = k.disabled
        ? '<span style="padding:2px 8px;border-radius:10px;font-size:11px;background:rgba(239,68,68,0.12);color:var(--red)">Disabled</span>'
        : '<span style="padding:2px 8px;border-radius:10px;font-size:11px;background:rgba(39,166,68,0.12);color:var(--green)">Active</span>';

      // Safely pass key data to onclick via data attributes
      html += '<tr style="border-bottom:1px solid var(--border)">' +
        '<td style="padding:10px;color:var(--text-secondary)">' + escHtml(k.name || 'unnamed') + '</td>' +
        '<td style="padding:10px;font-family:var(--mono);font-size:12px;color:var(--text-secondary)">' + escHtml(k.key_masked) + '</td>' +
        '<td style="padding:10px;text-align:center">' + roleBadge + '</td>' +
        '<td style="padding:10px;font-size:11px">' + modelsDisplay + '</td>' +
        '<td style="padding:10px;text-align:right;font-family:var(--mono);color:var(--text-secondary)">' + rpmDisplay + '</td>' +
        '<td style="padding:10px;text-align:right">' + quotaDisplay + '</td>' +
        '<td style="padding:10px;text-align:right;font-family:var(--mono);color:var(--text-tertiary)">' + formatNum(k.requests) + '</td>' +
        '<td style="padding:10px;text-align:center">' + statusBadge + '</td>' +
        '<td style="padding:10px;text-align:right">' +
          '<button class="btn-ghost edit-key-btn" data-key="' + escHtml(k.key_masked) + '" style="padding:3px 8px;font-size:12px">Edit</button> ' +
          '<button class="btn-ghost delete-key-btn" data-key="' + escHtml(k.key_masked) + '" style="padding:3px 8px;font-size:12px;color:var(--red)">Delete</button>' +
        '</td>' +
      '</tr>';
    });

    html += '</tbody></table>';
    container.innerHTML = html;
  }).catch(function(e) {
    container.innerHTML = '<div style="text-align:center;padding:32px;color:var(--red);font-size:13px">Error loading keys: ' + escHtml(e.message) + '</div>';
  });
}

function formatNum(n) {
  if (n >= 1e6) return (n/1e6).toFixed(1) + 'M';
  if (n >= 1e3) return (n/1e3).toFixed(1) + 'K';
  return String(n);
}

/* ══════════════════════════════════════════════════════════
   MODELS PAGE
   ══════════════════════════════════════════════════════════ */
var aliasMap = {
  'grok-4.5-high':   { upstream: 'grok-4.5', effort: 'high' },
  'grok-4.5-medium': { upstream: 'grok-4.5', effort: 'medium' },
  'grok-4.5-low':    { upstream: 'grok-4.5', effort: 'low' },
  'grok-4.5-xhigh': { upstream: 'grok-4.5', effort: 'high' }
};

function switchMTab(tab) {
  var panes = ['models', 'custom', 'combos'];
  panes.forEach(function(t) {
    var pane = document.getElementById('mtab-pane-' + t);
    var btn = document.getElementById('mtab-' + t);
    if (!pane || !btn) return;
    if (t === tab) {
      pane.style.display = '';
      btn.style.borderBottom = '2px solid var(--brand)';
      btn.style.color = 'var(--text-primary)';
    } else {
      pane.style.display = 'none';
      btn.style.borderBottom = '2px solid transparent';
      btn.style.color = 'var(--text-tertiary)';
    }
  });
  if (tab === 'custom') {
    loadCustomModels();
    loadAliases();
  }
  if (tab === 'combos') {
    loadCombos();
    loadComboModelSelector();
  }
}

// === Combo Model Selector (checkbox-based, mirip API keys selector) ===
var _comboModelsCache = [];

function loadComboModelSelector() {
  var container = document.getElementById('cbxModelSelector');
  if (!container) return;
  if (_comboModelsCache.length > 0) {
    renderComboModelSelector(_comboModelsCache);
    return;
  }
  container.innerHTML = '<div style="text-align:center;padding:8px;color:var(--text-tertiary);font-size:12px">Loading models...</div>';
  fetchJSON('/v1/models').then(function(data) {
    _comboModelsCache = (data.data || []).map(function(m) { return m.id; }).sort();
    renderComboModelSelector(_comboModelsCache);
  }).catch(function(e) {
    container.innerHTML = '<div style="padding:8px;color:var(--red);font-size:12px">Failed to load: ' + escHtml(e.message) + '</div>';
  });
}

function renderComboModelSelector(models) {
  var container = document.getElementById('cbxModelSelector');
  if (!container) return;
  if (!models || models.length === 0) {
    container.innerHTML = '<div style="padding:8px;color:var(--text-tertiary);font-size:12px">No models available</div>';
    return;
  }
  var groups = {};
  models.forEach(function(m) {
    var prefix = m.startsWith('grok-') ? 'grok' : (m.startsWith('cb/') ? 'cb' : (m.startsWith('combo/') ? 'combo' : 'other'));
    if (!groups[prefix]) groups[prefix] = [];
    groups[prefix].push(m);
  });
  var html = '';
  ['grok', 'cb', 'combo', 'other'].forEach(function(prefix) {
    if (!groups[prefix]) return;
    var label = prefix === 'grok' ? 'Grok' : (prefix === 'cb' ? 'CodeBuddy' : (prefix === 'combo' ? 'Combos' : 'Other'));
    html += '<div style="margin-bottom:6px">';
    html += '<div style="display:flex;align-items:center;gap:6px;padding:4px 0;border-bottom:1px solid var(--border);margin-bottom:4px">';
    html += '<span style="font-size:11px;font-weight:600;color:var(--text-secondary);text-transform:uppercase;letter-spacing:0.5px">' + escHtml(label) + '</span>';
    html += '</div>';
    groups[prefix].forEach(function(m) {
      html += '<label style="display:flex;align-items:center;gap:6px;padding:3px 8px;cursor:pointer;font-size:12px">';
      html += '<input type="checkbox" class="comboModelCheckbox" value="' + escHtml(m) + '" style="accent-color:var(--brand)" onchange="updateComboSelectedCount()" />';
      html += '<span style="font-family:var(--mono);color:var(--text-secondary)">' + escHtml(m) + '</span>';
      html += '</label>';
    });
    html += '</div>';
  });
  container.innerHTML = html;
  updateComboSelectedCount();
}

function getSelectedComboModels() {
  var checkboxes = document.querySelectorAll('.comboModelCheckbox');
  var selected = [];
  checkboxes.forEach(function(cb) {
    if (cb.checked && cb.value) selected.push(cb.value);
  });
  return selected;
}

function toggleAllComboModels(check) {
  var checkboxes = document.querySelectorAll('.comboModelCheckbox');
  checkboxes.forEach(function(cb) { cb.checked = check; });
  updateComboSelectedCount();
}

function toggleComboGroup(prefix) {
  var checkboxes = document.querySelectorAll('.comboModelCheckbox');
  checkboxes.forEach(function(cb) {
    var isGroup = (prefix === 'grok' && cb.value.startsWith('grok-')) ||
                  (prefix === 'cb' && cb.value.startsWith('cb/'));
    cb.checked = isGroup;
  });
  updateComboSelectedCount();
}

function updateComboSelectedCount() {
  var count = getSelectedComboModels().length;
  var el = document.getElementById('cbxSelectedCount');
  if (el) el.textContent = count + ' selected';
}

// ══════════════════════════════════════════════════════════
// MODEL CAPABILITIES (badges: Vision, Reasoning, Code, Fast, Long-ctx)
// ══════════════════════════════════════════════════════════
function modelCaps(id) {
  var m = id.toLowerCase();
  var caps = [];

  // Vision (multimodal image input)
  if (m.indexOf('grok-4') >= 0 || m.indexOf('gpt-4o') >= 0 ||
      m.indexOf('gpt-4.1') >= 0 || m.indexOf('gpt-5') >= 0 ||
      m.indexOf('claude') >= 0 || m.indexOf('gemini') >= 0 ||
      m.indexOf('glm-5') >= 0) {
    caps.push('<span title="Image input" style="padding:2px 6px;border-radius:8px;font-size:10px;background:rgba(100,181,246,0.14);color:#64b5f6;font-weight:600;letter-spacing:0.3px">👁 Vision</span>');
  }

  // Reasoning (extended thinking — verified via reasoning_tokens in usage)
  // Almost all modern models support reasoning_effort param. gpt-4.1 rejects it.
  if (m.indexOf('o3') >= 0 || m.indexOf('o4') >= 0 ||
      m.indexOf('grok-4') >= 0 ||
      m.indexOf('-reasoning') >= 0 || m.indexOf('deepseek') >= 0 ||
      m.indexOf('claude-opus') >= 0 || m.indexOf('kimi') >= 0 ||
      m.indexOf('gpt-5') >= 0 || m.indexOf('glm-5') >= 0 ||
      m.indexOf('gemini-3') >= 0) {
    caps.push('<span title="Chain-of-thought reasoning (reasoning_effort param)" style="padding:2px 6px;border-radius:8px;font-size:10px;background:rgba(171,71,188,0.14);color:#ab47bc;font-weight:600;letter-spacing:0.3px">🧠 Reasoning</span>');
  }

  // Code (specialized coding models)
  if (m.indexOf('codex') >= 0 || m.indexOf('code') >= 0 ||
      m.indexOf('grok-code') >= 0 || m.indexOf('kimi') >= 0) {
    caps.push('<span title="Code generation" style="padding:2px 6px;border-radius:8px;font-size:10px;background:rgba(129,199,132,0.14);color:#81c784;font-weight:600;letter-spacing:0.3px">💻 Code</span>');
  }

  // Fast (low-latency / cheap)
  if (m.indexOf('flash') >= 0 || m.indexOf('fast') >= 0 ||
      m.indexOf('haiku') >= 0 || m.indexOf('mini') >= 0 ||
      m.indexOf('flash-lite') >= 0) {
    caps.push('<span title="Low latency" style="padding:2px 6px;border-radius:8px;font-size:10px;background:rgba(255,183,77,0.14);color:#ffb74d;font-weight:600;letter-spacing:0.3px">⚡ Fast</span>');
  }

  // Long-context (1M+ tokens)
  if (m.indexOf('1m') >= 0 || m.indexOf('2.5-pro') >= 0 ||
      m.indexOf('3.1-pro') >= 0 || m.indexOf('3.0-pro') >= 0) {
    caps.push('<span title="1M+ token context" style="padding:2px 6px;border-radius:8px;font-size:10px;background:rgba(255,138,101,0.14);color:#ff8a65;font-weight:600;letter-spacing:0.3px">📝 Long-ctx</span>');
  }

  // Grok reasoning_effort aliases
  if (m.indexOf('grok-4.5-') >= 0 && m.indexOf('none') < 0) {
    caps.push('<span title="Tunable reasoning_effort" style="padding:2px 6px;border-radius:8px;font-size:10px;background:rgba(129,199,132,0.14);color:#81c784;font-weight:600;letter-spacing:0.3px">🎚 Effort</span>');
  }

  return caps.length ? caps.join(' ') : '<span style="color:var(--text-tertiary);font-size:11px">—</span>';
}

async function loadModels() {
  var container = document.getElementById('modelsContainer');
  if (!container) return;

  var hoursEl = document.getElementById('modelHours');
  var hours = hoursEl ? hoursEl.value : '24';

  container.innerHTML = '<div style="text-align:center;padding:32px;color:var(--text-tertiary);font-size:13px">Loading models...</div>';

  try {
    var results = await Promise.all([
      fetchJSON('/v1/models'),
      fetchJSON('/history?hours=' + hours)
    ]);
    var modelsData = results[0].data || [];
    var history = results[1];

    // Build usage map from by_model
    var usageMap = {};
    if (history.by_model) {
      history.by_model.forEach(function(m) {
        usageMap[m.model] = m;
      });
    }
    // Group by owner
    var groups = {};
    modelsData.forEach(function(m) {
      var owner = m.owned_by || 'unknown';
      if (!groups[owner]) groups[owner] = [];
      groups[owner].push(m);
    });

    var html = '';
    var ownerLabels = { xai: 'Grok (xAI)', codebuddy: 'CodeBuddy' };

    for (var owner in groups) {
      if (!groups.hasOwnProperty(owner)) continue;
      var models = groups[owner];
      html += '<div style="margin-bottom:24px">';
      html += '<div style="font-size:11px;text-transform:uppercase;letter-spacing:0.5px;color:var(--text-tertiary);margin-bottom:8px;font-weight:600">' + escHtml(ownerLabels[owner] || owner) + '</div>';
      html += '<table style="width:100%;border-collapse:collapse;font-size:13px">';
      html += '<thead><tr style="border-bottom:1px solid var(--border-strong)">' +
        '<th style="text-align:left;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Model</th>' +
        '<th style="text-align:left;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Type</th>' +
        '<th style="text-align:left;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Capabilities</th>' +
        '<th style="text-align:right;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Requests</th>' +
        '<th style="text-align:right;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Tokens In</th>' +
        '<th style="text-align:right;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Tokens Out</th>' +
        '<th style="text-align:right;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Avg Latency</th>' +
        '<th style="text-align:right;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Errors</th>' +
        '<th style="text-align:center;padding:8px 10px;color:var(--text-tertiary);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:0.5px">Test</th>' +
        '</tr></thead><tbody>';

      models.forEach(function(m) {
        var id = m.id;
        var isAlias = aliasMap[id] !== undefined;
        var typeLabel = isAlias
          ? '<span style="padding:2px 8px;border-radius:10px;font-size:11px;background:rgba(94,106,210,0.12);color:var(--brand)">alias → ' + aliasMap[id].upstream + ' (' + aliasMap[id].effort + ')</span>'
          : '<span style="padding:2px 8px;border-radius:10px;font-size:11px;background:rgba(255,255,255,0.04);color:var(--text-tertiary)">direct</span>';

        var u = usageMap[id] || {};
        var reqs = u.total_requests || 0;
        var tokIn = u.total_tokens_in || 0;
        var tokOut = u.total_tokens_out || 0;
        var avgLat = u.avg_latency_ms ? Math.round(u.avg_latency_ms) + 'ms' : '—';
        var errs = u.total_errors || 0;
        var errStyle = errs > 0 ? 'color:var(--red)' : 'color:var(--text-tertiary)';

        html += '<tr style="border-bottom:1px solid var(--border)">' +
          '<td style="padding:10px;font-family:var(--mono);font-size:12px;color:var(--text-primary)">' + escHtml(id) + '</td>' +
          '<td style="padding:10px">' + typeLabel + '</td>' +
          '<td style="padding:10px">' + modelCaps(id) + '</td>' +
          '<td style="padding:10px;text-align:right;font-family:var(--mono);color:var(--text-secondary)">' + (reqs > 0 ? formatNum(reqs) : '—') + '</td>' +
          '<td style="padding:10px;text-align:right;font-family:var(--mono);color:var(--text-secondary)">' + (tokIn > 0 ? formatNum(tokIn) : '—') + '</td>' +
          '<td style="padding:10px;text-align:right;font-family:var(--mono);color:var(--text-secondary)">' + (tokOut > 0 ? formatNum(tokOut) : '—') + '</td>' +
          '<td style="padding:10px;text-align:right;font-family:var(--mono);color:var(--text-tertiary)">' + avgLat + '</td>' +
          '<td style="padding:10px;text-align:right;font-family:var(--mono);' + errStyle + '">' + (errs > 0 ? errs : '—') + '</td>' +
          '<td style="padding:10px;text-align:center"><button class="btn-ghost quick-test-btn" data-model="' + escHtml(id) + '" style="padding:3px 10px;font-size:12px">Test</button></td>' +
        '</tr>';
      });

      html += '</tbody></table>';
      html += '</div>';
    }

    // Alias info box
    html += '<div style="margin-top:16px;padding:14px;background:var(--bg-elevated);border-radius:var(--radius);border:1px solid var(--border);font-size:12px;color:var(--text-tertiary);line-height:1.6">';
    html += '<strong style="color:var(--text-secondary)">Model Aliases</strong> — ';
    html += '<code style="font-family:var(--mono);color:var(--brand)">grok-4.5-high</code>, ';
    html += '<code style="font-family:var(--mono);color:var(--brand)">grok-4.5-medium</code>, ';
    html += '<code style="font-family:var(--mono);color:var(--brand)">grok-4.5-low</code> map to ';
    html += '<code style="font-family:var(--mono);color:var(--text-secondary)">grok-4.5</code> + ';
    html += '<code style="font-family:var(--mono);color:var(--text-secondary)">reasoning_effort</code> param. Client-set ';
    html += '<code style="font-family:var(--mono);color:var(--text-secondary)">reasoning_effort</code> takes precedence.';
    html += '</div>';

    container.innerHTML = html;
  } catch(e) {
    container.innerHTML = '<div style="text-align:center;padding:32px;color:var(--red);font-size:13px">Error loading models: ' + escHtml(e.message) + '</div>';
  }
}

async function quickTestModel(model, btnEl) {
  // D1: btnEl is passed by the delegated click handler; fall back to event.target
  // for any legacy inline caller (kept for safety, no known callers remain).
  var btn = btnEl || (typeof event !== 'undefined' && event ? event.target : null);
  if (!btn) return;
  var origText = btn.textContent;
  btn.textContent = 'Testing...';
  btn.disabled = true;

  try {
    var r = await fetch('/v1/chat/completions', {
      method: 'POST',
      // D3: in cookie-auth mode authKey is '' — sending "Bearer " (empty) makes
      // the gateway reject with 401. Mirror runTest(): only add the header when
      // we actually have a key (cookie session provides auth otherwise).
      headers: { 'Content-Type': 'application/json', ...(authKey ? { 'Authorization': 'Bearer ' + authKey } : {}) },
      body: JSON.stringify({ model: model, messages: [{role:'user',content:'Say OK'}], stream: false })
    });
    var data = await r.json();
    if (r.ok && data.choices) {
      var content = data.choices[0].message.content || '';
      var usage = data.usage || {};
      var rt = usage.completion_tokens_details ? usage.completion_tokens_details.reasoning_tokens : 0;
      btn.textContent = '✓ OK';
      btn.style.color = 'var(--green)';
      setTimeout(function() { btn.textContent = origText; btn.style.color = ''; btn.disabled = false; }, 3000);
      console.log('[Test] ' + model + ': ' + content.substring(0,100) + ' | reasoning=' + rt + ' tokens=' + (usage.total_tokens||0));
    } else {
      var errMsg = data.error ? (data.error.message || JSON.stringify(data.error)).substring(0,80) : 'Unknown error';
      btn.textContent = '✗ ' + r.status;
      btn.style.color = 'var(--red)';
      btn.title = errMsg;
      setTimeout(function() { btn.textContent = origText; btn.style.color = ''; btn.disabled = false; }, 3000);
    }
  } catch(e) {
    btn.textContent = '✗ Error';
    btn.style.color = 'var(--red)';
    setTimeout(function() { btn.textContent = origText; btn.style.color = ''; btn.disabled = false; }, 3000);
  }
}

/* ══════════════════════════════════════════════════════════
   MODEL SELECTOR (checkbox list for allowed_models)
   ══════════════════════════════════════════════════════════ */
var _availableModels = []; // cached model list from /v1/models

function loadModelSelector() {
  var container = document.getElementById('keyModelSelector');
  if (!container) return Promise.resolve();
  // Use cached models if available
  if (_availableModels.length > 0) {
    renderModelSelector(_availableModels);
    return Promise.resolve();
  }
  container.innerHTML = '<div style="text-align:center;padding:8px;color:var(--text-tertiary);font-size:12px">Loading models...</div>';
  return fetchJSON('/v1/models').then(function(data) {
    _availableModels = (data.data || []).map(function(m) { return m.id; }).sort();
    renderModelSelector(_availableModels);
  }).catch(function(e) {
    container.innerHTML = '<div style="padding:8px;color:var(--red);font-size:12px">Failed to load models: ' + escHtml(e.message) + '</div>';
  });
}

function renderModelSelector(models) {
  var container = document.getElementById('keyModelSelector');
  if (!container) return;
  if (!models || models.length === 0) {
    container.innerHTML = '<div style="padding:8px;color:var(--text-tertiary);font-size:12px">No models available</div>';
    return;
  }
  // Group by prefix (grok-* or cb/*)
  var groups = {};
  models.forEach(function(m) {
    var prefix = m.startsWith('grok-') ? 'grok' : (m.startsWith('cb/') ? 'cb' : 'other');
    if (!groups[prefix]) groups[prefix] = [];
    groups[prefix].push(m);
  });
  var html = '';
  ['grok', 'cb', 'other'].forEach(function(prefix) {
    if (!groups[prefix]) return;
    var label = prefix === 'grok' ? 'Grok' : (prefix === 'cb' ? 'CodeBuddy' : 'Other');
    var globPattern = prefix === 'grok' ? 'grok-*' : (prefix === 'cb' ? 'cb/*' : null);
    html += '<div style="margin-bottom:6px">';
    html += '<div style="display:flex;align-items:center;gap:6px;padding:4px 0;border-bottom:1px solid var(--border);margin-bottom:4px">';
    html += '<span style="font-size:11px;font-weight:600;color:var(--text-secondary);text-transform:uppercase;letter-spacing:0.5px">' + escHtml(label) + '</span>';
    if (globPattern) {
      html += '<label style="display:flex;align-items:center;gap:3px;font-size:11px;color:var(--text-tertiary);cursor:pointer;margin-left:auto">';
      html += '<input type="checkbox" id="modelGlob_' + prefix + '" value="' + escHtml(globPattern) + '" style="accent-color:var(--brand)" onchange="onModelCheckboxChange()" />';
      html += '<span style="font-family:var(--mono)">' + escHtml(globPattern) + '</span> <span style="font-size:10px">(all)</span>';
      html += '</label>';
    }
    html += '</div>';
    // Individual models
    groups[prefix].forEach(function(m) {
      html += '<label style="display:flex;align-items:center;gap:6px;padding:3px 8px;cursor:pointer;font-size:12px">';
      html += '<input type="checkbox" class="modelCheckbox" value="' + escHtml(m) + '" style="accent-color:var(--brand)" onchange="onModelCheckboxChange()" />';
      html += '<span style="font-family:var(--mono);color:var(--text-secondary)">' + escHtml(m) + '</span>';
      html += '</label>';
    });
    html += '</div>';
  });
  container.innerHTML = html;
}

function getSelectedModels() {
  var checkboxes = document.querySelectorAll('.modelCheckbox, #keyModelSelector input[type=checkbox]');
  var selected = [];
  checkboxes.forEach(function(cb) {
    if (cb.checked && cb.value) selected.push(cb.value);
  });
  return selected;
}

function setSelectedModels(models) {
  var modelSet = {};
  models.forEach(function(m) { modelSet[m] = true; });
  var checkboxes = document.querySelectorAll('.modelCheckbox, #keyModelSelector input[type=checkbox]');
  checkboxes.forEach(function(cb) {
    cb.checked = !!modelSet[cb.value];
  });
}

function toggleAllModels(check) {
  var checkboxes = document.querySelectorAll('#keyModelSelector input[type=checkbox]');
  checkboxes.forEach(function(cb) { cb.checked = check; });
}

function toggleModelGroup(prefix) {
  // Select only models in this group, deselect others
  var checkboxes = document.querySelectorAll('#keyModelSelector input[type=checkbox]');
  var globVal = prefix === 'grok' ? 'grok-*' : 'cb/*';
  checkboxes.forEach(function(cb) {
    var isGroup = (prefix === 'grok' && (cb.value.startsWith('grok-') || cb.value === 'grok-*')) ||
                  (prefix === 'cb' && (cb.value.startsWith('cb/') || cb.value === 'cb/*'));
    cb.checked = isGroup;
  });
}

function onModelCheckboxChange() {
  // Placeholder for future live-validation hooks
}

function showCreateKeyModal() {
  editingKeyMasked = null;
  document.getElementById('keyModalTitle').textContent = 'Create Gateway Key';
  document.getElementById('keyName').value = '';
  document.getElementById('keyRole').value = 'inference';
  document.getElementById('keyRPM').value = '0';
  document.getElementById('keyBurst').value = '0';
  document.getElementById('keyQuota').value = '0';
  document.getElementById('keyDisabled').checked = false;
  document.getElementById('keyEditDisabled').style.display = 'none';
  document.getElementById('keyCreateResult').style.display = 'none';
  document.getElementById('keyModalSave').textContent = 'Create';
  document.getElementById('keyModal').classList.add('show');
  // Load model selector + clear selection
  loadModelSelector().then(function() { setSelectedModels([]); });
}

function showEditKeyModal(masked) {
  editingKeyMasked = masked;
  document.getElementById('keyModalTitle').textContent = 'Edit Gateway Key';
  // Fetch current key data to populate form
  apiFetch('/api/keys/' + encodeURIComponent(masked) + '/usage')
    .then(function(k) {
      document.getElementById('keyName').value = k.name || '';
      document.getElementById('keyRole').value = k.role || 'inference';
      document.getElementById('keyRPM').value = k.rpm || 0;
      document.getElementById('keyBurst').value = k.burst || 0;
      document.getElementById('keyQuota').value = k.token_quota || 0;
      document.getElementById('keyDisabled').checked = k.disabled || false;
      document.getElementById('keyEditDisabled').style.display = 'block';
      document.getElementById('keyCreateResult').style.display = 'none';
      document.getElementById('keyModalSave').textContent = 'Save';
      document.getElementById('keyModal').classList.add('show');
      // Load model selector + set current selection
      loadModelSelector().then(function() { setSelectedModels(k.allowed_models || []); });
    })
    .catch(function(e) {
      alert('Failed to load key: ' + e.message);
    });
}

function closeKeyModal() {
  document.getElementById('keyModal').classList.remove('show');
}

function saveKeyModal() {
  var name = document.getElementById('keyName').value.trim() || 'unnamed';
  var role = document.getElementById('keyRole').value;
  var models = getSelectedModels();
  var rpm = parseInt(document.getElementById('keyRPM').value) || 0;
  var burst = parseInt(document.getElementById('keyBurst').value) || 0;
  var quota = parseInt(document.getElementById('keyQuota').value) || 0;

  if (editingKeyMasked) {
    // Edit mode — send allowed_models (empty array = clear whitelist)
    var disabled = document.getElementById('keyDisabled').checked;
    apiFetch('/api/keys/' + encodeURIComponent(editingKeyMasked), {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: name, role: role, allowed_models: models, rpm: rpm, burst: burst, token_quota: quota, disabled: disabled })
    }).then(function() {
      closeKeyModal();
      loadGatewayKeys();
    }).catch(function(e) {
      alert('Update failed: ' + e.message);
    });
  } else {
    // Create mode
    apiFetch('/api/keys', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: name, role: role, allowed_models: models, rpm: rpm, burst: burst, token_quota: quota })
    }).then(function(data) {
      // Show the full key once
      document.getElementById('keyCreateResult').style.display = 'block';
      document.getElementById('newKeyValue').textContent = data.key;
      document.getElementById('keyModalSave').textContent = 'Close';
      document.getElementById('keyModalSave').onclick = function() {
        closeKeyModal();
        loadGatewayKeys();
        // Restore onclick for next time
        document.getElementById('keyModalSave').onclick = saveKeyModal;
        document.getElementById('keyModalSave').textContent = 'Create';
      };
    }).catch(function(e) {
      alert('Create failed: ' + e.message);
    });
  }
}

function copyNewKey() {
  var key = document.getElementById('newKeyValue').textContent;
  navigator.clipboard.writeText(key).then(function() {
    var btn = event.target.closest('button');
    if (btn) {
      var orig = btn.innerHTML;
      btn.innerHTML = '✓';
      setTimeout(function() { btn.innerHTML = orig; }, 1500);
    }
  });
}

function deleteKey(masked) {
  if (!confirm('Delete key ' + masked + '? This cannot be undone.')) return;
  apiFetch('/api/keys/' + encodeURIComponent(masked), {
    method: 'DELETE'
  }).then(function() {
    loadGatewayKeys();
  }).catch(function(e) {
    alert('Delete failed: ' + e.message);
  });
}

function deleteCBKey(key) {
  if (!confirm('Delete CodeBuddy key ' + key + '? This cannot be undone.')) return;
  apiFetch('/cb/keys/' + encodeURIComponent(key), {
    method: 'DELETE'
  }).then(function() {
    refresh();
    loadCBStats();
  }).catch(function(e) {
    alert('Delete failed: ' + e.message);
  });
}

async function testCBKey(key, btn) {
  if (!key) return;
  var originalText = btn ? btn.textContent : null;
  if (btn) { btn.textContent = '…'; btn.disabled = true; }
  try {
    var r = await apiFetch('/cb/keys/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ key: key })
    });
    var lat = (r.latency_ms != null) ? (Number(r.latency_ms) / 1000).toFixed(1) + 's' : '?';
    if (r.ok) {
      var extra = '';
      if (r.credit != null && r.credit !== 0) extra += ' credit=' + r.credit;
      else if (r.credit === 0) extra += ' credit=0';
      if (r.content) extra += ' "' + String(r.content).substring(0, 40) + '"';
      alert('OK ' + (r.status || 200) + ' ' + lat + extra);
    } else {
      alert('FAIL ' + (r.status || '?') + ' ' + lat + ' ' + (r.error || 'unknown error'));
    }
  } catch (e) {
    if (e.message === '__redirect__') return;
    alert('Test failed: ' + e.message);
  } finally {
    if (btn && originalText !== null) { btn.textContent = originalText; btn.disabled = false; }
  }
}

function deleteGrokAccount(email) {
  if (!confirm('Delete Grok account ' + email + '? This cannot be undone.')) return;
  apiFetch('/accounts/' + encodeURIComponent(email), {
    method: 'DELETE'
  }).then(function() {
    refresh();
  }).catch(function(e) {
    alert('Delete failed: ' + e.message);
  });
}

async function testGrokAccount(email, btn) {
  if (!email) return;
  var originalText = btn ? btn.textContent : null;
  if (btn) { btn.textContent = '…'; btn.disabled = true; }
  try {
    var r = await apiFetch('/accounts/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email: email })
    });
    var lat = (r.latency_ms != null) ? (Number(r.latency_ms) / 1000).toFixed(1) + 's' : '?';
    if (r.ok) {
      var extra = '';
      if (r.token_status) extra += ' token=' + r.token_status;
      if (r.content) extra += ' "' + String(r.content).substring(0, 40) + '"';
      alert('OK ' + (r.status || 200) + ' ' + lat + extra);
    } else {
      alert('FAIL ' + (r.status || '?') + ' ' + lat + ' ' + (r.error || 'unknown error'));
    }
  } catch (e) {
    if (e.message === '__redirect__') return;
    alert('Test failed: ' + e.message);
  } finally {
    if (btn && originalText !== null) { btn.textContent = originalText; btn.disabled = false; }
  }
}

function cleanupDisabled(type) {
  var label = type === 'grok' ? 'Grok accounts' : (type === 'cb' ? 'CodeBuddy keys' : 'disabled items');
  if (!confirm('Permanently delete all disabled ' + label + '? Cooldown keys are preserved. This cannot be undone.')) return;
  apiFetch('/cleanup/disabled?type=' + type, {
    method: 'POST'
  }).then(function(data) {
    var msg = '';
    if (type === 'grok' || type === 'all') msg += 'Grok removed: ' + (data.grok_removed || 0) + ', remaining: ' + (data.grok_remaining || 0) + '\n';
    if (type === 'cb' || type === 'all') msg += 'CB removed: ' + (data.cb_removed || 0) + ', remaining: ' + (data.cb_remaining || 0);
    alert(msg);
    refresh();
    loadCBStats();
  }).catch(function(e) {
    alert('Cleanup failed: ' + e.message);
  });
}

function cleanupBanned(type) {
  type = type || 'grok';
  if (!confirm('Permanently delete all BANNED Grok accounts (token_status=banned)? Cooldown accounts are preserved. This cannot be undone.')) return;
  apiFetch('/cleanup/banned?type=' + type, {
    method: 'POST'
  }).then(function(data) {
    var msg = 'Banned Grok removed: ' + (data.grok_removed || 0) + ', remaining: ' + (data.grok_remaining || 0);
    alert(msg);
    refresh();
  }).catch(function(e) {
    alert('Cleanup banned failed: ' + e.message);
  });
}

/* ══════════════════════════════════════════════════════════
   INIT (moved to end of LAST script block — must run after
   ALL functions from both script blocks are defined, e.g.
   mediaInit in block 2; otherwise the typeof guards skip them)
   ══════════════════════════════════════════════════════════ */
