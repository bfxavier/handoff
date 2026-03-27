// Agent Relay Dashboard
// Vanilla JS — no framework dependencies.

'use strict';

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

/** @type {string} */
let apiKey = '';

/** @type {string} */
let baseUrl = window.location.origin;

/** @type {string | null} */
let currentChannel = null;

/** @type {string | null} */
let activeTab = 'messages';

/** @type {string} */
let messagesCursor = '';

/** @type {ReturnType<typeof setInterval> | null} */
let messagesPoller = null;

/** @type {ReturnType<typeof setInterval> | null} */
let channelsPoller = null;

/** @type {boolean} */
let isConnected = false;

/** @type {{ name: string; sender: string } | null} */
let currentTeamInfo = null;

// Per-sender color pool. Deterministic based on sender string.
const SENDER_COLORS = [
  'bg-blue-500/10 border-blue-500/20',
  'bg-purple-500/10 border-purple-500/20',
  'bg-emerald-500/10 border-emerald-500/20',
  'bg-amber-500/10 border-amber-500/20',
  'bg-rose-500/10 border-rose-500/20',
  'bg-cyan-500/10 border-cyan-500/20',
  'bg-indigo-500/10 border-indigo-500/20',
  'bg-fuchsia-500/10 border-fuchsia-500/20',
];

const SENDER_LABEL_COLORS = [
  'text-blue-400',
  'text-purple-400',
  'text-emerald-400',
  'text-amber-400',
  'text-rose-400',
  'text-cyan-400',
  'text-indigo-400',
  'text-fuchsia-400',
];

/** @param {string} sender @returns {number} */
function senderIndex(sender) {
  let h = 0;
  for (let i = 0; i < sender.length; i++) {
    h = (h * 31 + sender.charCodeAt(i)) >>> 0;
  }
  return h % SENDER_COLORS.length;
}

// ---------------------------------------------------------------------------
// API helper
// ---------------------------------------------------------------------------

/**
 * @param {string} method
 * @param {string} path
 * @param {unknown} [body]
 * @returns {Promise<unknown>}
 */
async function api(method, path, body) {
  const url = `${baseUrl}${path}`;
  /** @type {HeadersInit} */
  const headers = {
    'Authorization': `Bearer ${apiKey}`,
    'Content-Type': 'application/json',
  };
  const res = await fetch(url, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`;
    try {
      const errBody = await res.json();
      if (errBody && typeof errBody.error === 'string') {
        msg = errBody.error;
      }
    } catch { /* ignore parse error */ }
    throw new Error(msg);
  }
  return res.json();
}

/**
 * Unauthenticated POST — used for signup.
 * @param {string} path
 * @param {unknown} body
 * @returns {Promise<unknown>}
 */
async function apiPublic(path, body) {
  const url = `${baseUrl}${path}`;
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`;
    try {
      const errBody = await res.json();
      if (errBody && typeof errBody.error === 'string') {
        msg = errBody.error;
      }
    } catch { /* ignore */ }
    throw new Error(msg);
  }
  return res.json();
}

// ---------------------------------------------------------------------------
// Toast notifications
// ---------------------------------------------------------------------------

/**
 * @param {string} message
 * @param {'error' | 'success' | 'info'} [type]
 */
function showToast(message, type = 'info') {
  const container = document.getElementById('toast-container');
  if (!container) return;

  const colorMap = {
    error: 'bg-red-900/80 border-red-700/50 text-red-200',
    success: 'bg-green-900/80 border-green-700/50 text-green-200',
    info: 'bg-[#1c1c2e] border-[#2a2a3d] text-gray-300',
  };

  const el = document.createElement('div');
  el.className = `toast border rounded px-3 py-2 text-xs shadow-lg ${colorMap[type]}`;
  el.textContent = message;

  container.appendChild(el);

  setTimeout(() => {
    el.style.opacity = '0';
    el.style.transition = 'opacity 0.3s';
    setTimeout(() => el.remove(), 300);
  }, 4000);
}

// ---------------------------------------------------------------------------
// Connection status
// ---------------------------------------------------------------------------

