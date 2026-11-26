import { $, element, icon, request, menu, closeMenu, preference, savePreference } from './ui.js';
import { makeEditor } from './editor.js';
import { paintMarkdown } from './markdown.js';
import { animateTorus } from './torus.js';
import { attachmentList, droppedItems } from './attachments.js';
const notify = (msg) => parent.postMessage(msg, location.origin);
let defaultCWD = '',
  catalog = [],
  catalogLoad,
  folderCursor = '',
  folderVersion = 0,
  reportedState = '';
const scroll = $('scroll');
const records = new Map();
let chats = [],
  agents = [],
  view,
  selectionRange,
  selectedQuote,
  selectionAnchor,
  paintFrame;
const textNodes = new Map();
const toolRows = new Map();
// Role-relative positions survive a live transcript being reloaded from SQLite.
const commentBodies = new Map();
function commentBody(kind, body) {
  if (!commentBodies.has(kind)) commentBodies.set(kind, []);
  commentBodies.get(kind).push(body);
  body.dataset.commentKind = kind;
  return body;
}

const newView = () => ({
  id: '',
  agent: 'base',
  model: '',
  workspace: '',
  cwd: '',
  editorState: null,
  formatting: false,
  draft: null,
  name: '',
  messages: [],
  comments: [],
  attachments: [],
  prompt: '',
  busy: false,
  follow: true,
  top: 0,
  error: '',
});

function showError(error, target = view) {
  target.error = error.message || String(error);
  if (target === view) controls();
}

