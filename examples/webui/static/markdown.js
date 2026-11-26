import { renderMarkdown } from './editor.js';

// Retain matching nodes while the Markdown grammar settles. In particular,
// append to a text node when only its tail changed so a reading selection survives.
function syncChildren(dst, src) {
  for (let i = 0; i < src.childNodes.length; i++) {
    const fresh = src.childNodes[i],
      old = dst.childNodes[i];
    if (!old) {
      dst.append(fresh.cloneNode(true));
      continue;
    }
    if (old.isEqualNode(fresh)) continue;
    if (old.nodeName !== fresh.nodeName) {
      old.replaceWith(fresh.cloneNode(true));
      continue;
    }
    if (old.nodeType === Node.TEXT_NODE) {
      if (fresh.data.startsWith(old.data)) old.appendData(fresh.data.slice(old.length));
      else old.data = fresh.data;
    } else if (old.nodeType === Node.ELEMENT_NODE) {
      for (const attr of [...old.attributes])
        if (!fresh.hasAttribute(attr.name)) old.removeAttribute(attr.name);
      for (const attr of fresh.attributes)
        if (old.getAttribute(attr.name) !== attr.value) old.setAttribute(attr.name, attr.value);
      syncChildren(old, fresh);
    }
  }
  while (dst.childNodes.length > src.childNodes.length) dst.lastChild.remove();
}
export function paintMarkdown(body, text) {
  const template = document.createElement('template');
  template.innerHTML = renderMarkdown(text);
  syncChildren(body, template.content);
}