/** @param {boolean} connected */
function setConnectionStatus(connected) {
  isConnected = connected;
  const dot = document.getElementById('connection-dot');
  const label = document.getElementById('connection-label');
  if (!dot || !label) return;

  if (connected) {
    dot.className = 'w-2 h-2 rounded-full bg-green-500 flex-shrink-0';
    label.textContent = 'connected';
    label.className = 'text-green-400 text-xs';
  } else {
    dot.className = 'w-2 h-2 rounded-full bg-red-500 animate-pulse-dot flex-shrink-0';
    label.textContent = 'unreachable';
    label.className = 'text-red-400 text-xs';
  }
}

// ---------------------------------------------------------------------------
// Channels
// ---------------------------------------------------------------------------

async function loadChannels() {
  try {
    const channels = /** @type {Array<{name: string; description: string | null; created_at: string}>} */ (await api('GET', '/api/channels'));
    setConnectionStatus(true);
    renderChannelList(channels);
  } catch (err) {
    setConnectionStatus(false);
  }
}

/**
 * @param {Array<{name: string; description: string | null; created_at: string}>} channels
 */
function renderChannelList(channels) {
  const list = document.getElementById('channel-list');
  if (!list) return;

  if (!channels || channels.length === 0) {
    list.innerHTML = '<p class="text-gray-700 text-xs px-3 py-2">No channels yet.</p>';
    return;
  }

  list.innerHTML = channels.map(ch => `
    <button
      class="channel-item w-full text-left px-3 py-2 text-xs transition-colors hover:bg-[#1c1c2e] ${currentChannel === ch.name ? 'active' : ''}"
      data-channel="${escHtml(ch.name)}"
    >
      <span class="text-gray-400">#</span>
      <span class="text-gray-300 ml-1">${escHtml(ch.name)}</span>
      ${ch.description ? `<span class="text-gray-600 block text-xs truncate pl-3 mt-0.5">${escHtml(ch.description)}</span>` : ''}
    </button>
  `).join('');

  list.querySelectorAll('.channel-item').forEach(btn => {
    btn.addEventListener('click', () => {
      const name = btn.getAttribute('data-channel');
      if (name) selectChannel(name);
    });
  });
}

// ---------------------------------------------------------------------------
// Select channel
// ---------------------------------------------------------------------------