function applyWorkspace(data) {
  chats = data.chats;
  agents = data.agents;
  defaultCWD = data.cwd;
  if (!agents.some((a) => a.id === view.agent)) view.agent = 'base';
  pickers();
  renderAgents();
  controls();
}
let nextRefresh = 0;
const refreshRequests = new Map();
function refresh() {
  const id = ++nextRefresh;
  return new Promise((resolve, reject) => {
    refreshRequests.set(id, { resolve, reject });
    notify({ type: 'changed', id });
  });
}
function activeModel() {
  return (
    view.draft?.model || view.model || agents.find((a) => a.id === view.agent)?.settings.model || ''
  );
}
function pickers() {
  $('agent').querySelector('span').textContent =
    agents.find((a) => a.id === view.agent)?.name || 'base';
  const model = activeModel();
  $('model').querySelector('span').textContent = model.replace(/^llmgateway\//, '') || 'Model';
  $('model').title = model;
  $('folder').querySelector('span').textContent = view.workspace
    ? view.workspace.split('/').pop() || '/'
    : view.id
      ? 'No workspace'
      : 'Folder';
  $('folder').title = view.workspace || 'No workspace · ' + (view.cwd || defaultCWD);
  $('folder').disabled = !!view.id || view.busy || view.loading;
  $('thinking').textContent = view.draft?.thinking || 'low';
}
function readDraft() {
  return {
    model: view.draft.model,
    system: $('instructions').value,
    thinking: view.draft.thinking,
    computer: $('computer').checked,
    skills: $('skills').checked,
    delegate: $('delegate').checked,
  };
}

function switchView(next) {
  if (view) {
    if (view !== next && view.stream && !view.pendingPrompt) view.controller?.abort();
    view.top = scroll.scrollTop;
    view.editorState = editor.snapshot();
  }
  closeMenu();
  $('folder-dialog').close();
  closeAnnotation();
  view = next;

  const building = !!view.draft;
  $('shell').classList.toggle('building', building);
  $('builder').hidden = $('preview-label').hidden = $('draft-label').hidden = !building;
  $('agent').hidden = building;
  if (building) {
    const d = view.draft;

    $('instructions').value = d.system;

    $('computer').checked = d.computer;
    $('skills').checked = d.skills;
    $('delegate').checked = d.delegate;
    $('agent-name').value = view.name;
  }
  $('section').textContent = building ? 'Agent builder' : 'Workspace';
  $('empty-title').textContent = building ? 'Test your agent' : 'New conversation';
  $('empty-hint').textContent = building
    ? 'Try a prompt with the current configuration.'
    : 'Choose an agent and send a message.';
  editor.restore(view.editorState);
  setFormatting(view.formatting);
  $('messages').replaceChildren();
  textNodes.clear();
  toolRows.clear();
  commentBodies.clear();
  for (const m of view.messages) drawMessage(m);
  if (view.pendingPrompt) drawPendingPrompt(view.pendingPrompt);
  pickers();
  renderAgents();
  drawComments();
  drawAttachments();
  controls();
  scroll.scrollTop = view.follow ? scroll.scrollHeight : view.top;
}

async function openChat(id) {
  const next = records.get(id) || newView();
  records.set(id, next);
  next.id = id;
  switchView(next);
  if (next.busy || next.loading) return;
  next.loading = true;
  controls();
  try {
    const data = await (await request('chats/' + encodeURIComponent(id) + '?live')).json();
    next.error = '';
    next.messages = data.messages;
    const history = data.messages.flatMap((m) => m.comments || []);
    next.comments = loadDraftComments(next);
    restoreComments(next, history);
    next.commentsHidden = preference('comments-hidden.' + id, 'false') === 'true';
    next.ready = data.ready;
    next.usage = data.usage;
    if (next.usage?.model) loadModelWindow(next, next.usage);
    next.agent = data.agent;
    next.draft = data.draft;
    next.workspace = data.options.workspace;
    next.model = data.options.model;
    next.cwd = data.options.cwd;
    if (view === next) {
      switchView(next);
      if (data.stream) {
        next.busy = true;
        next.phase = 'Reconnecting…';
        next.stream = data.stream;
        followReply(next);
      }
    }
  } finally {
    next.loading = false;
    if (view === next) controls();
  }
}

function argumentText(raw) {
  try {
    const args = JSON.parse(raw);
    return JSON.stringify(args, null, 2);
  } catch {
    return raw;
  }
}

function updateToolResult(call) {
  const row = toolRows.get(call);
  if (!row) return;
  row.status.textContent = call.failed ? 'Failed' : call.running ? 'Running…' : '';
  row.output.textContent =
    call.output ?? (call.running ? 'Waiting for output…' : 'No result recorded.');
  if (call.output === '') row.output.textContent = '(empty output)';
  repaintCommentBody(row.output);
}

function drawTool(m) {
  const call = m.call;
  const transcript = $('messages');
  let group = transcript.lastElementChild;
  if (!group?.classList.contains('tool-group')) {
    group = element('details', 'tool-group');
    group.append(element('summary', 'tool-heading'));
    // Let the reader inspect this batch while the response continues below.
    group.addEventListener('click', (e) => {
      if (e.target.closest('summary')) view.follow = false;
    });
    transcript.append(group);
  }
  const row = element('details', 'tool-row');
  const heading = element('summary');
  const name = element('strong', '', call.name || 'Tool');
  const preview = element('span', 'tool-preview');
  let label = call.arguments;
  try {
    const args = JSON.parse(call.arguments);
    label = args?.command || args?.path || label;
  } catch {
    /* Show raw arguments from older transcripts. */
  }
  preview.textContent = (typeof label === 'string' ? label : '').replace(/\s+/g, ' ').slice(0, 140);
  const status = element('span', 'tool-state');
  heading.append(name, preview, status);
  const output = commentBody('output', element('pre', 'body tool-body'));
  row.append(
    heading,
    element('div', 'tool-label', 'Arguments'),
    commentBody(
      'arguments',
      element('pre', 'body tool-body', argumentText(call.arguments) || '(no arguments)'),
    ),
    element('div', 'tool-label', 'Output'),
    output,
    messageActions(m),
  );
  group.append(row);
  const count = group.childElementCount - 1;
  if (count === 1) group.dataset.name = call.name;
  if (group.dataset.name !== call.name) group.dataset.name = '';
  group.firstElementChild.textContent =
    count +
    (count === 1 ? ' tool call' : ' tool calls') +
    (group.dataset.name ? ' · ' + group.dataset.name : '');
  toolRows.set(call, { status, output });
  updateToolResult(call);
}

function drawMessage(m) {
  if (m.role === 'tool') {
    drawTool(m);
    return;
  }
  const box = element('article', 'msg ' + m.role);
  if (m.role === 'assistant' || m.role === 'user') {
    box.append(element('div', 'speaker', m.role === 'user' ? 'You' : 'Toroid'));
    const body = commentBody(m.role, element('div', 'body markdown'));
    box.append(body);
    paintMarkdown(body, m.text || '');
    if (m.attachments?.length) box.append(attachmentList(m.attachments));
    if (m.role === 'assistant') textNodes.set(m, { body, shown: m.text.length });
  } else {
    box.textContent = m.text;
  }
  box.append(messageActions(m));
  $('messages').append(box);
}

function drawPendingPrompt({text, attachments}) {
  const box = element('article', 'msg user');
  box.id = 'pending-prompt';
  const body = element('div', 'body markdown');
  paintMarkdown(body, text);
  box.append(element('div', 'speaker', 'You'), body);
  if (attachments.length) box.append(attachmentList(attachments));
  $('messages').append(box);
}

const clockTime = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' });
const fullTime = new Intl.DateTimeFormat(undefined, { dateStyle: 'full', timeStyle: 'long' });

function messageActions(message) {
  const bar = element('div', 'message-actions');
  if (message.timestamp) {
    const stamp = element('time', '', clockTime.format(message.timestamp));
    stamp.dateTime = new Date(message.timestamp).toISOString();
    stamp.title = fullTime.format(message.timestamp);
    stamp.setAttribute('aria-label', stamp.title);
    bar.append(stamp);
  }
  const copy = element('button', 'copy-action');
  copy.type = 'button';
  copy.title = copy.ariaLabel = 'Copy message';
  const label = element('span', '', 'Copy');
  label.setAttribute('aria-live', 'polite');
  copy.append(icon('copy'), label);
  let reset;
  copy.onclick = async () => {
    const call = message.call;
    const content = call
      ? [call.name, argumentText(call.arguments), call.output]
          .filter((part) => part != null && part !== '')
          .join('\n\n')
      : [message.text, ...(message.attachments || []).map((a) => a.path)].filter(Boolean).join('\n');
    try {
      await navigator.clipboard.writeText(content);
      label.textContent = 'Copied';
      clearTimeout(reset);
      reset = setTimeout(() => {
        label.textContent = 'Copy';
      }, 1500);
    } catch {
      showError(Error('Could not copy. Check clipboard access for this page.'));
    }
  };
  bar.append(copy, forkButton(message));
  return bar;
}

function forkButton(message) {
  const b = element('button', 'fork-action');
  b.type = 'button';
  b.title = 'Fork from this message';
  b.setAttribute('aria-label', b.title);
  b.append(icon('fork'), element('span', '', 'Fork'));
  b.onclick = async () => {
    const source = view;
    b.disabled = true;
    try {
      const path = 'chats/' + encodeURIComponent(source.id);
      let point = message.point;
      if (!point) {
        // Stream frames precede the final event. Resolve the displayed message
        // against the persisted transcript before creating the branch.
        const { messages } = await (await request(path)).json();
        const index = source.messages.indexOf(message);
        const ordinal = source.messages
          .slice(0, index)
          .filter((m) => m.role === message.role).length;
        const saved = messages.filter((m) => m.role === message.role)[ordinal];
        if (
          !saved ||
          (message.call ? message.call.id !== saved.call?.id : (message.text || '') !== (saved.text || ''))
        )
          throw Error('This message is still being recorded. Fork it when the step finishes.');
        point = saved.point;
      }
      const result = await (await request(path + '/fork', 'POST', { point })).json();
      await refresh();
      if (source === view) await openChat(result.id);
    } catch (error) {
      showError(error, source);
    } finally {
      b.disabled = false;
    }
  };
  return b;
}
function addMessage(v, role, text, timestamp, call, attachments) {
  const m = { role, text, timestamp, call, attachments };
  v.messages.push(m);
  if (v === view) {
    $('empty').hidden = true;
    drawMessage(m);
    schedulePaint();
  }
  return m;
}

// Render ordinary tokens on the next frame. Smooth larger bursts for at
// most 80 ms, including on a slow display. Never queue animation after EOF.
const dirty = new Set();
const reduceMotion = matchMedia('(prefers-reduced-motion: reduce)');
function schedulePaint(m) {
  if (m) dirty.add(m);
  if (paintFrame) return;
  paintFrame = requestAnimationFrame((now) => {
    paintFrame = 0;
    for (const item of dirty) {
      const target = textNodes.get(item);
      if (!target) {
        dirty.delete(item);
        continue;
      }
      if (selectionRange && target.body.contains(selectionRange.commonAncestorContainer)) continue;
      const left = item.text.length - target.shown;
      target.deadline ??= now + 80;
      const frames = Math.max(1, Math.ceil((target.deadline - now) / 16));
      const step = reduceMotion.matches || !view.busy ? left : Math.max(12, Math.ceil(left / frames));
      target.shown = Math.min(item.text.length, target.shown + step);
      if (target.shown < item.text.length && /[\uD800-\uDBFF]/.test(item.text[target.shown - 1]))
        target.shown++;
      paintMarkdown(target.body, item.text.slice(0, target.shown));
      repaintCommentBody(target.body);
      target.body.classList.toggle(
        'revealing',
        target.shown < item.text.length || (view.busy && !reduceMotion.matches),
      );
      if (target.shown === item.text.length) {
        dirty.delete(item);
        delete target.deadline;
      }
    }
    if (view.comments.length) positionComments();
    if (view.follow) scroll.scrollTop = scroll.scrollHeight;
    updateJump();
    if (dirty.size && !selectionRange) schedulePaint();
  });
}

function atBottom() {
  return scroll.scrollHeight - scroll.clientHeight - scroll.scrollTop < 4;
}
function updateJump() {
  $('jump').hidden = view.follow || atBottom();
}
scroll.addEventListener(
  'scroll',
  () => {
    // An upward gesture detaches immediately, even before its scroll event.
    view.follow = atBottom() && !selectionRange;
    updateJump();
    if (selectionRange) placeAnnotation();
  },
  { passive: true },
);
scroll.addEventListener(
  'wheel',
  (e) => {
    if (e.deltaY < 0) view.follow = false;
  },
  { passive: true },
);
let lastTouchY;
scroll.addEventListener(
  'touchstart',
  (e) => {
    lastTouchY = e.touches[0].clientY;
  },
  { passive: true },
);
scroll.addEventListener(
  'touchmove',
  (e) => {
    const y = e.touches[0].clientY;
    if (y > lastTouchY) view.follow = false;
    lastTouchY = y;
  },
  { passive: true },
);
$('jump').onclick = () => {
  closeAnnotation();
  view.follow = true;
  scroll.scrollTop = scroll.scrollHeight;
  updateJump();
};

function controls() {
  drawUsage();
  const building = !!view.draft;
  $('section').textContent = building
    ? 'Agent builder'
    : view.workspace.split('/').filter(Boolean).pop() || 'Conversations';
  $('title').textContent = building
    ? chats.find((c) => c.id === view.id)?.title || view.name.trim() || 'Untitled agent'
    : chats.find((c) => c.id === view.id)?.title || (view.id ? 'Conversation' : 'New chat');
  $('rename-chat').hidden = !view.id || !!view.loading;
  $('empty').hidden = !!view.messages.length || !!view.pendingPrompt || !!view.loading;
  $('state').textContent = view.loading ? 'Loading…' : (view.busy ? view.phase : '');
  $('send').classList.toggle('stop', view.busy);
  $('send').setAttribute('aria-label', view.busy ? 'Stop reply' : 'Send message');
  $('send').disabled =
    !agents.length ||
    !!view.loading ||
    (!view.busy && !view.prompt.trim() && !view.comments.some((c) => !c.resolved) && !view.attachments.length);
  $('save-agent').disabled = view.busy || !!view.saving;
  $('notice').hidden = !view.error;
  $('notice').textContent = view.error;
  pickers();
  const state = JSON.stringify([view.id, view.busy, view.loading, view.ready, $('title').textContent]);
  if (state !== reportedState) {
    reportedState = state;
    notify({ type: 'state', id: view.id, busy: view.busy, ready: view.loading || view.busy ? 0 : view.ready, title: $('title').textContent });
  }
  updateJump();
}

const draftKey = (chat, id = '') => 'toroid.comment-draft.' + chat + '.' + id;

function saveDraftComment(c) {
  const { id, created, quote, note, anchor: { kind, index, start } } = c;
  try {
    localStorage.setItem(draftKey(view.id, id), JSON.stringify({ id, created, quote, note, anchor: { kind, index, start } }));
  } catch {
    showError(Error('Browser storage is unavailable. This draft is saved only in the open pane.'));
  }
}

function forgetDraftComment(chat, id) {
  try { localStorage.removeItem(draftKey(chat, id)); } catch {}
}

function loadDraftComments(v) {
  const prefix = draftKey(v.id);
  try {
    const drafts = [];
    for (const key of Object.keys(localStorage)) {
      if (!key.startsWith(prefix)) continue;
      const c = JSON.parse(localStorage.getItem(key));
      if (c?.anchor && typeof c.quote === 'string' && typeof c.note === 'string') {
        drafts.push({ ...c, anchor: { ...c.anchor, text: c.quote } });
      }
    }
    return drafts.sort((a, b) => a.created - b.created);
  } catch {
    return v.comments.filter((c) => !c.resolved);
  }
}

function restoreComments(v, saved) {
  const ids = new Set(saved.map((c) => c.id).filter(Boolean));
  for (const c of v.comments) {
    if (!c.resolved && ids.has(c.id)) forgetDraftComment(v.id, c.id);
  }
  v.comments = v.comments.filter((c) => !ids.has(c.id));
  for (const c of saved) {
    v.comments.push({ ...c, resolved: true, anchor: c.anchor && { ...c.anchor, text: c.quote } });
  }
}

function anchorRange(anchor) {
  if (!anchor) return;
  const body = commentBodies.get(anchor.kind)?.[anchor.index];
  if (!body) return;
  if (
    anchor.range && body.contains(anchor.range.commonAncestorContainer) &&
    anchor.range.toString() === anchor.text
  )
    return anchor.range;

  // Streaming Markdown may replace text nodes or consume its delimiters.
  // Restore the same occurrence, including when the quote spans inline elements.
  const text = body.textContent;
  let start = anchor.start;
  if (!text.startsWith(anchor.text, start)) {
    const before = text.lastIndexOf(anchor.text, start);
    const after = text.indexOf(anchor.text, start);
    start = before < 0 || (after >= 0 && after - start < start - before) ? after : before;
    if (start < 0) return;
  }
  const end = start + anchor.text.length;
  const nodes = document.createTreeWalker(body, NodeFilter.SHOW_TEXT);
  const range = document.createRange();
  let offset = 0;
  for (let node = nodes.nextNode(); node; node = nodes.nextNode()) {
    const next = offset + node.length;
    if (offset <= start && start < next) range.setStart(node, start - offset);
    if (end <= next) {
      range.setEnd(node, end - offset);
      anchor.range = range;
      return range;
    }
    offset = next;
  }
}

function paintCommentHighlights() {
  if (!CSS.highlights) return;
  const pending = [], resolved = [];
  if (!view?.commentsHidden) {
    for (const c of view?.comments || []) {
      const range = anchorRange(c.anchor);
      if (range) (c.resolved ? resolved : pending).push(range);
    }
    if (selectionRange) pending.push(selectionRange);
  }
  for (const [name, ranges] of [['resolved-passages', resolved], ['comment-passages', pending]]) {
    if (ranges.length) CSS.highlights.set(name, new Highlight(...ranges));
    else CSS.highlights.delete(name);
  }
}
function repaintCommentBody(body) {
  if (!view.commentsHidden && view.comments.some(({ anchor }) => anchor && commentBodies.get(anchor.kind)?.[anchor.index] === body))
    paintCommentHighlights();
}

async function loadModelWindow(v, usage) {
  try {
    const models = catalog.length ? catalog : await loadModels();
    if (v.usage !== usage) return;
    usage.modelWindow = models.find((m) => m.id === usage.model)?.contextLength;
    if (view === v) drawUsage();
  } catch { /* The kernel window remains available offline. */ }
}

const rupees = new Intl.NumberFormat('en-IN', { style: 'currency', currency: 'INR' });
const costLabel = (usd) => usd == null ? '—' : rupees.format(usd * 100);
function compactTokens(n) {
  if (n == null) return '—';
  if (n < 1000) return String(n);
  const unit = n < 1_000_000 ? 1000 : 1_000_000;
  const count = n < 10 * unit ? Number((n / unit).toFixed(1)) : Math.floor(n / unit);
  return count + (unit === 1000 ? 'k' : 'M');
}
function drawUsage() {
  const u = view.usage;
  $('usage-strip').hidden = !u;
  if (!u) return;
  $('chat-spend').textContent = costLabel(u.spendUSD);
  $('spend-total').title = u.spendUSD == null ? 'Total unavailable: missing usage or pricing.' :
    (u.estimatedSpend ? 'Estimated from recorded usage and gateway rates.' : 'Reported cost of completed calls.') + ' Converted at ₹100 per USD.';
  const capacity = u.modelWindow || u.window;
  const ratio = capacity > 0 && u.used != null ? Math.min(u.used / capacity, 1) : null;
  const meter = $('context-meter'), bar = $('context-bar');
  const count = u.used == null ? 'Usage unavailable' : u.used.toLocaleString() + ' tokens';
  const detail = [count, u.modelWindow && 'Model input limit ' + u.modelWindow.toLocaleString(),
    u.window && 'Kernel window ' + u.window.toLocaleString()].filter(Boolean).join(' · ');
  const filled = Math.floor((ratio ?? 0) * 20);
  bar.textContent = '█'.repeat(filled) + '░'.repeat(20 - filled);
  if (ratio == null) bar.removeAttribute('aria-valuenow');
  else bar.setAttribute('aria-valuenow', String(Math.round(ratio * 100)));
  bar.setAttribute('aria-valuetext', detail);
  meter.title = detail;
  $('context-label').textContent = compactTokens(u.used) + '/' + compactTokens(capacity || null);
}

function positionComments() {
  const rail = $('comments');
  if (rail.hidden) return;
  const railTop = rail.getBoundingClientRect().top;
  const cards = view.comments.map((c, i) => {
    const range = anchorRange(c.anchor);
    const body = c.anchor && commentBodies.get(c.anchor.kind)?.[c.anchor.index];
    const closed = body?.closest('details:not([open])');
    const rect = closed?.closest('.tool-group:not([open])')?.getBoundingClientRect() ||
      closed?.getBoundingClientRect() || range?.getClientRects()[0];
    const card = rail.children[i];
    return {card, top: rect ? rect.top - railTop : 0, height: card.offsetHeight};
  }).sort((a, b) => a.top - b.top);
  let bottom = 0;
  for (const {card, top, height} of cards) {
    const y = Math.max(0, Math.round(top), bottom);
    card.style.top = y + 'px';
    bottom = y + height + 8;
  }
  rail.style.minHeight = bottom + 'px';
}

function removeComment(c) {
  forgetDraftComment(view.id, c.id);
  view.comments = view.comments.filter((item) => item !== c);
  drawComments();
  controls();
  editor.focus();
}

function composerComment(c, noteInMargin, number, submitting) {
  const row = element('div', 'composer-comment');
  const quote = element('span', 'composer-quote', '“' + c.quote + '”');
  quote.title = c.quote;
  const field = element('textarea');
  field.rows = 1;
  field.maxLength = 8000;
  field.value = c.note;
  field.readOnly = submitting;
  field.setAttribute('aria-label', 'Comment ' + number + ' on “' + c.quote + '”');
  field.oninput = () => {
    c.note = field.value;
    saveDraftComment(c);
    noteInMargin.textContent = c.note;
    positionComments();
  };
  field.onkeydown = (event) => {
    if (!event.isComposing && event.key === 'Enter' && (event.metaKey || event.ctrlKey || event.shiftKey)) {
      event.preventDefault();
      if (!view.busy) send();
    }
  };
  const remove = element('button', 'composer-comment-remove');
  remove.type = 'button';
  remove.disabled = submitting;
  remove.title = remove.ariaLabel = 'Remove comment ' + number + ' from next message';
  remove.append(icon('close'));
  remove.onclick = () => removeComment(c);
  row.append(quote, remove, field);
  return row;
}

function drawComments() {
  paintCommentHighlights();
  const group = $('comments');
  group.replaceChildren();
  const pending = $('pending-comments');
  pending.replaceChildren();
  const toggle = $('toggle-comments');
  toggle.hidden = !view.id;
  toggle.title = toggle.ariaLabel = view.commentsHidden ? 'Show comments' : 'Hide comments';
  toggle.setAttribute('aria-expanded', String(!view.commentsHidden));
  group.hidden = view.commentsHidden || !view.comments.length;
  let number = 0;
  view.comments.forEach((c, i) => {
    const note = element('p', 'comment-note', c.note);
    const submitting = !!view.pendingPrompt?.comments?.includes(c);
    if (!c.resolved) pending.append(composerComment(c, note, ++number, submitting));
    if (group.hidden) return;
    const card = element('article', 'comment-card' + (c.resolved ? ' resolved' : ''));
    const head = element('div', 'comment-head');
    const quote = element('button', 'comment-quote', '“' + c.quote + '”');
    quote.title = c.quote;
    quote.onclick = () => {
      const body = c.anchor && commentBodies.get(c.anchor.kind)?.[c.anchor.index];
      for (let node = body; node; node = node.parentElement) {
        if (node.tagName === 'DETAILS') node.open = true;
      }
      const rect = anchorRange(c.anchor)?.getClientRects()[0];
      if (rect) {
        view.follow = false;
        scroll.scrollTop += rect.top - scroll.getBoundingClientRect().top - 20;
      }
    };
    head.append(quote);
    if (c.resolved) {
      quote.disabled = true;
      const label = element('span', 'comment-resolved', 'Resolved');
      head.append(label);
    } else {
      const remove = element('button', 'comment-remove', '×');
      remove.type = 'button';
      remove.setAttribute('aria-label', 'Remove comment ' + (i + 1));
      remove.disabled = submitting;
      remove.onclick = () => removeComment(c);
      head.append(remove);
    }
    card.append(head, note);
    group.append(card);
  });
  pending.hidden = !pending.childElementCount;
  positionComments();
}

function closeAnnotation() {
  selectionRange = null;
  selectedQuote = '';
  selectionAnchor = null;
  paintCommentHighlights();
  $('annotation').hidden = true;
  $('note').value = '';
  if (view && dirty.size) schedulePaint();
}
function placeAnnotation() {
  const rects = selectionRange.getClientRects();
  if (!rects.length) return;
  const last = rects[rects.length - 1],
    panel = $('annotation'),
    viewport = scroll.getBoundingClientRect();
  panel.hidden = last.bottom < viewport.top || last.top > viewport.bottom;
  if (panel.hidden) return;
  panel.style.left = Math.max(5, Math.min(last.left, innerWidth - panel.offsetWidth - 5)) + 'px';
  panel.style.top =
    Math.max(5, Math.min(last.bottom + 5, innerHeight - panel.offsetHeight - 5)) + 'px';
}
function offerComment() {
  if (view.commentsHidden) return;
  const selection = getSelection();
  if (!selection?.rangeCount || selection.isCollapsed) return;
  const range = selection.getRangeAt(0);
  const start = range.startContainer;
  const body = (start.nodeType === Node.ELEMENT_NODE ? start : start.parentElement)?.closest(
    '[data-comment-kind]',
  );
  if (!body || !body.contains(range.endContainer)) return;
  const quote = selection.toString().trim();
  if (!quote) return;
  if (quote.length > 16000) {
    showError(Error('Select a shorter passage to comment.'));
    return;
  }
  view.follow = false;
  selectionRange = range.cloneRange();
  selectedQuote = quote;
  const prefix = document.createRange();
  prefix.selectNodeContents(body);
  prefix.setEnd(range.startContainer, range.startOffset);
  const kind = body.dataset.commentKind;
  selectionAnchor = {
    kind,
    index: commentBodies.get(kind).indexOf(body),
    start: prefix.toString().length + range.toString().indexOf(quote),
    text: quote,
    range: selectionRange,
  };
  paintCommentHighlights();
  $('annotation').hidden = false;
  placeAnnotation();
  $('note').focus({ preventScroll: true });
}
scroll.addEventListener('pointerdown', (e) => {
  if (e.target.closest('.body')) view.follow = false;
});
scroll.addEventListener('pointerup', () => setTimeout(offerComment, 0));
scroll.addEventListener('keyup', (e) => {
  if (e.shiftKey) offerComment();
});
window.addEventListener('resize', () => {
  if (selectionRange) placeAnnotation();
  schedulePaint();
});
function commitComment() {
  const note = $('note').value.trim();
  if (selectedQuote && note) {
    const top = selectionRange.getBoundingClientRect().top;
    const c = { id: crypto.randomUUID(), created: Date.now(), quote: selectedQuote, note, anchor: selectionAnchor };
    view.comments.push(c);
    saveDraftComment(c);
    drawComments();
    scroll.scrollTop += selectionRange.getBoundingClientRect().top - top;
    controls();
  }
  closeAnnotation();
}
$('toggle-comments').onclick = () => {
  view.commentsHidden = !view.commentsHidden;
  savePreference('comments-hidden.' + view.id, String(view.commentsHidden));
  closeAnnotation();
  drawComments();
  if (view.follow) scroll.scrollTop = scroll.scrollHeight;
};
$('annotation').onsubmit = (e) => {
  e.preventDefault();
  commitComment();
  getSelection()?.removeAllRanges();
  editor.focus();
};
document.addEventListener('pointerdown', (e) => {
  if (selectionRange && !$('annotation').contains(e.target)) commitComment();
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    closeAnnotation();
  }
});

