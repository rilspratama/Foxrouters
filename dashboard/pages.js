// === Add/Bulk CB Key ===
function showAddCBModal() {
  document.getElementById('addCBKeyInput').value = '';
  document.getElementById('addCBModal').style.display = 'flex';
  document.getElementById('addCBKeyInput').focus();
}
function closeAddCBModal() { document.getElementById('addCBModal').style.display = 'none'; }

function submitAddCB() {
  var key = document.getElementById('addCBKeyInput').value.trim();
  if (!key) { alert('API key is required'); return; }
  apiFetch('/cb/import', { method: 'POST', body: JSON.stringify({ api_key: key }) })
    .then(function(d) {
      alert(d.added ? 'Key added successfully (' + d.total + ' total)' : 'Key already exists (' + d.total + ' total)');
      closeAddCBModal();
      loadCBStats();
      if (typeof refresh === 'function') refresh();
    })
    .catch(function(e) { alert('Error: ' + e.message); });
}

// === Add CB OAuth ===
var cbOAuthMode = 'manual';
var cbOAuthStateVal = '';
var cbOAuthPollTimer = null;
var cbOAuthPollStart = 0;
var cbOAuthPollMaxMs = 5 * 60 * 1000; // 5 minutes
var cbOAuthPollIntervalMs = 3000;

function cbOAuthSwitchMode(mode) {
  cbOAuthMode = mode;
  var mBtn = document.getElementById('cbOAuthModeManual');
  var uBtn = document.getElementById('cbOAuthModeUrl');
  var manualPane = document.getElementById('cbOAuthManualPane');
  var urlPane = document.getElementById('cbOAuthUrlPane');
  var saveBtn = document.getElementById('cbOAuthSave');
  if (mode === 'manual') {
    mBtn.style.background = 'var(--brand)'; mBtn.style.color = '#fff';
    uBtn.style.background = 'var(--bg-primary)'; uBtn.style.color = 'var(--text-primary)';
    manualPane.style.display = '';
    urlPane.style.display = 'none';
    if (saveBtn) { saveBtn.style.display = ''; saveBtn.textContent = 'Import'; }
  } else {
    uBtn.style.background = 'var(--brand)'; uBtn.style.color = '#fff';
    mBtn.style.background = 'var(--bg-primary)'; mBtn.style.color = 'var(--text-primary)';
    manualPane.style.display = 'none';
    urlPane.style.display = '';
    if (saveBtn) { saveBtn.style.display = 'none'; }
    cbOAuthCancelPoll();
  }
}

function showAddCBOAuthModal() {
  ['cbOAuthEmail','cbOAuthExpiresIn','cbOAuthAccessToken','cbOAuthRefreshToken','cbOAuthUrlEmail'].forEach(function(id) {
    var el = document.getElementById(id);
    if (el) el.value = '';
  });
  var res = document.getElementById('cbOAuthResult');
  if (res) res.style.display = 'none';
  var urlBox = document.getElementById('cbOAuthUrlBox');
  if (urlBox) urlBox.style.display = 'none';
  var pollStatus = document.getElementById('cbOAuthPollStatus');
  if (pollStatus) pollStatus.style.display = 'none';
  cbOAuthStateVal = '';
  cbOAuthCancelPoll();
  cbOAuthSwitchMode('manual');
  document.getElementById('addCBOAuthModal').style.display = 'flex';
  document.getElementById('cbOAuthEmail').focus();
}
function closeAddCBOAuthModal() {
  cbOAuthCancelPoll();
  document.getElementById('addCBOAuthModal').style.display = 'none';
}

function cbOAuthGenerateUrl() {
  var genBtn = document.getElementById('cbOAuthGenBtn');
  genBtn.disabled = true;
  genBtn.textContent = 'Generating...';
  var res = document.getElementById('cbOAuthResult');
  res.style.display = 'none';
  apiFetch('/cb/oauth/device/start', { method: 'POST', headers: {'content-type':'application/json'}, body: '{}' })
    .then(function(d) {
      cbOAuthStateVal = d.state;
      document.getElementById('cbOAuthAuthUrl').textContent = d.auth_url;
      document.getElementById('cbOAuthState').textContent = d.state;
      var openLink = document.getElementById('cbOAuthOpenLink');
      if (openLink) openLink.href = d.auth_url;
      document.getElementById('cbOAuthUrlBox').style.display = '';
      genBtn.disabled = false;
      genBtn.textContent = 'Regenerate URL';
      document.getElementById('cbOAuthPollBtn').style.display = '';
      document.getElementById('cbOAuthCancelPollBtn').style.display = '';
      // Auto-poll
      cbOAuthStartPoll();
    })
    .catch(function(e) {
      genBtn.disabled = false;
      genBtn.textContent = 'Generate login URL';
      res.style.display = 'block';
      res.style.background = 'rgba(239,68,68,0.1)';
      res.style.color = 'var(--red)';
      res.textContent = '✗ Failed: ' + e.message;
    });
}

function cbOAuthCopyUrl() {
  var url = document.getElementById('cbOAuthAuthUrl').textContent;
  if (!url) return;
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(url).then(function() {
      var btn = event.target;
      var orig = btn.textContent;
      btn.textContent = 'Copied!';
      setTimeout(function() { btn.textContent = orig; }, 1500);
    });
  } else {
    // Fallback
    var ta = document.createElement('textarea');
    ta.value = url; document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy'); } catch(e) {}
    document.body.removeChild(ta);
  }
}

function cbOAuthStartPoll() {
  cbOAuthCancelPoll();
  cbOAuthPollStart = Date.now();
  var pollStatus = document.getElementById('cbOAuthPollStatus');
  pollStatus.style.display = 'block';
  pollStatus.style.background = 'rgba(234,179,8,0.1)';
  pollStatus.style.color = 'var(--yellow)';
  pollStatus.textContent = '⏳ Waiting for login...';
  cbOAuthPollTick();
  cbOAuthPollTimer = setInterval(cbOAuthPollTick, cbOAuthPollIntervalMs);
}

function cbOAuthCancelPoll() {
  if (cbOAuthPollTimer) { clearInterval(cbOAuthPollTimer); cbOAuthPollTimer = null; }
  var cancelBtn = document.getElementById('cbOAuthCancelPollBtn');
  var pollBtn = document.getElementById('cbOAuthPollBtn');
  if (cancelBtn) cancelBtn.style.display = 'none';
  if (pollBtn) pollBtn.style.display = 'none';
  var pollStatus = document.getElementById('cbOAuthPollStatus');
  if (pollStatus && pollStatus.textContent.indexOf('Waiting') !== -1) {
    pollStatus.style.display = 'none';
  }
}

function cbOAuthPollNow() {
  if (!cbOAuthStateVal) return;
  cbOAuthPollTick();
}

function cbOAuthPollTick() {
  if (!cbOAuthStateVal) return;
  if (Date.now() - cbOAuthPollStart > cbOAuthPollMaxMs) {
    cbOAuthCancelPoll();
    var pollStatus = document.getElementById('cbOAuthPollStatus');
    pollStatus.style.display = 'block';
    pollStatus.style.background = 'rgba(239,68,68,0.1)';
    pollStatus.style.color = 'var(--red)';
    pollStatus.textContent = '✗ Timed out after 5 minutes. Click Generate to retry.';
    return;
  }
  apiFetch('/cb/oauth/device/poll?state=' + encodeURIComponent(cbOAuthStateVal))
    .then(function(d) {
      if (d.status === 'ready') {
        cbOAuthCancelPoll();
        var pollStatus = document.getElementById('cbOAuthPollStatus');
        pollStatus.style.display = 'block';
        pollStatus.style.background = 'rgba(34,197,94,0.1)';
        pollStatus.style.color = 'var(--green)';
        pollStatus.innerHTML = '✓ Login successful! Importing account...';
        // Pre-fill email if we have it and user hasn't typed one
        var emailField = document.getElementById('cbOAuthUrlEmail');
        if (!emailField.value.trim() && d.email) {
          emailField.value = d.email;
        }
        // Auto-import via existing endpoint
        var body = {
          email: emailField.value.trim() || d.email,
          access_token: d.access_token,
          refresh_token: d.refresh_token
        };
        if (d.expires_in) body.expires_in = d.expires_in;
        apiFetch('/cb/oauth/import', { method: 'POST', headers: {'content-type':'application/json'}, body: JSON.stringify(body) })
          .then(function(imp) {
            var res = document.getElementById('cbOAuthResult');
            res.style.display = 'block';
            res.style.background = imp.added ? 'rgba(34,197,94,0.1)' : 'rgba(234,179,8,0.1)';
            res.style.color = imp.added ? 'var(--green)' : 'var(--yellow)';
            res.innerHTML = (imp.added ? '✓ Imported ' : '↻ Updated ') + '<b>' + escHtml(imp.email || body.email) + '</b>' +
              (imp.expires_at ? ' · expires ' + escHtml(imp.expires_at) : '') +
              ' · total ' + imp.total;
            pollStatus.innerHTML = '✓ Done! Account imported.';
            var saveBtn = document.getElementById('cbOAuthSave');
            if (saveBtn) { saveBtn.style.display = ''; saveBtn.textContent = 'Done'; saveBtn.disabled = false;
              saveBtn.onclick = function() { closeAddCBOAuthModal(); loadCBStats(); if (typeof refresh === 'function') refresh(); };
            }
            if (typeof refresh === 'function') refresh();
          })
          .catch(function(e) {
            var pollStatus2 = document.getElementById('cbOAuthPollStatus');
            pollStatus2.style.background = 'rgba(239,68,68,0.1)';
            pollStatus2.style.color = 'var(--red)';
            pollStatus2.innerHTML = '✗ Import failed: ' + escHtml(e.message) + '<br>Tokens received but not imported. Try Manual mode.';
            // Show save button so user can switch to manual and retry
            var saveBtn = document.getElementById('cbOAuthSave');
            if (saveBtn) { saveBtn.style.display = ''; saveBtn.textContent = 'Import'; saveBtn.disabled = false; }
          });
      } else if (d.status === 'error') {
        cbOAuthCancelPoll();
        var pollStatus = document.getElementById('cbOAuthPollStatus');
        pollStatus.style.display = 'block';
        pollStatus.style.background = 'rgba(239,68,68,0.1)';
        pollStatus.style.color = 'var(--red)';
        pollStatus.textContent = '✗ Error: ' + (d.error || 'unknown');
      }
      // else: pending — keep polling
    })
    .catch(function(e) {
      // Network error — keep polling, don't abort
      var pollStatus = document.getElementById('cbOAuthPollStatus');
      if (pollStatus && pollStatus.style.display !== 'none') {
        pollStatus.textContent = '⏳ Waiting for login... (retrying after network error)';
      }
    });
}

