import { element, icon } from './ui.js';

export function attachmentList(items, remove) {
  const list = element('div', 'attachment-list');
  for (const item of items || []) {
    const chip = element('div', 'attachment-chip');
    const name = item.path.split('/').pop() || item.path;
    chip.title = item.path;
    if (item.kind === 'image') {
      const link = element('a');
      link.href = '/api/attachments/preview?path=' + encodeURIComponent(item.path);
      link.target = '_blank';
      link.rel = 'noopener';
      const img = element('img');
      img.alt = 'Preview of ' + name;
      img.loading = 'lazy';
      img.src = link.href;
      link.append(img);
      chip.append(link);
    } else {
      chip.append(icon(item.kind === 'folder' ? 'folder' : 'file'));
    }
    chip.append(element('span', '', name));
    if (remove) {
      const b = element('button', 'icon');
      b.type = 'button';
      b.ariaLabel = 'Remove ' + name;
      b.append(icon('close'));
      b.onclick = () => remove(item);
      chip.append(b);
    }
    list.append(chip);
  }
  return list;
}

// Read drag metadata synchronously while the drop data store is accessible.
// A browser entry's fullPath is relative to the drag root, not the filesystem.
export function droppedItems(transfer) {
  const entries = Array.from(transfer.items || []).filter((i) => i.kind === 'file').map((i) => {
    const entry = i.webkitGetAsEntry?.();
    const file = i.getAsFile();
    return {name: entry?.name || file?.name, folder: entry ? entry.isDirectory : undefined, path: file?.path};
  });
  if (!entries.length) {
    for (const f of transfer.files) entries.push({name: f.name, path: f.path});
  }
  const paths = [];
  for (const line of transfer.getData('text/uri-list').split(/\r?\n/)) {
    try {
      const u = new URL(line);
      if (u.protocol === 'file:' && (!u.hostname || u.hostname === 'localhost'))
        paths.push(decodeURIComponent(u.pathname).replace(/\/$/, '') || '/');
    } catch { /* Ignore non-file drag metadata. */ }
  }
  if (!entries.length) return paths.map((path) => ({path}));
  for (const e of entries) {
    if (!e.path && entries.filter((other) => other.name === e.name).length === 1) {
      const matches = paths.filter((path) => path.split('/').pop() === e.name);
      if (matches.length === 1) e.path = matches[0];
    }
  }
  return entries;
}