let renameTarget;
$('rename-chat').onclick = () => {
  renameTarget = view.id;
  $('chat-title').value = chats.find((c) => c.id === view.id)?.title || '';
  $('rename-dialog').showModal();
  $('chat-title').select();
};
$('rename-form').onsubmit = async (e) => {
  e.preventDefault();
  const field = $('chat-title');
  if (!field.value.trim()) {
    field.focus();
    return;
  }
  const submit = e.currentTarget.querySelector('.primary');
  submit.disabled = true;
  try {
    await request('chats/' + encodeURIComponent(renameTarget) + '/title', 'PUT', {
      title: field.value,
    });
    $('rename-dialog').close();
    await refresh();
  } catch (error) {
    $('rename-dialog').close();
    showError(error);
  } finally {
    submit.disabled = false;
  }
};
$('mod-enter').textContent = /Mac|iPhone|iPad/.test(navigator.platform) ? '⌘ ↵' : 'Ctrl ↵';

$('agent-form').oninput = () => {
  view.draft = readDraft();
  view.name = $('agent-name').value;
  controls();
};
$('agent-form').onsubmit = async (e) => {
  e.preventDefault();
  const v = view;
  v.saving = true;
  controls();
  try {
    const item = await (
      await request('agents', 'POST', { name: v.name, settings: v.draft, preview: v.id })
    ).json();
    v.agent = item.id;
    v.draft = null;
    v.error = '';
    await refresh();
    if (view === v) switchView(v);
  } catch (error) {
    showError(error, v);
  } finally {
    v.saving = false;
    if (view === v) controls();
  }
};