function submitAddCBOAuth() {
  if (cbOAuthMode === 'url') return; // URL mode auto-imports
  var email = document.getElementById('cbOAuthEmail').value.trim();
  var at = document.getElementById('cbOAuthAccessToken').value.trim();
  var rt = document.getElementById('cbOAuthRefreshToken').value.trim();
  var expRaw = document.getElementById('cbOAuthExpiresIn').value.trim();
  if (!email || !at || !rt) { alert('email, access_token, and refresh_token are required'); return; }
  var body = { email: email, access_token: at, refresh_token: rt };
  if (expRaw) body.expires_in = parseInt(expRaw, 10);
  var saveBtn = document.getElementById('cbOAuthSave');
  saveBtn.textContent = 'Importing...';
  saveBtn.disabled = true;
  apiFetch('/cb/oauth/import', { method: 'POST', body: JSON.stringify(body) })
    .then(function(d) {
      var res = document.getElementById('cbOAuthResult');
      res.style.display = 'block';
      res.style.background = d.added ? 'rgba(34,197,94,0.1)' : 'rgba(234,179,8,0.1)';
      res.style.color = d.added ? 'var(--green)' : 'var(--yellow)';
      res.innerHTML = (d.added ? '✓ Imported ' : '↻ Updated ') + '<b>' + escHtml(d.email || email) + '</b>' +
        (d.expires_at ? ' · expires ' + escHtml(d.expires_at) : '') +
        ' · total ' + d.total;
      saveBtn.textContent = 'Done';
      saveBtn.disabled = false;
      saveBtn.onclick = function() { closeAddCBOAuthModal(); loadCBStats(); if (typeof refresh === 'function') refresh(); };
      if (typeof refresh === 'function') refresh();
    })
    .catch(function(e) {
      var res = document.getElementById('cbOAuthResult');
      res.style.display = 'block';
      res.style.background = 'rgba(239,68,68,0.1)';
      res.style.color = 'var(--red)';
      res.textContent = '✗ Error: ' + e.message;
      saveBtn.textContent = 'Import';
      saveBtn.disabled = false;
    });
}

// === Bulk CB OAuth ===
function showBulkCBOAuthModal() {
  document.getElementById('bulkCBOAuthInput').value = '';
  var res = document.getElementById('bulkCBOAuthResult');
  if (res) res.style.display = 'none';
  var save = document.getElementById('bulkCBOAuthSave');
  if (save) { save.textContent = 'Import'; save.disabled = false; save.onclick = submitBulkCBOAuth; }
  document.getElementById('bulkCBOAuthModal').style.display = 'flex';
  document.getElementById('bulkCBOAuthInput').focus();
}
function closeBulkCBOAuthModal() { document.getElementById('bulkCBOAuthModal').style.display = 'none'; }

function submitBulkCBOAuth() {
  var raw = document.getElementById('bulkCBOAuthInput').value.trim();
  if (!raw) { alert('Paste a JSON array of OAuth accounts'); return; }
  var accounts;
  try {
    accounts = JSON.parse(raw);
  } catch (e) {
    alert('Invalid JSON: ' + e.message);
    return;
  }
  if (!Array.isArray(accounts) || accounts.length === 0) {
    alert('JSON must be a non-empty array');
    return;
  }
  if (!confirm('Import ' + accounts.length + ' OAuth accounts? Existing emails will be updated.')) return;
  var saveBtn = document.getElementById('bulkCBOAuthSave');
  saveBtn.textContent = 'Importing...';
  saveBtn.disabled = true;
  apiFetch('/cb/oauth/import/bulk', { method: 'POST', body: JSON.stringify({ accounts: accounts }) })
    .then(function(d) {
      var res = document.getElementById('bulkCBOAuthResult');
      res.style.display = 'block';
      var ok = (d.added || 0) + (d.updated || 0);
      res.style.background = ok > 0 ? 'rgba(34,197,94,0.1)' : 'rgba(234,179,8,0.1)';
      res.style.color = ok > 0 ? 'var(--green)' : 'var(--yellow)';
      res.innerHTML = '✓ Added: <b>' + (d.added || 0) + '</b> · Updated: <b>' + (d.updated || 0) +
        '</b> · Failed: ' + (d.failed || 0) + ' · Total pool: ' + (d.total || '?');
      if (d.errors && d.errors.length) {
        res.innerHTML += '<div style="margin-top:6px;font-size:11px;opacity:0.85">' +
          d.errors.slice(0, 5).map(function(e) {
            return '• [' + e.index + '] ' + escHtml(e.email || '') + ' ' + escHtml(e.error || '');
          }).join('<br>') + (d.errors.length > 5 ? '<br>…' : '') + '</div>';
      }
      saveBtn.textContent = 'Done';
      saveBtn.disabled = false;
      saveBtn.onclick = function() {
        closeBulkCBOAuthModal();
        loadCBStats();
        if (typeof refresh === 'function') refresh();
      };
      if (typeof refresh === 'function') refresh();
    })
    .catch(function(e) {
      var res = document.getElementById('bulkCBOAuthResult');
      res.style.display = 'block';
      res.style.background = 'rgba(239,68,68,0.1)';
      res.style.color = 'var(--red)';
      res.textContent = '✗ Error: ' + e.message;
      saveBtn.textContent = 'Import';
      saveBtn.disabled = false;
    });
}

function showBulkCBModal() {
  document.getElementById('bulkCBInput').value = '';
  document.getElementById('bulkCBResult').style.display = 'none';
  document.getElementById('bulkGrokModal'); // ensure not conflicting
  document.getElementById('bulkCBModal').style.display = 'flex';
  document.getElementById('bulkCBInput').focus();
}
function closeBulkCBModal() { document.getElementById('bulkCBModal').style.display = 'none'; }

function submitBulkCB() {
  var raw = document.getElementById('bulkCBInput').value.trim();
  if (!raw) { alert('Paste at least one key'); return; }
  var lines = raw.split(/[\n,\r\t ]+/).map(function(s){return s.trim()}).filter(Boolean);
  if (lines.length === 0) { alert('No valid keys found'); return; }
  if (!confirm('Import ' + lines.length + ' keys? Duplicates will be skipped.')) return;
  document.getElementById('bulkCBSave').textContent = 'Importing...';
  document.getElementById('bulkCBSave').disabled = true;
  apiFetch('/cb/import/bulk', { method: 'POST', body: JSON.stringify({ api_keys: lines }) })
    .then(function(d) {
      var res = document.getElementById('bulkCBResult');
      res.style.display = 'block';
      res.style.background = d.added > 0 ? 'rgba(34,197,94,0.1)' : 'rgba(234,179,8,0.1)';
      res.style.color = d.added > 0 ? 'var(--green)' : 'var(--yellow)';
      res.innerHTML = '✓ Added: <b>' + d.added + '</b> · Skipped: ' + d.skipped + ' · Total: ' + d.total;
      document.getElementById('bulkCBSave').textContent = 'Done';
      document.getElementById('bulkCBSave').disabled = false;
      document.getElementById('bulkCBSave').onclick = function() { closeBulkCBModal(); loadCBStats(); };
    })
    .catch(function(e) {
      var res = document.getElementById('bulkCBResult');
      res.style.display = 'block';
      res.style.background = 'rgba(239,68,68,0.1)';
      res.style.color = 'var(--red)';
      res.textContent = '✗ Error: ' + e.message;
      document.getElementById('bulkCBSave').textContent = 'Import';
      document.getElementById('bulkCBSave').disabled = false;
    });
}

// === Add/Bulk Grok Account ===
function showAddGrokModal() {
  ['grokEmail','grokExpiresIn','grokAccessToken','grokRefreshToken','grokIDToken'].forEach(function(id) {
    document.getElementById(id).value = '';
  });
  document.getElementById('addGrokModal').style.display = 'flex';
  document.getElementById('grokEmail').focus();
}
function closeAddGrokModal() { document.getElementById('addGrokModal').style.display = 'none'; }

function submitAddGrok() {
  var email = document.getElementById('grokEmail').value.trim();
  var at = document.getElementById('grokAccessToken').value.trim();
  var rt = document.getElementById('grokRefreshToken').value.trim();
  if (!email || !at || !rt) { alert('Email, Access Token, and Refresh Token are required'); return; }
  var body = { email: email, access_token: at, refresh_token: rt };
  var idt = document.getElementById('grokIDToken').value.trim();
  if (idt) body.id_token = idt;
  var ei = parseInt(document.getElementById('grokExpiresIn').value);
  if (ei > 0) body.expires_in = ei;
  apiFetch('/accounts/import', { method: 'POST', body: JSON.stringify(body) })
    .then(function(d) {
      alert('Account imported: ' + d.email + ' (total: ' + d.total + ')');
      closeAddGrokModal();
      loadCBStats();
    })
    .catch(function(e) { alert('Error: ' + e.message); });
}

function showBulkGrokModal() {
  document.getElementById('bulkGrokInput').value = '';
  document.getElementById('bulkGrokResult').style.display = 'none';
  document.getElementById('bulkGrokModal').style.display = 'flex';
  document.getElementById('bulkGrokInput').focus();
}
function closeBulkGrokModal() { document.getElementById('bulkGrokModal').style.display = 'none'; }

function submitBulkGrok() {
  var raw = document.getElementById('bulkGrokInput').value.trim();
  if (!raw) { alert('Paste JSON array'); return; }
  var accounts;
  try { accounts = JSON.parse(raw); }
  catch(e) { alert('Invalid JSON: ' + e.message); return; }
  if (!Array.isArray(accounts) || accounts.length === 0) { alert('Input must be a JSON array of account objects'); return; }
  if (!confirm('Import ' + accounts.length + ' accounts? Existing emails will be updated.')) return;
  document.getElementById('bulkGrokSave').textContent = 'Importing...';
  document.getElementById('bulkGrokSave').disabled = true;
  apiFetch('/accounts/import/bulk', { method: 'POST', body: JSON.stringify({ accounts: accounts }) })
    .then(function(d) {
      var res = document.getElementById('bulkGrokResult');
      res.style.display = 'block';
      res.style.background = d.added > 0 ? 'rgba(34,197,94,0.1)' : 'rgba(234,179,8,0.1)';
      res.style.color = d.added > 0 ? 'var(--green)' : 'var(--yellow)';
      res.innerHTML = '✓ Added: <b>' + d.added + '</b> · Updated: ' + d.updated + (d.failed > 0 ? ' · Failed: ' + d.failed : '') + ' · Total: ' + d.total;
      document.getElementById('bulkGrokSave').textContent = 'Done';
      document.getElementById('bulkGrokSave').disabled = false;
      document.getElementById('bulkGrokSave').onclick = function() { closeBulkGrokModal(); loadCBStats(); };
    })
    .catch(function(e) {
      var res = document.getElementById('bulkGrokResult');
      res.style.display = 'block';
      res.style.background = 'rgba(239,68,68,0.1)';
      res.style.color = 'var(--red)';
      res.textContent = '✗ Error: ' + e.message;
      document.getElementById('bulkGrokSave').textContent = 'Import';
      document.getElementById('bulkGrokSave').disabled = false;
    });
}

