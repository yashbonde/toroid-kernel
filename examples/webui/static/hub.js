import { $, element, request, preference, savePreference, menu, closeMenu } from './ui.js';
import { animateTorus } from './torus.js';

const board = element('div', 'pane-board');
document.querySelector('main').replaceWith(board);
let selected,
  workspace = { agents: [], chats: [] },
  version = 0;
const panes = [];
let restoring = false;
function saveLayout() {
  if (restoring || !selected) return;
  try {
    sessionStorage.setItem('toroid.panes', JSON.stringify({ids: panes.map((p) => p.id), active: panes.indexOf(selected)}));
  } catch {}
}
const marking = new Map();
function markSeen() {
  const p = selected;
  if (!p || !p.displayed || p.pending || document.hidden || !document.hasFocus()) return;
  const c = workspace.chats.find((chat) => chat.id === p.id);
  if (!c || c.ready <= (c.seen || 0) || p.displayed < c.ready || marking.get(c.id) === c.ready) return;
  const ready = c.ready;
  marking.set(c.id, ready);
  request('chats/' + encodeURIComponent(c.id) + '/seen', 'PUT', {ready}).then(() => {
    const chat = workspace.chats.find((item) => item.id === c.id);
    if (chat) chat.seen = Math.max(chat.seen || 0, ready);
    renderChats();
  }).catch(report).finally(() => {
    if (marking.get(c.id) === ready) marking.delete(c.id);
  });
}
window.addEventListener('focus', markSeen);
const post = (p, msg) => p.frame.contentWindow?.postMessage(msg, location.origin);
let chatIndex = history.state?.chatIndex ?? 0;
let nextNavigation = 0;
function writeChatURL(id, mode) {
  const url = new URL(location.href);
  chatIndex = history.state?.chatIndex ?? 0;
  if ((url.searchParams.get('chat') || '') === id) return;
  if (id) url.searchParams.set('chat', id);
  else url.searchParams.delete('chat');
  if (mode === 'push') chatIndex++;
  history[mode === 'push' ? 'pushState' : 'replaceState']({ ...history.state, chatIndex }, '', url);
}
function navigate(p, action, id, mode) {
  p.pending = { type: 'navigate', action, id, mode, ticket: ++nextNavigation };
  if (p.ready) post(p, p.pending);
}
function select(p, mode = 'push') {
  selected = p;
  writeChatURL(p.id, mode);
  for (const pane of panes) pane.frame.classList.toggle('active-pane', pane === p);
  saveLayout();
  markSeen();
  renderChats();
}
function layout() {
  board.style.setProperty('--panes', panes.length);
  $('add-pane').disabled = panes.length >= 3;
  board.classList.toggle('multiple', panes.length > 1);
  for (const p of panes) if (p.ready) post(p, { type: 'layout', multiple: panes.length > 1 });
}
function addPane(chatID = '', mode = 'push') {
  if (panes.length >= 3) return;
  const frame = element('iframe', 'chat-pane');
  frame.title = 'New chat';
  frame.src = '/?pane=1';
  const p = { frame, id: chatID, ready: false, displayed: 0 };
  if (chatID) navigate(p, 'chat', chatID, 'replace');
  panes.push(p);
  board.append(frame);
  select(p, mode);
  layout();
  closeNav();
  frame.scrollIntoView({ block: 'nearest', inline: 'nearest' });
}
function command(action, id = '', mode = 'push') {
  closeNav();
  closeMenu();
  if (!selected) return;
  const existing = (id || mode === 'restore') && panes.find((p) => p.id === id && !p.pending);
  if (existing) {
    select(existing, mode);
    existing.frame.scrollIntoView({ block: 'nearest', inline: 'nearest' });
    return;
  }
  navigate(selected, action, id, mode);
}
window.addEventListener('popstate', () => {
  const id = new URL(location.href).searchParams.get('chat') || '';
  command(id ? 'chat' : 'new', id, 'restore');
});
function closePane(p) {
  if (panes.length === 1) return;
  panes.splice(panes.indexOf(p), 1);
  p.frame.remove();
  if (selected === p) select(panes[0]);
  saveLayout();
  layout();
}
async function refresh() {
  const generation = ++version;
  const data = await (await request('workspace')).json();
  if (generation !== version) return;
  const changed = JSON.stringify(workspace) !== JSON.stringify(data);
  workspace = data;
  markSeen();
  if (!changed) return;
  renderChats();
  for (const p of panes) if (p.ready) post(p, { type: 'workspace', data });
}
window.addEventListener('message', (e) => {
  if (e.origin !== location.origin) return;
  const p = panes.find((p) => p.frame.contentWindow === e.source);
  if (!p) return;
  switch (e.data.type) {
    case 'pair-browser':
      pairBrowser();
      break;
    case 'ready':
      p.ready = true;
      post(p, { type: 'workspace', data: workspace });
      layout();
      if (p.pending) post(p, p.pending);
      break;
    case 'active':
      settingsMenu.hidePopover();
      if (p !== selected) select(p);
      break;
    case 'state': {
      if (!p.ready) break;
      const oldID = p.id;
      p.displayed = e.data.ready;
      p.id = e.data.id;
      p.frame.title = e.data.title;
      // The first accepted message gives the draft its permanent URL.
      // Navigation acknowledgments handle sidebar and browser-history moves.
      if (p === selected && !p.pending && oldID !== p.id)
        writeChatURL(p.id, oldID ? 'push' : 'replace');
      if (!p.pending) saveLayout();
      markSeen();
      renderChats();
      break;
    }
    case 'navigated': {
      if (!p.pending || e.data.ticket !== p.pending.ticket) break;
      const { mode } = p.pending;
      p.pending = null;
      if (p !== selected) break;
      writeChatURL(p.id, mode);
      saveLayout();
      markSeen();
      break;
    }
    case 'changed':
      refresh()
        .then(() => post(p, { type: 'refreshed', id: e.data.id }))
        .catch((error) => {
          report(error);
          post(p, { type: 'refreshed', id: e.data.id, error: error.message });
        });
      break;
    case 'choose-chat':
      if (selected !== p) select(p);
      command(e.data.id ? 'chat' : 'new', e.data.id);
      break;
    case 'close':
      closePane(p);
      break;
    case 'sidebar':
      toggleNav();
      break;
  }
});
function report(e) {
  $('hub-status').textContent = e.message;
}
const status = element('p', 'side-empty');
status.id = 'hub-status';
status.setAttribute('role', 'status');
$('chats').after(status);
const recentDate = new Intl.DateTimeFormat(undefined, {
  weekday: 'long',
  month: 'short',
  day: 'numeric',
});
const fullDate = new Intl.DateTimeFormat(undefined, {
  year: 'numeric',
  month: 'short',
  day: 'numeric',
});

