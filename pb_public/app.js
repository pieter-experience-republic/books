const pb = new PocketBase(window.location.origin);

const $ = (sel) => document.querySelector(sel);
const authSection = $('#auth');
const appSection = $('#app');
const userBar = $('#user-bar');
const userEmail = $('#user-email');
const statusPill = $('#status-pill');
const cooldownEl = $('#cooldown');
const searchBtn = $('#search-btn');
const searchesBody = $('#searches-table tbody');
const downloadsBody = $('#downloads-table tbody');
const resultsBody = $('#results-table tbody');
const resultsTable = $('#results-table');
const resultsHeading = $('#results-heading');
const resultsStatus = $('#results-status');
const toastContainer = $('#toast-container');

const RESULTS_PREVIEW = 10;

let activeSearchId = null;
let activeSearchMeta = null; // {id, query, status, error} for the active search
let cooldownTimer = null;
let activeResults = [];   // all rows for the active search
let resultsExpanded = false;
const downloadOutcomesSeen = new Set(); // download ids already toasted with a terminal outcome
const downloadStartedSeen = new Set(); // download ids already toasted once transfer began
const resultDownloadButtons = new Map(); // download id -> the result row's button, so it can track status past "Queued…"

// ---- auth ----

let fileToken = null;

async function refreshAuthUI() {
  const m = pb.authStore.record;
  if (m) {
    authSection.hidden = true;
    appSection.hidden = false;
    userBar.hidden = false;
    userEmail.textContent = m.email;
    try {
      // Short-lived token used to access protected file URLs.
      fileToken = await pb.files.getToken();
    } catch (e) { fileToken = null; }
    bootstrapData();
  } else {
    fileToken = null;
    authSection.hidden = false;
    appSection.hidden = true;
    userBar.hidden = true;
  }
}

$('#auth-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  $('#auth-error').textContent = '';
  try {
    await pb.collection('users').authWithPassword(
      $('#auth-email').value,
      $('#auth-password').value,
    );
    refreshAuthUI();
  } catch (err) {
    $('#auth-error').textContent = err?.message || 'sign-in failed';
  }
});

$('#auth-register').addEventListener('click', async () => {
  $('#auth-error').textContent = '';
  const email = $('#auth-email').value;
  const password = $('#auth-password').value;
  if (!email || password.length < 8) {
    $('#auth-error').textContent = 'email + 8+ char password required';
    return;
  }
  try {
    await pb.collection('users').create({
      email, password, passwordConfirm: password,
    });
    await pb.collection('users').authWithPassword(email, password);
    refreshAuthUI();
  } catch (err) {
    $('#auth-error').textContent = err?.message || 'registration failed';
  }
});

$('#logout').addEventListener('click', () => {
  pb.authStore.clear();
  refreshAuthUI();
});

// ---- main app ----

async function bootstrapData() {
  renderActiveStatus();
  await loadSearches();
  await loadDownloads();
  subscribe();
  pollStatus();
  setInterval(pollStatus, 5000);
}

async function pollStatus() {
  try {
    const s = await pb.send('/api/ebooks/status', {});
    const label = s.connected ? 'Connected' : s.connecting ? 'Connecting…' : 'Offline';
    const cls = s.connected ? 'pill-on' : s.connecting ? 'pill-warn' : 'pill-off';
    statusPill.innerHTML = `<span class="status-dot" aria-hidden="true"></span>${label}`;
    statusPill.className = 'pill ' + cls;

    const next = new Date(s.next_search_at);
    const wait = next.getTime() - Date.now();
    updateCooldown(wait);
  } catch (e) { /* ignored — likely a transient 401 */ }
}

function updateCooldown(ms) {
  if (cooldownTimer) clearInterval(cooldownTimer);
  function tick() {
    if (ms <= 0) {
      cooldownEl.textContent = '';
      searchBtn.disabled = false;
      clearInterval(cooldownTimer);
      return;
    }
    searchBtn.disabled = true;
    cooldownEl.textContent = `cooldown ${Math.ceil(ms / 1000)}s`;
    ms -= 1000;
  }
  tick();
  cooldownTimer = setInterval(tick, 1000);
}