// Close modals on Escape
document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') {
    ['addCBModal','bulkCBModal','addGrokModal','bulkGrokModal'].forEach(function(id) {
      var m = document.getElementById(id);
      if (m && m.style.display === 'flex') m.style.display = 'none';
    });
  }
});

/* ══════════════════════════════════════════════════════════
   CUSTOM MODELS & ALIASES (admin only — v1.3.0)
   ══════════════════════════════════════════════════════════ */
async function loadCustomModels() {
  var tbody = document.getElementById('cmTableBody');
  if (!tbody) return;
  try {
    var res = await apiFetch('/api/models/custom');
    var models = res.models || {};
    var ids = Object.keys(models).sort();
    if (ids.length === 0) {
      tbody.innerHTML = '<tr><td colspan="5" style="text-align:center;padding:20px;color:var(--text-tertiary)">No custom models. Add one above.</td></tr>';
      return;
    }
    tbody.innerHTML = ids.map(function(id) {
      var m = models[id] || {};
      return '<tr>' +
        '<td style="padding:8px;border-bottom:1px solid var(--border);font-family:var(--mono);color:var(--emerald)">' + escHtml(id) + '</td>' +
        '<td style="padding:8px;border-bottom:1px solid var(--border)">' + escHtml(m.upstream || '') + '</td>' +
        '<td style="padding:8px;border-bottom:1px solid var(--border);font-family:var(--mono)">' + escHtml(m.model_name || '') + '</td>' +
        '<td style="padding:8px;border-bottom:1px solid var(--border)">' + escHtml(m.owned_by || '') + '</td>' +
        '<td style="padding:8px;border-bottom:1px solid var(--border)"><button class="btn-ghost delete-custom-btn" data-id="' + escHtml(id) + '" style="padding:4px 10px;font-size:11px;color:var(--red)">Delete</button></td>' +
        '</tr>';
    }).join('');
  } catch (e) {
    tbody.innerHTML = '<tr><td colspan="5" style="text-align:center;padding:20px;color:var(--red)">Error: ' + escHtml(e.message) + '</td></tr>';
  }
}

async function addCustomModel() {
  var errBox = document.getElementById('cmError');
  errBox.style.display = 'none';
  var id = document.getElementById('cmId').value.trim();
  var upstream = document.getElementById('cmUpstream').value;
  var modelName = document.getElementById('cmModelName').value.trim();
  var ownedBy = document.getElementById('cmOwnedBy').value.trim();
  if (!id) {
    errBox.textContent = 'Model ID is required';
    errBox.style.display = 'block';
    return;
  }
  try {
    await apiFetch('/api/models/custom', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify({id: id, upstream: upstream, model_name: modelName, owned_by: ownedBy})
    });
    document.getElementById('cmId').value = '';
    document.getElementById('cmModelName').value = '';
    document.getElementById('cmOwnedBy').value = '';
    loadCustomModels();
  } catch (e) {
    errBox.textContent = e.message;
    errBox.style.display = 'block';
  }
}

async function deleteCustomModel(id) {
  if (!confirm('Delete custom model "' + id + '"?')) return;
  try {
    // Path may contain slashes (e.g. cb/kimi-k3) — encode each segment to
    // preserve them; server DELETE /api/models/custom/*id reads the raw path.
    await apiFetch('/api/models/custom/' + id.split('/').map(encodeURIComponent).join('/'), {method: 'DELETE'});
    loadCustomModels();
  } catch (e) {
    alert('Delete failed: ' + e.message);
  }
}

async function loadAliases() {
  var tbody = document.getElementById('alTableBody');
  if (!tbody) return;
  try {
    var res = await apiFetch('/api/aliases');
    var aliases = res.aliases || {};
    var names = Object.keys(aliases).sort();
    if (names.length === 0) {
      tbody.innerHTML = '<tr><td colspan="3" style="text-align:center;padding:20px;color:var(--text-tertiary)">No aliases. Add one above.</td></tr>';
      return;
    }
    tbody.innerHTML = names.map(function(a) {
      return '<tr>' +
        '<td style="padding:8px;border-bottom:1px solid var(--border);font-family:var(--mono);color:var(--emerald)">' + escHtml(a) + '</td>' +
        '<td style="padding:8px;border-bottom:1px solid var(--border);font-family:var(--mono)">' + escHtml(aliases[a]) + '</td>' +
        '<td style="padding:8px;border-bottom:1px solid var(--border)"><button class="btn-ghost delete-alias-btn" data-alias="' + escHtml(a) + '" style="padding:4px 10px;font-size:11px;color:var(--red)">Delete</button></td>' +
        '</tr>';
    }).join('');
  } catch (e) {
    tbody.innerHTML = '<tr><td colspan="3" style="text-align:center;padding:20px;color:var(--red)">Error: ' + escHtml(e.message) + '</td></tr>';
  }
}

async function addAlias() {
  var errBox = document.getElementById('alError');
  errBox.style.display = 'none';
  var alias = document.getElementById('alAlias').value.trim();
  var target = document.getElementById('alTarget').value.trim();
  if (!alias || !target) {
    errBox.textContent = 'Alias and target are required';
    errBox.style.display = 'block';
    return;
  }
  try {
    await apiFetch('/api/aliases', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify({alias: alias, target: target})
    });
    document.getElementById('alAlias').value = '';
    document.getElementById('alTarget').value = '';
    loadAliases();
  } catch (e) {
    errBox.textContent = e.message;
    errBox.style.display = 'block';
  }
}

async function deleteAlias(alias) {
  if (!confirm('Delete alias "' + alias + '"?')) return;
  try {
    await apiFetch('/api/aliases/' + encodeURIComponent(alias), {method: 'DELETE'});
    loadAliases();
  } catch (e) {
    alert('Delete failed: ' + e.message);
  }
}

/* ══════════════════════════════════════════════════════════
   COMBOS (v1.4.0) — group models with fallback / round-robin
   ══════════════════════════════════════════════════════════ */
async function loadCombos() {
  var tbody = document.getElementById('cbxTableBody');
  if (!tbody) return;
  try {
    var res = await apiFetch('/api/combos');
    var combos = res.combos || [];
    if (combos.length === 0) {
      tbody.innerHTML = '<tr><td colspan="5" style="text-align:center;padding:20px;color:var(--text-tertiary)">No combos yet. Create one above.</td></tr>';
      return;
    }
    combos.sort(function(a, b) { return (a.name || '').localeCompare(b.name || ''); });
    tbody.innerHTML = combos.map(function(c) {
      var name = c.name || '';
      var strategy = c.strategy || 'fallback';
      var models = Array.isArray(c.models) ? c.models : [];
      var desc = c.description || '';
      var strategyBadge = strategy === 'round_robin'
        ? '<span style="padding:2px 8px;border-radius:10px;background:rgba(90,200,120,0.15);color:var(--emerald);font-size:11px">Round Robin</span>'
        : '<span style="padding:2px 8px;border-radius:10px;background:rgba(100,150,255,0.15);color:var(--brand);font-size:11px">Fallback</span>';
      var modelsHtml = models.map(function(m) {
        return '<code style="font-family:var(--mono);font-size:11px;padding:1px 6px;background:var(--bg);border-radius:3px;margin-right:4px;display:inline-block;margin-bottom:2px">' + escHtml(m) + '</code>';
      }).join('');
      return '<tr>' +
        '<td style="padding:8px;border-bottom:1px solid var(--border);font-family:var(--mono);color:var(--brand)">combo/' + escHtml(name) + '</td>' +
        '<td style="padding:8px;border-bottom:1px solid var(--border)">' + strategyBadge + '</td>' +
        '<td style="padding:8px;border-bottom:1px solid var(--border)">' + modelsHtml + '</td>' +
        '<td style="padding:8px;border-bottom:1px solid var(--border);color:var(--text-tertiary)">' + escHtml(desc) + '</td>' +
        '<td style="padding:8px;border-bottom:1px solid var(--border)"><button class="btn-ghost delete-combo-btn" data-name="' + escHtml(name) + '" style="padding:4px 10px;font-size:11px;color:var(--red)">Delete</button></td>' +
      '</tr>';
    }).join('');
  } catch (e) {
    tbody.innerHTML = '<tr><td colspan="5" style="text-align:center;padding:20px;color:var(--red)">Error: ' + escHtml(e.message) + '</td></tr>';
  }
}

async function addCombo() {
  var name = document.getElementById('cbxName').value.trim();
  var strategy = document.getElementById('cbxStrategy').value;
  var desc = document.getElementById('cbxDesc').value.trim();
  var errBox = document.getElementById('cbxError');
  errBox.style.display = 'none';

  // Get selected models from checkbox selector
  var models = getSelectedComboModels();

  if (!name) {
    errBox.textContent = 'Name is required';
    errBox.style.display = 'block';
    return;
  }
  if (models.length === 0) {
    errBox.textContent = 'Select at least one model';
    errBox.style.display = 'block';
    return;
  }
  try {
    await apiFetch('/api/combos', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify({name: name, strategy: strategy, models: models, description: desc})
    });
    document.getElementById('cbxName').value = '';
    document.getElementById('cbxDesc').value = '';
    // Clear all checkboxes
    toggleAllComboModels(false);
    document.getElementById('cbxDesc').value = '';
    loadCombos();
  } catch (e) {
    errBox.textContent = 'Add failed: ' + e.message;
    errBox.style.display = 'block';
  }
}