async function consume(response, signal, onFrame) {
  const reader = response.body.getReader(),
    decoder = new TextDecoder();
  let buffer = '';
  const detach = () => reader.cancel().catch(() => {});
  signal.addEventListener('abort', detach, { once: true });
  try {
    while (true) {
      const { value, done } = await reader.read();
      buffer += decoder.decode(value, { stream: !done });
      let end;
      while ((end = buffer.indexOf('\n')) !== -1) {
        const line = buffer.slice(0, end);
        buffer = buffer.slice(end + 1);
        if (line) onFrame(JSON.parse(line));
      }
      if (done) break;
    }
  } finally {
    signal.removeEventListener('abort', detach);
    reader.releaseLock();
  }
}

async function send() {
  const v = view;
  if (v.busy) {
    v.stopRequested = true;
    if (v.stream) await stopReply(v);
    return;
  }
  if (selectionRange) commitComment();
  const sent = v.prompt,
    comments = v.comments.filter((c) => !c.resolved),
    attachments = [...v.attachments];
  if ((!sent.trim() && !comments.length && !attachments.length) || v.loading || v.saving) return;
  const emptyNote = [...$('pending-comments').querySelectorAll('textarea')].find((field) => !field.value.trim());
  if (emptyNote) {
    emptyNote.focus();
    emptyNote.setCustomValidity('Add a comment or remove this quote.');
    emptyNote.reportValidity();
    emptyNote.setCustomValidity('');
    return;
  }
  v.error = '';
  v.busy = true;
  v.phase = 'Sending…';
  const text = [sent.trim(), ...comments.map(({ quote, note }) =>
    '> ' + quote.trim().replaceAll('\n', '\n> ') + '\n' + note.trim(),
  )].filter(Boolean).join('\n\n');
  v.pendingPrompt = {text, attachments, comments};
  drawComments();
  drawPendingPrompt(v.pendingPrompt);
  schedulePaint();
  v.stopRequested = false;
  controls();
  try {
    const response = await request(
      'turns',
      'POST',
      {
        chat: v.id,
        agent: v.agent,
        model: v.draft ? '' : v.model,
        workspace: v.workspace,
        draft: v.draft,
        text: sent,
        attachments: attachments.map((item) => item.path),
        comments: comments.map(({ id, quote, note, anchor: { kind, index, start } }) =>
          ({ id, quote, note, anchor: { kind, index, start } })),
      },
    );
    v.id = response.headers.get('X-Toroid-Chat');
    v.stream = response.headers.get('X-Toroid-Stream');
    records.set(v.id, v);
    if (view === v) controls();
    if (v.stopRequested) await stopReply(v);
    await followReply(v, response, { sent, attachments });
  } catch (error) {
    v.busy = false;
    v.pendingPrompt = '';
    if (v === view) {
      $('pending-prompt')?.remove();
      drawComments();
    }
    showError(error, v);
  }
}