$('#search-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const query = $('#query').value.trim();
  if (!query) return;
  searchBtn.disabled = true;
  try {
    const res = await pb.send('/api/ebooks/search', {
      method: 'POST',
      body: { query },
    });
    activateSearch({ id: res.id, query, status: res.status, error: '' });
  } catch (err) {
    alert(err?.message || 'search failed');
  } finally {
    pollStatus();
  }
});

// Make a search (new or from history) the one shown in the results panel.
function activateSearch(rec) {
  activeSearchId = rec.id;
  activeSearchMeta = { id: rec.id, query: rec.query, status: rec.status, error: rec.error };
  renderActiveStatus();
  showResultsFor(rec.id);
}

async function loadSearches() {
  const list = await pb.collection('searches').getList(1, 30, { sort: '-created' });
  searchesBody.innerHTML = '';
  list.items.forEach(addOrUpdateSearchRow);
}

async function loadDownloads() {
  const list = await pb.collection('downloads').getList(1, 30, { sort: '-created' });
  downloadsBody.innerHTML = '';
  list.items.forEach(addOrUpdateDownloadRow);
}

function subscribe() {
  pb.collection('searches').subscribe('*', (e) => {
    addOrUpdateSearchRow(e.record);
    if (e.record.id === activeSearchId) {
      activeSearchMeta = {
        id: e.record.id, query: e.record.query,
        status: e.record.status, error: e.record.error,
      };
      renderActiveStatus();
    }
  });
  pb.collection('search_results').subscribe('*', (e) => {
    if (e.record.search !== activeSearchId) return;
    activeResults.push(e.record);
    activeResults.sort(compareResults);
    renderResults();
  });
  pb.collection('downloads').subscribe('*', (e) => {
    addOrUpdateDownloadRow(e.record);
    handleDownloadStarted(e.record);
    handleDownloadOutcome(e.record);
    updateResultButton(e.record);
  });
}

// Fires once, right as a download actually starts moving bytes — the
// only signal, short of opening Download history, that a queued
// request wasn't silently dropped (it can otherwise sit quiet for up
// to the 5-minute timeout before you'd hear anything either way).
function handleDownloadStarted(rec) {
  if (rec.status !== 'downloading' || downloadStartedSeen.has(rec.id)) return;
  downloadStartedSeen.add(rec.id);
  const label = rec.filename || filenameHint(rec.command);
  showToast(`Started downloading “${label}”`, { duration: 4000 });
}

// Keeps a search result's own Download button in sync with the download
// it kicked off, past the initial "Queued…" — otherwise it just sits
// there forever regardless of what actually happens to the download.
function updateResultButton(rec) {
  const btn = resultDownloadButtons.get(rec.id);
  if (!btn) return;
  if (rec.status === 'downloading') {
    setResultButtonActive(btn, rec.id, 'Downloading…');
  } else if (rec.status === 'complete') {
    btn.disabled = true;
    btn.classList.remove('btn-active-download');
    delete btn.dataset.downloadId;
    btn.removeAttribute('title');
    btn.textContent = 'Downloaded';
    resultDownloadButtons.delete(rec.id);
  } else if (rec.status === 'failed' || rec.status === 'cancelled') {
    btn.disabled = false;
    btn.classList.remove('btn-active-download');
    delete btn.dataset.downloadId;
    btn.removeAttribute('title');
    btn.textContent = 'Retry';
    resultDownloadButtons.delete(rec.id);
  }
}

// Renders the queued/downloading state: a spinner, the status label, and
// a small "×" that — since the whole button stays clickable — cancels
// the download right where the user is watching it.
function setResultButtonActive(btn, downloadId, label) {
  btn.disabled = false;
  btn.dataset.downloadId = downloadId;
  btn.title = 'Click to cancel';
  btn.classList.add('btn-active-download');
  btn.innerHTML = `<span class="spinner" aria-hidden="true"></span>` +
    `<span>${label}</span><span class="cancel-x" aria-hidden="true">×</span>`;
}