async function deleteCombo(name) {
  if (!confirm('Delete combo "' + name + '"?')) return;
  try {
    await apiFetch('/api/combos/' + encodeURIComponent(name), {method: 'DELETE'});
    loadCombos();
  } catch (e) {
    alert('Delete failed: ' + e.message);
  }
}

/* ══════════════════════════════════════════════════════════
   PROXY POOL (v1.5.0)
   ══════════════════════════════════════════════════════════ */
window._allProxies = [];

async function loadProxies() {
  try {
    var d = await fetchJSON('/api/proxies');
    var list = d.proxies || [];
    var stats = d.stats || {};
    window._allProxies = list;
    document.getElementById('proxyTotal').textContent = stats.total || 0;
    document.getElementById('proxyEnabled').textContent = stats.enabled || 0;
    document.getElementById('proxyDisabled').textContent = stats.disabled || 0;
    document.getElementById('proxyActive').textContent = stats.active || 0;
    var meta = document.getElementById('proxyPoolMeta');
    if (meta) meta.textContent = stats.enabled + ' active in round-robin';
    renderProxies();
  } catch (e) {
    if (e.message === '__redirect__') return;
    document.getElementById('proxyBody').innerHTML = '<tr><td colspan="9" style="text-align:center;padding:24px;color:var(--red)">Load failed: ' + escHtml(e.message) + '</td></tr>';
  }
}

function renderProxies() {
  var list = window._allProxies || [];
  var body = document.getElementById('proxyBody');
  if (!list.length) {
    body.innerHTML = '<tr><td colspan="9" style="text-align:center;padding:24px;color:var(--text-tertiary)">No proxies configured. Requests use direct connection.</td></tr>';
    return;
  }
  var rows = '';
  for (var i = 0; i < list.length; i++) {
    var p = list[i];
    var status = p.enabled
      ? '<span class="ok" style="font-size:11px">● enabled</span>'
      : '<span style="color:var(--text-tertiary);font-size:11px">● disabled</span>';
    var auth = p.username ? '<span class="mono" style="font-size:11px">' + escHtml(p.username) + ':***</span>' : '<span style="color:var(--text-tertiary)">—</span>';
    var lastUsed = '—';
    if (p.last_used_at) {
      var ts = new Date(p.last_used_at).getTime();
      if (ts > 0) {
        var secs = Math.floor((Date.now() - ts) / 1000);
        if (secs < 60) lastUsed = secs + 's ago';
        else if (secs < 3600) lastUsed = Math.floor(secs / 60) + 'm ago';
        else if (secs < 86400) lastUsed = Math.floor(secs / 3600) + 'h ago';
        else lastUsed = Math.floor(secs / 86400) + 'd ago';
      }
    }
    var failStyle = p.fail_count >= 3 ? 'color:var(--red);font-weight:600' : (p.fail_count > 0 ? 'color:#e6a23c' : '');
    var upstreams = Array.isArray(p.upstreams) && p.upstreams.length ? p.upstreams : ['all'];
    var badges = '';
    for (var j = 0; j < upstreams.length; j++) {
      var u = upstreams[j];
      var color = '#7c8590', label = u;
      if (u === 'all')       { color = 'var(--brand)';  label = 'All'; }
      else if (u === 'grok') { color = '#4a9eff';       label = 'Grok'; }
      else if (u === 'codebuddy') { color = '#c084fc';  label = 'CodeBuddy'; }
      badges += '<span style="display:inline-block;padding:2px 7px;margin-right:4px;font-size:10px;font-weight:500;border-radius:3px;background:' + color + '22;color:' + color + ';border:1px solid ' + color + '55">' + escHtml(label) + '</span>';
    }
    rows +=
      '<tr>' +
        '<td>' + escHtml(p.label || '(no label)') + '</td>' +
        '<td class="mono" style="text-transform:uppercase;font-size:11px">' + escHtml(p.protocol) + '</td>' +
        '<td class="mono">' + escHtml(p.host) + ':' + escHtml(String(p.port)) + '</td>' +
        '<td>' + auth + '</td>' +
        '<td>' + badges + '</td>' +
        '<td>' + status + '</td>' +
        '<td class="mono" style="' + failStyle + '">' + (p.fail_count || 0) + '</td>' +
        '<td style="color:var(--text-tertiary);font-size:11px">' + lastUsed + '</td>' +
        '<td>' +
          '<button class="btn-ghost test-proxy-btn" data-proxy-id="' + escHtml(p.id) + '" style="padding:3px 8px;font-size:11px;margin-right:4px">Test</button>' +
          '<button class="btn-ghost toggle-proxy-btn" data-proxy-id="' + escHtml(p.id) + '" style="padding:3px 8px;font-size:11px;margin-right:4px">' + (p.enabled ? 'Disable' : 'Enable') + '</button>' +
          '<button class="btn-ghost edit-proxy-btn" data-proxy-id="' + escHtml(p.id) + '" style="padding:3px 8px;font-size:11px;margin-right:4px">Edit</button>' +
          '<button class="btn-ghost delete-proxy-btn" data-proxy-id="' + escHtml(p.id) + '" style="padding:3px 8px;font-size:11px;color:var(--red)">Delete</button>' +
        '</td>' +
      '</tr>';
  }
  body.innerHTML = rows;
}

// -------- upstream-checkbox helpers --------
// Read the selected upstreams for the given modal target ("add" or "edit").
// Returns e.g. ["all"] or ["grok","codebuddy"]. Always non-empty (falls
// back to ["all"] if the operator unchecks everything).
function readUpstreams(target) {
  var boxes = document.querySelectorAll('.px-upstream[data-target="' + target + '"]');
  var out = [];
  for (var i = 0; i < boxes.length; i++) {
    if (boxes[i].checked) out.push(boxes[i].getAttribute('data-scope'));
  }
  if (out.length === 0) out = ['all'];
  // If "all" was checked with anything else, collapse to ["all"] to match
  // server-side normalisation.
  if (out.indexOf('all') !== -1) return ['all'];
  return out;
}

// Set checkbox state for the given target from a []string list. Missing/
// nil list defaults to ["all"].
function writeUpstreams(target, list) {
  var arr = (Array.isArray(list) && list.length) ? list : ['all'];
  var boxes = document.querySelectorAll('.px-upstream[data-target="' + target + '"]');
  for (var i = 0; i < boxes.length; i++) {
    var scope = boxes[i].getAttribute('data-scope');
    boxes[i].checked = arr.indexOf(scope) !== -1;
  }
}

// Delegated change handler for upstream checkboxes:
//   - Checking "All"      → uncheck Grok + CodeBuddy in the same target
//   - Checking Grok or CB → uncheck "All" in the same target
//   - Unchecking the last non-All box does NOT re-check "All" (operator
//     might be mid-edit); readUpstreams falls back to ["all"] on submit.
document.addEventListener('change', function(ev) {
  var t = ev.target;
  if (!(t && t.classList && t.classList.contains('px-upstream'))) return;
  var target = t.getAttribute('data-target');
  var scope = t.getAttribute('data-scope');
  if (!target || !scope) return;
  if (!t.checked) return; // only enforce exclusivity on check
  var boxes = document.querySelectorAll('.px-upstream[data-target="' + target + '"]');
  if (scope === 'all') {
    for (var i = 0; i < boxes.length; i++) {
      if (boxes[i].getAttribute('data-scope') !== 'all') boxes[i].checked = false;
    }
  } else {
    for (var j = 0; j < boxes.length; j++) {
      if (boxes[j].getAttribute('data-scope') === 'all') boxes[j].checked = false;
    }
  }
});

async function addProxy() {
  var errBox = document.getElementById('pxError');
  errBox.style.display = 'none';
  var protocol = document.getElementById('pxProtocol').value;
  var host = document.getElementById('pxHost').value.trim();
  var port = parseInt(document.getElementById('pxPort').value, 10);
  var username = document.getElementById('pxUsername').value.trim();
  var password = document.getElementById('pxPassword').value;
  var label = document.getElementById('pxLabel').value.trim();
  var upstreams = readUpstreams('add');
  if (!host || !port) {
    errBox.textContent = 'Host and port are required';
    errBox.style.display = 'block';
    return;
  }
  try {
    await apiFetch('/api/proxies', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify({protocol: protocol, host: host, port: port, username: username, password: password, label: label, upstreams: upstreams})
    });
    document.getElementById('pxHost').value = '';
    document.getElementById('pxPort').value = '';
    document.getElementById('pxUsername').value = '';
    document.getElementById('pxPassword').value = '';
    document.getElementById('pxLabel').value = '';
    closeAddProxyModal();
    loadProxies();
  } catch (e) {
    if (e.message === '__redirect__') return;
    errBox.textContent = 'Add failed: ' + e.message;
    errBox.style.display = 'block';
  }
}

async function deleteProxy(id) {
  var entry = (window._allProxies || []).find(function(p){return p.id === id;});
  var label = entry ? (entry.label || (entry.host + ':' + entry.port)) : id;
  if (!confirm('Delete proxy "' + label + '"?')) return;
  try {
    await apiFetch('/api/proxies/' + encodeURIComponent(id), {method: 'DELETE'});
    loadProxies();
  } catch (e) {
    if (e.message === '__redirect__') return;
    alert('Delete failed: ' + e.message);
  }
}

async function toggleProxy(id) {
  try {
    await apiFetch('/api/proxies/' + encodeURIComponent(id) + '/toggle', {method: 'POST'});
    loadProxies();
  } catch (e) {
    if (e.message === '__redirect__') return;
    alert('Toggle failed: ' + e.message);
  }
}

async function testProxy(id, btn) {
  var originalText = btn ? btn.textContent : null;
  if (btn) { btn.textContent = 'Testing…'; btn.disabled = true; }
  var box = document.getElementById('proxyTestResult');
  box.style.display = 'block';
  box.innerHTML = 'Testing proxy…';
  try {
    var r = await apiFetch('/api/proxies/' + encodeURIComponent(id) + '/test', {method: 'POST'});
    if (r.success) {
      box.innerHTML = '<span class="ok">✓ OK</span> — egress IP: <span class="mono">' + escHtml(r.ip || '(unknown)') + '</span> · ' + (r.latency_ms || 0) + 'ms';
    } else {
      box.innerHTML = '<span class="err">✗ FAILED</span> — ' + escHtml(r.error || 'unknown error') + ' · ' + (r.latency_ms || 0) + 'ms';
    }
  } catch (e) {
    if (e.message === '__redirect__') return;
    box.innerHTML = '<span class="err">✗ Test request failed</span>: ' + escHtml(e.message);
  } finally {
    if (btn && originalText !== null) { btn.textContent = originalText; btn.disabled = false; }
  }
}