function streamPath(v) {
  return 'chats/' + encodeURIComponent(v.id) + '/stream/' + encodeURIComponent(v.stream);
}
async function stopReply(v) {
  try {
    await request(streamPath(v), 'DELETE');
  } catch (e) { showError(e, v); }
}

async function followReply(v, response, draft) {
  const turns = new Map(), pendingTools = new Map();
  const controller = new AbortController();
  v.controller = controller;
  let complete = false, reload = false;
  try {
    if (!response) response = await request(streamPath(v), 'GET', undefined, controller.signal);
    if (response.status === 204) { reload = true; return; }
    await consume(response, controller.signal, (frame) => {
      switch (frame.type) {
        case 'accepted':
          v.pendingPrompt = '';
          v.phase = 'Waiting for model…';
          if (view === v) $('pending-prompt')?.remove();
          v.id = frame.chat;
          records.set(v.id, v);
          if (draft && v.prompt === draft.sent) {
            v.prompt = '';
            v.editorState = null;
            if (view === v) editor.clear();
          }
          restoreComments(v, frame.comments || []);
          if (draft) {
            v.attachments = v.attachments.filter((a) => !draft.attachments.includes(a));
          }
          addMessage(v, 'user', frame.text, frame.timestamp, null, frame.attachments);
          if (view === v) {
            controls();
            drawComments();
            drawAttachments();
          }
          if (draft) refresh().catch((e) => showError(e, v));
          if (view !== v) controller.abort();
          break;
        case 'delta': {
          let m = turns.get(frame.turn);
          if (!m) {
            v.phase = 'Responding…';
            m = addMessage(v, 'assistant', '', frame.timestamp);
            turns.set(frame.turn, m);
            if (view === v) controls();
          }
          m.text += frame.text;
          if (view === v) schedulePaint(m);
          break;
        }
        case 'usage':
          v.usage = frame.usage;
          if (v.usage?.model) loadModelWindow(v, v.usage);
          if (view === v) drawUsage();
          break;
        case 'tool': {
          v.phase = 'Working…';
          if (view === v) controls();
          const call = frame.call;
          call.running = true;
          pendingTools.set(call.id, call);
          addMessage(v, 'tool', '', frame.timestamp, call);
          break;
        }
        case 'tool_result': {
          const call = pendingTools.get(frame.id);
          if (call) {
            Object.assign(call, { output: frame.output, failed: frame.failed, running: false });
            pendingTools.delete(frame.id);
            if (view === v) {
              updateToolResult(call);
              schedulePaint();
            }
          }
          break;
        }
        case 'done':
          v.ready = frame.ready;
          complete = true;
          break;
        case 'error':
          complete = true;
          showError(Error(frame.text), v);
          break;
      }
    });
    if (!complete) throw Error('The connection closed before the reply finished.');
  } catch (error) {
    reload = true;
    if (!controller.signal.aborted) showError(error, v);
  } finally {
    if (v.pendingPrompt) {
      v.pendingPrompt = '';
      if (v === view) {
        $('pending-prompt')?.remove();
        drawComments();
      }
    }
    for (const call of pendingTools.values()) {
      call.running = false;
      if (view === v) updateToolResult(call);
    }
    v.busy = false;
    if (view === v) {
      for (const m of turns.values()) textNodes.get(m)?.body.classList.remove('revealing');
      schedulePaint();
    }
    v.controller = null;
    v.stream = null;
    if (view === v) controls();
    if (!complete && view === v && (reload || controller.signal.aborted)) {
      setTimeout(() => {
        if (view === v && !v.busy) openChat(v.id).catch((e) => showError(e, v));
      }, response?.status === 204 || controller.signal.aborted ? 0 : 700);
    }
    if (complete) refresh().catch((e) => showError(e, v));
  }
}