/** @param {string} name */
function selectChannel(name) {
  if (currentChannel === name) return;

  currentChannel = name;
  messagesCursor = '';

  // Update sidebar active state
  document.querySelectorAll('.channel-item').forEach(btn => {
    if (btn.getAttribute('data-channel') === name) {
      btn.classList.add('active');
    } else {
      btn.classList.remove('active');
    }
  });

  // Show channel view
  const noState = document.getElementById('no-channel-state');
  const channelView = document.getElementById('channel-view');
  if (noState) noState.classList.add('hidden');
  if (channelView) channelView.classList.remove('hidden');

  const titleEl = document.getElementById('channel-title');
  if (titleEl) titleEl.textContent = name;

  // Reset to messages tab
  switchTab('messages');

  // Clear messages feed
  const feed = document.getElementById('messages-feed');
  if (feed) feed.innerHTML = '';

  // Initial load
  loadMessages(name, true);
  if (activeTab === 'status') loadStatus(name);
  if (activeTab === 'timeline') loadStatusChanges(name);
  if (activeTab === 'acks') loadAcks(name);

  // Restart poller
  if (messagesPoller) clearInterval(messagesPoller);
  messagesPoller = setInterval(() => {
    if (currentChannel && activeTab === 'messages') {
      loadMessages(currentChannel, false);
    }
  }, 3000);
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

/**
 * @param {string} channel
 * @param {boolean} initial — if true, load full history; if false, use cursor
 */
async function loadMessages(channel, initial) {
  const indicator = document.getElementById('refresh-indicator');

  try {
    if (!initial && indicator) {
      indicator.classList.remove('hidden');
    }

    const params = new URLSearchParams({ limit: '100' });
    if (!initial && messagesCursor) {
      params.set('after_id', messagesCursor);
    }

    const result = /** @type {{ messages: Array<any>; next_after_id: string; channel: string }} */ (
      await api('GET', `/api/channels/${encodeURIComponent(channel)}/messages?${params}`)
    );

    setConnectionStatus(true);

    if (result.next_after_id) {
      messagesCursor = result.next_after_id;
    }

    if (result.messages && result.messages.length > 0) {
      appendMessages(result.messages, initial);

      // Ack the last message
      const lastId = result.messages[result.messages.length - 1].id;
      api('POST', `/api/channels/${encodeURIComponent(channel)}/ack`, { last_read_id: lastId }).catch(() => {});
    }
  } catch (err) {
    setConnectionStatus(false);
    if (initial) {
      showToast(`Failed to load messages: ${String(err)}`, 'error');
    }
  } finally {
    if (indicator) indicator.classList.add('hidden');
  }
}

/**
 * @param {Array<{id: string; sender: string; content: string; mention: string | null; created_at: string}>} messages
 * @param {boolean} initial
 */
function appendMessages(messages, initial) {
  const feed = document.getElementById('messages-feed');
  if (!feed) return;

  if (initial) {
    feed.innerHTML = '';
  }

  const wasAtBottom = feed.scrollHeight - feed.scrollTop - feed.clientHeight < 60;

  const fragment = document.createDocumentFragment();
  for (const msg of messages) {
    // Skip if already rendered (by id)
    if (document.getElementById(`msg-${msg.id}`)) continue;
    fragment.appendChild(buildMessageEl(msg));
  }
  feed.appendChild(fragment);

  if (initial || wasAtBottom) {
    feed.scrollTop = feed.scrollHeight;
  }
}

/**
 * @param {{id: string; sender: string; content: string; mention: string | null; created_at: string}} msg
 * @returns {HTMLElement}
 */
function buildMessageEl(msg) {
  const idx = senderIndex(msg.sender);
  const bgClass = SENDER_COLORS[idx];
  const labelClass = SENDER_LABEL_COLORS[idx];

  const ts = formatTimestamp(msg.created_at);
  const isMine = currentTeamInfo && msg.sender === currentTeamInfo.sender;

  const div = document.createElement('div');
  div.id = `msg-${msg.id}`;
  div.className = `msg-bubble border rounded px-3 py-2 text-xs ${bgClass} ${isMine ? 'ml-8' : 'mr-8'}`;

  const mention = msg.mention
    ? `<span class="bg-blue-600/30 text-blue-300 rounded px-1 py-0.5 text-xs ml-1">@${escHtml(msg.mention)}</span>`
    : '';

  div.innerHTML = `
    <div class="flex items-center gap-2 mb-1">
      <span class="${labelClass} font-semibold">${escHtml(msg.sender)}</span>
      ${mention}
      <span class="text-gray-600 text-xs ml-auto flex-shrink-0">${ts}</span>
    </div>
    <div class="text-gray-200 whitespace-pre-wrap break-words leading-relaxed">${escHtml(msg.content)}</div>
  `;

  return div;
}

// ---------------------------------------------------------------------------
// Status board
// ---------------------------------------------------------------------------

/** @param {string} channel */
async function loadStatus(channel) {
  try {
    const statuses = /** @type {Array<{channel: string; key: string; value: string; updated_by: string | null; updated_at: string}>} */ (
      await api('GET', `/api/channels/${encodeURIComponent(channel)}/status`)
    );
    renderStatusGrid(statuses);
  } catch (err) {
    showToast(`Failed to load status: ${String(err)}`, 'error');
  }
}

/**
 * @param {Array<{key: string; value: string; updated_by: string | null; updated_at: string}>} statuses
 */
function renderStatusGrid(statuses) {
  const grid = document.getElementById('status-grid');
  if (!grid) return;

  if (!statuses || statuses.length === 0) {
    grid.innerHTML = '<p class="text-gray-600 text-xs">No status entries.</p>';
    return;
  }

  grid.innerHTML = `
    <div class="w-full overflow-x-auto">
      <table class="w-full text-xs border-collapse">
        <thead>
          <tr class="border-b border-[#2a2a3d]">
            <th class="text-left text-gray-500 uppercase tracking-wider py-2 pr-4 font-normal">Key</th>
            <th class="text-left text-gray-500 uppercase tracking-wider py-2 pr-4 font-normal">Value</th>
            <th class="text-left text-gray-500 uppercase tracking-wider py-2 pr-4 font-normal">Updated by</th>
            <th class="text-left text-gray-500 uppercase tracking-wider py-2 font-normal">Updated at</th>
          </tr>
        </thead>
        <tbody>
          ${statuses.map(s => `
            <tr class="border-b border-[#1c1c2e] hover:bg-[#1c1c2e]/50 transition-colors">
              <td class="py-2 pr-4 text-blue-300 font-semibold">${escHtml(s.key)}</td>
              <td class="py-2 pr-4 text-gray-200">${escHtml(s.value)}</td>
              <td class="py-2 pr-4 text-gray-500">${s.updated_by ? escHtml(s.updated_by) : '—'}</td>
              <td class="py-2 text-gray-600">${formatTimestamp(s.updated_at)}</td>
            </tr>
          `).join('')}
        </tbody>
      </table>
    </div>
  `;
}

// ---------------------------------------------------------------------------
// Status timeline
// ---------------------------------------------------------------------------

/** @param {string} channel */
async function loadStatusChanges(channel) {
  try {
    const result = /** @type {{ changes: Array<any>; next_after_id: string }} */ (
      await api('GET', `/api/channels/${encodeURIComponent(channel)}/status/changes?limit=50`)
    );
    renderTimeline(result.changes || []);
  } catch (err) {
    showToast(`Failed to load timeline: ${String(err)}`, 'error');
  }
}

/**
 * @param {Array<{id: string; key: string; value: string; changed_by: string | null; changed_at: string}>} changes
 */
function renderTimeline(changes) {
  const list = document.getElementById('timeline-list');
  if (!list) return;

  if (!changes || changes.length === 0) {
    list.innerHTML = '<p class="text-gray-600 text-xs">No changes recorded.</p>';
    return;
  }

  // Newest first
  const sorted = [...changes].sort((a, b) => b.changed_at.localeCompare(a.changed_at));

  list.innerHTML = sorted.map((c, i) => `
    <div class="relative flex gap-3 pb-4 timeline-line">
      <div class="w-3.5 h-3.5 rounded-full bg-[#2a2a3d] border border-blue-500/40 flex-shrink-0 mt-0.5 z-10"></div>
      <div class="flex-1 bg-[#14141f] border border-[#2a2a3d] rounded px-3 py-2 text-xs">
        <div class="flex items-center gap-2 mb-1 flex-wrap">
          <span class="text-blue-300 font-semibold">${escHtml(c.key)}</span>
          <span class="text-gray-600">&rarr;</span>
          <span class="text-gray-200">${escHtml(c.value)}</span>
          ${c.changed_by ? `<span class="text-gray-500 text-xs ml-auto">by ${escHtml(c.changed_by)}</span>` : ''}
        </div>
        <div class="text-gray-600 text-xs">${formatTimestamp(c.changed_at)}</div>
      </div>
    </div>
  `).join('');
}

// ---------------------------------------------------------------------------
// Read receipts
// ---------------------------------------------------------------------------

/** @param {string} channel */
async function loadAcks(channel) {
  try {
    const acks = /** @type {Array<{sender: string; last_read_id: string; acked_at: string}>} */ (
      await api('GET', `/api/channels/${encodeURIComponent(channel)}/acks`)
    );
    renderAcks(acks);
  } catch (err) {
    showToast(`Failed to load acks: ${String(err)}`, 'error');
  }
}

/**
 * @param {Array<{sender: string; last_read_id: string; acked_at: string}>} acks
 */
function renderAcks(acks) {
  const list = document.getElementById('acks-list');
  if (!list) return;

  if (!acks || acks.length === 0) {
    list.innerHTML = '<p class="text-gray-600 text-xs">No read receipts.</p>';
    return;
  }

  const sorted = [...acks].sort((a, b) => b.acked_at.localeCompare(a.acked_at));

  list.innerHTML = sorted.map(ack => {
    const idx = senderIndex(ack.sender);
    const labelClass = SENDER_LABEL_COLORS[idx];
    const bgClass = SENDER_COLORS[idx];
    return `
      <div class="flex items-center gap-3 border ${bgClass.split(' ')[1]} bg-transparent rounded px-3 py-2 text-xs ${bgClass.split(' ')[0]}">
        <span class="${labelClass} font-semibold flex-shrink-0">${escHtml(ack.sender)}</span>
        <span class="text-gray-500">last read:</span>
        <code class="text-gray-400 text-xs font-mono truncate flex-1">${escHtml(ack.last_read_id)}</code>
        <span class="text-gray-600 flex-shrink-0">${formatTimestamp(ack.acked_at)}</span>
      </div>
    `;
  }).join('');
}

// ---------------------------------------------------------------------------
// Tab switching
// ---------------------------------------------------------------------------

/** @param {string} tab */
function switchTab(tab) {
  activeTab = tab;

  document.querySelectorAll('.tab-btn').forEach(btn => {
    if (btn.getAttribute('data-tab') === tab) {
      btn.classList.add('active', 'text-gray-200');
      btn.classList.remove('text-gray-500');
    } else {
      btn.classList.remove('active', 'text-gray-200');
      btn.classList.add('text-gray-500');
    }
  });

  const panels = ['messages', 'status', 'timeline', 'acks'];
  panels.forEach(p => {
    const el = document.getElementById(`tab-${p}`);
    if (!el) return;
    if (p === tab) {
      el.classList.remove('hidden');
    } else {
      el.classList.add('hidden');
    }
  });

  // Lazy-load tab data
  if (currentChannel) {
    if (tab === 'status') loadStatus(currentChannel);
    else if (tab === 'timeline') loadStatusChanges(currentChannel);
    else if (tab === 'acks') loadAcks(currentChannel);
    else if (tab === 'messages') {
      // Scroll to bottom when switching back
      setTimeout(() => {
        const feed = document.getElementById('messages-feed');
        if (feed) feed.scrollTop = feed.scrollHeight;
      }, 50);
    }
  }
}

// ---------------------------------------------------------------------------
// Send message
// ---------------------------------------------------------------------------

async function sendMessage() {
  if (!currentChannel) return;
  const input = /** @type {HTMLTextAreaElement | null} */ (document.getElementById('msg-input'));
  const mentionInput = /** @type {HTMLInputElement | null} */ (document.getElementById('msg-mention'));
  const btn = document.getElementById('msg-send-btn');
  if (!input || !btn) return;

  const content = input.value.trim();
  if (!content) return;

  const mention = mentionInput?.value.trim().replace(/^@/, '') || undefined;

  btn.setAttribute('disabled', '');
  const prevContent = input.value;
  input.value = '';
  if (mentionInput) mentionInput.value = '';

  try {
    const msg = await api('POST', `/api/channels/${encodeURIComponent(currentChannel)}/messages`, {
      content,
      ...(mention ? { mention } : {}),
    });

    // Optimistic: message came back from server, append directly
    appendMessages([/** @type {any} */ (msg)], false);
    messagesCursor = /** @type {any} */ (msg).id;
  } catch (err) {
    showToast(`Failed to send message: ${String(err)}`, 'error');
    // Restore input
    input.value = prevContent;
    if (mentionInput && mention) mentionInput.value = mention;
  } finally {
    btn.removeAttribute('disabled');
    input.focus();
  }
}

// ---------------------------------------------------------------------------
// Set status
// ---------------------------------------------------------------------------

async function setStatus() {
  if (!currentChannel) return;
  const keyInput = /** @type {HTMLInputElement | null} */ (document.getElementById('status-key-input'));
  const valInput = /** @type {HTMLInputElement | null} */ (document.getElementById('status-val-input'));
  const btn = document.getElementById('status-submit-btn');
  if (!keyInput || !valInput || !btn) return;

  const key = keyInput.value.trim();
  const value = valInput.value.trim();
  if (!key || !value) {
    showToast('Key and value are required', 'error');
    return;
  }

  btn.setAttribute('disabled', '');
  try {
    await api('POST', `/api/channels/${encodeURIComponent(currentChannel)}/status`, { key, value });
    keyInput.value = '';
    valInput.value = '';
    showToast(`Status "${key}" updated`, 'success');
    await loadStatus(currentChannel);
  } catch (err) {
    showToast(`Failed to set status: ${String(err)}`, 'error');
  } finally {
    btn.removeAttribute('disabled');
  }
}

// ---------------------------------------------------------------------------
// Create channel
// ---------------------------------------------------------------------------

async function createChannel() {
  const nameInput = /** @type {HTMLInputElement | null} */ (document.getElementById('new-channel-name'));
  const descInput = /** @type {HTMLInputElement | null} */ (document.getElementById('new-channel-desc'));
  const btn = document.getElementById('new-channel-submit');
  if (!nameInput || !btn) return;

  const name = nameInput.value.trim();
  if (!name) {
    showToast('Channel name is required', 'error');
    return;
  }

  btn.setAttribute('disabled', '');
  try {
    await api('POST', '/api/channels', {
      name,
      description: descInput?.value.trim() || undefined,
    });
    closeNewChannelModal();
    await loadChannels();
    selectChannel(name);
    showToast(`Channel #${name} created`, 'success');
  } catch (err) {
    showToast(`Failed to create channel: ${String(err)}`, 'error');
  } finally {
    btn.removeAttribute('disabled');
  }
}

function closeNewChannelModal() {
  const modal = document.getElementById('new-channel-modal');
  if (modal) modal.classList.add('hidden');
  const nameInput = /** @type {HTMLInputElement | null} */ (document.getElementById('new-channel-name'));
  const descInput = /** @type {HTMLInputElement | null} */ (document.getElementById('new-channel-desc'));
  if (nameInput) nameInput.value = '';
  if (descInput) descInput.value = '';
}

// ---------------------------------------------------------------------------
// Connection setup
// ---------------------------------------------------------------------------

/** @param {string} key @param {string} url */
async function connect(key, url) {
  apiKey = key;
  baseUrl = url.replace(/\/$/, '');

  const btn = document.getElementById('setup-connect-btn');
  const errEl = document.getElementById('setup-error');
  if (btn) btn.setAttribute('disabled', '');
  if (errEl) {
    errEl.classList.add('hidden');
    errEl.textContent = '';
  }

  try {
    // Validate by fetching channels — also tells us team info indirectly
    const channels = /** @type {any[]} */ (await api('GET', '/api/channels'));

    // Extract sender from key via a keys endpoint isn't available,
    // so we use a probe: POST /api/keys with an invalid body will still 401 or 400,
    // but GET /api/channels success is enough to confirm the key works.
    // The sender name comes from the key itself (stored server-side), so we
    // retrieve it by looking at a sent message. Instead, we store what we know.
    // The key format is opaque, so we just note we're connected.

    localStorage.setItem('ar_api_key', key);
    localStorage.setItem('ar_base_url', baseUrl);

    setConnectionStatus(true);
    showApp(channels);
  } catch (err) {
    setConnectionStatus(false);
    if (errEl) {
      errEl.textContent = `Connection failed: ${String(err)}`;
      errEl.classList.remove('hidden');
    }
    if (btn) btn.removeAttribute('disabled');
  }
}

/**
 * @param {any[]} initialChannels
 */
function showApp(initialChannels) {
  const overlay = document.getElementById('setup-overlay');
  const app = document.getElementById('app');
  if (overlay) overlay.classList.add('hidden');
  if (app) app.classList.remove('hidden');

  renderChannelList(initialChannels);

  // Start channels poller
  if (channelsPoller) clearInterval(channelsPoller);
  channelsPoller = setInterval(loadChannels, 10000);

  // Display team/sender info — probe with a known key field
  // We'll discover the sender when a message is sent or by labeling it from stored key attempt
  const senderEl = document.getElementById('sender-name-display');
  const teamEl = document.getElementById('team-name-display');
  // We can infer sender from acks or messages later, but for now show placeholder
  if (senderEl) senderEl.textContent = '(connected)';
  if (teamEl) teamEl.textContent = '—';

  // If we have stored team info from signup, show it
  const storedTeam = localStorage.getItem('ar_team_info');
  if (storedTeam) {
    try {
      const info = JSON.parse(storedTeam);
      currentTeamInfo = info;
      if (info.name && teamEl) teamEl.textContent = info.name;
      if (info.sender && senderEl) senderEl.textContent = info.sender;
    } catch { /* ignore */ }
  }
}

function disconnect() {
  if (messagesPoller) { clearInterval(messagesPoller); messagesPoller = null; }
  if (channelsPoller) { clearInterval(channelsPoller); channelsPoller = null; }

  apiKey = '';
  currentChannel = null;
  messagesCursor = '';
  currentTeamInfo = null;

  localStorage.removeItem('ar_api_key');
  localStorage.removeItem('ar_base_url');
  localStorage.removeItem('ar_team_info');

  const overlay = document.getElementById('setup-overlay');
  const app = document.getElementById('app');
  const keyInput = /** @type {HTMLInputElement | null} */ (document.getElementById('setup-api-key'));
  const errEl = document.getElementById('setup-error');
  const connectBtn = document.getElementById('setup-connect-btn');

  if (app) app.classList.add('hidden');
  if (overlay) overlay.classList.remove('hidden');
  if (keyInput) keyInput.value = '';
  if (errEl) { errEl.classList.add('hidden'); errEl.textContent = ''; }
  if (connectBtn) connectBtn.removeAttribute('disabled');

  setConnectionStatus(false);
}

// ---------------------------------------------------------------------------
// Signup flow
// ---------------------------------------------------------------------------

async function signup() {
  const teamInput = /** @type {HTMLInputElement | null} */ (document.getElementById('signup-team'));
  const senderInput = /** @type {HTMLInputElement | null} */ (document.getElementById('signup-sender'));
  const urlInput = /** @type {HTMLInputElement | null} */ (document.getElementById('signup-base-url'));
  const btn = document.getElementById('signup-btn');
  const errEl = document.getElementById('signup-error');
  const resultEl = document.getElementById('signup-result');
  const keyDisplay = document.getElementById('signup-key-display');

  if (!teamInput || !senderInput || !btn) return;

  const teamName = teamInput.value.trim();
  const senderName = senderInput.value.trim();
  const signupUrl = urlInput?.value.trim().replace(/\/$/, '') || window.location.origin;

  if (!teamName || !senderName) {
    if (errEl) { errEl.textContent = 'Team name and agent name are required'; errEl.classList.remove('hidden'); }
    return;
  }

  btn.setAttribute('disabled', '');
  if (errEl) { errEl.classList.add('hidden'); errEl.textContent = ''; }
  if (resultEl) resultEl.classList.add('hidden');

  try {
    const url = `${signupUrl}/api/signup`;
    const res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ team_name: teamName, sender_name: senderName }),
    });

    if (!res.ok) {
      let msg = `${res.status} ${res.statusText}`;
      try { const b = await res.json(); if (b.error) msg = b.error; } catch { /* ignore */ }
      throw new Error(msg);
    }

    const data = /** @type {{ team: { id: string; name: string }; api_key: string }} */ (await res.json());

    if (keyDisplay) keyDisplay.textContent = data.api_key;
    if (resultEl) resultEl.classList.remove('hidden');

    // Store for later use
    localStorage.setItem('ar_pending_key', data.api_key);
    localStorage.setItem('ar_pending_url', signupUrl);
    localStorage.setItem('ar_team_info', JSON.stringify({ name: data.team.name, sender: senderName }));

    const useBtn = document.getElementById('signup-use-key-btn');
    if (useBtn) {
      useBtn.onclick = () => {
        const setupKey = /** @type {HTMLInputElement | null} */ (document.getElementById('setup-api-key'));
        const setupUrl = /** @type {HTMLInputElement | null} */ (document.getElementById('setup-base-url'));
        if (setupKey) setupKey.value = data.api_key;
        if (setupUrl) setupUrl.value = signupUrl;

        const signupPanel = document.getElementById('signup-panel');
        if (signupPanel) signupPanel.classList.add('hidden');
      };
    }
  } catch (err) {
    if (errEl) {
      errEl.textContent = `Signup failed: ${String(err)}`;
      errEl.classList.remove('hidden');
    }
  } finally {
    btn.removeAttribute('disabled');
  }
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