function showEditProxyModal(id) {
  var entry = (window._allProxies || []).find(function(p){return p.id === id;});
  if (!entry) { alert('Proxy not found: ' + id); return; }
  document.getElementById('epxId').value = entry.id;
  document.getElementById('epxProtocol').value = entry.protocol;
  document.getElementById('epxHost').value = entry.host || '';
  document.getElementById('epxPort').value = entry.port || '';
  document.getElementById('epxUsername').value = entry.username || '';
  document.getElementById('epxPassword').value = '';
  document.getElementById('epxLabel').value = entry.label || '';
  writeUpstreams('edit', entry.upstreams);
  document.getElementById('epxError').style.display = 'none';
  document.getElementById('editProxyModal').classList.add('show');
}

function showAddProxyModal() {
  document.getElementById('pxProtocol').value = 'http';
  document.getElementById('pxHost').value = '';
  document.getElementById('pxPort').value = '';
  document.getElementById('pxUsername').value = '';
  document.getElementById('pxPassword').value = '';
  document.getElementById('pxLabel').value = '';
  writeUpstreams('add', ['all']);
  document.getElementById('pxError').style.display = 'none';
  document.getElementById('addProxyModal').classList.add('show');
}

function closeAddProxyModal() {
  document.getElementById('addProxyModal').classList.remove('show');
}

function closeEditProxyModal() {
  document.getElementById('editProxyModal').classList.remove('show');
}

async function saveEditProxy() {
  var id = document.getElementById('epxId').value;
  var errBox = document.getElementById('epxError');
  errBox.style.display = 'none';
  var protocol = document.getElementById('epxProtocol').value;
  var host = document.getElementById('epxHost').value.trim();
  var port = parseInt(document.getElementById('epxPort').value, 10);
  var username = document.getElementById('epxUsername').value.trim();
  var passwordRaw = document.getElementById('epxPassword').value;
  var label = document.getElementById('epxLabel').value.trim();
  var upstreams = readUpstreams('edit');
  // Blank password field = keep current (send "***" sentinel).
  var password = passwordRaw === '' ? '***' : passwordRaw;
  if (!host || !port) {
    errBox.textContent = 'Host and port are required';
    errBox.style.display = 'block';
    return;
  }
  try {
    await apiFetch('/api/proxies/' + encodeURIComponent(id), {
      method: 'PUT',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify({protocol: protocol, host: host, port: port, username: username, password: password, label: label, upstreams: upstreams})
    });
    closeEditProxyModal();
    loadProxies();
  } catch (e) {
    if (e.message === '__redirect__') return;
    errBox.textContent = 'Save failed: ' + e.message;
    errBox.style.display = 'block';
  }
}

/* ══════════════════════════════════════════════════════════
   TUNNEL (v1.6.0) — Cloudflare Tunnel management.
   Backed by /api/tunnel/{status,enable,disable,restart}.
   ══════════════════════════════════════════════════════════ */
window._tunnelPollTimer = null;

function startTunnelPolling() {
  if (window._tunnelPollTimer) return;
  window._tunnelPollTimer = setInterval(function(){
    if (window.location.hash === '#/tunnel') loadTunnelStatus();
  }, 5000);
}
function stopTunnelPolling() {
  if (window._tunnelPollTimer) {
    clearInterval(window._tunnelPollTimer);
    window._tunnelPollTimer = null;
  }
}

async function loadTunnelStatus() {
  try {
    var data = await apiFetch('/api/tunnel/status');
    var st = data && data.status ? data.status : data;
    renderTunnelStatus(st);
  } catch (e) {
    if (e.message === '__redirect__') return;
    document.getElementById('tunLogs').textContent = 'Status load failed: ' + e.message;
  }
}

function renderTunnelStatus(st) {
  if (!st) return;
  document.getElementById('tunModeVal').textContent = st.mode || 'none';
  document.getElementById('tunModeSub').textContent =
    (st.mode === 'none' || !st.mode) ? 'off · click Enable to start' :
    (st.mode + ' · updated ' + (st.updated_at ? new Date(st.updated_at*1000).toLocaleTimeString() : '—'));

  document.getElementById('tunQuickVal').textContent = st.quick_running ? 'RUNNING' : 'stopped';
  document.getElementById('tunQuickVal').style.color = st.quick_running ? 'var(--emerald)' : 'var(--text-tertiary)';
  document.getElementById('tunNamedVal').textContent = st.named_running ? 'RUNNING' : 'stopped';
  document.getElementById('tunNamedVal').style.color = st.named_running ? 'var(--emerald)' : 'var(--text-tertiary)';
  document.getElementById('tunUpstreamVal').textContent = st.upstream_url || '—';

  document.getElementById('tunQuickURL').textContent = st.quick_url || '—';
  document.getElementById('tunNamedURL').textContent = st.tunnel_domain ? ('https://' + st.tunnel_domain) : '—';

  // Credentials summary — only booleans, so we never echo secrets.
  var cred = document.getElementById('tunCredSummary');
  cred.innerHTML =
    tunCredBadge('API Token', st.has_api_token) +
    tunCredBadge('Account ID', st.has_account_id) +
    tunCredBadge('Zone ID', st.has_zone_id) +
    tunCredBadge('Hostname', !!st.tunnel_domain) +
    (st.tunnel_id ? '<span style="color:var(--text-tertiary);font-family:var(--mono);font-size:11px">tunnel_id: ' + escHtml(st.tunnel_id) + '</span>' : '');

  var logs = (st.logs && st.logs.length) ? st.logs.join('\n') : 'no output yet';
  document.getElementById('tunLogs').textContent = logs;
  document.getElementById('tunLogsMeta').textContent = st.logs ? (st.logs.length + ' lines') : '';
}

function tunCredBadge(label, present) {
  var col = present ? 'var(--emerald)' : 'var(--text-tertiary)';
  var dot = present ? '●' : '○';
  return '<span style="color:' + col + '">' + dot + ' ' + escHtml(label) + '</span>';
}

/* ═══════════════════ SETTINGS — Turnstile Solver ═══════════════════ */
async function loadTurnstileSettings() {
  var res = document.getElementById('settingsResult');
  try {
    var d = await apiFetch('/settings/turnstile');
    if (!d) return;
    document.getElementById('setSolverUrl').value = d.solver_url || '';
    document.getElementById('setSiteKey').value = d.sitekey || '';
    if (res) res.textContent = 'config loaded';
  } catch (e) {
    if (e.message === '__redirect__') return;
    if (res) res.textContent = 'load failed: ' + e.message;
  }
}

async function saveTurnstileSettings() {
  var url = document.getElementById('setSolverUrl').value.trim();
  var sk = document.getElementById('setSiteKey').value.trim();
  var res = document.getElementById('settingsResult');
  if (!url || !sk) { if (res) res.textContent = 'solver_url + sitekey required'; return; }
  if (res) { res.textContent = 'saving…'; res.style.color = 'var(--text-tertiary)'; }
  try {
    var d = await apiFetch('/settings/turnstile', { method: 'PUT', body: JSON.stringify({ solver_url: url, sitekey: sk }) });
    if (res) { res.textContent = 'saved ✓ (runtime + Redis)'; res.style.color = 'var(--emerald)'; }
    refreshTurnstileStatus();
  } catch (e) {
    if (e.message === '__redirect__') return;
    if (res) { res.textContent = 'save failed: ' + e.message; res.style.color = 'var(--red)'; }
  }
}

async function testTurnstileSettings() {
  var res = document.getElementById('settingsResult');
  var btn = document.querySelector('#page-settings .btn-ghost');
  if (btn) btn.disabled = true;
  if (res) { res.textContent = 'solving…'; res.style.color = 'var(--text-tertiary)'; }
  try {
    var d = await apiFetch('/settings/turnstile/test', { method: 'POST' });
    if (res) {
      if (d.ok) { res.textContent = 'solver OK · token ' + d.token_len + ' chars · ' + d.elapsed_ms + 'ms'; res.style.color = 'var(--emerald)'; }
      else { res.textContent = 'solver error: ' + d.error; res.style.color = 'var(--red)'; }
    }
  } catch (e) {
    if (e.message === '__redirect__') return;
    if (res) { res.textContent = 'test failed: ' + e.message; res.style.color = 'var(--red)'; }
  } finally {
    if (btn) btn.disabled = false;
  }
}

/* ═══════════════════ SETTINGS — Content Filters ═══════════════════ */
async function loadFilterSettings() {
  var list = document.getElementById('setFiltersList');
  var res = document.getElementById('filtersResult');
  try {
    var d = await apiFetch('/settings/filters');
    var en = document.getElementById('setFiltersEnabled');
    if (en) en.checked = !!d.enabled;
    if (list) {
      var rules = (d.rules || []).map(function (r) {
        return '<label style="display:flex;align-items:center;gap:8px;cursor:pointer;padding:5px 6px;border-radius:6px;font-size:12px;color:var(--text-secondary)" title="' + escHtml(r.id) + '">' +
          '<input type="checkbox" data-fid="' + escHtml(r.id) + '"' + (r.is_active ? ' checked' : '') + ' style="width:14px;height:14px;accent-color:var(--brand);cursor:pointer" />' +
          '<span style="flex:1">' + escHtml(r.label || r.id) + '</span>' +
          '<code style="font-size:10px;color:var(--text-tertiary);font-family:var(--mono)">' + escHtml(r.id) + '</code></label>';
      }).join('');
      list.innerHTML = rules || '<div style="font-size:12px;color:var(--text-tertiary)">no rules</div>';
    }
    if (res) { res.textContent = 'filters loaded'; res.style.color = 'var(--text-tertiary)'; }
  } catch (e) {
    if (e.message === '__redirect__') return;
    if (list) list.innerHTML = '<div style="font-size:12px;color:var(--red)">load failed: ' + escHtml(e.message) + '</div>';
    if (res) { res.textContent = 'load failed'; res.style.color = 'var(--red)'; }
  }
}