// Fires once per download, the moment it actually finishes (live only —
// never for the pre-existing rows loadDownloads() populates on startup).
// Saves the file straight to the device and confirms with a toast, so
// there's no need to go find the downloads table to grab it.
function handleDownloadOutcome(rec) {
  if (downloadOutcomesSeen.has(rec.id)) return;
  const label = rec.filename || filenameHint(rec.command);
  if (rec.status === 'complete' && rec.file) {
    downloadOutcomesSeen.add(rec.id);
    saveDownloadToDevice(rec);
    showToast(`Downloaded “${label}”`, {
      actionLabel: 'Save again',
      onAction: () => saveDownloadToDevice(rec),
    });
  } else if (rec.status === 'failed') {
    downloadOutcomesSeen.add(rec.id);
    showToast(`Download failed: ${label}${rec.error ? ' — ' + rec.error : ''}`, {
      variant: 'err', duration: 8000,
    });
  } else if (rec.status === 'cancelled') {
    downloadOutcomesSeen.add(rec.id);
    showToast(`Cancelled “${label}”`, { duration: 4000 });
  }
}

function saveDownloadToDevice(rec) {
  const url = pb.files.getURL(rec, rec.file, fileToken ? { token: fileToken } : {});
  const a = document.createElement('a');
  a.href = url;
  a.download = rec.filename || '';
  document.body.appendChild(a);
  a.click();
  a.remove();
  return url;
}

// Lightweight toast: message + optional action button, auto-dismissing.
// Browsers can silently block auto-triggered downloads that don't come
// from a direct click (e.g. Chrome's "multiple automatic downloads"
// guard), so every toast carries a manual fallback action.
function showToast(message, { actionLabel, onAction, variant, duration = 6000 } = {}) {
  const el = document.createElement('div');
  el.className = 'toast' + (variant === 'err' ? ' toast-err' : '');
  const text = document.createElement('span');
  text.textContent = message;
  el.appendChild(text);
  let timer = null;
  const dismiss = () => {
    clearTimeout(timer);
    el.classList.add('leaving');
    el.addEventListener('animationend', () => el.remove(), { once: true });
  };
  if (actionLabel) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.textContent = actionLabel;
    btn.addEventListener('click', () => { onAction?.(); dismiss(); });
    el.appendChild(btn);
  }
  const closeBtn = document.createElement('button');
  closeBtn.type = 'button';
  closeBtn.textContent = '✕';
  closeBtn.setAttribute('aria-label', 'Dismiss');
  closeBtn.addEventListener('click', dismiss);
  el.appendChild(closeBtn);
  toastContainer.appendChild(el);
  timer = setTimeout(dismiss, duration);
}

function addOrUpdateSearchRow(rec) {
  let tr = searchesBody.querySelector(`tr[data-id="${rec.id}"]`);
  if (!tr) {
    tr = document.createElement('tr');
    tr.dataset.id = rec.id;
    tr.innerHTML = '<td></td><td></td><td></td><td></td>';
    tr.style.cursor = 'pointer';
    tr.addEventListener('click', () => activateSearch(tr._record));
    searchesBody.prepend(tr);
  }
  tr._record = rec;
  const cells = tr.children;
  cells[0].textContent = rec.query;
  cells[1].innerHTML = `<span class="status-${rec.status}">${rec.status}</span>` +
    (rec.error ? ` <span class="dim">— ${escape(rec.error)}</span>` : '');
  cells[2].textContent = rec.result_count ?? '';
  cells[3].textContent = formatTime(rec.created);
}

async function showResultsFor(searchId) {
  activeResults = [];
  resultsExpanded = false;
  renderResults();
  const list = await pb.collection('search_results').getList(1, 200, {
    filter: `search="${searchId}"`,
    sort: 'is_duplicate,-relevance,author,title',
  });
  if (searchId !== activeSearchId) return; // user moved on to a different search
  activeResults = list.items.sort(compareResults);
  renderResults();
}