const editor = makeEditor(
  $('prompt'),
  $('format-bar'),
  (md) => {
    view.prompt = md;
    controls();
  },
  () => {
    if (!view.busy) send();
  },
);
function setFormatting(on) {
  view.formatting = on;
  $('format-bar').hidden = !on;
  $('format-toggle').setAttribute('aria-expanded', String(on));
}
$('format-toggle').onmousedown = (e) => e.preventDefault();
$('format-toggle').onclick = () => {
  setFormatting(!view.formatting);
  editor.focus();
};
$('composer').onsubmit = (e) => {
  e.preventDefault();
  send();
};
$('agent').onclick = () =>
  menu(
    $('agent'),
    'Agent',
    agents.map((a) => ({ label: a.name, value: a.id })),
    view.agent,
    (id) => {
      view.agent = id;
      view.model = '';
      pickers();
      editor.focus();
    },
  );
$('thinking').onclick = () =>
  menu(
    $('thinking'),
    'Thinking',
    ['none', 'low', 'high'].map((v) => ({ value: v, label: v })),
    view.draft.thinking,
    (v) => {
      view.draft.thinking = v;
      pickers();
    },
  );
async function loadModels() {
  if (!catalogLoad)
    catalogLoad = request('models')
      .then((r) => r.json())
      .then((list) => {
        catalog = list;
        return list;
      })
      .finally(() => (catalogLoad = null));
  return catalogLoad;
}
$('model').onclick = async () => {
  const v = view;
  try {
    $('model').setAttribute('aria-busy', 'true');
    if (!catalog.length) await loadModels();
    if (view !== v) return;
    const current = activeModel();
    const hasImage = v.attachments.some((item) => item.kind === 'image');
    const options = catalog.map((m) => ({ label: m.name + (hasImage && m.images ? ' · images' : ''), value: m.id }));
    if (current && !options.some((o) => o.value === current))
      options.unshift({ label: current, value: current });
    menu(
      $('model'),
      'Model',
      options,
      current,
      (id) => {
        if (view.draft) view.draft.model = id;
        else view.model = id;
        pickers();
        editor.focus();
      },
      { search: true },
    );
  } catch (e) {
    showError(e, v);
  } finally {
    $('model').removeAttribute('aria-busy');
  }
};
let pickerMode = 'workspace', missingDrops = [], matchedDrops = [];
const pickedPaths = new Set();

