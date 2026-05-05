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

const RESULTS_PREVIEW = 10;

let activeSearchId = null;
let cooldownTimer = null;
let activeResults = [];   // all rows for the active search
let resultsExpanded = false;

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
  await loadSearches();
  await loadDownloads();
  subscribe();
  pollStatus();
  setInterval(pollStatus, 5000);
}

async function pollStatus() {
  try {
    const s = await pb.send('/api/ebooks/status', {});
    statusPill.textContent = s.connected ? `irc: ${s.nick}` : 'irc: offline';
    statusPill.className = 'pill ' + (s.connected ? 'pill-on' : 'pill-off');

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
    activeSearchId = res.id;
    showResultsFor(activeSearchId);
  } catch (err) {
    alert(err?.message || 'search failed');
  } finally {
    pollStatus();
  }
});

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
  });
  pb.collection('search_results').subscribe('*', (e) => {
    if (e.record.search !== activeSearchId) return;
    activeResults.push(e.record);
    renderResults();
  });
  pb.collection('downloads').subscribe('*', (e) => {
    addOrUpdateDownloadRow(e.record);
  });
}

function addOrUpdateSearchRow(rec) {
  let tr = searchesBody.querySelector(`tr[data-id="${rec.id}"]`);
  if (!tr) {
    tr = document.createElement('tr');
    tr.dataset.id = rec.id;
    tr.innerHTML = '<td></td><td></td><td></td><td></td>';
    tr.style.cursor = 'pointer';
    tr.addEventListener('click', () => {
      activeSearchId = rec.id;
      showResultsFor(rec.id);
    });
    searchesBody.prepend(tr);
  }
  const cells = tr.children;
  cells[0].textContent = rec.query;
  cells[1].innerHTML = `<span class="status-${rec.status}">${rec.status}</span>` +
    (rec.error ? ` <span class="dim">— ${escape(rec.error)}</span>` : '');
  cells[2].textContent = rec.result_count ?? '';
  cells[3].textContent = formatTime(rec.created);
}

async function showResultsFor(searchId) {
  resultsHeading.hidden = false;
  resultsTable.hidden = false;
  resultsBody.innerHTML = '';
  activeResults = [];
  resultsExpanded = false;
  const list = await pb.collection('search_results').getList(1, 200, {
    filter: `search="${searchId}"`,
    sort: 'server,author,title',
  });
  activeResults = list.items;
  renderResults();
}

function renderResults() {
  resultsBody.innerHTML = '';
  const total = activeResults.length;
  const limit = resultsExpanded ? total : Math.min(RESULTS_PREVIEW, total);
  for (let i = 0; i < limit; i++) {
    resultsBody.appendChild(buildResultRow(activeResults[i]));
  }
  if (!resultsExpanded && total > RESULTS_PREVIEW) {
    const tr = document.createElement('tr');
    tr.className = 'show-more';
    tr.innerHTML = `<td colspan="5">Show all ${total} results</td>`;
    tr.addEventListener('click', () => {
      resultsExpanded = true;
      renderResults();
    });
    resultsBody.appendChild(tr);
  }
}

function buildResultRow(rec) {
  const tr = document.createElement('tr');
  tr.innerHTML = `
    <td>${escape(rec.server)}</td>
    <td>${escape(rec.author)}</td>
    <td>${escape(rec.title)}</td>
    <td>${escape(rec.size)}</td>
    <td><button>Download</button></td>
  `;
  tr.querySelector('button').addEventListener('click', async (e) => {
    e.target.disabled = true;
    e.target.textContent = 'Queued…';
    try {
      await pb.send('/api/ebooks/download', {
        method: 'POST',
        body: { command: rec.full, result_id: rec.id },
      });
      // Bring the downloads table into view so the new row is obvious.
      document.querySelector('#downloads-table').scrollIntoView({
        behavior: 'smooth', block: 'center',
      });
    } catch (err) {
      alert(err?.message || 'download failed');
      e.target.disabled = false;
      e.target.textContent = 'Download';
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
    // save-link itself (we don't want to swallow the download click).
    tr.addEventListener('click', (e) => {
      if (e.target.closest('a')) return;
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