// Non-duplicates first, best query match first, alphabetical tiebreak.
// Mirrors the server-side `sort` param above so live-streamed rows (which
// bypass that sort) end up in the same order as a fresh page load.
function compareResults(a, b) {
  if (a.is_duplicate !== b.is_duplicate) return a.is_duplicate ? 1 : -1;
  if (a.relevance !== b.relevance) return b.relevance - a.relevance;
  if (a.author !== b.author) return a.author < b.author ? -1 : 1;
  return a.title < b.title ? -1 : a.title > b.title ? 1 : 0;
}

function renderResults() {
  resultsBody.innerHTML = '';
  const total = activeResults.length;
  resultsHeading.hidden = total === 0;
  resultsTable.hidden = total === 0;
  if (total > 0) {
    resultsHeading.innerHTML = `Results (${total}) <span class="dim">— epub only</span>`;
  }
  const limit = resultsExpanded ? total : Math.min(RESULTS_PREVIEW, total);
  for (let i = 0; i < limit; i++) {
    resultsBody.appendChild(buildResultRow(activeResults[i]));
  }
  if (total > RESULTS_PREVIEW) {
    const tr = document.createElement('tr');
    tr.className = 'show-more';
    tr.innerHTML = `<td colspan="5">${resultsExpanded ? 'Show fewer' : `Show all ${total} results`}</td>`;
    tr.addEventListener('click', () => {
      resultsExpanded = !resultsExpanded;
      renderResults();
    });
    resultsBody.appendChild(tr);
  }
  renderActiveStatus();
}

// Status line shown above the results table: idle / queued / searching /
// failed / no-results. Hidden once a non-empty results table is showing.
function renderActiveStatus() {
  if (!activeSearchMeta) {
    resultsStatus.hidden = false;
    resultsStatus.textContent = 'Run a search above to see results here.';
    return;
  }
  const { id, query, status, error } = activeSearchMeta;
  if (status === 'queued' || status === 'searching') {
    resultsStatus.hidden = false;
    resultsStatus.innerHTML =
      `<span class="spinner" aria-hidden="true"></span>` +
      `<span>${status === 'queued' ? 'Queued' : 'Searching for'} “${escape(query)}”… ` +
      `<span class="dim">can take up to a minute</span></span>` +
      `<button type="button" class="cancel-search-btn">Cancel</button>`;
    resultsStatus.querySelector('.cancel-search-btn').addEventListener('click', (e) => {
      e.target.disabled = true;
      e.target.textContent = 'Cancelling…';
      cancelActiveSearch(id);
    });
  } else if (status === 'cancelled') {
    resultsStatus.hidden = false;
    resultsStatus.innerHTML = `<span class="dim">Search “${escape(query)}” cancelled.</span>`;
  } else if (status === 'failed') {
    resultsStatus.hidden = false;
    resultsStatus.innerHTML = `<span class="status-failed">Search failed${error ? ': ' + escape(error) : ''}</span>`;
  } else {
    resultsStatus.hidden = activeResults.length !== 0;
    if (!resultsStatus.hidden) {
      resultsStatus.textContent = `No epub results for “${query}”.`;
    }
  }
}

async function cancelActiveSearch(id) {
  try {
    await pb.send('/api/ebooks/search/cancel', { method: 'POST', body: { id } });
  } catch (err) {
    alert(err?.message || 'cancel failed');
    return;
  }
  // Optimistic — the realtime "searches" update will confirm this shortly.
  if (activeSearchId === id && activeSearchMeta) {
    activeSearchMeta = { ...activeSearchMeta, status: 'cancelled' };
    renderActiveStatus();
  }
}

async function cancelDownload(id) {
  try {
    await pb.send('/api/ebooks/download/cancel', { method: 'POST', body: { id } });
    // The realtime "downloads" update will repaint the row as cancelled.
  } catch (err) {
    alert(err?.message || 'cancel failed');
  }
}