function chatGroup(chat, grouping) {
  switch (grouping) {
    case 'date':
      return new Date(chat.updated).setHours(0, 0, 0, 0);
    case 'workspace':
      return chat.workspace || 'Conversations';
    case 'agent':
      return workspace.agents.find((a) => a.id === chat.agent)?.name || 'base';
    default:
      return chat.pinned ? 'Pinned' : 'Last 15 days';
  }
}

function renderChats() {
  const list = $('chats');
  list.replaceChildren();
  const q = $('filter').value.toLowerCase(),
    grouping = preference('arrange', 'none');
  const matches = workspace.chats.filter((c) => c.title.toLowerCase().includes(q));
  const groups = new Map();
  for (const c of matches) {
    const key = chatGroup(c, grouping);
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(c);
  }
  const names = [...groups.keys()];
  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const weekStart = new Date(today);
  weekStart.setDate(today.getDate() - 6);
  if (grouping === 'date') names.sort((a, b) => b - a);
  else if (grouping !== 'none') names.sort((a, b) => a.localeCompare(b));
  for (const name of names) {
    let label = name;
    if (grouping === 'date') {
      const format = name >= weekStart && name <= today ? recentDate : fullDate;
      label = format.format(name);
    } else if (grouping === 'workspace') {
      label = name.split('/').filter(Boolean).pop() || '/';
    }
    const h = element('div', 'group', label);
    h.title = grouping === 'workspace' ? name : label;
    list.append(h);
    const items = groups.get(name).sort((a, b) => b.updated - a.updated);
    for (const c of items) {
      const row = element('div', 'row' + (c.id === selected?.id ? ' active' : ''));
      const link = element('button', 'chat-link', c.title);
      link.title = c.title;
      link.onclick = () => command('chat', c.id);
      const pin = element('button', 'icon pin' + (c.pinned ? ' on' : ''));
      pin.title = (c.pinned ? 'Unpin ' : 'Pin ') + c.title;
      pin.setAttribute('aria-label', pin.title);
      const icon = document.createElementNS('http://www.w3.org/2000/svg', 'svg'),
        use = document.createElementNS(icon.namespaceURI, 'use');
      use.setAttribute('href', '#i-pin');
      icon.append(use);
      pin.append(icon);
      pin.onclick = async () => {
        try {
          await request('chats/' + encodeURIComponent(c.id) + '/pin', 'PUT', { pinned: !c.pinned });
          await refresh();
        } catch (e) {
          report(e);
        }
      };
      if (c.ready > (c.seen || 0)) {
        const dot = element('span', 'reply-ready');
        dot.title = 'Unread reply';
        dot.setAttribute('role', 'img');
        dot.setAttribute('aria-label', 'Unread reply');
        row.append(dot);
      }
      row.append(link, pin);
      list.append(row);
    }
  }
  if (!matches.length)
    list.append(element('p', 'side-empty', q ? 'No matching chats.' : 'No recent conversations.'));
}
function closeNav() {
  $('shell').classList.remove('nav-open');
  $('scrim').hidden = true;
}
function toggleNav() {
  if (matchMedia('(max-width:850px)').matches) {
    $('scrim').hidden = !$('shell').classList.toggle('nav-open');
  } else $('shell').classList.toggle('nav-hidden');
}
$('add-pane').onclick = () => {
  const available = workspace.chats.filter((c) => !panes.some((p) => p.id === c.id));
  menu($('add-pane'), 'Open in a new pane',
    [{label:'New chat',value:''}, ...available.map((c) => ({label:c.title,value:c.id}))],
    null, (id) => addPane(id), {search:'Filter chats'});
};
$('new-chat').onclick = () => command('new');
$('build-agent').onclick = () => command('build');
$('close-nav').onclick = $('scrim').onclick = closeNav;
$('find').onclick = () => {
  const f = $('filter');
  f.hidden = !f.hidden;
  if (f.hidden) {
    f.value = '';
    renderChats();
  } else f.focus();
};
$('filter').oninput = renderChats;
$('arrange').onclick = () =>
  menu(
    $('arrange'),
    'Arrange conversations',
    [
      { label: 'No grouping', value: 'none' },
      { label: 'Date', value: 'date' },
      { label: 'Workspace', value: 'workspace' },
      { label: 'Agent', value: 'agent' },
    ],
    preference('arrange', 'none'),
    (value) => {
      savePreference('arrange', value);
      renderChats();
    },
  );