function drawAttachments() {
  const host = $('attachments');
  host.replaceChildren();
  if (view.attachments.length) host.append(attachmentList(view.attachments, (item) => {
    view.attachments = view.attachments.filter((a) => a !== item);
    drawAttachments();
    controls();
  }));
}

async function attachPaths(paths, owner = view) {
  const items = await (await request('attachments', 'POST', { paths })).json();
  const additions = items.filter((item) => !owner.attachments.some((a) => a.path === item.path));
  if (owner.attachments.length + additions.length > 16)
    throw Error('Choose up to 16 attachments per message.');
  owner.attachments.push(...additions);
  owner.error = '';
  if (owner === view) {
    drawAttachments();
    controls();
  }
}

function openPicker(mode, drops = []) {
  pickerMode = mode;
  missingDrops = drops;
  pickedPaths.clear();
  $('folder-heading').textContent = {workspace: 'Workspace folder', attach: 'Attach files or folders', drop: 'Locate dropped items'}[mode];
  $('no-folder').textContent = mode === 'workspace' ? 'No workspace' : 'Cancel';
  $('folder-dialog').showModal();
  browse((mode !== 'workspace' && view.attachmentDir) || view.workspace || view.cwd || defaultCWD);
}

function selectionButton(file, folder = false) {
  const b = element('button', folder ? 'icon' : 'folder-entry attachment-option');
  b.type = 'button';
  if (folder) {
    b.ariaLabel = 'Select folder ' + file.name;
    b.title = b.ariaLabel;
    b.append(icon('plus'));
  } else {
    b.append(icon('file'), element('span', '', file.name), element('span', 'choice-tick', '✓'));
  }
  b.setAttribute('aria-pressed', String(pickedPaths.has(file.path)));
  b.onclick = () => {
    if (pickedPaths.has(file.path)) pickedPaths.delete(file.path);
    else pickedPaths.add(file.path);
    b.setAttribute('aria-pressed', String(pickedPaths.has(file.path)));
    pickerAction();
  };
  return b;
}

function pickerAction() {
  const b = $('use-folder');
  b.disabled = pickerMode === 'drop' && !matchedDrops.length;
  if (pickerMode === 'workspace') b.textContent = 'Select folder';
  else if (pickerMode === 'drop') b.textContent = 'Attach matched items';
  else b.textContent = pickedPaths.size ? 'Attach selected (' + pickedPaths.size + ')' : 'Attach this folder';
}