async function saveFilterSettings() {
  var res = document.getElementById('filtersResult');
  var enabled = !!(document.getElementById('setFiltersEnabled') || {}).checked;
  var boxes = document.querySelectorAll('#setFiltersList input[data-fid]');
  var rules = [];
  boxes.forEach(function (b) { rules.push({ id: b.getAttribute('data-fid'), is_active: b.checked }); });
  if (res) { res.textContent = 'saving…'; res.style.color = 'var(--text-tertiary)'; }
  try {
    var d = await apiFetch('/settings/filters', { method: 'PUT', body: JSON.stringify({ enabled: enabled, rules: rules }) });
    if (res) { res.textContent = 'saved ✓ runtime + Redis (' + d.rules + ' rules)'; res.style.color = 'var(--emerald)'; }
  } catch (e) {
    if (e.message === '__redirect__') return;
    if (res) { res.textContent = 'save failed: ' + e.message; res.style.color = 'var(--red)'; }
  }
}

async function refreshTurnstileStatus() {
  try {
    var d = await apiFetch('/settings/turnstile/test', { method: 'POST' });
    var r = document.getElementById('setReachVal');
    if (d.ok) {
      if (r) { r.textContent = 'YES'; r.style.color = 'var(--emerald)'; }
      var ls = document.getElementById('setLastSolve');
      if (ls) { ls.textContent = d.token_len + ' chars'; ls.style.color = 'var(--emerald)'; }
      var el = document.getElementById('setElapsedVal');
      if (el) { el.textContent = d.elapsed_ms + 'ms'; el.style.color = 'var(--emerald)'; }
    } else {
      if (r) { r.textContent = 'NO'; r.style.color = 'var(--red)'; }
      var ls2 = document.getElementById('setLastSolve');
      if (ls2) { ls2.textContent = 'error'; ls2.style.color = 'var(--red)'; }
      var el2 = document.getElementById('setElapsedVal');
      if (el2) { el2.textContent = (d.elapsed_ms || 0) + 'ms'; el2.style.color = 'var(--red)'; }
    }
  } catch (e) {
    var r2 = document.getElementById('setReachVal');
    if (r2) { r2.textContent = 'NO'; r2.style.color = 'var(--red)'; }
  }
}

/* ═══════════════════ MEDIA STUDIO — Grok image gen/edit/video ═══════════════════ */
var mediaEditB64 = null;
var mediaEditMime = 'image/jpeg';
var mediaEditResults = [];
var mediaVidTimer = null;

function mediaTab(t) {
  var panes = { chat: 'mediaChat', gen: 'mediaGen', edit: 'mediaEdit', vid: 'mediaVid' };
  Object.keys(panes).forEach(function (k) {
    document.getElementById(panes[k]).style.display = (k === t) ? 'grid' : 'none';
  });
  document.querySelectorAll('#page-media .media-tab').forEach(function (b) {
    b.classList.toggle('active', b.getAttribute('data-mtab') === t);
  });
}

/* ── Chat (LLM prompt engineer) ─────────────────────────────────────────── */
var MEDIA_SYS_PROMPT = 'You are an expert image prompt engineer for Grok Imagine (xAI image model). ' +
  'The user describes what they want; you convert it into ONE high-quality image generation prompt. ' +
  'Rules: be specific and vivid — subject, style, lighting, composition, mood, color palette, camera/angle, ' +
  'artistic references. 50-120 words. Reply with ONLY the final prompt — no preamble, no markdown, no quotes. ' +
  'If the request is ambiguous, ask ONE short clarifying question instead.';
var mediaChatMsgs = [];

function mediaChatBubble(role, text) {
  var b = document.createElement('div');
  b.style.cssText = 'max-width:85%;padding:8px 11px;border-radius:10px;white-space:pre-wrap;word-break:break-word;line-height:1.45;' +
    (role === 'user'
      ? 'align-self:flex-end;background:var(--brand);color:#000;border-bottom-right-radius:2px;font-weight:500'
      : 'align-self:flex-start;background:var(--bg2,#222);border:1px solid var(--border-strong);color:var(--text-primary);border-bottom-left-radius:2px');
  b.textContent = text;
  return b;
}

function renderMediaChat() {
  var box = document.getElementById('mcMessages');
  // keep the intro hint as the first child
  box.innerHTML = '<div style="color:var(--text-tertiary);padding:6px 0">Describe what you want to create — the LLM turns it into an optimized prompt for Grok Imagine. Keep chatting to refine; the last assistant reply becomes the image prompt.</div>';
  mediaChatMsgs.forEach(function (m) { box.appendChild(mediaChatBubble(m.role, m.content)); });
  box.scrollTop = box.scrollHeight;
  // Simple UI hero: show only while the chat is empty
  var hero = document.getElementById('mcHero');
  if (hero) hero.style.display = mediaChatMsgs.length ? 'none' : 'block';
}

function mediaChatGreeting() {
  var el = document.getElementById('mcGreeting');
  if (!el) return;
  var h = new Date().getHours();
  var g = h < 5 ? 'Late night' : h < 12 ? 'Good morning' : h < 17 ? 'Good afternoon' : 'Good evening';
  el.textContent = g + ' — let\'s craft something';
}

function mediaChatGen() {
  // Simple UI 🎨: generate image straight from the composer text, no tab switch
  var t = document.getElementById('mcInput').value.trim();
  if (!t) return;
  document.getElementById('mgPrompt').value = t;
  mediaGenerate();
}

function mediaChatClear() {
  if (mediaChatMsgs.length) mediaPersistChat();
  mediaChatMsgs = [];
  document.getElementById('mcCurPrompt').textContent = '';
  document.getElementById('mcHistory').value = '';
  renderMediaChat();
}

function mediaChatNew() {
  if (mediaChatMsgs.length) mediaPersistChat();
  mediaChatId = null;
  mediaChatMsgs = [];
  document.getElementById('mcCurPrompt').textContent = '';
  document.getElementById('mcHistory').value = '';
  renderMediaChat();
  document.getElementById('mcInput').focus();
}

/* ── Chat history (localStorage, survives refresh/navigation) ───────────── */
var mediaChatsKey = 'foxrouters_media_chats';
var mediaChatId = null;

function mediaLoadChats() {
  try { return JSON.parse(localStorage.getItem(mediaChatsKey) || '[]'); } catch (e) { return []; }
}
function mediaSaveChats(chats) {
  try { localStorage.setItem(mediaChatsKey, JSON.stringify(chats.slice(0, 25))); } catch (e) {}
}
function mediaPersistChat() {
  if (!mediaChatMsgs.length) return;
  var chats = mediaLoadChats();
  var now = Date.now();
  if (!mediaChatId) mediaChatId = 'c' + Date.now() + '-' + Math.floor(Math.random() * 1e6);
  var title = '';
  for (var i = 0; i < mediaChatMsgs.length; i++) {
    if (mediaChatMsgs[i].role === 'user') { title = mediaChatMsgs[i].content.slice(0, 48); break; }
  }
  var entry = { id: mediaChatId, title: title || 'untitled', updated_at: now, messages: mediaChatMsgs };
  var found = false;
  for (var i = 0; i < chats.length; i++) {
    if (chats[i].id === mediaChatId) { chats[i] = entry; found = true; break; }
  }
  if (!found) chats.unshift(entry);
  mediaSaveChats(chats);
  mediaRenderHistory();
}
function mediaRenderHistory() {
  var sel = document.getElementById('mcHistory');
  if (!sel) return;
  var cur = sel.value;
  sel.innerHTML = '<option value="">— saved chats —</option>';
  mediaLoadChats().forEach(function (c) {
    var o = document.createElement('option');
    o.value = c.id;
    var d = new Date(c.updated_at);
    var hm = ('0' + d.getHours()).slice(-2) + ':' + ('0' + d.getMinutes()).slice(-2);
    o.textContent = c.title + ' (' + hm + ')';
    sel.appendChild(o);
  });
  if (cur) sel.value = cur;
}
function mediaHistorySelect(id) {
  if (!id) return;
  var chats = mediaLoadChats();
  for (var i = 0; i < chats.length; i++) {
    if (chats[i].id === id) {
      if (mediaChatMsgs.length) mediaPersistChat();
      mediaChatId = id;
      mediaChatMsgs = chats[i].messages.slice();
      renderMediaChat();
      var last = '';
      for (var j = mediaChatMsgs.length - 1; j >= 0; j--) {
        if (mediaChatMsgs[j].role === 'assistant') { last = mediaChatMsgs[j].content; break; }
      }
      mediaChatSetPrompt(last);
      document.getElementById('mcHistory').value = id;
      return;
    }
  }
}

function mediaChatSetPrompt(text) {
  document.getElementById('mcCurPrompt').textContent = text;
  document.getElementById('mgPrompt').value = text;
  document.getElementById('mePrompt').value = '';
}

async function mediaChatSend() {
  var inp = document.getElementById('mcInput');
  var btn = document.getElementById('mcSendBtn');
  var text = inp.value.trim();
  if (!text) return;
  inp.value = '';
  mediaChatMsgs.push({ role: 'user', content: text });
  renderMediaChat();
  btn.disabled = true;
  btn.textContent = '…';
  // live assistant bubble (streaming)
  var box = document.getElementById('mcMessages');
  var live = document.createElement('div');
  live.style.cssText = 'align-self:flex-start;max-width:85%;background:var(--surface,#1f1f1f);border:1px solid var(--border-strong);border-radius:12px 12px 12px 4px;padding:9px 11px;font-size:12px;white-space:pre-wrap;word-break:break-word;color:var(--text-primary)';
  box.appendChild(live);
  var acc = '';
  try {
    var model = document.getElementById('mcModel').value || 'glm-5.2';
    var eff = document.getElementById('mcReasoning').value;
    var msgs = [{ role: 'system', content: MEDIA_SYS_PROMPT }].concat(mediaChatMsgs.slice(-30));
    var body = { model: model, messages: msgs, stream: true };
    if (eff) body.reasoning_effort = eff;   // reasoning_content gated by CodeBuddy unless requested
    var r = await fetch('/v1/chat/completions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      credentials: 'same-origin'
    });
    if (r.status === 401) { window._stopped = true; window.location.href = '/login'; throw new Error('__redirect__'); }
    if (!r.ok) { var t = await r.text().catch(function () { return ''; }); throw new Error('HTTP ' + r.status + ': ' + t.slice(0, 300)); }
    var reader = r.body.getReader();
    var dec = new TextDecoder();
    var buf = '';
    for (;;) {
      var chunk = await reader.read();
      if (chunk.done) break;
      buf += dec.decode(chunk.value, { stream: true });
      var lines = buf.split('\n');
      buf = lines.pop();
      for (var i = 0; i < lines.length; i++) {
        var line = lines[i].trim();
        if (!line || line.indexOf('data:') !== 0) continue;
        var payload = line.slice(5).trim();
        if (payload === '[DONE]') { buf = ''; break; }
        try {
          var obj = JSON.parse(payload);
          var delta = (obj.choices && obj.choices[0] && obj.choices[0].delta && obj.choices[0].delta.content) || '';
          if (delta) { acc += delta; live.textContent = acc; box.scrollTop = box.scrollHeight; }
        } catch (e) { /* partial chunk — skip */ }
      }
    }
    if (!acc) throw new Error('empty LLM reply');
    mediaChatMsgs.push({ role: 'assistant', content: acc });
    renderMediaChat();
    mediaChatSetPrompt(acc);
    mediaPersistChat();
  } catch (e) {
    if (e.message === '__redirect__') return;
    live.remove();
    var err = document.createElement('div');
    err.style.cssText = 'color:var(--red);font-size:12px;align-self:flex-start';
    err.textContent = 'error: ' + e.message;
    box.appendChild(err);
  } finally {
    btn.disabled = false;
    btn.textContent = 'Send';
  }
}