/** @param {string} str @returns {string} */
function escHtml(str) {
  return str
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#039;');
}

/** @param {string} isoString @returns {string} */
function formatTimestamp(isoString) {
  try {
    const d = new Date(isoString);
    const now = new Date();
    const diffMs = now.getTime() - d.getTime();
    const diffMins = Math.floor(diffMs / 60000);
    const diffHours = Math.floor(diffMs / 3600000);
    const diffDays = Math.floor(diffMs / 86400000);

    if (diffMins < 1) return 'just now';
    if (diffMins < 60) return `${diffMins}m ago`;
    if (diffHours < 24) return `${diffHours}h ago`;
    if (diffDays < 7) return `${diffDays}d ago`;

    return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  } catch {
    return isoString;
  }
}

// ---------------------------------------------------------------------------
// Bootstrap
// ---------------------------------------------------------------------------

function init() {
  // Pre-fill setup form with stored values
  const storedKey = localStorage.getItem('ar_api_key');
  const storedUrl = localStorage.getItem('ar_base_url');

  const setupKey = /** @type {HTMLInputElement | null} */ (document.getElementById('setup-api-key'));
  const setupUrl = /** @type {HTMLInputElement | null} */ (document.getElementById('setup-base-url'));

  if (setupUrl) setupUrl.value = storedUrl || window.location.origin;

  if (storedKey) {
    // Auto-connect
    if (setupKey) setupKey.value = storedKey;
    connect(storedKey, storedUrl || window.location.origin);
  }

  // --- Setup overlay ---

  document.getElementById('setup-connect-btn')?.addEventListener('click', () => {
    const key = (/** @type {HTMLInputElement | null} */ (document.getElementById('setup-api-key')))?.value.trim() ?? '';
    const url = (/** @type {HTMLInputElement | null} */ (document.getElementById('setup-base-url')))?.value.trim() || window.location.origin;
    if (!key) {
      const errEl = document.getElementById('setup-error');
      if (errEl) { errEl.textContent = 'API key is required'; errEl.classList.remove('hidden'); }
      return;
    }
    connect(key, url);
  });

  document.getElementById('setup-api-key')?.addEventListener('keydown', (e) => {
    if (/** @type {KeyboardEvent} */ (e).key === 'Enter') {
      document.getElementById('setup-connect-btn')?.click();
    }
  });

  document.getElementById('show-signup-btn')?.addEventListener('click', () => {
    const panel = document.getElementById('signup-panel');
    const urlInput = /** @type {HTMLInputElement | null} */ (document.getElementById('signup-base-url'));
    const setupUrl = /** @type {HTMLInputElement | null} */ (document.getElementById('setup-base-url'));
    if (panel) panel.classList.remove('hidden');
    if (urlInput && setupUrl) urlInput.value = setupUrl.value;
  });

  document.getElementById('back-to-connect-btn')?.addEventListener('click', () => {
    document.getElementById('signup-panel')?.classList.add('hidden');
  });

  document.getElementById('signup-btn')?.addEventListener('click', signup);

  // --- App UI ---

  document.getElementById('disconnect-btn')?.addEventListener('click', disconnect);

  document.getElementById('new-channel-btn')?.addEventListener('click', () => {
    document.getElementById('new-channel-modal')?.classList.remove('hidden');
    document.getElementById('new-channel-name')?.focus();
  });

  document.getElementById('new-channel-cancel')?.addEventListener('click', closeNewChannelModal);

  document.getElementById('new-channel-modal')?.addEventListener('click', (e) => {
    if (e.target === document.getElementById('new-channel-modal')) closeNewChannelModal();
  });

  document.getElementById('new-channel-submit')?.addEventListener('click', createChannel);

  document.getElementById('new-channel-name')?.addEventListener('keydown', (e) => {
    if (/** @type {KeyboardEvent} */ (e).key === 'Enter') createChannel();
  });

  // Tab buttons
  document.querySelectorAll('.tab-btn').forEach(btn => {
    btn.addEventListener('click', () => {
      const tab = btn.getAttribute('data-tab');
      if (tab) switchTab(tab);
    });
  });

  // Message send
  document.getElementById('msg-send-btn')?.addEventListener('click', sendMessage);

  document.getElementById('msg-input')?.addEventListener('keydown', (e) => {
    const ke = /** @type {KeyboardEvent} */ (e);
    if (ke.key === 'Enter' && (ke.ctrlKey || ke.metaKey)) {
      e.preventDefault();
      sendMessage();
    }
  });

  // Status form toggle
  document.getElementById('set-status-toggle-btn')?.addEventListener('click', () => {
    const form = document.getElementById('set-status-form');
    if (!form) return;
    const isHidden = form.classList.contains('hidden');
    if (isHidden) {
      form.classList.remove('hidden');
      document.getElementById('status-key-input')?.focus();
    } else {
      form.classList.add('hidden');
    }
  });

  document.getElementById('status-submit-btn')?.addEventListener('click', setStatus);

  document.getElementById('status-val-input')?.addEventListener('keydown', (e) => {
    if (/** @type {KeyboardEvent} */ (e).key === 'Enter') setStatus();
  });
}

document.addEventListener('DOMContentLoaded', init);