async function browse(path) {
  const version = ++folderVersion;
  $('folder-status').textContent = 'Loading folders…';
  $('use-folder').disabled = true;
  try {
    const data = await (await request('folders?path=' + encodeURIComponent(path) +
      (pickerMode === 'workspace' ? '' : '&files=1'))).json();
    if (version !== folderVersion) return;
    folderCursor = data.path;
    const crumbs = $('folder-path');
    crumbs.replaceChildren();
    let current = '';
    for (const name of ['/', ...data.path.split('/').filter(Boolean)]) {
      current = name === '/' ? '/' : current.replace(/\/$/, '') + '/' + name;
      const path = current;
      const b = element('button', '', name);
      b.type = 'button';
      b.title = path;
      b.disabled = path === data.path;
      b.onclick = () => browse(path);
      if (crumbs.childElementCount) crumbs.append(icon('chevron'));
      crumbs.append(b);
    }
    crumbs.scrollLeft = crumbs.scrollWidth;
    $('folder-up').disabled = data.parent === data.path;
    $('folder-up').onclick = () => browse(data.parent);
    const list = $('folder-list');
    list.replaceChildren();
    for (const f of data.folders) {
      const b = element('button', 'folder-entry');
      b.type = 'button';
      b.append(icon('folder'), element('span', '', f.name), icon('chevron'));
      b.onclick = () => browse(f.path);
      if (pickerMode === 'attach') {
        const row = element('div', 'attach-folder-row');
        row.append(b, selectionButton(f, true));
        list.append(row);
      } else list.append(b);
    }
    if (pickerMode === 'attach') {
      for (const f of data.files) list.append(selectionButton(f));
    }
    list.scrollTop = 0;
    if (pickerMode === 'drop') {
      matchedDrops = [];
      const entries = new Map([...data.folders, ...data.files].map((f) => [f.name, f]));
      for (const item of missingDrops) {
        const f = entries.get(item.name);
        if (!f || (item.folder !== undefined && item.folder !== data.folders.includes(f))) continue;
        matchedDrops.push({item, path: f.path});
        entries.delete(item.name);
      }
      $('folder-status').textContent = matchedDrops.length
        ? 'Attach from this folder: ' + matchedDrops.map((m) => m.item.name).join(', ')
        : 'Choose the folder containing ' + missingDrops.map((m) => m.name).join(', ') + '.';
    } else {
      $('folder-status').textContent = list.childElementCount ? '' : 'This folder is empty.';
    }
    pickerAction();
  } catch (e) {
    if (version === folderVersion) $('folder-status').textContent = e.message;
  }
}
$('folder').onclick = () => {
  if (view.id || view.busy) return;
  openPicker('workspace');
};
$('attach').onclick = () => openPicker('attach');
function chooseFolder(path) {
  if (!view.id && !view.busy) view.workspace = path;
  $('folder-dialog').close();
  pickers();
  editor.focus();
}
$('use-folder').onclick = async () => {
  if (pickerMode === 'workspace') return chooseFolder(folderCursor);
  const owner = view, version = folderVersion;
  const matches = matchedDrops;
  const paths = pickerMode === 'drop' ? matches.map((m) => m.path) : pickedPaths.size ? [...pickedPaths] : [folderCursor];
  $('use-folder').disabled = true;
  try {
    await attachPaths(paths, owner);
    if (view !== owner || version !== folderVersion) return;
    owner.attachmentDir = folderCursor;
    if (pickerMode === 'drop') {
      missingDrops = missingDrops.filter((item) => !matches.some((m) => m.item === item));
      if (missingDrops.length) return browse(folderCursor);
    }
    $('folder-dialog').close();
    editor.focus();
  } catch (e) {
    if (view === owner && version === folderVersion) {
      $('folder-status').textContent = e.message;
      pickerAction();
    }
  }
};
$('no-folder').onclick = () => pickerMode === 'workspace' ? chooseFolder('') : $('folder-dialog').close();

const conversation = $('conversation');
function fileDrag(event) {
  return Array.from(event.dataTransfer.types).some((type) => type === 'Files' || type === 'text/uri-list');
}
conversation.addEventListener('dragover', (e) => {
  if (!fileDrag(e)) return;
  e.preventDefault();
  e.dataTransfer.dropEffect = 'copy';
  conversation.classList.add('file-drag');
}, true);
conversation.addEventListener('dragleave', (e) => {
  if (!conversation.contains(e.relatedTarget)) conversation.classList.remove('file-drag');
});
conversation.addEventListener('drop', async (e) => {
  conversation.classList.remove('file-drag');
  if (!fileDrag(e)) return;
  const drops = droppedItems(e.dataTransfer);
  if (!drops.length) return;
  e.preventDefault();
  e.stopPropagation();
  const owner = view;
  try {
    const paths = drops.filter((item) => item.path).map((item) => item.path);
    if (paths.length) await attachPaths(paths, owner);
    const hiddenPaths = drops.filter((item) => !item.path);
    if (hiddenPaths.length && owner === view) openPicker('drop', hiddenPaths);
  } catch (error) {
    showError(error, owner);
  }
}, true);
let deletion;
function renderAgents() {
  const saved = agents.filter((a) => a.id !== 'base');
  $('manage-agents').hidden = !saved.length;
  const list = $('agent-list');
  list.replaceChildren();
  for (const a of saved) {
    const row = element('div', 'management-row');
    const b = element('button', 'remove-agent', 'Delete');
    b.type = 'button';
    b.setAttribute('aria-label', 'Delete ' + a.name);
    b.onclick = () => {
      deletion = a;
      $('delete-dialog').querySelector('p').textContent =
        'Delete “' + a.name + '”? Its chats will use base on their next turn.';
      $('delete-dialog').showModal();
    };
    row.append(element('span', '', a.name), b);
    list.append(row);
  }
}
$('confirm-delete').onclick = async () => {
  if (!deletion) return;
  $('confirm-delete').disabled = true;
  try {
    await request('agents/' + encodeURIComponent(deletion.id), 'DELETE');
    $('delete-dialog').close();
    deletion = null;
    await refresh();
  } catch (e) {
    $('delete-dialog').close();
    showError(e);
  } finally {
    $('confirm-delete').disabled = false;
  }
};
$('nav-toggle').onclick = () => notify({ type: 'sidebar' });
$('pick-chat').onclick = () => menu($('pick-chat'), 'Choose chat for this pane',
  [{label:'New chat',value:''}, ...chats.map((c) => ({label:c.title,value:c.id}))], view.id,
  (id) => notify({type:'choose-chat',id}), {search:'Filter chats'});
$('close-pane').onclick = () => notify({ type: 'close' });
document.addEventListener('pointerdown', () => notify({ type: 'active' }), true);
document.addEventListener('focusin', () => notify({ type: 'active' }));
window.addEventListener('message', (e) => {
  if (e.origin !== location.origin || e.source !== parent) return;
  switch (e.data.type) {
    case 'workspace':
      applyWorkspace(e.data.data);
      const chat = chats.find((c) => c.id === view.id);
      if (chat && !view.busy && !view.loading && (chat.stream || chat.ready > (view.ready || 0)))
        openChat(chat.id).catch((e) => showError(e));

      break;
    case 'refreshed': {
      const req = refreshRequests.get(e.data.id);
      if (!req) break;
      refreshRequests.delete(e.data.id);
      if (e.data.error) req.reject(Error(e.data.error));
      else req.resolve();
      break;
    }
    case 'layout':
      $('close-pane').hidden = !e.data.multiple;
      break;
    case 'navigate': {
      if (e.data.action === 'chat') {
        openChat(e.data.id).catch((e) => showError(e));
      } else {
        const next = newView();
        if (e.data.action === 'build' && agents.length) next.draft = { ...agents[0].settings };
        switchView(next);
        if (next.draft) $('agent-name').focus();
        else editor.focus();
      }
      notify({ type: 'navigated', ticket: e.data.ticket });
      break;
    }
  }
});
animateTorus($('torus'));
switchView(newView());
new ResizeObserver(() => schedulePaint()).observe(scroll);
new ResizeObserver(() => {
  if (view.comments.length) positionComments();
}).observe($('messages'));
$('messages').addEventListener('toggle', () => {
  if (view.comments.length) positionComments();
}, true);
notify({ type: 'ready' });