function mediaGenerateFromChat() {
  var p = document.getElementById('mcCurPrompt').textContent.trim();
  if (!p) return;
  document.getElementById('mgPrompt').value = p;
  mediaTab('gen');
  mediaGenerate();
}

function mediaUsePromptInEdit() {
  var p = document.getElementById('mcCurPrompt').textContent.trim();
  if (!p) return;
  document.getElementById('mePrompt').value = p;
  mediaTab('edit');
}

var MEDIA_FALLBACK_MODELS = ['glm-5.2', 'gpt-5.5', 'grok-4.5', 'claude-sonnet-4.6', 'deepseek-v3', 'kimi-k3', 'gemini-3.1-pro'];

function mediaFillModelSelect(sel, models) {
  // drop cb/ aliases (dupes of bare names)
  var seen = {};
  models = models.filter(function (id) {
    var bare = id.replace(/^cb\//, '');
    if (seen[bare]) return false;
    seen[bare] = true;
    return true;
  });
  if (!models.length) return false;
  var cur = sel.value;
  sel.innerHTML = '';
  models.forEach(function (id) {
    var o = document.createElement('option');
    o.value = id;
    o.textContent = id;
    sel.appendChild(o);
  });
  if (models.indexOf(cur) !== -1) sel.value = cur;
  else if (seen['glm-5.2']) sel.value = 'glm-5.2';
  else sel.value = sel.options[0].value;
  return true;
}

function mediaInit() {
  // seed immediately so the dropdown is never empty/black while models load
  var sel = document.getElementById('mcModel');
  if (sel.options.length === 0) {
    MEDIA_FALLBACK_MODELS.forEach(function (id) {
      var o = document.createElement('option');
      o.value = id;
      o.textContent = id;
      sel.appendChild(o);
    });
    sel.value = 'glm-5.2';
  }
  mediaRenderHistory();
  mediaChatGreeting();
  renderMediaGallery();
  // reuse the models the dashboard already fetched (refresh() caches every 5s)
  if (_availableModels && _availableModels.length) {
    mediaFillModelSelect(sel, _availableModels);
    return;
  }
  fetchJSON('/v1/models').then(function (d) {
    mediaFillModelSelect(sel, (d.data || []).map(function (m) { return m.id; }));
  }).catch(function () {});
}

function mediaCard(title, bodyHtml) {
  return '<div style="background:var(--bg);border:1px solid var(--border-strong);border-radius:var(--radius-sm);padding:12px">' +
    (title ? '<div style="font-size:11px;color:var(--text-tertiary);margin-bottom:8px">' + title + '</div>' : '') +
    bodyHtml + '</div>';
}

async function mediaGenerate() {
  var btn = document.querySelector('#mediaGen .btn-primary');
  var out = document.getElementById('mgResult');
  var prompt = document.getElementById('mgPrompt').value.trim();
  if (!prompt) { out.innerHTML = mediaCard('', '<span style="color:var(--red)">prompt is required</span>'); return; }
  btn.disabled = true;
  out.innerHTML = mediaCard('', '<span style="color:var(--text-tertiary)">generating… (~10s, lazy SSO if cookie expired)</span>');
  try {
    var d = await apiFetch('/v1/images/generations', {
      method: 'POST',
      body: JSON.stringify({
        model: 'grok-imagine-image',
        prompt: prompt,
        aspect_ratio: document.getElementById('mgAspect').value,
        n: parseInt(document.getElementById('mgN').value, 10) || 1,
        response_format: 'b64_json'
      })
    });
    var b64 = d.data[0].b64_json;
    mediaEditB64 = b64;
    mediaEditResults = d.data;
    var cards = d.data.map(function (it, idx) {
      var b = it.b64_json;
      mediaGalleryAdd(b, prompt);
      return mediaCard('', '<img src="data:image/jpeg;base64,' + b + '" style="max-width:100%;border-radius:8px;border:1px solid var(--border-strong)" />' +
        '<div style="margin-top:8px;display:flex;gap:8px;flex-wrap:wrap">' +
        '<a class="btn-ghost" download="grok-image-' + idx + '.jpg" href="data:image/jpeg;base64,' + b + '" style="padding:6px 12px;font-size:11px;text-decoration:none">Download</a>' +
        '<button class="btn-ghost" onclick="mediaUseInEdit(' + idx + ')" style="padding:6px 12px;font-size:11px">Send to Edit</button></div>');
    }).join('');
    out.innerHTML = cards;
  } catch (e) {
    if (e.message === '__redirect__') return;
    out.innerHTML = mediaCard('', '<span style="color:var(--red)">' + escHtml(e.message) + '</span>');
  } finally {
    btn.disabled = false;
  }
}

/* ── Created With Art gallery (Simple UI) ───────────────────────────────── */
var MEDIA_GALLERY_KEY = 'foxrouters_media_gallery';

function mediaGalleryLoad() {
  try { return JSON.parse(localStorage.getItem(MEDIA_GALLERY_KEY) || '[]'); } catch (e) { return []; }
}
function mediaGallerySave(g) {
  try { localStorage.setItem(MEDIA_GALLERY_KEY, JSON.stringify(g.slice(0, 12))); } catch (e) {}
}
function mediaGalleryAdd(b64, prompt) {
  if (!b64) return;
  var g = mediaGalleryLoad();
  g.unshift({ b64: b64, prompt: (prompt || '').slice(0, 90), ts: Date.now() });
  mediaGallerySave(g);
  renderMediaGallery();
}
function renderMediaGallery() {
  var wrap = document.getElementById('mcGalleryWrap');
  var box = document.getElementById('mcGallery');
  if (!wrap || !box) return;
  var g = mediaGalleryLoad();
  if (!g.length) { wrap.style.display = 'none'; return; }
  wrap.style.display = 'block';
  box.innerHTML = g.map(function (it, i) {
    return '<div class="mc-gal-card" onclick="mediaGalleryEdit(' + i + ')" title="' + escHtml(it.prompt || '') + '" ' +
      'style="flex:0 0 auto;width:132px;height:160px;border-radius:14px;border:1px solid var(--border-strong);cursor:pointer;position:relative;overflow:hidden;background:var(--bg2,#222) center/cover no-repeat url(data:image/jpeg;base64,' + it.b64 + ')">' +
      '<span style="position:absolute;left:6px;top:6px;background:rgba(0,0,0,.55);color:#fff;font-size:10px;padding:3px 8px;border-radius:999px;pointer-events:none">View</span>' +
      '</div>';
  }).join('');
}
function mediaGalleryEdit(i) {
  var g = mediaGalleryLoad();
  if (!g[i] || !g[i].b64) return;
  mediaEditB64 = g[i].b64;
  mediaEditMime = 'image/jpeg';
  var img = document.getElementById('meSrcImg');
  var prev = document.getElementById('meSrcPrev');
  if (img) img.src = 'data:image/jpeg;base64,' + mediaEditB64;
  if (prev) prev.style.display = 'flex';
  mediaTab('edit');
}

function mediaUseInEdit(idx) {
  if (mediaEditResults[idx] && mediaEditResults[idx].b64_json) {
    mediaEditB64 = mediaEditResults[idx].b64_json;
  }
  if (!mediaEditB64) return;
  mediaEditMime = 'image/jpeg';
  document.getElementById('meSrcImg').src = 'data:' + mediaEditMime + ';base64,' + mediaEditB64;
  document.getElementById('meSrcPrev').style.display = 'flex';
  mediaTab('edit');
}

function mediaClearEditSrc() {
  mediaEditB64 = null;
  document.getElementById('meSrcImg').src = '';
  document.getElementById('meSrcPrev').style.display = 'none';
  document.getElementById('meFile').value = '';
}

function mediaFileToB64(file) {
  return new Promise(function (res, rej) {
    var r = new FileReader();
    r.onload = function () { res(String(r.result).split(',')[1]); };
    r.onerror = rej;
    r.readAsDataURL(file);
  });
}

var MEDIA_EDIT_SYS_PROMPT = 'You are an expert image editing prompt engineer for Grok Imagine (xAI image model). ' +
  'You are shown the CURRENT image plus the user\'s desired change in plain words. ' +
  'Write ONE precise image-edit prompt that: ' +
  '(1) briefly describes what you can SEE in the current image (subject, setting, style, lighting), ' +
  '(2) instructs the model to keep the original EXACTLY the same — same subject, same setting, same composition, same style and lighting, ' +
  '(3) then describes ONLY the requested change in specific detail (exact position, size, appearance, quantity). ' +
  '40-100 words. Reply with ONLY the final edit prompt — no preamble, no markdown, no quotes. ' +
  'If the request is ambiguous, ask ONE short clarifying question instead.';

async function mediaAiCompose() {
  var btn = document.getElementById('meAiBtn');
  var out = document.getElementById('meResult');
  var hint = document.getElementById('meAiHint').value.trim();
  if (!hint) { out.innerHTML = mediaCard('', '<span style="color:var(--red)">describe the edit in plain words first</span>'); return; }
  if (!mediaEditB64) { out.innerHTML = mediaCard('', '<span style="color:var(--red)">pick an image file or generate one first (Send to Edit)</span>'); return; }
  btn.disabled = true;
  var old = btn.textContent; btn.textContent = '…';
  out.innerHTML = mediaCard('', '<span style="color:var(--text-tertiary)">LLM looking at the image…</span>');
  try {
    var model = document.getElementById('meAiModel').value;
    var msgs = [
      { role: 'system', content: MEDIA_EDIT_SYS_PROMPT },
      { role: 'user', content: [
        { type: 'text', text: 'My edit: ' + hint },
        { type: 'image_url', image_url: { url: 'data:' + mediaEditMime + ';base64,' + mediaEditB64 } }
      ]}
    ];
    var d = await apiFetch('/v1/chat/completions', {
      method: 'POST',
      body: JSON.stringify({ model: model, messages: msgs, stream: false })
    });
    var content = d.choices[0].message.content;
    if (!content || !content.trim()) throw new Error('LLM returned an empty prompt');
    document.getElementById('mePrompt').value = content.trim();
    out.innerHTML = mediaCard('', '<span style="color:var(--green)">prompt composed with vision — review, tweak if needed, then hit Edit Image</span>');
  } catch (e) {
    if (e.message === '__redirect__') return;
    out.innerHTML = mediaCard('', '<span style="color:var(--red)">' + escHtml(e.message) + '</span>');
  } finally {
    btn.disabled = false; btn.textContent = old;
  }
}

async function mediaEditRun() {
  var btn = document.querySelector('#mediaEdit .btn-primary');
  var out = document.getElementById('meResult');
  var prompt = document.getElementById('mePrompt').value.trim();
  if (!prompt) { out.innerHTML = mediaCard('', '<span style="color:var(--red)">prompt is required</span>'); return; }
  var f = document.getElementById('meFile').files[0];
  var img = mediaEditB64;
  if (f) { img = await mediaFileToB64(f); mediaEditB64 = img; mediaEditMime = f.type || 'image/jpeg'; }
  if (!img) { out.innerHTML = mediaCard('', '<span style="color:var(--red)">pick an image file or generate one first (Send to Edit)</span>'); return; }
  btn.disabled = true;
  out.innerHTML = mediaCard('', '<span style="color:var(--text-tertiary)">editing… (~10s)</span>');
  try {
    var d = await apiFetch('/v1/images/edits', {
      method: 'POST',
      body: JSON.stringify({ model: 'grok-imagine-image', prompt: prompt, image: img })
    });
    var url = d.data[0].url;
    out.innerHTML = mediaCard('', '<img src="' + url + '" style="max-width:100%;border-radius:8px;border:1px solid var(--border-strong)" />' +
      '<div style="margin-top:8px"><a class="btn-ghost" download="grok-edit.jpg" href="' + url + '" style="padding:6px 12px;font-size:11px;text-decoration:none">Download</a></div>');
  } catch (e) {
    if (e.message === '__redirect__') return;
    out.innerHTML = mediaCard('', '<span style="color:var(--red)">' + escHtml(e.message) + '</span>');
  } finally {
    btn.disabled = false;
  }
}

async function mediaVideo() {
  var btn = document.querySelector('#mediaVid .btn-primary');
  var prog = document.getElementById('mvProgress');
  var out = document.getElementById('mvResult');
  var prompt = document.getElementById('mvPrompt').value.trim();
  if (!prompt) { out.innerHTML = mediaCard('', '<span style="color:var(--red)">prompt is required</span>'); return; }
  btn.disabled = true;
  prog.textContent = 'starting…';
  out.innerHTML = '';
  try {
    var d = await apiFetch('/v1/videos/generations', {
      method: 'POST',
      body: JSON.stringify({ model: 'grok-imagine-image', prompt: prompt })
    });
    var id = d.id;
    prog.textContent = 'queued (' + id + ') — polling…';
    if (mediaVidTimer) clearInterval(mediaVidTimer);
    mediaVidTimer = setInterval(function () { mediaVideoPoll(id); }, 5000);
  } catch (e) {
    if (e.message === '__redirect__') return;
    prog.textContent = '';
    out.innerHTML = mediaCard('', '<span style="color:var(--red)">' + escHtml(e.message) + '</span>');
    btn.disabled = false;
  }
}

async function mediaVideoPoll(id) {
  var prog = document.getElementById('mvProgress');
  var out = document.getElementById('mvResult');
  try {
    var d = await apiFetch('/v1/videos/' + id);
    if (d.status === 'completed') {
      if (mediaVidTimer) { clearInterval(mediaVidTimer); mediaVidTimer = null; }
      document.querySelector('#mediaVid .btn-primary').disabled = false;
      prog.textContent = '';
      var url = d.data[0].url;
      out.innerHTML = mediaCard('', '<video controls src="' + url + '" style="max-width:100%;border-radius:8px;border:1px solid var(--border-strong)"></video>' +
        '<div style="margin-top:8px"><a class="btn-ghost" download="grok-video.mp4" href="' + url + '" style="padding:6px 12px;font-size:11px;text-decoration:none">Download</a></div>');
    } else {
      prog.textContent = 'status: ' + (d.status || 'pending') + ' · progress: ' + (d.progress || 0) + '%';
    }
  } catch (e) {
    if (e.message === '__redirect__') return;
    if (mediaVidTimer) { clearInterval(mediaVidTimer); mediaVidTimer = null; }
    document.querySelector('#mediaVid .btn-primary').disabled = false;
    prog.textContent = '';
    out.innerHTML = mediaCard('', '<span style="color:var(--red)">poll failed: ' + escHtml(e.message) + '</span>');
  }
}

async function tunnelEnableQuick() {
  await tunnelEnable({mode: 'quick'});
}

function showTunnelNamedModal(hybrid) {
  document.getElementById('tunModalMode').value = hybrid ? 'hybrid' : 'named';
  document.getElementById('tunnelNamedModalTitle').textContent =
    hybrid ? 'Hybrid Tunnel (Quick + Named) — Cloudflare Credentials'
           : 'Named Tunnel — Cloudflare Credentials';
  document.getElementById('tunAPIToken').value = '';
  document.getElementById('tunAccountID').value = '';
  document.getElementById('tunZoneID').value = '';
  document.getElementById('tunDomain').value = '';
  document.getElementById('tunModalError').style.display = 'none';

  // Show which credentials are already saved in Redis
  var savedEl = document.getElementById('tunSavedCreds');
  if (savedEl) {
    apiFetch('/api/tunnel/status').then(function(r) { return r.json(); }).then(function(st) {
      var badges = [];
      if (st.has_api_token) badges.push('<span style="color:var(--emerald)">● API Token</span>');
      if (st.has_account_id) badges.push('<span style="color:var(--emerald)">● Account ID</span>');
      if (st.has_zone_id) badges.push('<span style="color:var(--emerald)">● Zone ID</span>');
      if (st.tunnel_domain) badges.push('<span style="color:var(--emerald)">● Hostname: ' + escHtml(st.tunnel_domain) + '</span>');
      if (badges.length) {
        savedEl.innerHTML = '<div style="font-size:11px;color:var(--text-tertiary);margin-bottom:6px">Saved credentials (leave blank to keep):</div>' +
          '<div style="display:flex;gap:12px;flex-wrap:wrap;font-size:12px">' + badges.join('') + '</div>';
        savedEl.style.display = 'block';
      } else {
        savedEl.style.display = 'none';
      }
    }).catch(function() { savedEl.style.display = 'none'; });
  }

  document.getElementById('tunnelNamedModal').classList.add('active');
}
function closeTunnelNamedModal() {
  document.getElementById('tunnelNamedModal').classList.remove('active');
}

async function tunnelEnableNamed() {
  var mode = document.getElementById('tunModalMode').value || 'named';
  var body = {
    mode: mode,
    cloudflare_api_token: document.getElementById('tunAPIToken').value.trim(),
    cloudflare_account_id: document.getElementById('tunAccountID').value.trim(),
    cloudflare_zone_id: document.getElementById('tunZoneID').value.trim(),
    tunnel_domain: document.getElementById('tunDomain').value.trim()
  };
  var errBox = document.getElementById('tunModalError');
  errBox.style.display = 'none';
  try {
    await tunnelEnable(body);
    closeTunnelNamedModal();
  } catch (e) {
    if (e.message === '__redirect__') return;
    errBox.textContent = 'Enable failed: ' + e.message;
    errBox.style.display = 'block';
  }
}

async function tunnelEnable(body) {
  var meta = document.getElementById('tunActionResult');
  meta.textContent = 'Enabling…';
  try {
    var data = await apiFetch('/api/tunnel/enable', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify(body || {})
    });
    meta.textContent = 'Enabled · ' + (body.mode || 'quick');
    setTimeout(function(){ meta.textContent = ''; }, 4000);
    if (data && data.status) renderTunnelStatus(data.status);
    else loadTunnelStatus();
  } catch (e) {
    if (e.message === '__redirect__') return;
    meta.textContent = 'Enable failed: ' + e.message;
    meta.style.color = 'var(--red)';
    setTimeout(function(){ meta.style.color = 'var(--text-tertiary)'; meta.textContent = ''; }, 6000);
    throw e;
  }
}