function buildResultRow(rec) {
  const tr = document.createElement('tr');
  tr.classList.toggle('result-duplicate', !!rec.is_duplicate);
  tr.innerHTML = `
    <td>${escape(rec.server)}</td>
    <td>${escape(rec.author)}</td>
    <td>${escape(rec.title)}</td>
    <td>${escape(rec.size)}</td>
    <td><button type="button">Download</button></td>
  `;
  const btn = tr.querySelector('button');
  // While a download is in flight the button itself doubles as the
  // cancel action (via btn.dataset.downloadId) — this is where the user
  // is actually looking mid-download, not the collapsed history panel.
  btn.addEventListener('click', async () => {
    if (btn.dataset.downloadId) {
      btn.disabled = true;
      cancelDownload(btn.dataset.downloadId);
      return;
    }
    btn.disabled = true;
    btn.innerHTML = '<span class="spinner" aria-hidden="true"></span>Queued…';
    try {
      const res = await pb.send('/api/ebooks/download', {
        method: 'POST',
        body: { command: rec.full, result_id: rec.id },
      });
      // Track this button against the download so it can follow its
      // status (downloading/complete/failed) instead of being stuck
      // reading "Queued…" forever. No need to jump to the downloads
      // section either — a toast will confirm completion.
      resultDownloadButtons.set(res.id, btn);
      setResultButtonActive(btn, res.id, 'Queued…');
    } catch (err) {
      alert(err?.message || 'download failed');
      btn.disabled = false;
      btn.textContent = 'Download';
    }
  });
  return tr;
}

function addOrUpdateDownloadRow(rec) {
  let tr = downloadsBody.querySelector(`tr[data-id="${rec.id}"]`);
  const isNew = !tr;
  if (isNew) {
    tr = document.createElement('tr');
    tr.dataset.id = rec.id;
    tr.innerHTML = '<td></td><td></td><td></td><td></td>';
    // Tap anywhere on the row toggles full filename, except on the
    // save-link or cancel button (we don't want to swallow those clicks).
    tr.addEventListener('click', (e) => {
      if (e.target.closest('a, button')) return;
      tr.classList.toggle('expanded');
    });
    downloadsBody.prepend(tr);
  }
  tr.dataset.status = rec.status;
  const cells = tr.children;
  cells[0].textContent = rec.filename || filenameHint(rec.command);
  cells[1].innerHTML = pendingIndicator(rec.status) +
    `<span class="status-${rec.status}">${rec.status}</span>` +
    (rec.error ? ` <span class="dim">— ${escape(rec.error)}</span>` : '');
  cells[2].textContent = rec.size_bytes ? humanBytes(rec.size_bytes) : '';
  if (rec.status === 'complete' && rec.file) {
    const url = pb.files.getURL(rec, rec.file, fileToken ? { token: fileToken } : {});
    cells[3].innerHTML = `<a href="${escape(url)}" download>save</a>`;
  } else if (rec.status === 'queued' || rec.status === 'downloading') {
    cells[3].innerHTML = `<button type="button" class="cancel-download-btn">Cancel</button>`;
    cells[3].querySelector('button').addEventListener('click', (e) => {
      e.target.disabled = true;
      e.target.textContent = '…';
      cancelDownload(rec.id);
    });
  } else {
    cells[3].innerHTML = '';
  }
  if (isNew) {
    tr.classList.add('flash');
    setTimeout(() => tr.classList.remove('flash'), 1400);
  }
}

function pendingIndicator(status) {
  if (status === 'queued' || status === 'downloading') {
    return '<span class="spinner" aria-hidden="true"></span>';
  }
  return '';
}

function filenameHint(command) {
  if (!command) return '';
  // Pick the last whitespace-separated token containing a "." — usually the
  // book filename in "!Bot Author - Title.epub".
  const parts = command.split(' ');
  for (let i = parts.length - 1; i >= 0; i--) {
    if (parts[i].includes('.')) return parts[i];
  }
  return command;
}

// ---- helpers ----

function escape(s) {
  if (s == null) return '';
  return String(s).replace(/[<>&"]/g, (c) => ({
    '<': '&lt;', '>': '&gt;', '&': '&amp;', '"': '&quot;',
  })[c]);
}

function formatTime(s) {
  if (!s) return '';
  const d = new Date(s);
  return d.toLocaleString();
}

function humanBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

refreshAuthUI();
