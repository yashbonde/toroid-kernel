export const $ = (id) => document.getElementById(id);
export function element(tag, cls, text) {
  const el = document.createElement(tag);
  if (cls) el.className = cls;
  if (text !== undefined) el.textContent = text;
  return el;
}
export function icon(id) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  const use = document.createElementNS(svg.namespaceURI, 'use');
  use.setAttribute('href', '#i-' + id);
  svg.setAttribute('aria-hidden', 'true');
  svg.append(use);
  return svg;
}
export async function request(path, method = 'GET', data, signal) {
  const res = await fetch('/api/' + path, {
    method,
    signal,
    headers: data ? { 'Content-Type': 'application/json' } : {},
    body: data ? JSON.stringify(data) : undefined,
  });
  if (!res.ok) {
    if (res.status === 401) {
      if (window === parent) window.dispatchEvent(new Event('pair-browser'));
      else parent.postMessage({ type: 'pair-browser' }, location.origin);
    }
    throw Error((await res.text()).trim() || 'Request failed.');
  }
  return res;
}
export function preference(name, fallback) {
  try {
    return localStorage.getItem('toroid.' + name) || fallback;
  } catch {
    return fallback;
  }
}
export function savePreference(name, value) {
  try {
    localStorage.setItem('toroid.' + name, value);
  } catch {}
  appearance();
}
export function appearance() {
  const theme = preference('theme', 'system');
  document.documentElement.dataset.theme =
    theme === 'system'
      ? matchMedia('(prefers-color-scheme:dark)').matches
        ? 'dark'
        : 'light'
      : theme;
  document.documentElement.dataset.width = preference('width', 'default');
}
appearance();
window.addEventListener('storage', appearance);
matchMedia('(prefers-color-scheme:dark)').addEventListener('change', appearance);
document.addEventListener('click', (e) => {
  if (e.target.closest('[data-close]')) e.target.closest('dialog')?.close();
});

let dismiss;
export function closeMenu() {
  dismiss?.();
}
export function menu(anchor, title, choices, selected, choose, { search = false } = {}) {
  closeMenu();
  const panel = element('div', 'choice-menu');
  panel.setAttribute('role', 'dialog');
  panel.setAttribute('aria-label', title);
  const heading = element('div', 'menu-title', title);
  panel.append(heading);
  const list = element('div', 'choice-list');
  let field;
  if (search) {
    field = element('input', 'choice-search');
    field.type = 'search';
    const label = typeof search === 'string' ? search : 'Filter models';
    field.placeholder = label + '…';
    field.setAttribute('aria-label', label);
    panel.append(field);
  }
  panel.append(list);
  document.body.append(panel);
  anchor.setAttribute('aria-expanded', 'true');
  function populate() {
    list.replaceChildren();
    for (const item of choices) {
      if (field && !item.label.toLowerCase().includes(field.value.toLowerCase())) continue;
      const b = element('button', 'choice');
      b.type = 'button';
      b.title = item.label;
      b.setAttribute('aria-pressed', String(item.value === selected));
      b.append(
        element('span', '', item.label),
        element('span', 'check', item.value === selected ? '✓' : ''),
      );
      b.onclick = () => {
        closeMenu();
        choose(item.value);
      };
      list.append(b);
    }
    if (!list.childElementCount) list.append(element('div', 'menu-empty', 'No matching options.'));
  }
  populate();
  const rect = anchor.getBoundingClientRect(),
    gap = 6;
  panel.style.left = Math.max(5, Math.min(rect.left, innerWidth - panel.offsetWidth - 5)) + 'px';
  const above = rect.top > innerHeight - rect.bottom;
  panel.style.maxHeight =
    Math.max(90, (above ? rect.top : innerHeight - rect.bottom) - 2 * gap) + 'px';
  if (above) panel.style.bottom = innerHeight - rect.top + gap + 'px';
  else panel.style.top = rect.bottom + gap + 'px';
  if (field) {
    field.oninput = populate;
    field.focus();
  } else (list.querySelector('[aria-pressed=true]') || list.firstElementChild)?.focus();
  const outside = (e) => {
    if (!panel.contains(e.target) && !anchor.contains(e.target)) closeMenu();
  };
  function keys(e) {
    const buttons = [...list.querySelectorAll('button')],
      i = buttons.indexOf(document.activeElement);
    if (e.key === 'Escape') {
      e.preventDefault();
      closeMenu();
    }
    if (['ArrowUp', 'ArrowDown'].includes(e.key) && buttons.length) {
      e.preventDefault();
      buttons[(i + (e.key === 'ArrowDown' ? 1 : -1) + buttons.length) % buttons.length].focus();
    }
    if (e.key === 'Enter' && document.activeElement === field) {
      e.preventDefault();
      buttons[0]?.click();
    }
    if (e.key === 'Tab') closeMenu();
  }
  panel.addEventListener('keydown', keys);
  document.addEventListener('pointerdown', outside, true);
  dismiss = () => {
    panel.remove();
    anchor.setAttribute('aria-expanded', 'false');
    document.removeEventListener('pointerdown', outside, true);
    dismiss = null;
    anchor.focus({ preventScroll: true });
  };
}