async function tunnelDisable() {
  if (!confirm('Disable Cloudflare Tunnel? cloudflared subprocesses will stop.')) return;
  var meta = document.getElementById('tunActionResult');
  meta.textContent = 'Disabling…';
  try {
    var data = await apiFetch('/api/tunnel/disable', {method:'POST', headers:{'content-type':'application/json'}, body:'{}'});
    meta.textContent = 'Disabled';
    setTimeout(function(){ meta.textContent = ''; }, 4000);
    if (data && data.status) renderTunnelStatus(data.status);
    else loadTunnelStatus();
  } catch (e) {
    if (e.message === '__redirect__') return;
    meta.textContent = 'Disable failed: ' + e.message;
  }
}

async function tunnelRestart() {
  var meta = document.getElementById('tunActionResult');
  meta.textContent = 'Restarting…';
  try {
    var data = await apiFetch('/api/tunnel/restart', {method:'POST', headers:{'content-type':'application/json'}, body:'{}'});
    meta.textContent = 'Restarted';
    setTimeout(function(){ meta.textContent = ''; }, 4000);
    if (data && data.status) renderTunnelStatus(data.status);
    else loadTunnelStatus();
  } catch (e) {
    if (e.message === '__redirect__') return;
    meta.textContent = 'Restart failed: ' + e.message;
  }
}

/* ══════════════════════════════════════════════════════════
   INIT — runs here (end of last script block) so router() and
   its hooks (mediaInit, mediaPersistChat, loadTurnstileSettings,
   loadTunnelStatus…) see ALL function declarations.
   ══════════════════════════════════════════════════════════ */
router();           // initial route
loadHistory();       // initial history load
refresh();           // initial data load
setInterval(loadHistory, 10000);  // history every 10s
setInterval(refresh, 5000);       // health/accounts every 5s