const settingsMenu = $('settings-menu');
function preferenceGroup(label, key, values, fallback) {
  const group = element('div');
  group.setAttribute('role', 'group');
  group.setAttribute('aria-label', label);
  group.append(element('div', 'menu-title', label));
  function mark(selected) {
    for (const b of group.querySelectorAll('button')) {
      const checked = b.value === selected;
      b.setAttribute('aria-checked', String(checked));
      b.lastElementChild.textContent = checked ? '✓' : '';
    }
  }
  for (const value of values) {
    const b = element('button', 'choice');
    b.type = 'button';
    b.value = value;
    b.tabIndex = -1;
    b.setAttribute('role', 'menuitemradio');
    const check = element('span', 'check');
    check.setAttribute('aria-hidden', 'true');
    b.append(element('span', '', value[0].toUpperCase() + value.slice(1)), check);
    b.onclick = () => {
      savePreference(key, value);
      mark(value);
    };
    group.append(b);
  }
  mark(preference(key, fallback));
  return group;
}
settingsMenu.addEventListener('beforetoggle', (e) => {
  if (e.newState !== 'open') return;
  closeMenu();
  settingsMenu.replaceChildren(
    preferenceGroup('Appearance', 'theme', ['system', 'light', 'dark'], 'system'),
    preferenceGroup('Text width', 'width', ['narrow', 'default', 'wide'], 'default'),
  );
  settingsMenu.querySelector('[aria-checked=true]').autofocus = true;
});
settingsMenu.onkeydown = (e) => {
  const buttons = [...settingsMenu.querySelectorAll('button')];
  const index = buttons.indexOf(document.activeElement);
  if (e.key === 'Tab') {
    settingsMenu.hidePopover();
    $('settings').focus();
    return;
  }
  const next = {
    ArrowDown: (index + 1) % buttons.length,
    ArrowUp: (index - 1 + buttons.length) % buttons.length,
    Home: 0,
    End: buttons.length - 1,
  }[e.key];
  if (next === undefined) return;
  e.preventDefault();
  buttons[next].focus();
};
animateTorus($('brand-mark'), true);
function pairBrowser() {
  const dialog = $('connect-dialog');
  if (!dialog.open) dialog.showModal();
}
window.addEventListener('pair-browser', pairBrowser);
$('connect-dialog').addEventListener('cancel', (e) => e.preventDefault());
async function connect(key) {
  if (key) await request('unlock', 'POST', { key });
  await refresh();
  $('hub-status').textContent = '';
  if (!panes.length) {
    const id = new URLSearchParams(location.search).get('chat');
    const known = (id) => workspace.chats.some((c) => c.id === id);
    let saved;
    try { saved = JSON.parse(sessionStorage.getItem('toroid.panes')); } catch {}
    const ids = Array.isArray(saved?.ids) ? saved.ids.slice(0, 3).map((id) => known(id) ? id : '') : [];
    if (!ids.length) ids.push('');
    let index = Math.max(0, Math.min(ids.length - 1, Number(saved?.active) || 0));
    if (id && known(id)) {
      if (ids.includes(id)) index = ids.indexOf(id);
      else ids[index] = id;
    } else ids[index] = '';
    restoring = true;
    for (const chat of ids) addPane(chat, 'replace');
    restoring = false;
    select(panes[index], 'replace');
    panes[index].frame.scrollIntoView({block:'nearest',inline:'nearest'});
  }
  $('connect-dialog').close();
}
$('connect-form').onsubmit = async (e) => {
  e.preventDefault();
  const field = $('launch-link');
  try {
    const link = new URL(field.value.trim());
    if (link.origin !== location.origin || !link.hash.slice(1)) throw Error();
    await connect(link.hash.slice(1));
    field.value = '';
    $('connect-error').textContent = '';
  } catch {
    $('connect-error').textContent = 'Use the current server link for this workspace.';
    field.select();
  }
};
try {
  const key = location.hash.slice(1);
  history.replaceState({ ...history.state, chatIndex }, '', location.pathname + location.search);
  await connect(key);
} catch (e) {
  report(e);
  pairBrowser();
}
document.addEventListener('visibilitychange', () => {
  if (!document.hidden && !$('connect-dialog').open) refresh().catch(report);
});

// Detached panes have no reader to announce completion. Poll while the page is
// visible, and check more often while any reply is in flight.
async function poll() {
  if (!document.hidden && panes.length && !$('connect-dialog').open) {
    try { await refresh(); } catch (error) { report(error); }
  }
  setTimeout(poll, workspace.chats.some((c) => c.stream) ? 1500 : 5000);
}
setTimeout(poll, 1500);
